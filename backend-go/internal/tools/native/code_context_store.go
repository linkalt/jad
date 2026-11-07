package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// codeContextTaskPattern is the task key: it names the task's file, so it is a plain name.
var codeContextTaskPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// codeContextLocks serializes the writes of one task file across every catalog of the
// process, so profiles that share an absolute storage_dir do not lose each other's
// writes. Separate processes are not coordinated.
var codeContextLocks = NewPathLocks()

// codeContextEntry is one command: read(Path, Start, End), 1-based and inclusive, with
// the description of that code and the hashes of what was read.
type codeContextEntry struct {
	ID          int64  `json:"id"`
	Priority    int64  `json:"priority"`
	Path        string `json:"path"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	Description string `json:"description"`
	// FileSHA256 is the whole file when the range was last read; ContentSHA256 the exact
	// bytes of the range.
	FileSHA256    string `json:"file_sha256"`
	ContentSHA256 string `json:"content_sha256"`
	UpdatedBy     string `json:"updated_by"`
	UpdatedAt     string `json:"updated_at"`
	// Generation is the task generation of the entry's last write.
	Generation int64 `json:"generation"`
}

type codeContextDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Task          string `json:"task"`
	// Generation is bumped on every write, so an output and the entries it came from
	// can be matched.
	Generation    int64              `json:"generation"`
	NextID        int64              `json:"next_id"`
	LastUpdatedBy string             `json:"last_updated_by"`
	Entries       []codeContextEntry `json:"entries"`
}

type codeContextStoreError struct{ message string }

func (e *codeContextStoreError) Error() string { return e.message }

type codeContextStore struct {
	dir string
}

func (s codeContextStore) path(task string) string {
	return filepath.Join(s.dir, task+".json")
}

// load reads a task; a task never written is an empty document.
func (s codeContextStore) load(task string) (codeContextDocument, error) {
	data, err := os.ReadFile(s.path(task))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codeContextDocument{SchemaVersion: 1, Task: task, NextID: 1, Entries: []codeContextEntry{}}, nil
		}
		return codeContextDocument{}, &codeContextStoreError{fmt.Sprintf("could not read code_context storage: %v", err)}
	}
	var document codeContextDocument
	if err := json.Unmarshal(bytes.TrimSpace(data), &document); err != nil {
		return codeContextDocument{}, &codeContextStoreError{"code_context storage is not valid JSON"}
	}
	if document.SchemaVersion != 1 || document.Task != task || document.NextID < 1 || document.Entries == nil {
		return codeContextDocument{}, &codeContextStoreError{"code_context storage has an unsupported or invalid format"}
	}
	seen := make(map[int64]struct{}, len(document.Entries))
	for _, entry := range document.Entries {
		if entry.ID < 1 || entry.ID >= document.NextID || entry.Start < 1 || entry.End < entry.Start || entry.Path == "" {
			return codeContextDocument{}, &codeContextStoreError{"code_context storage has an unsupported or invalid format"}
		}
		if _, exists := seen[entry.ID]; exists {
			return codeContextDocument{}, &codeContextStoreError{"code_context storage contains duplicate entry ids"}
		}
		seen[entry.ID] = struct{}{}
	}
	return document, nil
}

// mutate loads the task under its lock, applies operation and writes the result
// atomically. The generation is bumped once per write.
func (s codeContextStore) mutate(task, by string, maxBytes int64, operation func(*codeContextDocument) error) (codeContextDocument, error) {
	release := codeContextLocks.Lock(s.path(task))
	defer release()
	document, err := s.load(task)
	if err != nil {
		return codeContextDocument{}, err
	}
	document.Generation++
	document.LastUpdatedBy = by
	if err := operation(&document); err != nil {
		return codeContextDocument{}, err
	}
	if err := s.write(document, maxBytes); err != nil {
		return codeContextDocument{}, err
	}
	return document, nil
}

func (s codeContextStore) write(document codeContextDocument, maxBytes int64) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	data := buffer.Bytes()
	if int64(len(data)) > maxBytes {
		return &codeContextStoreError{fmt.Sprintf("code_context storage of task %s would be %d bytes; max_storage_bytes is %d", document.Task, len(data), maxBytes)}
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	temporary, err := os.CreateTemp(s.dir, document.Task+".json.tmp-")
	if err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, writeErr := temporary.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if closeErr := temporary.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", writeErr)}
	}
	if err := os.Rename(temporaryPath, s.path(document.Task)); err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	return nil
}

// clear removes the task's file; a task never written is already clear.
func (s codeContextStore) clear(task string) (bool, error) {
	release := codeContextLocks.Lock(s.path(task))
	defer release()
	err := os.Remove(s.path(task))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, &codeContextStoreError{fmt.Sprintf("could not clear code_context storage: %v", err)}
	}
	return true, nil
}

func codeContextTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func normalizeCodeContextDescription(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
}
