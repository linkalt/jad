package native

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
)

var (
	memoryTypePattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	memoryFilenamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}-[0-9]{8}T[0-9]{6}\.[0-9]{3}Z-(?:[0-9]{16}-)?[0-9a-f-]{36}\.json$`)
	memoryCursorIDPattern = regexp.MustCompile(`^m_([0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12})$`)
	memoryUUIDPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
	memoryPathSegment     = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// MemoryStoreError describes invalid, missing, or inaccessible memory data.
type MemoryStoreError struct {
	Message string
	// Kind is set for the errors whose answer lists the valid values.
	Kind memoryErrorKind
}

type memoryErrorKind int

const (
	memoryErrorOther memoryErrorKind = iota
	memoryErrorType
	memoryErrorRef
)

func (e *MemoryStoreError) Error() string { return e.Message }

// MemoryConflictError reports the current ETag after a stale conditional update.
type MemoryConflictError struct{ CurrentETag string }

func (e *MemoryConflictError) Error() string {
	return "memory record changed; retrieve it before updating"
}

// MemoryItem is one persistent record. The private value preserves JSON object
// insertion order from the source record for TypeScript-compatible rendering.
type MemoryItem struct {
	Ref     string         `json:"ref"`
	ETag    string         `json:"etag"`
	Data    map[string]any `json:"data"`
	value   *memoryJSONValue
	rawText []byte
}

// MarshalJSON keeps data's original property order while exposing the same
// public {ref, etag, data} record shape.
func (item MemoryItem) MarshalJSON() ([]byte, error) {
	value := item.value
	if value == nil {
		value = memoryValueFromGo(item.Data)
	}
	ref, err := json.Marshal(item.Ref)
	if err != nil {
		return nil, err
	}
	etag, err := json.Marshal(item.ETag)
	if err != nil {
		return nil, err
	}
	encoded, err := memoryJSONCompact(value)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(ref)+len(etag)+len(encoded)+28)
	out = append(out, `{"ref":`...)
	out = append(out, ref...)
	out = append(out, `,"etag":`...)
	out = append(out, etag...)
	out = append(out, `,"data":`...)
	out = append(out, encoded...)
	out = append(out, '}')
	return out, nil
}

// MemoryListResult is a bounded page of memory records.
type MemoryListResult struct {
	Items      []MemoryItem
	NextCursor string
	Warnings   []string
	HasMore    bool
}

type memoryTotals struct{ types, records, bytes, typeRecords int64 }
type memoryLegacyCursor struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Order  string `json:"order"`
	Filter string `json:"filter"`
	Last   string `json:"last"`
}
type memoryStoredCursor struct {
	V                 int              `json:"v"`
	Type              string           `json:"type"`
	Order             string           `json:"order"`
	Filter            map[string]any   `json:"filter"`
	ExcludeAny        []map[string]any `json:"excludeAny,omitempty"`
	FilterFingerprint string           `json:"filterFingerprint"`
	Last              string           `json:"last"`
}
type memoryListOptions struct {
	CreateCursor bool
	ExcludeAny   []map[string]any
}

// MemoryStore stores one JSON file per record beneath a configured root.
// Resolver-owned PathLocks serialize operations across catalogs sharing a root.
type MemoryStore struct {
	root  string
	locks *PathLocks
	now   func() time.Time
	// sizes remembers the byte size of every record file this store has seen, by
	// type directory and file name, so the quota scan stats only files it has not seen.
	// Guarded by the root lock (withLock). A record another store instance rewrites in
	// place keeps its remembered size until this store rewrites it.
	sizes map[string]map[string]int64
}

// NewMemoryStore creates a memory store. An optional shared PathLocks enables
// serialization across independently resolved catalogs using the same root.
func NewMemoryStore(root string, shared ...*PathLocks) *MemoryStore {
	absolute, err := filepath.Abs(root)
	if err == nil {
		root = filepath.Clean(absolute)
	} else {
		root = filepath.Clean(root)
	}
	locks := NewPathLocks()
	if len(shared) > 0 && shared[0] != nil {
		locks = shared[0]
	}
	return &MemoryStore{root: root, locks: locks, now: time.Now, sizes: make(map[string]map[string]int64)}
}

// Create persists a new record using JSON object data.
func (s *MemoryStore) Create(recordType string, data map[string]any, policy sessionfs.NativeMemorySettings) (MemoryItem, error) {
	return s.CreateValue(recordType, memoryValueFromGo(data), data, policy)
}

// CreateValue persists a record from an insertion-order-preserving JSON value.
func (s *MemoryStore) CreateValue(recordType string, value *memoryJSONValue, data map[string]any, policy sessionfs.NativeMemorySettings) (MemoryItem, error) {
	var item MemoryItem
	if err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		if err := s.checkData(value, policy); err != nil {
			return err
		}
		totals, err := s.totals(recordType, policy)
		if err != nil {
			return err
		}
		text, err := memoryJSONPretty(value)
		if err != nil {
			return err
		}
		if totals.typeRecords >= policy.MaxRecordsPerType || totals.records >= policy.MaxTotalRecords || totals.bytes+int64(len(text)) > policy.MaxTotalBytes || (totals.typeRecords == 0 && totals.types >= policy.MaxTypes) {
			return &MemoryStoreError{Message: "memory capacity has been reached"}
		}
		sequence, err := s.nextSequence()
		if err != nil {
			return err
		}
		if _, err := s.typeDirectory(recordType, true); err != nil {
			return err
		}
		name, err := memoryFilename(recordType, sequence, s.now())
		if err != nil {
			return err
		}
		path := filepath.Join(s.root, recordType, name)
		if err := s.atomicWrite(path, []byte(text)); err != nil {
			return err
		}
		s.rememberSize(recordType, name, int64(len(text)))
		item = MemoryItem{Ref: recordType + "/" + name, ETag: memoryETag([]byte(text)), Data: data, value: value, rawText: []byte(text)}
		return nil
	}); err != nil {
		return MemoryItem{}, err
	}
	return item, nil
}

// Get reads one record and returns its current ETag.
func (s *MemoryStore) Get(recordType, ref string, policy sessionfs.NativeMemorySettings) (MemoryItem, error) {
	var item MemoryItem
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		loaded, err := s.load(recordType, ref)
		if err != nil {
			return err
		}
		item = loaded
		return nil
	})
	return item, err
}

// Update replaces record data, optionally requiring an exact current ETag.
func (s *MemoryStore) Update(recordType, ref, ifMatch string, data map[string]any, policy sessionfs.NativeMemorySettings) (MemoryItem, error) {
	return s.UpdateValue(recordType, ref, ifMatch, memoryValueFromGo(data), data, policy)
}

// UpdateValue is the insertion-order-preserving form of Update.
func (s *MemoryStore) UpdateValue(recordType, ref, ifMatch string, value *memoryJSONValue, data map[string]any, policy sessionfs.NativeMemorySettings) (MemoryItem, error) {
	var item MemoryItem
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		if err := s.checkData(value, policy); err != nil {
			return err
		}
		current, err := s.load(recordType, ref)
		if err != nil {
			return err
		}
		if ifMatch != "" && current.ETag != ifMatch {
			return &MemoryConflictError{CurrentETag: current.ETag}
		}
		next, err := memoryJSONPretty(value)
		if err != nil {
			return err
		}
		totals, err := s.totals(recordType, policy)
		if err != nil {
			return err
		}
		if totals.bytes-int64(len(currentText(current)))+int64(len(next)) > policy.MaxTotalBytes {
			return &MemoryStoreError{Message: "memory capacity has been reached"}
		}
		path, err := s.target(recordType, ref)
		if err != nil {
			return err
		}
		if err := s.atomicWrite(path, []byte(next)); err != nil {
			return err
		}
		s.rememberSize(recordType, strings.TrimPrefix(ref, recordType+"/"), int64(len(next)))
		item = MemoryItem{Ref: ref, ETag: memoryETag([]byte(next)), Data: data, value: value, rawText: []byte(next)}
		return nil
	})
	return item, err
}

// List returns a bounded page, preserving TypeScript cursor and ordering rules.
func (s *MemoryStore) List(recordType string, requestedFilter map[string]any, limit int64, requestedOrder, token any, policy sessionfs.NativeMemorySettings) (MemoryListResult, error) {
	return s.list(recordType, requestedFilter, limit, requestedOrder, token, policy, memoryListOptions{CreateCursor: true})
}

func (s *MemoryStore) list(recordType string, requestedFilter map[string]any, limit int64, requestedOrder, token any, policy sessionfs.NativeMemorySettings, options memoryListOptions) (MemoryListResult, error) {
	var result MemoryListResult
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		legacy := parseMemoryLegacyCursor(token)
		var stored *memoryStoredCursor
		if token != nil && legacy == nil {
			var err error
			stored, err = s.readStoredCursor(token, policy)
			if err != nil {
				return err
			}
			if stored == nil {
				return &MemoryStoreError{Message: "memory cursor is invalid"}
			}
		}
		if token != nil && legacy == nil && stored == nil {
			return &MemoryStoreError{Message: "memory cursor is invalid"}
		}
		filter := requestedFilter
		if filter == nil {
			if stored != nil {
				filter = stored.Filter
			} else {
				filter = map[string]any{}
			}
		}
		excludeAny := options.ExcludeAny
		if excludeAny == nil && stored != nil {
			excludeAny = stored.ExcludeAny
		}
		if excludeAny == nil {
			excludeAny = []map[string]any{}
		}
		if int64(len(excludeAny)) > policy.MaxFilterFields {
			return &MemoryStoreError{Message: "memory filter exceeds configured limits (exclude_any)"}
		}
		for _, clause := range excludeAny {
			if clause == nil || len(clause) == 0 {
				return &MemoryStoreError{Message: "memory filter exceeds configured limits (exclude_any)"}
			}
		}
		filterKeys := make([]string, 0, len(filter))
		for key := range filter {
			filterKeys = append(filterKeys, key)
		}
		for _, clause := range excludeAny {
			for key := range clause {
				filterKeys = append(filterKeys, key)
			}
		}
		if int64(len(filterKeys)) > policy.MaxFilterFields {
			return &MemoryStoreError{Message: "memory filter exceeds configured limits (filter/exclude_any)"}
		}
		for _, key := range filterKeys {
			parts := strings.Split(key, ".")
			if int64(len(parts)) > policy.MaxFilterDepth {
				return &MemoryStoreError{Message: "memory filter exceeds configured limits (filter/exclude_any)"}
			}
			for _, part := range parts {
				if !memoryPathSegment.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
					return &MemoryStoreError{Message: "memory filter exceeds configured limits (filter/exclude_any)"}
				}
			}
		}
		order, err := memoryOrder(requestedOrder, legacy, stored)
		if err != nil {
			return err
		}
		fp, err := memoryFilterFingerprint(filter, excludeAny)
		if err != nil {
			return err
		}
		if legacy != nil && (legacy.Type != recordType || legacy.Order != order || legacy.Filter != fp) {
			return &MemoryStoreError{Message: "memory cursor is invalid"}
		}
		if stored != nil && (stored.Type != recordType || stored.Order != order || stored.FilterFingerprint != fp) {
			return &MemoryStoreError{Message: "memory cursor is invalid"}
		}
		last := ""
		if legacy != nil {
			last = legacy.Last
		} else if stored != nil {
			last = stored.Last
		}
		if limit < 1 {
			limit = policy.ListLimit
		}
		if limit > policy.MaxListLimit {
			limit = policy.MaxListLimit
		}
		names, err := s.files(recordType)
		if err != nil {
			return err
		}
		sort.Slice(names, func(i, j int) bool {
			if order == "newest" {
				return names[i] > names[j]
			}
			return names[i] < names[j]
		})
		positionIndex := -1
		if last != "" {
			base := strings.TrimPrefix(last, recordType+"/")
			for index, name := range names {
				if name == base {
					positionIndex = index
					break
				}
			}
			if positionIndex < 0 {
				return &MemoryStoreError{Message: "memory cursor position was not found"}
			}
		}
		start := positionIndex
		if legacy != nil && legacy.V == 1 {
			start--
		}
		items := make([]MemoryItem, 0)
		itemsJSONBytes := int64(2)
		warnings := make([]string, 0)
		var scanned, scannedBytes int64
		position := ""
		stopped := false
		for _, name := range names[max(0, start+1):] {
			if scanned >= policy.MaxScanRecords {
				stopped = true
				break
			}
			ref := recordType + "/" + name
			scanned++
			text, readErr := s.readRecordText(recordType, name)
			if readErr != nil {
				position = ref
				if len(warnings) < 8 {
					warnings = append(warnings, "unreadable record: "+ref)
				}
				continue
			}
			if scannedBytes+int64(len(text)) > policy.MaxScanBytes {
				stopped = true
				break
			}
			scannedBytes += int64(len(text))
			value, parseErr := parseMemoryJSON([]byte(text))
			if parseErr != nil || value.kind != memoryObject {
				position = ref
				if len(warnings) < 8 {
					warnings = append(warnings, "unreadable record: "+ref)
				}
				continue
			}
			data := memoryGoMap(value)
			if !memoryMatches(data, filter, excludeAny) {
				position = ref
				continue
			}
			candidate := MemoryItem{Ref: ref, ETag: memoryETag([]byte(text)), Data: data, value: value, rawText: []byte(text)}
			if int64(len(items)) >= limit {
				stopped = true
				break
			}
			candidateJSON, err := memoryJSONCompact(memoryValueFromGo(candidate))
			if err != nil {
				return err
			}
			nextItemsJSONBytes := itemsJSONBytes + int64(len(candidateJSON))
			if len(items) > 0 {
				nextItemsJSONBytes++
			}
			if nextItemsJSONBytes > policy.MaxListBytes {
				stopped = true
				break
			}
			items = append(items, candidate)
			itemsJSONBytes = nextItemsJSONBytes
			position = ref
		}
		nextCursor := ""
		if stopped && position != "" && options.CreateCursor {
			nextCursor, err = s.createCursor(recordType, order, filter, excludeAny, fp, position, policy)
			if err != nil {
				return err
			}
		}
		result = MemoryListResult{Items: items, NextCursor: nextCursor, Warnings: warnings, HasMore: stopped}
		return nil
	})
	return result, err
}

func (s *MemoryStore) withLock(operation func() error) error {
	if s == nil {
		return &MemoryStoreError{Message: "memory storage is not available"}
	}
	release := s.locks.Lock("memory:" + s.root)
	defer release()
	return operation()
}

// RecentRefs returns up to limit refs of one type, newest first.
func (s *MemoryStore) RecentRefs(recordType string, limit int64) []string {
	var refs []string
	_ = s.withLock(func() error {
		names, err := s.files(recordType)
		if err != nil {
			return err
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		if int64(len(names)) > limit {
			names = names[:limit]
		}
		for _, name := range names {
			refs = append(refs, recordType+"/"+name)
		}
		return nil
	})
	return refs
}

func (s *MemoryStore) checkType(recordType string, policy sessionfs.NativeMemorySettings) error {
	if !memoryTypePattern.MatchString(recordType) {
		return &MemoryStoreError{Message: "memory type is invalid", Kind: memoryErrorType}
	}
	if policy.AllowedTypes != nil {
		for _, allowed := range policy.AllowedTypes {
			if allowed == recordType {
				return nil
			}
		}
		return &MemoryStoreError{Message: "memory type is not allowed", Kind: memoryErrorType}
	}
	return nil
}

func (s *MemoryStore) checkData(value *memoryJSONValue, policy sessionfs.NativeMemorySettings) error {
	if value == nil || value.kind != memoryObject || memoryJSONDepth(value) > policy.MaxJSONDepth {
		return &MemoryStoreError{Message: "memory data must be a JSON object within the configured depth"}
	}
	text, err := memoryJSONPretty(value)
	if err != nil {
		return err
	}
	if int64(len(text)) > policy.MaxRecordBytes {
		return &MemoryStoreError{Message: "memory data exceeds the configured record size"}
	}
	return nil
}

func (s *MemoryStore) ensureRoot() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(s.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return &MemoryStoreError{Message: "memory storage path is not a safe directory"}
	}
	return nil
}

func (s *MemoryStore) typeDirectory(recordType string, create bool) (string, error) {
	if err := s.ensureRoot(); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, recordType)
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", &MemoryStoreError{Message: "memory type storage is not a safe directory"}
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		if errors.Is(err, os.ErrNotExist) && !create {
			return "", nil
		}
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", &MemoryStoreError{Message: "memory type storage is not a safe directory"}
	}
	return path, nil
}

func (s *MemoryStore) files(recordType string) ([]string, error) {
	return s.filesKnowing(recordType, nil)
}

// filesKnowing is files that skips the name check for a name in known: the store has
// already validated that name as a record file, and the entry is still a regular file.
func (s *MemoryStore) filesKnowing(recordType string, known map[string]int64) ([]string, error) {
	directory, err := s.typeDirectory(recordType, false)
	if err != nil || directory == "" {
		return []string{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if _, seen := known[name]; seen {
			names = append(names, name)
			continue
		}
		if memoryFilenamePattern.MatchString(name) && strings.HasPrefix(name, recordType+"-") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (s *MemoryStore) rememberSize(recordType, name string, size int64) {
	if s.sizes[recordType] == nil {
		s.sizes[recordType] = make(map[string]int64)
	}
	s.sizes[recordType][name] = size
}

func (s *MemoryStore) totals(recordType string, policy sessionfs.NativeMemorySettings) (memoryTotals, error) {
	var totals memoryTotals
	if err := s.ensureRoot(); err != nil {
		return totals, err
	}
	directories, err := os.ReadDir(s.root)
	if err != nil {
		return totals, err
	}
	scanned := make(map[string]map[string]int64, len(directories))
	for _, directory := range directories {
		if !directory.IsDir() || !memoryTypePattern.MatchString(directory.Name()) {
			continue
		}
		totals.types++
		known := s.sizes[directory.Name()]
		names, err := s.filesKnowing(directory.Name(), known)
		if err != nil {
			return memoryTotals{}, err
		}
		current := make(map[string]int64, len(names))
		for _, name := range names {
			size, seen := known[name]
			if !seen {
				info, err := os.Stat(filepath.Join(s.root, directory.Name(), name))
				if err != nil {
					return memoryTotals{}, err
				}
				size = info.Size()
			}
			current[name] = size
			totals.records++
			totals.bytes += size
			if directory.Name() == recordType {
				totals.typeRecords++
			}
			if totals.records > policy.MaxScanRecords || totals.bytes > policy.MaxScanBytes {
				return memoryTotals{}, &MemoryStoreError{Message: "memory quota scan exceeded its configured budget"}
			}
		}
		scanned[directory.Name()] = current
	}
	s.sizes = scanned
	return totals, nil
}

func (s *MemoryStore) target(recordType, ref string) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] != recordType || !memoryFilenamePattern.MatchString(parts[1]) || !strings.HasPrefix(parts[1], recordType+"-") {
		return "", &MemoryStoreError{Message: "memory ref is invalid for the selected type", Kind: memoryErrorRef}
	}
	return filepath.Join(s.root, recordType, parts[1]), nil
}

func (s *MemoryStore) load(recordType, ref string) (MemoryItem, error) {
	path, err := s.target(recordType, ref)
	if err != nil {
		return MemoryItem{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return MemoryItem{}, &MemoryStoreError{Message: "memory record was not found", Kind: memoryErrorRef}
	}
	if err != nil {
		return MemoryItem{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return MemoryItem{}, &MemoryStoreError{Message: "memory record is not a safe file"}
	}
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return MemoryItem{}, &MemoryStoreError{Message: "memory record was not found", Kind: memoryErrorRef}
	}
	if err != nil {
		return MemoryItem{}, err
	}
	value, err := parseMemoryJSON(text)
	if err != nil || value.kind != memoryObject {
		return MemoryItem{}, &MemoryStoreError{Message: "memory record is malformed"}
	}
	return MemoryItem{Ref: ref, ETag: memoryETag(text), Data: memoryGoMap(value), value: value, rawText: append([]byte(nil), text...)}, nil
}

func (s *MemoryStore) readRecordText(recordType, name string) (string, error) {
	path := filepath.Join(s.root, recordType, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("not a regular memory record")
	}
	text, err := os.ReadFile(path)
	return string(text), err
}

func (s *MemoryStore) atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	id, err := memoryUUID()
	if err != nil {
		return err
	}
	temporary := path + "." + id + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func (s *MemoryStore) nextSequence() (uint64, error) {
	path := filepath.Join(s.root, ".memory-sequence")
	current := uint64(0)
	data, err := os.ReadFile(path)
	if err == nil {
		text := strings.TrimSuffix(string(data), "\n")
		if text == "" || strings.TrimSpace(text) != text || !regexp.MustCompile(`^[0-9]+$`).MatchString(text) {
			return 0, &MemoryStoreError{Message: "memory sequence is malformed"}
		}
		current, err = strconv.ParseUint(text, 10, 64)
		if err != nil || current > math.MaxInt64 {
			return 0, &MemoryStoreError{Message: "memory sequence is malformed"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if current >= 9_007_199_254_740_991 {
		return 0, &MemoryStoreError{Message: "memory sequence is exhausted"}
	}
	next := current + 1
	if err := s.atomicWrite(path, []byte(strconv.FormatUint(next, 10)+"\n")); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *MemoryStore) cursorDirectory(create bool) (string, error) {
	if err := s.ensureRoot(); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, ".memory-cursors")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", &MemoryStoreError{Message: "memory cursor storage is not a safe directory"}
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		if errors.Is(err, os.ErrNotExist) && !create {
			return "", nil
		}
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", &MemoryStoreError{Message: "memory cursor storage is not a safe directory"}
	}
	return path, nil
}

func (s *MemoryStore) pruneCursors(directory string, policy sessionfs.NativeMemorySettings) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	now := time.Now()
	type cursorFile struct {
		name  string
		mtime time.Time
	}
	active := make([]cursorFile, 0)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !memoryUUIDPattern.MatchString(strings.TrimSuffix(entry.Name(), ".json")) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) >= time.Duration(policy.CursorTTLMS)*time.Millisecond {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			active = append(active, cursorFile{name: entry.Name(), mtime: info.ModTime()})
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].mtime.Before(active[j].mtime) })
	keepFrom := max(0, len(active)-int(policy.MaxCursors-1))
	for _, entry := range active[:keepFrom] {
		if err := os.Remove(filepath.Join(directory, entry.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) readStoredCursor(value any, policy sessionfs.NativeMemorySettings) (*memoryStoredCursor, error) {
	token, ok := value.(string)
	if !ok || len(token) > 64 || !memoryCursorIDPattern.MatchString(token) {
		return nil, &MemoryStoreError{Message: "memory cursor is invalid"}
	}
	match := memoryCursorIDPattern.FindStringSubmatch(token)
	directory, err := s.cursorDirectory(false)
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return nil, &MemoryStoreError{Message: "memory cursor was not found"}
	}
	path := filepath.Join(directory, strings.ToLower(match[1])+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &MemoryStoreError{Message: "memory cursor was not found"}
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, &MemoryStoreError{Message: "memory cursor is not a safe file"}
	}
	if time.Since(info.ModTime()) >= time.Duration(policy.CursorTTLMS)*time.Millisecond {
		_ = os.Remove(path)
		return nil, &MemoryStoreError{Message: "memory cursor expired"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cursor memoryStoredCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.V != 1 || cursor.Type == "" || (cursor.Order != "newest" && cursor.Order != "oldest") || cursor.Filter == nil || cursor.Last == "" || cursor.FilterFingerprint == "" {
		return nil, &MemoryStoreError{Message: "memory cursor is invalid"}
	}
	fingerprint, err := memoryFilterFingerprint(cursor.Filter, cursor.ExcludeAny)
	if err != nil || fingerprint != cursor.FilterFingerprint {
		return nil, &MemoryStoreError{Message: "memory cursor is invalid"}
	}
	return &cursor, nil
}

func (s *MemoryStore) createCursor(recordType, order string, filter map[string]any, excludeAny []map[string]any, filterFingerprint, last string, policy sessionfs.NativeMemorySettings) (string, error) {
	cursor := memoryStoredCursor{V: 1, Type: recordType, Order: order, Filter: filter, FilterFingerprint: filterFingerprint, Last: last}
	if len(excludeAny) > 0 {
		cursor.ExcludeAny = excludeAny
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	if int64(len(data)) > policy.MaxCursorBytes {
		return "", &MemoryStoreError{Message: "memory cursor exceeds configured size"}
	}
	directory, err := s.cursorDirectory(true)
	if err != nil {
		return "", err
	}
	if err := s.pruneCursors(directory, policy); err != nil {
		return "", err
	}
	id, err := memoryUUID()
	if err != nil {
		return "", err
	}
	if err := s.atomicWrite(filepath.Join(directory, id+".json"), data); err != nil {
		return "", err
	}
	return "m_" + id, nil
}

func parseMemoryLegacyCursor(value any) *memoryLegacyCursor {
	token, ok := value.(string)
	if !ok || len(token) > 2048 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil
	}
	var cursor memoryLegacyCursor
	if json.Unmarshal(data, &cursor) != nil || (cursor.V != 1 && cursor.V != 2) || cursor.Type == "" || (cursor.Order != "newest" && cursor.Order != "oldest") || cursor.Filter == "" || cursor.Last == "" {
		return nil
	}
	return &cursor
}

func memoryOrder(requested any, legacy *memoryLegacyCursor, stored *memoryStoredCursor) (string, error) {
	if requested != nil {
		order, ok := requested.(string)
		if !ok || (order != "newest" && order != "oldest") {
			return "", &MemoryStoreError{Message: "memory order is invalid"}
		}
		return order, nil
	}
	if legacy != nil {
		return legacy.Order, nil
	}
	if stored != nil {
		return stored.Order, nil
	}
	return "newest", nil
}

func memoryFilename(recordType string, sequence uint64, now time.Time) (string, error) {
	id, err := memoryUUID()
	if err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405.000Z")
	return fmt.Sprintf("%s-%s-%016d-%s.json", recordType, stamp, sequence, id), nil
}

func memoryUUID() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func memoryETag(text []byte) string {
	digest := sha256.Sum256(text)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func currentText(item MemoryItem) []byte {
	if item.rawText != nil {
		return item.rawText
	}
	if item.value == nil {
		encoded, _ := memoryJSONPretty(memoryValueFromGo(item.Data))
		return []byte(encoded)
	}
	encoded, _ := memoryJSONPretty(item.value)
	return []byte(encoded)
}

func memoryListItemsSize(items []MemoryItem) (int, error) {
	encoded, err := memoryJSONCompact(memoryArrayValueFromItems(items))
	return len(encoded), err
}

func memoryFilterFingerprint(filter map[string]any, excludeAny []map[string]any) (string, error) {
	filterValue := memoryValueFromGo(filter)
	canonical, err := memoryJSONCanonical(filterValue)
	if err != nil {
		return "", err
	}
	if len(excludeAny) > 0 {
		clauses := make([]*memoryJSONValue, 0, len(excludeAny))
		for _, clause := range excludeAny {
			clauses = append(clauses, memoryValueFromGo(clause))
		}
		encoded, err := memoryJSONCanonical(memoryArrayValue(clauses))
		if err != nil {
			return "", err
		}
		canonical += "\x00" + encoded
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:]), nil
}

func memoryMatches(data, filter map[string]any, excludeAny []map[string]any) bool {
	if !memoryMatchesClause(data, filter) {
		return false
	}
	for _, clause := range excludeAny {
		if memoryMatchesClause(data, clause) {
			return false
		}
	}
	return true
}

func memoryMatchesClause(data, clause map[string]any) bool {
	for key, expected := range clause {
		actual, found := memoryValueAt(data, key)
		if !found || !memoryDeepEqual(actual, expected) {
			return false
		}
	}
	return true
}

func memoryValueAt(data map[string]any, path string) (any, bool) {
	var current any = data
	for _, part := range strings.Split(path, ".") {
		if !memoryPathSegment.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
			return nil, false
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func memoryDeepEqual(left, right any) bool {
	leftCanonical, leftErr := memoryJSONCanonical(memoryValueFromGo(left))
	rightCanonical, rightErr := memoryJSONCanonical(memoryValueFromGo(right))
	return leftErr == nil && rightErr == nil && leftCanonical == rightCanonical
}

func memoryListContentText(result MemoryListResult) (string, error) {
	pairs := []memoryJSONPair{{key: "items", value: memoryArrayValueFromItems(result.Items)}}
	if result.NextCursor != "" {
		pairs = append(pairs, memoryJSONPair{key: "next_cursor", value: memoryValueFromGo(result.NextCursor)})
	}
	if len(result.Warnings) > 0 {
		warnings := make([]*memoryJSONValue, 0, len(result.Warnings))
		for _, warning := range result.Warnings {
			warnings = append(warnings, memoryValueFromGo(warning))
		}
		pairs = append(pairs, memoryJSONPair{key: "warnings", value: memoryArrayValue(warnings)})
	}
	var output bytes.Buffer
	if err := writeMemoryJSON(&output, memoryObjectValue(pairs), 2, 0); err != nil {
		return "", err
	}
	return output.String(), nil
}
