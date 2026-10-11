package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"example.com/agent-harness/backend-go/internal/logging"
)

const (
	manifestFilename = "manifest.json"
	itemsFilename    = "items.jsonl"
	indexFilename    = "items.index.jsonl"
	finalFilename    = "final.jsonl"
)

// TurnStore creates staged response turns and commits completed responses to a
// Journal. Recover reconciles interrupted turns with the Journal after restart.
type TurnStore struct {
	journal     *Journal
	stagingDir  string
	maxItem     int64
	metadata    ResponseMetadataMode
	options     TurnStoreOptions
	mu          sync.Mutex
	cursorMu    sync.Mutex
	cursorIndex *cursorCacheState
	// compactionGeneration counts in-process writes and removals of the
	// compaction cache files, so history handles can drop a parsed copy even
	// when a rewritten file happens to keep its size, mtime and inode.
	compactionGeneration atomic.Uint64
	activeTurns          map[*ResponseTurn]struct{}
	recovering           bool
}

// TurnStoreOptions contains session persistence settings that affect staged
// response records. MaxItemBytes is required and must come from configuration.
type TurnStoreOptions struct {
	// Logger receives persistence diagnostics. Nil discards them.
	Logger                     *slog.Logger
	MaxItemBytes               int64
	ResponseMetadata           ResponseMetadataMode
	CursorCacheCheckpointBytes int64
	MessagesBackupPath         string
	CompactionCachePath        string
	UsageCachePath             string
	CursorCachePath            string
	RunErrorCachePath          string
	StateContextPath           string
	AcceptedRequestPath        string
}

// TurnManifest is the durable per-turn state record.
type TurnManifest struct {
	SchemaVersion       int    `json:"schema_version"`
	TurnID              string `json:"turn_id"`
	Status              string `json:"status"`
	ItemCount           int    `json:"item_count"`
	NextSequence        int64  `json:"next_sequence"`
	ResponseID          string `json:"response_id,omitempty"`
	CanonicalLineSHA256 string `json:"canonical_line_sha256,omitempty"`
}

// CommitResult reports the committed line hash and whether it was already in
// the transcript.
type CommitResult struct {
	LineHash       string `json:"line_hash"`
	Bytes          int64  `json:"bytes"`
	AlreadyPresent bool   `json:"already_present"`
}

type itemIndexEntry struct {
	outputIndex int64
	offset      int64
	length      int64
}

// ResponseTurn stages completed output items and commits one terminal response.
type ResponseTurn struct {
	store        *TurnStore
	turnDir      string
	itemsPath    string
	indexPath    string
	manifestPath string
	manifest     TurnManifest
	itemIndex    map[int64]itemIndexEntry
	mu           sync.Mutex
	closed       bool
	registered   bool
}

// NewTurnStore configures staged response persistence for one messages.jsonl
// transcript. The Journal and TurnStore must not be shared with another process
// writing the same session.
func NewTurnStore(journal *Journal, stagingDir string, options TurnStoreOptions) (*TurnStore, error) {
	if journal == nil {
		return nil, errors.New("transcript journal is required")
	}
	if stagingDir == "" {
		return nil, errors.New("staging directory is required")
	}
	if options.MaxItemBytes <= 0 {
		return nil, errors.New("max item bytes must be positive")
	}
	if options.CursorCacheCheckpointBytes == 0 {
		options.CursorCacheCheckpointBytes = defaultCursorCacheCheckpointBytes
	}
	if options.CursorCacheCheckpointBytes < 1 || options.CursorCacheCheckpointBytes > maxCursorCacheCheckpointBytes {
		return nil, fmt.Errorf("cursor cache checkpoint bytes must be between 1 and %d", maxCursorCacheCheckpointBytes)
	}
	metadata := options.ResponseMetadata
	if metadata == "" {
		metadata = ResponseMetadataMinimal
	}
	if metadata != ResponseMetadataMinimal && metadata != ResponseMetadataFull {
		return nil, fmt.Errorf("unsupported response metadata mode %q", metadata)
	}
	store := &TurnStore{
		journal:     journal,
		stagingDir:  filepath.Clean(stagingDir),
		maxItem:     options.MaxItemBytes,
		metadata:    metadata,
		options:     options,
		activeTurns: make(map[*ResponseTurn]struct{}),
	}
	cleanupOrphanedPartialFiles(store.stagingDir, logging.Or(options.Logger))
	return store, nil
}

// BeginTurn creates a durable staging directory with an initial streaming
// manifest. Turn IDs use the same restricted alphabet as the TypeScript store.
func (s *TurnStore) BeginTurn(turnID string) (*ResponseTurn, error) {
	if s == nil || s.journal == nil {
		return nil, errors.New("turn store is not initialized")
	}
	if !validTurnID(turnID) {
		return nil, errors.New("turn ID must contain only letters, numbers, '.', '_' or '-' and be at most 128 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovering {
		return nil, errors.New("turn store recovery is in progress")
	}
	if err := os.MkdirAll(s.stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	turnDir := filepath.Join(s.stagingDir, turnID)
	if err := os.Mkdir(turnDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("turn already exists: %s", turnID)
		}
		return nil, fmt.Errorf("create turn directory: %w", err)
	}

	turn := &ResponseTurn{
		store:        s,
		turnDir:      turnDir,
		itemsPath:    filepath.Join(turnDir, itemsFilename),
		indexPath:    filepath.Join(turnDir, indexFilename),
		manifestPath: filepath.Join(turnDir, manifestFilename),
		manifest: TurnManifest{
			SchemaVersion: 1,
			TurnID:        turnID,
			Status:        "streaming",
		},
		itemIndex: make(map[int64]itemIndexEntry),
	}
	if err := turn.persistManifest(); err != nil {
		return nil, err
	}
	turn.registered = true
	s.activeTurns[turn] = struct{}{}
	return turn, nil
}

// TranscriptBytes reports the current messages.jsonl size for diagnostics.
func (s *TurnStore) TranscriptBytes() (int64, error) {
	if s == nil || s.journal == nil {
		return 0, errors.New("turn store is not initialized")
	}
	info, err := os.Stat(s.journal.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *TurnStore) unregister(turn *ResponseTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn.registered {
		delete(s.activeTurns, turn)
		turn.registered = false
	}
}

// StageOutputItem stages one response.output_item.done event. The item itself is
// compacted before staging, and the item index points into items.jsonl by byte
// offset and length so commit reads one item at a time.
func (t *ResponseTurn) StageOutputItem(event []byte) error {
	if t == nil {
		return errors.New("response turn is not initialized")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.assertWritable(); err != nil {
		return err
	}
	eventFields, err := objectFields(event)
	if err != nil {
		return errors.New("expected a response.output_item.done event")
	}
	typeRaw, exists := fieldValue(eventFields, "type")
	eventType, typeOK := stringValue(typeRaw)
	if !exists || !typeOK || eventType != "response.output_item.done" {
		return errors.New("expected a response.output_item.done event")
	}
	itemRaw, exists := fieldValue(eventFields, "item")
	if !exists {
		return errors.New("output item must be an object")
	}
	if err := validateOutputItemJSON(itemRaw); err != nil {
		return err
	}
	item, err := canonicalJSON(itemRaw)
	if err != nil {
		return fmt.Errorf("serialize output item: %w", err)
	}
	if int64(len(item)) > t.store.maxItem {
		return fmt.Errorf("output item is %d bytes, exceeding max item bytes %d", len(item), t.store.maxItem)
	}

	outputIndex := t.manifest.NextSequence
	if rawIndex, present := fieldValue(eventFields, "output_index"); present {
		parsed, valid := safeInteger(rawIndex, false)
		if !valid {
			return errors.New("output_index must be a non-negative safe integer")
		}
		outputIndex = parsed
	} else {
		for {
			if _, exists := t.itemIndex[outputIndex]; !exists {
				break
			}
			outputIndex++
		}
	}
	if outputIndex < 0 || outputIndex > maxJavaScriptSafeInteger {
		return errors.New("output_index must be a non-negative safe integer")
	}
	if _, exists := t.itemIndex[outputIndex]; exists {
		return fmt.Errorf("output_index %d has already been staged", outputIndex)
	}

	itemLine := makeItemRecord(outputIndex, item)
	itemOffset, err := appendStagingLine(t.itemsPath, itemLine, t.store.journal.durability == DurabilityStrict)
	if err != nil {
		return fmt.Errorf("stage output item: %w", err)
	}
	indexLine := makeIndexRecord(outputIndex, itemOffset, int64(len(itemLine)))
	if _, err := appendStagingLine(t.indexPath, indexLine, t.store.journal.durability == DurabilityStrict); err != nil {
		_ = truncateFile(t.itemsPath, itemOffset)
		return fmt.Errorf("stage output index: %w", err)
	}

	t.itemIndex[outputIndex] = itemIndexEntry{
		outputIndex: outputIndex,
		offset:      itemOffset,
		length:      int64(len(itemLine)),
	}
	t.manifest.ItemCount = len(t.itemIndex)
	if outputIndex >= t.manifest.NextSequence {
		t.manifest.NextSequence = outputIndex + 1
	}
	if err := t.persistManifest(); err != nil {
		return fmt.Errorf("persist turn manifest: %w", err)
	}
	return nil
}

// PartialContentPath returns the ephemeral UI-cache path for this response turn.
// The cache is separate from staged output items and is never transcript data.
func (t *ResponseTurn) PartialContentPath() string {
	if t == nil {
		return ""
	}
	return filepath.Join(t.turnDir, "responses.partial")
}

// Commit validates the terminal event and staged indexes, writes a canonical
// response.completed line, then appends it idempotently to messages.jsonl.
func (t *ResponseTurn) Commit(event []byte) (CommitResult, error) {
	return t.CommitWithObserver(event, nil)
}

// CommitWithObserver runs observer after the canonical transcript append and
// committed manifest are durable, but before response staging is removed. The
// observer is UI-only: a panic is contained and leaves staging for recovery,
// because the transcript append is already the point of no return.
func (t *ResponseTurn) CommitWithObserver(event []byte, observer func(CommitResult)) (CommitResult, error) {
	if t == nil {
		return CommitResult{}, errors.New("response turn is not initialized")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.assertWritable(); err != nil {
		return CommitResult{}, err
	}
	plan, err := makeCompletionPlan(event, len(t.itemIndex), t.store.metadata)
	if err != nil {
		return CommitResult{}, err
	}
	if t.manifest.ResponseID != "" && plan.responseID != "" && t.manifest.ResponseID != plan.responseID {
		return CommitResult{}, errors.New("response ID does not match the active turn")
	}
	for outputIndex := 0; outputIndex < plan.itemCount; outputIndex++ {
		if _, exists := t.itemIndex[int64(outputIndex)]; !exists {
			return CommitResult{}, fmt.Errorf("missing staged output_index %d", outputIndex)
		}
	}
	if len(t.itemIndex) != plan.itemCount {
		return CommitResult{}, fmt.Errorf("staged item count %d does not match completed output count %d", len(t.itemIndex), plan.itemCount)
	}
	if plan.responseID != "" {
		t.manifest.ResponseID = plan.responseID
	}
	t.manifest.Status = "ready"
	if err := t.persistManifest(); err != nil {
		return CommitResult{}, fmt.Errorf("persist ready turn manifest: %w", err)
	}
	if err := t.syncStageFiles(); err != nil {
		return CommitResult{}, fmt.Errorf("sync staged response items: %w", err)
	}
	t.manifest.Status = "committing"
	if err := t.persistManifest(); err != nil {
		return CommitResult{}, fmt.Errorf("persist committing turn manifest: %w", err)
	}

	finalPath := filepath.Join(t.turnDir, finalFilename)
	hash, byteCount, err := t.writeFinalLine(finalPath, plan)
	if err != nil {
		return CommitResult{}, fmt.Errorf("render canonical response line: %w", err)
	}
	t.manifest.CanonicalLineSHA256 = hash
	if err := t.persistManifest(); err != nil {
		return CommitResult{}, fmt.Errorf("persist canonical line hash: %w", err)
	}

	inserted, err := t.store.journal.appendFileIfAbsent(finalPath)
	if err != nil {
		return CommitResult{}, fmt.Errorf("append response line: %w", err)
	}
	t.manifest.Status = "committed"
	if err := t.persistManifest(); err != nil {
		return CommitResult{}, fmt.Errorf("persist committed turn manifest: %w", err)
	}
	commitResult := CommitResult{LineHash: hash, Bytes: byteCount, AlreadyPresent: !inserted}
	if observer != nil && !callCommitObserver(observer, commitResult, logging.Or(t.store.options.Logger)) {
		t.closed = true
		t.store.unregister(t)
		return commitResult, nil
	}
	if err := os.RemoveAll(t.turnDir); err != nil {
		if observer != nil {
			logging.Or(t.store.options.Logger).Warn("committed response staging cleanup deferred", slog.String("turn", t.manifest.TurnID), slog.Any("error", err))
			t.closed = true
			t.store.unregister(t)
			return commitResult, nil
		}
		return CommitResult{}, fmt.Errorf("remove committed turn staging directory: %w", err)
	}
	t.closed = true
	t.store.unregister(t)
	return commitResult, nil
}

func callCommitObserver(observer func(CommitResult), result CommitResult, logger *slog.Logger) (succeeded bool) {
	succeeded = true
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("response commit observer panicked; staging cleanup deferred", slog.Any("panic", recovered))
			succeeded = false
		}
	}()
	observer(result)
	return succeeded
}

// Close abandons the in-memory turn handle but retains its durable staging files.
func (t *ResponseTurn) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.store.unregister(t)
	return nil
}

func (t *ResponseTurn) assertWritable() error {
	if t.closed {
		return errors.New("response turn is closed")
	}
	if t.manifest.Status != "streaming" {
		return fmt.Errorf("response turn is not writable in status %q", t.manifest.Status)
	}
	return nil
}

func (t *ResponseTurn) persistManifest() error {
	data, err := json.Marshal(t.manifest)
	if err != nil {
		return fmt.Errorf("serialize turn manifest: %w", err)
	}
	data = append(data, '\n')
	return writeJSONAtomic(t.manifestPath, data, t.store.journal.durability == DurabilityStrict)
}

func (t *ResponseTurn) syncStageFiles() error {
	if t.store.journal.durability == DurabilityProcess {
		return nil
	}
	for _, path := range []string{t.itemsPath, t.indexPath} {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (t *ResponseTurn) writeFinalLine(path string, plan completionPlan) (string, int64, error) {
	file, err := os.CreateTemp(t.turnDir, finalFilename+".tmp-")
	if err != nil {
		return "", 0, err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)

	digest := sha256.New()
	var byteCount int64
	writeChunk := func(chunk []byte) error {
		return writeHashedChunk(file, digest, &byteCount, chunk)
	}
	if err := writeChunk(plan.prefix); err != nil {
		_ = file.Close()
		return "", 0, err
	}
	for outputIndex := 0; outputIndex < plan.itemCount; outputIndex++ {
		if outputIndex > 0 {
			if err := writeChunk([]byte{','}); err != nil {
				_ = file.Close()
				return "", 0, err
			}
		}
		entry := t.itemIndex[int64(outputIndex)]
		item, err := t.readStagedItem(entry)
		if err != nil {
			_ = file.Close()
			return "", 0, err
		}
		if err := writeChunk(item); err != nil {
			_ = file.Close()
			return "", 0, err
		}
	}
	if err := writeChunk(plan.suffix); err != nil {
		_ = file.Close()
		return "", 0, err
	}
	if t.store.journal.durability != DurabilityProcess {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return "", 0, err
		}
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), byteCount, nil
}

func (t *ResponseTurn) readStagedItem(entry itemIndexEntry) ([]byte, error) {
	if entry.length <= 0 || entry.length > int64(int(^uint(0)>>1)) {
		return nil, errors.New("staged item record has invalid length")
	}
	file, err := os.Open(t.itemsPath)
	if err != nil {
		return nil, err
	}
	record := make([]byte, int(entry.length))
	n, readErr := file.ReadAt(record, entry.offset)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if n != len(record) {
		return nil, io.ErrUnexpectedEOF
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(record) == 0 || record[len(record)-1] != '\n' {
		return nil, errors.New("staged item record is not newline-terminated")
	}
	fields, err := objectFields(record[:len(record)-1])
	if err != nil {
		return nil, fmt.Errorf("decode staged item record: %w", err)
	}
	indexRaw, hasIndex := fieldValue(fields, "output_index")
	storedIndex, validIndex := safeInteger(indexRaw, false)
	if !hasIndex || !validIndex || storedIndex != entry.outputIndex {
		return nil, errors.New("staged item index does not match its index record")
	}
	item, hasItem := fieldValue(fields, "item")
	if !hasItem {
		return nil, errors.New("staged item record has no item")
	}
	if err := validateOutputItemJSON(item); err != nil {
		return nil, err
	}
	return canonicalJSON(item)
}

func appendStagingLine(path string, line []byte, sync bool) (int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return 0, err
	}
	offset := info.Size()
	writeErr := writeAll(file, line)
	if writeErr == nil && sync {
		writeErr = file.Sync()
	}
	if writeErr != nil {
		_ = file.Truncate(offset)
	}
	closeErr := file.Close()
	if writeErr != nil {
		return offset, writeErr
	}
	if closeErr != nil {
		_ = truncateFile(path, offset)
		return offset, closeErr
	}
	return offset, nil
}

func truncateFile(path string, offset int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	truncateErr := file.Truncate(offset)
	closeErr := file.Close()
	if truncateErr != nil {
		return truncateErr
	}
	return closeErr
}

func makeItemRecord(outputIndex int64, item []byte) []byte {
	var record bytes.Buffer
	record.WriteString(`{"output_index":`)
	record.WriteString(strconv.FormatInt(outputIndex, 10))
	record.WriteString(`,"item":`)
	record.Write(item)
	record.WriteString("}\n")
	return record.Bytes()
}

func makeIndexRecord(outputIndex, offset, length int64) []byte {
	var record bytes.Buffer
	record.WriteString(`{"output_index":`)
	record.WriteString(strconv.FormatInt(outputIndex, 10))
	record.WriteString(`,"offset":`)
	record.WriteString(strconv.FormatInt(offset, 10))
	record.WriteString(`,"length":`)
	record.WriteString(strconv.FormatInt(length, 10))
	record.WriteString("}\n")
	return record.Bytes()
}

func writeJSONAtomic(path string, data []byte, sync bool) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	if err := writeAll(file, data); err != nil {
		_ = file.Close()
		return err
	}
	if sync {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func validTurnID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func writeHashedChunk(file *os.File, digest hash.Hash, byteCount *int64, chunk []byte) error {
	for len(chunk) > 0 {
		written, err := file.Write(chunk)
		if written > 0 {
			_, _ = digest.Write(chunk[:written])
			*byteCount += int64(written)
			chunk = chunk[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
