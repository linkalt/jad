package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionlock"
)

const taskStorageFileName = "tasks.json"

const maxSafeTaskID = int64(9_007_199_254_740_991)

var taskIDPattern = regexp.MustCompile(`^T-[0-9]{3,}$`)

type TaskStatus string

type TaskPriority string

const (
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in-progress"
	TaskBlocked    TaskStatus = "blocked"
	TaskDone       TaskStatus = "done"
)

const (
	TaskPriorityLow    TaskPriority = "low"
	TaskPriorityNormal TaskPriority = "normal"
	TaskPriorityHigh   TaskPriority = "high"
	TaskPriorityUrgent TaskPriority = "urgent"
)

// TaskRecord is a session task persisted in tasks.json.
type TaskRecord struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Status       TaskStatus   `json:"status"`
	Priority     TaskPriority `json:"priority"`
	Tags         []string     `json:"tags"`
	Documents    []string     `json:"documents"`
	CreatedAt    string       `json:"created_at"`
	UpdatedAt    string       `json:"updated_at"`
	hasCreatedAt bool
	hasUpdatedAt bool
	extra        map[string]json.RawMessage
}

func (task *TaskRecord) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("task record is not an object")
	}
	if err := decodeTaskStringField(fields, "id", &task.ID); err != nil {
		return err
	}
	if err := decodeTaskStringField(fields, "title", &task.Title); err != nil {
		return err
	}
	var status string
	if err := decodeTaskStringField(fields, "status", &status); err != nil {
		return err
	}
	task.Status = TaskStatus(status)
	var priority string
	if err := decodeTaskStringField(fields, "priority", &priority); err != nil {
		return err
	}
	task.Priority = TaskPriority(priority)
	rawTags, hasTags := fields["tags"]
	rawTags = bytes.TrimSpace(rawTags)
	if !hasTags || len(rawTags) == 0 || rawTags[0] != '[' || json.Unmarshal(rawTags, &task.Tags) != nil || task.Tags == nil {
		return errors.New("task tags are invalid")
	}
	rawDocuments, hasDocuments := fields["documents"]
	rawDocuments = bytes.TrimSpace(rawDocuments)
	if !hasDocuments || len(rawDocuments) == 0 || rawDocuments[0] != '[' || json.Unmarshal(rawDocuments, &task.Documents) != nil || task.Documents == nil {
		return errors.New("task documents are invalid")
	}
	if err := decodeTaskStringField(fields, "created_at", &task.CreatedAt); err != nil {
		return err
	}
	task.hasCreatedAt = true
	if err := decodeTaskStringField(fields, "updated_at", &task.UpdatedAt); err != nil {
		return err
	}
	task.hasUpdatedAt = true
	known := map[string]struct{}{
		"id": {}, "title": {}, "status": {}, "priority": {}, "tags": {}, "documents": {}, "created_at": {}, "updated_at": {},
	}
	for key, raw := range fields {
		if _, exists := known[key]; !exists {
			if task.extra == nil {
				task.extra = make(map[string]json.RawMessage)
			}
			task.extra[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return nil
}

func (task TaskRecord) MarshalJSON() ([]byte, error) {
	type taskRecordJSON struct {
		ID        string       `json:"id"`
		Title     string       `json:"title"`
		Status    TaskStatus   `json:"status"`
		Priority  TaskPriority `json:"priority"`
		Tags      []string     `json:"tags"`
		Documents []string     `json:"documents"`
		CreatedAt string       `json:"created_at"`
		UpdatedAt string       `json:"updated_at"`
	}
	encoded, err := json.Marshal(taskRecordJSON{
		ID: task.ID, Title: task.Title, Status: task.Status, Priority: task.Priority,
		Tags: task.Tags, Documents: task.Documents, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	})
	if err != nil || len(task.extra) == 0 {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for key, raw := range task.extra {
		if _, exists := fields[key]; !exists {
			fields[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return json.Marshal(fields)
}

func decodeTaskStringField(fields map[string]json.RawMessage, key string, target *string) error {
	raw, exists := fields[key]
	raw = bytes.TrimSpace(raw)
	if !exists || len(raw) == 0 || raw[0] != '"' {
		return fmt.Errorf("task field %s is not a string", key)
	}
	return json.Unmarshal(raw, target)
}

type TaskInput struct {
	Title     string
	Status    TaskStatus
	Priority  TaskPriority
	Tags      []string
	Documents []string
}

type TaskListFilter struct {
	Status   string
	Statuses []TaskStatus
	Priority TaskPriority
	Tag      string
	// Query keeps the tasks in which every word of it is a case-insensitive substring of
	// the title, a tag or a document path.
	Query string
	// Offset skips that many matches in list order before Limit counts.
	Offset int
	Limit  int
}

type TaskListResult struct {
	Tasks []TaskRecord
	// HasMore reports that matches remain after the page.
	HasMore bool
	// Total is the number of matches before Offset and Limit.
	Total int
}

type TaskPatch struct {
	Title     *string
	Status    *TaskStatus
	Priority  *TaskPriority
	Tags      *[]string
	Documents *[]string
}

type taskDocument struct {
	SchemaVersion int          `json:"schema_version"`
	NextID        int64        `json:"next_id"`
	Tasks         []TaskRecord `json:"tasks"`
}

// TaskStoreError describes invalid or unavailable persistent task storage.
type TaskStoreError struct {
	Message string
}

func (e *TaskStoreError) Error() string { return e.Message }

// TaskStore is a concurrency-safe JSON task store for one storage directory.
type TaskStore struct {
	mu         sync.Mutex
	storageDir string
}

func NewTaskStore(storageDir string) *TaskStore {
	return &TaskStore{storageDir: storageDir}
}

func (s *TaskStore) path() string { return filepath.Join(s.storageDir, taskStorageFileName) }

// TaskOp is one operation of Apply. Action selects which of the other fields it reads:
// add reads Input, update reads ID and Patch, get, done and delete read ID, list reads
// Filter.
type TaskOp struct {
	Action taskAction
	ID     string
	Input  TaskInput
	Patch  TaskPatch
	Filter TaskListFilter
}

// TaskOpResult is what one operation of Apply produced.
type TaskOpResult struct {
	// Task is the record an add, update, done or get produced or read, and the record
	// a delete removed.
	Task TaskRecord
	// List is the page a list produced.
	List TaskListResult
	// Changed reports that the operation changed the document; done on a task that is
	// already done does not.
	Changed bool
}

// TaskMiss describes the tasks that exist when an operation named an id that does not.
type TaskMiss struct {
	// OpenIDs are the ids of the tasks that are not done, in list order, at most the
	// policy's list limit of them.
	OpenIDs []string
	// OpenTotal is how many tasks are not done.
	OpenTotal int
}

// TaskApplyResult is the outcome of Apply.
type TaskApplyResult struct {
	// Results has one entry per operation that ran.
	Results []TaskOpResult
	// Failed is the index of the operation that stopped Apply, or -1.
	Failed int
	// Miss is set when the failed operation named an id that does not exist.
	Miss *TaskMiss
	// Wrote reports that the document was written.
	Wrote bool
}

func (op TaskOp) readOnly() bool { return op.Action == taskActionGet || op.Action == taskActionList }

// Apply runs ops in order against one loaded document and writes it once. The store
// is locked, in this process and across processes, from the load to the write. It is
// all or nothing: an operation that fails (an id that does not exist, a full store)
// stops Apply before anything is written, and Failed names it. Nothing is written
// either when no operation changed the document.
func (s *TaskStore) Apply(ops []TaskOp, policy sessionfs.NativeTaskSettings) (TaskApplyResult, error) {
	outcome := TaskApplyResult{Failed: -1}
	run := func(document *taskDocument, reload func() (taskDocument, error)) (bool, error) {
		results := make([]TaskOpResult, 0, len(ops))
		changed := false
		for index, op := range ops {
			result, found, err := applyTaskOp(document, op, policy)
			if err != nil {
				outcome.Failed = index
				return false, err
			}
			if !found {
				outcome.Failed = index
				outcome.Results = results
				pristine := document
				if changed {
					// Earlier operations changed the document in memory only; the ids to
					// offer are the ones that exist, so they come from the stored file.
					if stored, err := reload(); err == nil {
						pristine = &stored
					}
				}
				outcome.Miss = taskMiss(pristine, policy.ListLimit)
				return false, nil
			}
			changed = changed || result.Changed
			results = append(results, result)
		}
		outcome.Results = results
		return changed, nil
	}
	readOnly := true
	for _, op := range ops {
		readOnly = readOnly && op.readOnly()
	}
	var err error
	if readOnly {
		err = s.read(func(document *taskDocument) error {
			_, err := run(document, s.load)
			return err
		})
	} else {
		err = s.mutate(policy.MaxStorageBytes, func(document *taskDocument) (bool, error) {
			changed, err := run(document, s.load)
			outcome.Wrote = changed && err == nil
			return changed, err
		})
		if err != nil {
			outcome.Wrote = false
		}
	}
	return outcome, err
}

// applyTaskOp runs one operation on the in-memory document. found is false when the
// operation names an id that does not exist.
func applyTaskOp(document *taskDocument, op TaskOp, policy sessionfs.NativeTaskSettings) (TaskOpResult, bool, error) {
	if op.Action == taskActionList {
		return TaskOpResult{List: listTaskDocument(document, op.Filter)}, true, nil
	}
	if op.Action == taskActionAdd {
		if int64(len(document.Tasks)) >= policy.MaxTasks {
			return TaskOpResult{}, false, &TaskStoreError{Message: "the configured task capacity has been reached"}
		}
		if document.NextID < 1 || document.NextID > maxSafeTaskID {
			return TaskOpResult{}, false, &TaskStoreError{Message: "task id allocation is exhausted"}
		}
		timestamp := taskTimestamp()
		created := TaskRecord{
			ID:        fmt.Sprintf("T-%03d", document.NextID),
			Title:     op.Input.Title,
			Status:    op.Input.Status,
			Priority:  op.Input.Priority,
			Tags:      append([]string{}, op.Input.Tags...),
			Documents: append([]string{}, op.Input.Documents...),
			CreatedAt: timestamp,
			UpdatedAt: timestamp,
		}
		document.NextID++
		document.Tasks = append(document.Tasks, created)
		return TaskOpResult{Task: cloneTaskRecord(created), Changed: true}, true, nil
	}
	for index := range document.Tasks {
		task := &document.Tasks[index]
		if task.ID != op.ID {
			continue
		}
		switch op.Action {
		case taskActionGet:
			return TaskOpResult{Task: cloneTaskRecord(*task)}, true, nil
		case taskActionDelete:
			removed := cloneTaskRecord(*task)
			document.Tasks = append(document.Tasks[:index], document.Tasks[index+1:]...)
			return TaskOpResult{Task: removed, Changed: true}, true, nil
		case taskActionDone:
			if task.Status == TaskDone {
				return TaskOpResult{Task: cloneTaskRecord(*task)}, true, nil
			}
			task.Status = TaskDone
		default:
			patch := op.Patch
			if patch.Title != nil {
				task.Title = *patch.Title
			}
			if patch.Status != nil {
				task.Status = *patch.Status
			}
			if patch.Priority != nil {
				task.Priority = *patch.Priority
			}
			if patch.Tags != nil {
				task.Tags = append([]string{}, (*patch.Tags)...)
			}
			if patch.Documents != nil {
				task.Documents = append([]string{}, (*patch.Documents)...)
			}
		}
		task.UpdatedAt = taskTimestamp()
		return TaskOpResult{Task: cloneTaskRecord(*task), Changed: true}, true, nil
	}
	return TaskOpResult{}, false, nil
}

// taskMiss lists the ids of the tasks that are not done, in list order.
func taskMiss(document *taskDocument, limit int64) *TaskMiss {
	open := sortedTasks(document, TaskListFilter{Statuses: []TaskStatus{TaskTodo, TaskInProgress, TaskBlocked}})
	miss := &TaskMiss{OpenTotal: len(open)}
	for index, task := range open {
		if int64(index) >= limit {
			break
		}
		miss.OpenIDs = append(miss.OpenIDs, task.ID)
	}
	return miss
}

func (s *TaskStore) Create(input TaskInput, policy sessionfs.NativeTaskSettings) (TaskRecord, error) {
	outcome, err := s.Apply([]TaskOp{{Action: taskActionAdd, Input: input}}, policy)
	if err != nil || len(outcome.Results) != 1 {
		return TaskRecord{}, err
	}
	return outcome.Results[0].Task, nil
}

func (s *TaskStore) Get(id string) (TaskRecord, bool, error) {
	outcome, err := s.Apply([]TaskOp{{Action: taskActionGet, ID: id}}, sessionfs.NativeTaskSettings{})
	if err != nil || outcome.Failed >= 0 {
		return TaskRecord{}, false, err
	}
	return outcome.Results[0].Task, true, nil
}

// List returns one page of the tasks that match filter, in list order. Total counts the
// matches before the page: Offset of them are skipped and Limit are returned.
func (s *TaskStore) List(filter TaskListFilter) (TaskListResult, error) {
	outcome, err := s.Apply([]TaskOp{{Action: taskActionList, Filter: filter}}, sessionfs.NativeTaskSettings{})
	if err != nil || len(outcome.Results) != 1 {
		return TaskListResult{}, err
	}
	return outcome.Results[0].List, nil
}

func (s *TaskStore) Update(id string, patch TaskPatch, policy sessionfs.NativeTaskSettings) (TaskRecord, bool, error) {
	outcome, err := s.Apply([]TaskOp{{Action: taskActionUpdate, ID: id, Patch: patch}}, policy)
	if err != nil || outcome.Failed >= 0 {
		return TaskRecord{}, false, err
	}
	return outcome.Results[0].Task, true, nil
}

func (s *TaskStore) Delete(id string, policy sessionfs.NativeTaskSettings) (bool, error) {
	outcome, err := s.Apply([]TaskOp{{Action: taskActionDelete, ID: id}}, policy)
	if err != nil || outcome.Failed >= 0 {
		return false, err
	}
	return true, nil
}

// sortedTasks returns pointers into document of the tasks that match filter, in list
// order: status rank, priority rank, updated_at newest first, then id. Nothing is cloned.
func sortedTasks(document *taskDocument, filter TaskListFilter) []*TaskRecord {
	terms := recordsSearchTerms(filter.Query)
	matching := make([]*TaskRecord, 0, len(document.Tasks))
	for index := range document.Tasks {
		task := &document.Tasks[index]
		if filter.Statuses != nil {
			if !containsTaskStatus(filter.Statuses, task.Status) {
				continue
			}
		} else if filter.Status == "all" {
			// Include every task.
		} else if filter.Status != "" {
			if string(task.Status) != filter.Status {
				continue
			}
		} else if task.Status == TaskDone {
			continue
		}
		if filter.Priority != "" && task.Priority != filter.Priority {
			continue
		}
		if filter.Tag != "" && !containsString(task.Tags, filter.Tag) {
			continue
		}
		if !taskContainsTerms(task, terms) {
			continue
		}
		matching = append(matching, task)
	}
	sort.Slice(matching, func(i, j int) bool {
		left, right := matching[i], matching[j]
		if statusRank(left.Status) != statusRank(right.Status) {
			return statusRank(left.Status) < statusRank(right.Status)
		}
		if priorityRank(left.Priority) != priorityRank(right.Priority) {
			return priorityRank(left.Priority) < priorityRank(right.Priority)
		}
		if left.UpdatedAt != right.UpdatedAt {
			return left.UpdatedAt > right.UpdatedAt
		}
		return left.ID < right.ID
	})
	return matching
}

// listTaskDocument filters and sorts first and clones only the returned page.
func listTaskDocument(document *taskDocument, filter TaskListFilter) TaskListResult {
	matching := sortedTasks(document, filter)
	total := len(matching)
	offset := max(filter.Offset, 0)
	limit := max(filter.Limit, 0)
	start := min(offset, total)
	end := min(start+limit, total)
	page := make([]TaskRecord, 0, end-start)
	for _, task := range matching[start:end] {
		page = append(page, cloneTaskRecord(*task))
	}
	return TaskListResult{Tasks: page, HasMore: end < total, Total: total}
}

// taskContainsTerms reports whether every term is a case-insensitive substring of the
// title, a tag or a document path, the rule recordsContainsTerms applies to records.
func taskContainsTerms(task *TaskRecord, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	texts := make([]string, 0, 1+len(task.Tags)+len(task.Documents))
	texts = append(texts, strings.ToLower(task.Title))
	for _, tag := range task.Tags {
		texts = append(texts, strings.ToLower(tag))
	}
	for _, path := range task.Documents {
		texts = append(texts, strings.ToLower(path))
	}
	for _, term := range terms {
		found := false
		for _, text := range texts {
			if strings.Contains(text, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// locked holds the store's in-process mutex and the lock file beside tasks.json, so
// another process that shares the directory cannot load or write in between. A read of
// a directory that does not exist yet takes no lock file: there is nothing to protect
// and a read must not create the directory.
func (s *TaskStore) locked(read bool, operation func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if read {
		if _, err := os.Stat(s.storageDir); errors.Is(err, os.ErrNotExist) {
			return operation()
		}
	}
	lockPath, err := sessionlock.PathFor(s.path())
	if err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not lock task storage: %v", err)}
	}
	lock, err := sessionlock.Acquire(context.Background(), lockPath)
	if err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not lock task storage: %v", err)}
	}
	defer lock.Close()
	return operation()
}

func (s *TaskStore) read(operation func(*taskDocument) error) error {
	return s.locked(true, func() error {
		document, err := s.load()
		if err != nil {
			return err
		}
		return operation(&document)
	})
}

// mutate loads the document, runs operation on it and writes it back, all under the
// lock. operation reports whether it changed the document; when it did not, or failed,
// nothing is written.
func (s *TaskStore) mutate(maxStorageBytes int64, operation func(*taskDocument) (bool, error)) error {
	return s.locked(false, func() error {
		document, err := s.load()
		if err != nil {
			return err
		}
		changed, err := operation(&document)
		if err != nil || !changed {
			return err
		}
		return s.write(document, maxStorageBytes)
	})
}

func (s *TaskStore) load() (taskDocument, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return taskDocument{SchemaVersion: 1, NextID: 1, Tasks: []TaskRecord{}}, nil
		}
		return taskDocument{}, &TaskStoreError{Message: fmt.Sprintf("could not read task storage: %v", err)}
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return taskDocument{}, &TaskStoreError{Message: "task storage is not valid JSON"}
	}
	if trimmed[0] != '{' {
		return taskDocument{}, &TaskStoreError{Message: "task storage is not a JSON object"}
	}
	var document taskDocument
	if err := json.Unmarshal(trimmed, &document); err != nil {
		return taskDocument{}, &TaskStoreError{Message: "task storage has an unsupported or invalid format"}
	}
	if document.SchemaVersion != 1 || document.NextID < 1 || document.NextID > maxSafeTaskID || document.Tasks == nil {
		return taskDocument{}, &TaskStoreError{Message: "task storage has an unsupported or invalid format"}
	}
	seen := make(map[string]struct{}, len(document.Tasks))
	for _, task := range document.Tasks {
		if !validStoredTask(task) {
			return taskDocument{}, &TaskStoreError{Message: "task storage has an unsupported or invalid format"}
		}
		if _, exists := seen[task.ID]; exists {
			return taskDocument{}, &TaskStoreError{Message: "task storage contains duplicate task ids"}
		}
		seen[task.ID] = struct{}{}
	}
	return document, nil
}

func (s *TaskStore) write(document taskDocument, maxStorageBytes int64) error {
	data, err := marshalTaskDocument(document)
	if err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	data = append(data, '\n')
	if int64(len(data)) > maxStorageBytes {
		return &TaskStoreError{Message: "task storage exceeds the configured byte limit"}
	}
	if err := os.MkdirAll(s.storageDir, 0o755); err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	temporary, err := os.CreateTemp(s.storageDir, taskStorageFileName+".tmp-")
	if err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, writeErr := temporary.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		_ = temporary.Close()
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", writeErr)}
	}
	// The data reaches the disk before the rename makes it the stored file, so a power
	// cut leaves the old file or the new one, never an empty one.
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	if err := temporary.Close(); err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	if err := os.Rename(temporaryPath, s.path()); err != nil {
		return &TaskStoreError{Message: fmt.Sprintf("could not persist task storage: %v", err)}
	}
	return nil
}

func marshalTaskDocument(document taskDocument) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func validStoredTask(task TaskRecord) bool {
	if !taskIDPattern.MatchString(task.ID) || task.Title == "" || !task.hasCreatedAt || !task.hasUpdatedAt {
		return false
	}
	if task.Status != TaskTodo && task.Status != TaskInProgress && task.Status != TaskBlocked && task.Status != TaskDone {
		return false
	}
	if task.Priority != TaskPriorityLow && task.Priority != TaskPriorityNormal && task.Priority != TaskPriorityHigh && task.Priority != TaskPriorityUrgent {
		return false
	}
	return task.Tags != nil && task.Documents != nil
}

func cloneTaskRecord(task TaskRecord) TaskRecord {
	task.Tags = append([]string{}, task.Tags...)
	task.Documents = append([]string{}, task.Documents...)
	if len(task.extra) > 0 {
		extra := make(map[string]json.RawMessage, len(task.extra))
		for key, value := range task.extra {
			extra[key] = append(json.RawMessage(nil), value...)
		}
		task.extra = extra
	}
	return task
}

func containsTaskStatus(statuses []TaskStatus, status TaskStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func statusRank(status TaskStatus) int {
	switch status {
	case TaskBlocked:
		return 0
	case TaskInProgress:
		return 1
	case TaskTodo:
		return 2
	default:
		return 3
	}
}

func priorityRank(priority TaskPriority) int {
	switch priority {
	case TaskPriorityUrgent:
		return 0
	case TaskPriorityHigh:
		return 1
	case TaskPriorityNormal:
		return 2
	default:
		return 3
	}
}

func taskTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// TaskStateSource is the smallest interface needed to collect task state.
type TaskStateSource interface {
	List(TaskListFilter) (TaskListResult, error)
}
