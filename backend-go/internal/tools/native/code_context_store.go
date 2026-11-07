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
	"sort"
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
	// ContentBytes is the length of the range's bytes; with the content hash it finds the
	// same code after it moved. Zero in an entry written before it was kept.
	ContentBytes int64 `json:"content_bytes,omitempty"`
	// HeadSHA256 is the range's first line, so a search for moved code hashes a whole
	// candidate only where the first line matches.
	HeadSHA256 string `json:"head_sha256,omitempty"`
	UpdatedBy  string `json:"updated_by"`
	UpdatedAt  string `json:"updated_at"`
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
	return writeCodeContextFile(s.dir, s.path(document.Task), document.Task+".json.tmp-", data)
}

// writeCodeContextFile replaces path with data atomically, through a temporary file in dir.
func writeCodeContextFile(dir, path, pattern string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	temporary, err := os.CreateTemp(dir, pattern)
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return &codeContextStoreError{fmt.Sprintf("could not persist code_context storage: %v", err)}
	}
	return nil
}

// clear removes the task's file and its state; a task never written is already clear.
func (s codeContextStore) clear(task string) (bool, error) {
	release := codeContextLocks.Lock(s.path(task))
	err := os.Remove(s.path(task))
	release()
	releaseState := codeContextLocks.Lock(s.statePath(task))
	stateErr := os.Remove(s.statePath(task))
	releaseState()
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return false, &codeContextStoreError{fmt.Sprintf("could not clear code_context state: %v", stateErr)}
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, &codeContextStoreError{fmt.Sprintf("could not clear code_context storage: %v", err)}
	}
	return true, nil
}

// codeContextTaskFile is another task's file, for find and read across tasks.
type codeContextTaskFile struct {
	task     string
	modified time.Time
}

// otherTasks lists the task files of the directory except task, most recently written
// first, at most limit of them.
func (s codeContextStore) otherTasks(task string, limit int64) ([]codeContextTaskFile, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, &codeContextStoreError{fmt.Sprintf("could not list code_context storage: %v", err)}
	}
	var files []codeContextTaskFile
	for _, entry := range entries {
		name, isJSON := strings.CutSuffix(entry.Name(), ".json")
		if !isJSON || name == task || !entry.Type().IsRegular() || !codeContextTaskPattern.MatchString(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, codeContextTaskFile{task: name, modified: info.ModTime()})
	}
	sort.SliceStable(files, func(i, j int) bool {
		if !files[i].modified.Equal(files[j].modified) {
			return files[i].modified.After(files[j].modified)
		}
		return files[i].task < files[j].task
	})
	if int64(len(files)) > limit {
		files = files[:limit]
	}
	return files, nil
}

// codeContextState is what a task's calls leave besides its entries: the deliveries a
// later get can name with since, and counts of what the tool saved its callers. It is
// kept apart from the task file, so it never counts against max_storage_bytes of the
// entries and losing it loses no entry.
type codeContextState struct {
	SchemaVersion int                   `json:"schema_version"`
	Task          string                `json:"task"`
	Deliveries    []codeContextDelivery `json:"deliveries"`
	Stats         codeContextStats      `json:"stats"`
}

// codeContextDelivery is what one get printed: per entry id, the fingerprint of the
// code and description the caller now holds.
type codeContextDelivery struct {
	Token      string           `json:"token"`
	Generation int64            `json:"generation"`
	CreatedAt  string           `json:"created_at"`
	Entries    map[int64]string `json:"entries"`
}

// codeContextStats counts, per task, what the tool printed and what it spared.
type codeContextStats struct {
	Gets int64 `json:"gets"`
	// CodeBytesPrinted is the code get and read printed; CodeBytesNotRepeated the code a
	// get with since did not print again because the caller already had it.
	CodeBytesPrinted     int64 `json:"code_bytes_printed"`
	CodeBytesNotRepeated int64 `json:"code_bytes_not_repeated"`
	EntriesRelocated     int64 `json:"entries_relocated"`
	StaleSeen            int64 `json:"stale_seen"`
	Reads                int64 `json:"reads"`
	// ReadsAlreadySaved counts reads of lines an entry already held: a get would have
	// given them.
	ReadsAlreadySaved int64 `json:"reads_already_saved"`
	Finds             int64 `json:"finds"`
	FindsWithoutMatch int64 `json:"finds_without_match"`
}

// statePath keeps the state in a subdirectory: a task name cannot start with a dot, so
// it is never mistaken for a task file.
func (s codeContextStore) statePath(task string) string {
	return filepath.Join(s.dir, ".state", task+".json")
}

func (s codeContextStore) loadState(task string) codeContextState {
	empty := codeContextState{SchemaVersion: 1, Task: task, Deliveries: []codeContextDelivery{}}
	data, err := os.ReadFile(s.statePath(task))
	if err != nil {
		return empty
	}
	var state codeContextState
	// A state that cannot be read starts again empty: it holds no entry.
	if json.Unmarshal(bytes.TrimSpace(data), &state) != nil || state.SchemaVersion != 1 || state.Task != task {
		return empty
	}
	if state.Deliveries == nil {
		state.Deliveries = []codeContextDelivery{}
	}
	return state
}

// mutateState applies operation to the task's state under its lock and writes it. The
// oldest deliveries are forgotten beyond maxDeliveries or maxBytes.
func (s codeContextStore) mutateState(task string, maxDeliveries, maxBytes int64, operation func(*codeContextState)) error {
	path := s.statePath(task)
	release := codeContextLocks.Lock(path)
	defer release()
	state := s.loadState(task)
	operation(&state)
	if over := int64(len(state.Deliveries)) - maxDeliveries; over > 0 {
		state.Deliveries = state.Deliveries[over:]
	}
	for {
		data, err := json.Marshal(state)
		if err != nil {
			return &codeContextStoreError{fmt.Sprintf("could not persist code_context state: %v", err)}
		}
		if int64(len(data)) > maxBytes && len(state.Deliveries) > 0 {
			state.Deliveries = state.Deliveries[1:]
			continue
		}
		if int64(len(data)) > maxBytes {
			return &codeContextStoreError{fmt.Sprintf("code_context state of task %s would be %d bytes; max_storage_bytes is %d", task, len(data), maxBytes)}
		}
		return writeCodeContextFile(filepath.Dir(path), path, task+".json.tmp-", append(data, '\n'))
	}
}

func codeContextTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func normalizeCodeContextDescription(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
}
