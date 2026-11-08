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
	recordsTypePattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	recordsFilenamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}-[0-9]{8}T[0-9]{6}\.[0-9]{3}Z-(?:[0-9]{16}-)?[0-9a-f-]{36}\.json$`)
	recordsCursorIDPattern = regexp.MustCompile(`^m_([0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12})$`)
	recordsUUIDPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-(?:[0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$`)
	recordsSequencePattern = regexp.MustCompile(`^[0-9]+$`)
	recordsPathSegment     = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// RecordsStoreError describes invalid, missing, or inaccessible record data.
type RecordsStoreError struct {
	Message string
	// Kind is set for the errors whose answer lists the valid values.
	Kind recordsErrorKind
}

type recordsErrorKind int

const (
	recordsErrorOther recordsErrorKind = iota
	recordsErrorType
	recordsErrorRef
)

func (e *RecordsStoreError) Error() string { return e.Message }

// RecordsConflictError reports the current ETag after a stale conditional update.
type RecordsConflictError struct{ CurrentETag string }

func (e *RecordsConflictError) Error() string {
	return "record changed; retrieve it before updating"
}

// RecordsItem is one persistent record. The private value preserves JSON object
// insertion order from the source record for TypeScript-compatible rendering.
type RecordsItem struct {
	Ref     string         `json:"ref"`
	ETag    string         `json:"etag"`
	Data    map[string]any `json:"data"`
	value   *recordsJSONValue
	rawText []byte
}

// MarshalJSON keeps data's original property order while exposing the same
// public {ref, etag, data} record shape.
func (item RecordsItem) MarshalJSON() ([]byte, error) {
	value := item.value
	if value == nil {
		value = recordsValueFromGo(item.Data)
	}
	ref, err := json.Marshal(item.Ref)
	if err != nil {
		return nil, err
	}
	etag, err := json.Marshal(item.ETag)
	if err != nil {
		return nil, err
	}
	encoded, err := recordsJSONCompact(value)
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

// RecordsListResult is a bounded page of records.
type RecordsListResult struct {
	Items      []RecordsItem
	NextCursor string
	Warnings   []string
	HasMore    bool
}

type recordsTotals struct{ types, records, bytes, typeRecords int64 }
type recordsLegacyCursor struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Order  string `json:"order"`
	Filter string `json:"filter"`
	Last   string `json:"last"`
}
type recordsStoredCursor struct {
	V                 int              `json:"v"`
	Type              string           `json:"type"`
	Order             string           `json:"order"`
	Filter            map[string]any   `json:"filter"`
	ExcludeAny        []map[string]any `json:"excludeAny,omitempty"`
	FilterFingerprint string           `json:"filterFingerprint"`
	Last              string           `json:"last"`
	// Query is the normalized query of a search cursor; empty for a list cursor.
	Query string `json:"query,omitempty"`
}
type recordsListOptions struct {
	CreateCursor bool
	ExcludeAny   []map[string]any
	// Query, when set, keeps only records whose string and number values contain every
	// term (see recordsSearchTerms).
	Query string
}

// RecordsStore stores one JSON file per record beneath a configured root.
// Resolver-owned PathLocks serialize operations across catalogs sharing a root.
type RecordsStore struct {
	root  string
	locks *PathLocks
	now   func() time.Time
	// sizes remembers the byte size of every record file this store has seen, by
	// type directory and file name, so the quota scan stats only files it has not seen.
	// Guarded by the root lock (withLock). A record another store instance rewrites in
	// place keeps its remembered size until this store rewrites it.
	sizes map[string]map[string]int64
}

// NewRecordsStore creates a records store. An optional shared PathLocks enables
// serialization across independently resolved catalogs using the same root.
func NewRecordsStore(root string, shared ...*PathLocks) *RecordsStore {
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
	return &RecordsStore{root: root, locks: locks, now: time.Now, sizes: make(map[string]map[string]int64)}
}

// Create persists a new record using JSON object data.
func (s *RecordsStore) Create(recordType string, data map[string]any, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	return s.CreateValue(recordType, recordsValueFromGo(data), data, policy)
}

// CreateValue persists a record from an insertion-order-preserving JSON value.
func (s *RecordsStore) CreateValue(recordType string, value *recordsJSONValue, data map[string]any, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	var item RecordsItem
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
		text, err := recordsJSONPretty(value)
		if err != nil {
			return err
		}
		if totals.typeRecords >= policy.MaxRecordsPerType || totals.records >= policy.MaxTotalRecords || totals.bytes+int64(len(text)) > policy.MaxTotalBytes || (totals.typeRecords == 0 && totals.types >= policy.MaxTypes) {
			return &RecordsStoreError{Message: "records capacity has been reached"}
		}
		sequence, err := s.nextSequence()
		if err != nil {
			return err
		}
		if _, err := s.typeDirectory(recordType, true); err != nil {
			return err
		}
		name, err := recordsFilename(recordType, sequence, s.now())
		if err != nil {
			return err
		}
		path := filepath.Join(s.root, recordType, name)
		if err := s.atomicWrite(path, []byte(text)); err != nil {
			return err
		}
		s.rememberSize(recordType, name, int64(len(text)))
		item = RecordsItem{Ref: recordType + "/" + name, ETag: recordsETag([]byte(text)), Data: data, value: value, rawText: []byte(text)}
		return nil
	}); err != nil {
		return RecordsItem{}, err
	}
	return item, nil
}

// Get reads one record and returns its current ETag.
func (s *RecordsStore) Get(recordType, ref string, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	var item RecordsItem
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
func (s *RecordsStore) Update(recordType, ref, ifMatch string, data map[string]any, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	return s.UpdateValue(recordType, ref, ifMatch, recordsValueFromGo(data), data, policy)
}

// RepairLegacyRecord rewrites one record written with the doubled escapes. It changes
// the record only when recordsLegacyEscapeText proves it, and keeps a sibling backup until
// the rewrite succeeds. It reports false, without writing, for a record that is valid JSON.
func (s *RecordsStore) RepairLegacyRecord(recordType, ref string, policy sessionfs.NativeRecordsSettings) (bool, error) {
	repaired := false
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		path, err := s.target(recordType, ref)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return &RecordsStoreError{Message: "record was not found", Kind: recordsErrorRef}
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return &RecordsStoreError{Message: "record is not a safe file"}
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := parseRecordsJSON(text); err == nil {
			return nil
		}
		undone, _, ok := recordsLegacyEscapeText(text)
		if !ok {
			return &RecordsStoreError{Message: "record is malformed"}
		}
		backup := path + ".legacy-backup"
		if _, err := os.Lstat(backup); err == nil {
			return &RecordsStoreError{Message: "records repair backup already exists; remove it before repairing"}
		}
		if err := s.atomicWrite(backup, text); err != nil {
			return err
		}
		if err := s.atomicWrite(path, undone); err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		s.rememberSize(recordType, strings.TrimPrefix(ref, recordType+"/"), int64(len(undone)))
		repaired = true
		return nil
	})
	return repaired, err
}

// UpdateValue is the insertion-order-preserving form of Update.
func (s *RecordsStore) UpdateValue(recordType, ref, ifMatch string, value *recordsJSONValue, data map[string]any, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	var item RecordsItem
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
			return &RecordsConflictError{CurrentETag: current.ETag}
		}
		next, err := recordsJSONPretty(value)
		if err != nil {
			return err
		}
		totals, err := s.totals(recordType, policy)
		if err != nil {
			return err
		}
		if totals.bytes-int64(len(currentText(current)))+int64(len(next)) > policy.MaxTotalBytes {
			return &RecordsStoreError{Message: "records capacity has been reached"}
		}
		path, err := s.target(recordType, ref)
		if err != nil {
			return err
		}
		if err := s.atomicWrite(path, []byte(next)); err != nil {
			return err
		}
		s.rememberSize(recordType, strings.TrimPrefix(ref, recordType+"/"), int64(len(next)))
		item = RecordsItem{Ref: ref, ETag: recordsETag([]byte(next)), Data: data, value: value, rawText: []byte(next)}
		return nil
	})
	return item, err
}

// List returns a bounded page, preserving TypeScript cursor and ordering rules.
func (s *RecordsStore) List(recordType string, requestedFilter map[string]any, limit int64, requestedOrder, token any, policy sessionfs.NativeRecordsSettings) (RecordsListResult, error) {
	return s.list(recordType, requestedFilter, limit, requestedOrder, token, policy, recordsListOptions{CreateCursor: true})
}

// Search is List that keeps only the records whose string and number values contain
// every whitespace-separated term of query, compared case-insensitively. Its cursor
// carries the query, so it continues only the same search.
func (s *RecordsStore) Search(recordType, query string, requestedFilter map[string]any, limit int64, requestedOrder, token any, policy sessionfs.NativeRecordsSettings) (RecordsListResult, error) {
	if int64(len(query)) > policy.MaxQueryBytes {
		return RecordsListResult{}, &RecordsStoreError{Message: fmt.Sprintf("records search query exceeds the configured %d bytes", policy.MaxQueryBytes)}
	}
	terms := recordsSearchTerms(query)
	if len(terms) == 0 {
		return RecordsListResult{}, &RecordsStoreError{Message: "records search query has no words"}
	}
	return s.list(recordType, requestedFilter, limit, requestedOrder, token, policy, recordsListOptions{CreateCursor: true, Query: strings.Join(terms, " ")})
}

// Delete removes one record. With ifMatch set, the record must still have that ETag.
// It returns the ref and the ETag the record had. A malformed record can be deleted too.
// The type's directory is removed with its last record, so it no longer counts toward
// max_types.
func (s *RecordsStore) Delete(recordType, ref, ifMatch string, policy sessionfs.NativeRecordsSettings) (RecordsItem, error) {
	var item RecordsItem
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		path, err := s.target(recordType, ref)
		if err != nil {
			return err
		}
		text, err := s.readRecordText(recordType, filepath.Base(path))
		if errors.Is(err, os.ErrNotExist) {
			return &RecordsStoreError{Message: "record was not found", Kind: recordsErrorRef}
		}
		if err != nil {
			return &RecordsStoreError{Message: "record is not a safe file"}
		}
		etag := recordsETag(text)
		// readRecordText also refuses a symlink or directory under a record name; those
		// are left in place for a person to look at.
		if ifMatch != "" && etag != ifMatch {
			return &RecordsConflictError{CurrentETag: etag}
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &RecordsStoreError{Message: "record was not found", Kind: recordsErrorRef}
			}
			return err
		}
		delete(s.sizes[recordType], filepath.Base(path))
		// Fails, harmlessly, while the directory still holds anything.
		_ = os.Remove(filepath.Dir(path))
		item = RecordsItem{Ref: ref, ETag: etag}
		return nil
	})
	return item, err
}

func (s *RecordsStore) list(recordType string, requestedFilter map[string]any, limit int64, requestedOrder, token any, policy sessionfs.NativeRecordsSettings, options recordsListOptions) (RecordsListResult, error) {
	var result RecordsListResult
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		legacy := parseRecordsLegacyCursor(token)
		var stored *recordsStoredCursor
		if token != nil && legacy == nil {
			var err error
			stored, err = s.readStoredCursor(token, policy)
			if err != nil {
				return err
			}
			if stored == nil {
				return &RecordsStoreError{Message: "record cursor is invalid"}
			}
		}
		if token != nil && legacy == nil && stored == nil {
			return &RecordsStoreError{Message: "record cursor is invalid"}
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
			return &RecordsStoreError{Message: "records filter exceeds configured limits (exclude_any)"}
		}
		for _, clause := range excludeAny {
			if clause == nil || len(clause) == 0 {
				return &RecordsStoreError{Message: "records filter exceeds configured limits (exclude_any)"}
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
			return &RecordsStoreError{Message: "records filter exceeds configured limits (filter/exclude_any)"}
		}
		for _, key := range filterKeys {
			parts := strings.Split(key, ".")
			if int64(len(parts)) > policy.MaxFilterDepth {
				return &RecordsStoreError{Message: "records filter exceeds configured limits (filter/exclude_any)"}
			}
			for _, part := range parts {
				if !recordsPathSegment.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
					return &RecordsStoreError{Message: "records filter exceeds configured limits (filter/exclude_any)"}
				}
			}
		}
		order, err := recordsOrder(requestedOrder, legacy, stored)
		if err != nil {
			return err
		}
		fp, err := recordsCursorFingerprint(filter, excludeAny, options.Query)
		if err != nil {
			return err
		}
		if legacy != nil && (legacy.Type != recordType || legacy.Order != order || legacy.Filter != fp) {
			return &RecordsStoreError{Message: "record cursor is invalid"}
		}
		if stored != nil && (stored.Type != recordType || stored.Order != order || stored.FilterFingerprint != fp) {
			return &RecordsStoreError{Message: "record cursor is invalid"}
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
		// The cursor position is the last record of the previous page. Record names sort in
		// creation order, so a position that was deleted since still has its place: the page
		// goes on after the last name that sorts before it.
		positionIndex := -1
		positionFound := false
		if last != "" {
			base := strings.TrimPrefix(last, recordType+"/")
			for index, name := range names {
				if name == base {
					positionIndex = index
					positionFound = true
					break
				}
				if (order == "newest" && name < base) || (order == "oldest" && name > base) {
					break
				}
				positionIndex = index
			}
		}
		start := positionIndex
		if legacy != nil && legacy.V == 1 && positionFound {
			start--
		}
		var terms []string
		if options.Query != "" {
			terms = strings.Split(options.Query, " ")
		}
		items := make([]RecordsItem, 0)
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
			value, parseErr := parseStoredRecordsJSON(text)
			if parseErr != nil || value.kind != recordsObject {
				position = ref
				if len(warnings) < 8 {
					warnings = append(warnings, "unreadable record: "+ref)
				}
				continue
			}
			data := recordsGoMap(value)
			if !recordsMatches(data, filter, excludeAny) || !recordsContainsTerms(value, terms) {
				position = ref
				continue
			}
			// text is this record's own buffer and nothing writes to it, so the item keeps it.
			candidate := RecordsItem{Ref: ref, ETag: recordsETag(text), Data: data, value: value, rawText: text}
			if int64(len(items)) >= limit {
				stopped = true
				break
			}
			candidateJSON, err := recordsJSONCompact(recordsValueFromGo(candidate))
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
			nextCursor, err = s.createCursor(recordType, order, filter, excludeAny, options.Query, fp, position, policy)
			if err != nil {
				return err
			}
		}
		result = RecordsListResult{Items: items, NextCursor: nextCursor, Warnings: warnings, HasMore: stopped}
		return nil
	})
	return result, err
}

func (s *RecordsStore) withLock(operation func() error) error {
	if s == nil {
		return &RecordsStoreError{Message: "records storage is not available"}
	}
	release := s.locks.Lock("records:" + s.root)
	defer release()
	return operation()
}

// RecentRefs returns up to limit refs of one type, newest first.
func (s *RecordsStore) RecentRefs(recordType string, limit int64) []string {
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

func (s *RecordsStore) checkType(recordType string, policy sessionfs.NativeRecordsSettings) error {
	if !recordsTypePattern.MatchString(recordType) {
		return &RecordsStoreError{Message: "record type is invalid", Kind: recordsErrorType}
	}
	if policy.AllowedTypes != nil {
		for _, allowed := range policy.AllowedTypes {
			if allowed == recordType {
				return nil
			}
		}
		return &RecordsStoreError{Message: "record type is not allowed", Kind: recordsErrorType}
	}
	return nil
}

func (s *RecordsStore) checkData(value *recordsJSONValue, policy sessionfs.NativeRecordsSettings) error {
	if value == nil || value.kind != recordsObject || recordsJSONDepth(value) > policy.MaxJSONDepth {
		return &RecordsStoreError{Message: "record data must be a JSON object within the configured depth"}
	}
	text, err := recordsJSONPretty(value)
	if err != nil {
		return err
	}
	if int64(len(text)) > policy.MaxRecordBytes {
		return &RecordsStoreError{Message: "record data exceeds the configured record size"}
	}
	return nil
}

func (s *RecordsStore) ensureRoot() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(s.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return &RecordsStoreError{Message: "records storage path is not a safe directory"}
	}
	return nil
}

func (s *RecordsStore) typeDirectory(recordType string, create bool) (string, error) {
	if err := s.ensureRoot(); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, recordType)
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", &RecordsStoreError{Message: "record type storage is not a safe directory"}
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
		return "", &RecordsStoreError{Message: "record type storage is not a safe directory"}
	}
	return path, nil
}

func (s *RecordsStore) files(recordType string) ([]string, error) {
	return s.filesKnowing(recordType, nil)
}

// filesKnowing is files that skips the name check for a name in known: the store has
// already validated that name as a record file, and the entry is still a regular file.
func (s *RecordsStore) filesKnowing(recordType string, known map[string]int64) ([]string, error) {
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
		if recordsFilenamePattern.MatchString(name) && strings.HasPrefix(name, recordType+"-") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (s *RecordsStore) rememberSize(recordType, name string, size int64) {
	if s.sizes[recordType] == nil {
		s.sizes[recordType] = make(map[string]int64)
	}
	s.sizes[recordType][name] = size
}

func (s *RecordsStore) totals(recordType string, policy sessionfs.NativeRecordsSettings) (recordsTotals, error) {
	var totals recordsTotals
	if err := s.ensureRoot(); err != nil {
		return totals, err
	}
	directories, err := os.ReadDir(s.root)
	if err != nil {
		return totals, err
	}
	scanned := make(map[string]map[string]int64, len(directories))
	for _, directory := range directories {
		if !directory.IsDir() || !recordsTypePattern.MatchString(directory.Name()) {
			continue
		}
		totals.types++
		known := s.sizes[directory.Name()]
		names, err := s.filesKnowing(directory.Name(), known)
		if err != nil {
			return recordsTotals{}, err
		}
		current := make(map[string]int64, len(names))
		for _, name := range names {
			size, seen := known[name]
			if !seen {
				info, err := os.Stat(filepath.Join(s.root, directory.Name(), name))
				if err != nil {
					return recordsTotals{}, err
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
				return recordsTotals{}, &RecordsStoreError{Message: "records quota scan exceeded its configured budget"}
			}
		}
		scanned[directory.Name()] = current
	}
	s.sizes = scanned
	return totals, nil
}

func (s *RecordsStore) target(recordType, ref string) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] != recordType || !recordsFilenamePattern.MatchString(parts[1]) || !strings.HasPrefix(parts[1], recordType+"-") {
		return "", &RecordsStoreError{Message: "record ref is invalid for the selected type", Kind: recordsErrorRef}
	}
	return filepath.Join(s.root, recordType, parts[1]), nil
}

func (s *RecordsStore) load(recordType, ref string) (RecordsItem, error) {
	path, err := s.target(recordType, ref)
	if err != nil {
		return RecordsItem{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RecordsItem{}, &RecordsStoreError{Message: "record was not found", Kind: recordsErrorRef}
	}
	if err != nil {
		return RecordsItem{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return RecordsItem{}, &RecordsStoreError{Message: "record is not a safe file"}
	}
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RecordsItem{}, &RecordsStoreError{Message: "record was not found", Kind: recordsErrorRef}
	}
	if err != nil {
		return RecordsItem{}, err
	}
	value, err := parseStoredRecordsJSON(text)
	if err != nil || value.kind != recordsObject {
		return RecordsItem{}, &RecordsStoreError{Message: "record is malformed"}
	}
	return RecordsItem{Ref: ref, ETag: recordsETag(text), Data: recordsGoMap(value), value: value, rawText: append([]byte(nil), text...)}, nil
}

// readRecordText returns a record's bytes in a buffer of their own, which the caller may keep.
func (s *RecordsStore) readRecordText(recordType, name string) ([]byte, error) {
	path := filepath.Join(s.root, recordType, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a regular record")
	}
	return os.ReadFile(path)
}

func (s *RecordsStore) atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	id, err := recordsUUID()
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

func (s *RecordsStore) nextSequence() (uint64, error) {
	path := filepath.Join(s.root, ".memory-sequence")
	current := uint64(0)
	data, err := os.ReadFile(path)
	if err == nil {
		text := strings.TrimSuffix(string(data), "\n")
		if text == "" || strings.TrimSpace(text) != text || !recordsSequencePattern.MatchString(text) {
			return 0, &RecordsStoreError{Message: "record sequence is malformed"}
		}
		current, err = strconv.ParseUint(text, 10, 64)
		if err != nil || current > math.MaxInt64 {
			return 0, &RecordsStoreError{Message: "record sequence is malformed"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if current >= 9_007_199_254_740_991 {
		return 0, &RecordsStoreError{Message: "record sequence is exhausted"}
	}
	next := current + 1
	if err := s.atomicWrite(path, []byte(strconv.FormatUint(next, 10)+"\n")); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *RecordsStore) cursorDirectory(create bool) (string, error) {
	if err := s.ensureRoot(); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, ".memory-cursors")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", &RecordsStoreError{Message: "record cursor storage is not a safe directory"}
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
		return "", &RecordsStoreError{Message: "record cursor storage is not a safe directory"}
	}
	return path, nil
}

func (s *RecordsStore) pruneCursors(directory string, policy sessionfs.NativeRecordsSettings) error {
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
		if !entry.Type().IsRegular() || !recordsUUIDPattern.MatchString(strings.TrimSuffix(entry.Name(), ".json")) || !strings.HasSuffix(entry.Name(), ".json") {
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

func (s *RecordsStore) readStoredCursor(value any, policy sessionfs.NativeRecordsSettings) (*recordsStoredCursor, error) {
	token, ok := value.(string)
	if !ok || len(token) > 64 || !recordsCursorIDPattern.MatchString(token) {
		return nil, &RecordsStoreError{Message: "record cursor is invalid"}
	}
	match := recordsCursorIDPattern.FindStringSubmatch(token)
	directory, err := s.cursorDirectory(false)
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return nil, &RecordsStoreError{Message: "record cursor was not found"}
	}
	path := filepath.Join(directory, strings.ToLower(match[1])+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, &RecordsStoreError{Message: "record cursor was not found"}
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, &RecordsStoreError{Message: "record cursor is not a safe file"}
	}
	if time.Since(info.ModTime()) >= time.Duration(policy.CursorTTLMS)*time.Millisecond {
		_ = os.Remove(path)
		return nil, &RecordsStoreError{Message: "record cursor expired"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cursor recordsStoredCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.V != 1 || cursor.Type == "" || (cursor.Order != "newest" && cursor.Order != "oldest") || cursor.Filter == nil || cursor.Last == "" || cursor.FilterFingerprint == "" {
		return nil, &RecordsStoreError{Message: "record cursor is invalid"}
	}
	fingerprint, err := recordsCursorFingerprint(cursor.Filter, cursor.ExcludeAny, cursor.Query)
	if err != nil || fingerprint != cursor.FilterFingerprint {
		return nil, &RecordsStoreError{Message: "record cursor is invalid"}
	}
	return &cursor, nil
}

func (s *RecordsStore) createCursor(recordType, order string, filter map[string]any, excludeAny []map[string]any, query, filterFingerprint, last string, policy sessionfs.NativeRecordsSettings) (string, error) {
	cursor := recordsStoredCursor{V: 1, Type: recordType, Order: order, Filter: filter, FilterFingerprint: filterFingerprint, Last: last, Query: query}
	if len(excludeAny) > 0 {
		cursor.ExcludeAny = excludeAny
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	if int64(len(data)) > policy.MaxCursorBytes {
		return "", &RecordsStoreError{Message: "record cursor exceeds configured size"}
	}
	directory, err := s.cursorDirectory(true)
	if err != nil {
		return "", err
	}
	if err := s.pruneCursors(directory, policy); err != nil {
		return "", err
	}
	id, err := recordsUUID()
	if err != nil {
		return "", err
	}
	if err := s.atomicWrite(filepath.Join(directory, id+".json"), data); err != nil {
		return "", err
	}
	return "m_" + id, nil
}

func parseRecordsLegacyCursor(value any) *recordsLegacyCursor {
	token, ok := value.(string)
	if !ok || len(token) > 2048 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil
	}
	var cursor recordsLegacyCursor
	if json.Unmarshal(data, &cursor) != nil || (cursor.V != 1 && cursor.V != 2) || cursor.Type == "" || (cursor.Order != "newest" && cursor.Order != "oldest") || cursor.Filter == "" || cursor.Last == "" {
		return nil
	}
	return &cursor
}

func recordsOrder(requested any, legacy *recordsLegacyCursor, stored *recordsStoredCursor) (string, error) {
	if requested != nil {
		order, ok := requested.(string)
		if !ok || (order != "newest" && order != "oldest") {
			return "", &RecordsStoreError{Message: "records order is invalid"}
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

func recordsFilename(recordType string, sequence uint64, now time.Time) (string, error) {
	id, err := recordsUUID()
	if err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405.000Z")
	return fmt.Sprintf("%s-%s-%016d-%s.json", recordType, stamp, sequence, id), nil
}

func recordsUUID() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func recordsETag(text []byte) string {
	digest := sha256.Sum256(text)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func currentText(item RecordsItem) []byte {
	if item.rawText != nil {
		return item.rawText
	}
	if item.value == nil {
		encoded, _ := recordsJSONPretty(recordsValueFromGo(item.Data))
		return []byte(encoded)
	}
	encoded, _ := recordsJSONPretty(item.value)
	return []byte(encoded)
}

func recordsListItemsSize(items []RecordsItem) (int, error) {
	encoded, err := recordsJSONCompact(recordsArrayValueFromItems(items))
	return len(encoded), err
}

func recordsFilterFingerprint(filter map[string]any, excludeAny []map[string]any) (string, error) {
	filterValue := recordsValueFromGo(filter)
	canonical, err := recordsJSONCanonical(filterValue)
	if err != nil {
		return "", err
	}
	if len(excludeAny) > 0 {
		clauses := make([]*recordsJSONValue, 0, len(excludeAny))
		for _, clause := range excludeAny {
			clauses = append(clauses, recordsValueFromGo(clause))
		}
		encoded, err := recordsJSONCanonical(recordsArrayValue(clauses))
		if err != nil {
			return "", err
		}
		canonical += "\x00" + encoded
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:]), nil
}

func recordsMatches(data, filter map[string]any, excludeAny []map[string]any) bool {
	if !recordsMatchesClause(data, filter) {
		return false
	}
	for _, clause := range excludeAny {
		if recordsMatchesClause(data, clause) {
			return false
		}
	}
	return true
}

func recordsMatchesClause(data, clause map[string]any) bool {
	for key, expected := range clause {
		actual, found := recordsValueAt(data, key)
		if !found || !recordsDeepEqual(actual, expected) {
			return false
		}
	}
	return true
}

func recordsValueAt(data map[string]any, path string) (any, bool) {
	var current any = data
	for _, part := range strings.Split(path, ".") {
		if !recordsPathSegment.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
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

func recordsDeepEqual(left, right any) bool {
	leftCanonical, leftErr := recordsJSONCanonical(recordsValueFromGo(left))
	rightCanonical, rightErr := recordsJSONCanonical(recordsValueFromGo(right))
	return leftErr == nil && rightErr == nil && leftCanonical == rightCanonical
}

func recordsListContentText(result RecordsListResult) (string, error) {
	pairs := []recordsJSONPair{{key: "items", value: recordsArrayValueFromItems(result.Items)}}
	if result.NextCursor != "" {
		pairs = append(pairs, recordsJSONPair{key: "next_cursor", value: recordsValueFromGo(result.NextCursor)})
	}
	if len(result.Warnings) > 0 {
		warnings := make([]*recordsJSONValue, 0, len(result.Warnings))
		for _, warning := range result.Warnings {
			warnings = append(warnings, recordsValueFromGo(warning))
		}
		pairs = append(pairs, recordsJSONPair{key: "warnings", value: recordsArrayValue(warnings)})
	}
	var output bytes.Buffer
	if err := writeRecordsJSON(&output, recordsObjectValue(pairs), 2, 0); err != nil {
		return "", err
	}
	return output.String(), nil
}
