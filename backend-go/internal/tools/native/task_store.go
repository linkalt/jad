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
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
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
	Limit    int
}

type TaskListResult struct {
	Tasks   []TaskRecord
	HasMore bool
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

func (s *TaskStore) Create(input TaskInput, policy sessionfs.NativeTaskSettings) (TaskRecord, error) {
	var created TaskRecord
	err := s.mutate(policy.MaxStorageBytes, func(document *taskDocument) error {
		if int64(len(document.Tasks)) >= policy.MaxTasks {
			return &TaskStoreError{Message: "the configured task capacity has been reached"}
		}
		if document.NextID < 1 || document.NextID > maxSafeTaskID {
			return &TaskStoreError{Message: "task id allocation is exhausted"}
		}
		timestamp := taskTimestamp()
		created = TaskRecord{
			ID:        fmt.Sprintf("T-%03d", document.NextID),
			Title:     input.Title,
			Status:    input.Status,
			Priority:  input.Priority,
			Tags:      append([]string{}, input.Tags...),
			Documents: append([]string{}, input.Documents...),
			CreatedAt: timestamp,
			UpdatedAt: timestamp,
		}
		document.NextID++
		document.Tasks = append(document.Tasks, created)
		return nil
	})
	return cloneTaskRecord(created), err
}

func (s *TaskStore) Get(id string) (TaskRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.load()
	if err != nil {
		return TaskRecord{}, false, err
	}
	for _, task := range document.Tasks {
		if task.ID == id {
			return cloneTaskRecord(task), true, nil
		}
	}
	return TaskRecord{}, false, nil
}

func (s *TaskStore) List(filter TaskListFilter) (TaskListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.load()
	if err != nil {
		return TaskListResult{}, err
	}
	matching := make([]TaskRecord, 0, len(document.Tasks))
	for _, task := range document.Tasks {
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
		matching = append(matching, cloneTaskRecord(task))
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
	limit := filter.Limit
	if limit < 0 {
		limit = 0
	}
	hasMore := len(matching) > limit
	if limit < len(matching) {
		matching = matching[:limit]
	}
	return TaskListResult{Tasks: matching, HasMore: hasMore}, nil
}

func (s *TaskStore) Update(id string, patch TaskPatch, policy sessionfs.NativeTaskSettings) (TaskRecord, bool, error) {
	var updated TaskRecord
	found := false
	err := s.mutate(policy.MaxStorageBytes, func(document *taskDocument) error {
		for index := range document.Tasks {
			task := &document.Tasks[index]
			if task.ID != id {
				continue
			}
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
			task.UpdatedAt = taskTimestamp()
			updated = cloneTaskRecord(*task)
			found = true
			break
		}
		return nil
	})
	return updated, found, err
}

func (s *TaskStore) Delete(id string, policy sessionfs.NativeTaskSettings) (bool, error) {
	deleted := false
	err := s.mutate(policy.MaxStorageBytes, func(document *taskDocument) error {
		for index, task := range document.Tasks {
			if task.ID == id {
				document.Tasks = append(document.Tasks[:index], document.Tasks[index+1:]...)
				deleted = true
				break
			}
		}
		return nil
	})
	return deleted, err
}

func (s *TaskStore) mutate(maxStorageBytes int64, operation func(*taskDocument) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.load()
	if err != nil {
		return err
	}
	if err := operation(&document); err != nil {
		return err
	}
	return s.write(document, maxStorageBytes)
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
