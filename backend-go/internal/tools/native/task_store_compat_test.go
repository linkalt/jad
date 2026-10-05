package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskStorePreservesCompatibleTaskRecordFields(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings.StorageDir, taskStorageFileName)
	initial := `{"schema_version":1,"next_id":2,"tasks":[{"id":"T-001","title":"existing","status":"todo","priority":"normal","tags":[],"documents":[],"created_at":"","updated_at":"","extension":{"preserve":true}}]}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get("T-001"); err != nil || !found {
		t.Fatalf("get TypeScript task = found %v, err %v", found, err)
	}
	title := "updated"
	if _, found, err := store.Update("T-001", TaskPatch{Title: &title}, settings); err != nil || !found {
		t.Fatalf("update TypeScript task = found %v, err %v", found, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tasks) != 1 {
		t.Fatalf("decode updated task document: %v", err)
	}
	if _, exists := document.Tasks[0]["extension"]; !exists {
		t.Fatalf("unknown task field was dropped: %s", data)
	}
}

func TestTaskStoreRejectsTaskRecordsMissingRequiredProperties(t *testing.T) {
	settings := taskTestSettings(t.TempDir())
	store := NewTaskStore(settings.StorageDir)
	if err := os.MkdirAll(settings.StorageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings.StorageDir, taskStorageFileName)
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"next_id":2,"tasks":[{"id":"T-001","title":"missing timestamps","status":"todo","priority":"normal","tags":[],"documents":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get("T-001"); err == nil {
		t.Fatal("Get() accepted a task missing created_at and updated_at")
	}
}
