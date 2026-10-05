package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var processTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`)

// The expected strings were produced by running the TypeScript ProcessManager's
// recover() and list() on the same manifests (timestamps normalised).
func TestRecoveredProcessKeyOrderMatchesTypeScript(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "processes")
	manifests := map[string]string{
		"proc_a": `{"schema_version":1,"id":"proc_a","state":"running","cwd":"/w","started_at":"2025-01-01T00:00:00.000Z","pid":42}`,
		"proc_b": `{"schema_version":1,"id":"proc_b","state":"running","cwd":"/w","started_at":"2025-01-02T00:00:00.000Z","pid":43,"stop_reason":"stopped_by_agent","left_running":true}`,
		"proc_c": `{"schema_version":1,"id":"proc_c","state":"exited","cwd":"/w","started_at":"2025-01-03T00:00:00.000Z","ended_at":"2025-01-03T00:00:01.000Z","stop_reason":"backend_restarted","exit_code":0}`,
	}
	for id, body := range manifests {
		if err := os.MkdirAll(filepath.Join(directory, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, id, "manifest.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewProcessManager(directory)
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	want := map[string]string{
		"proc_a": `{"schema_version":1,"id":"proc_a","state":"lost","cwd":"/w","started_at":"2025-01-01T00:00:00.000Z","pid":42,"ended_at":"$T","stop_reason":"backend_restarted","stdout_stored_bytes":0,"stderr_stored_bytes":0,"stdout_capped":false,"stderr_capped":false}`,
		"proc_b": `{"schema_version":1,"id":"proc_b","state":"lost","cwd":"/w","started_at":"2025-01-02T00:00:00.000Z","pid":43,"stop_reason":"backend_restarted","left_running":true,"ended_at":"$T","stdout_stored_bytes":0,"stderr_stored_bytes":0,"stdout_capped":false,"stderr_capped":false}`,
		"proc_c": `{"schema_version":1,"id":"proc_c","state":"exited","cwd":"/w","started_at":"2025-01-03T00:00:00.000Z","ended_at":"2025-01-03T00:00:01.000Z","stop_reason":"backend_restarted","exit_code":0,"stdout_stored_bytes":0,"stderr_stored_bytes":0,"stdout_capped":false,"stderr_capped":false}`,
	}
	for _, snapshot := range manager.List("") {
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		got := processTimestamp.ReplaceAllLiteralString(string(encoded), "$T")
		expected := processTimestamp.ReplaceAllLiteralString(want[snapshot.ID], "$T")
		if got != expected {
			t.Errorf("%s encoded as\n %s\nwant\n %s", snapshot.ID, got, expected)
		}
	}

	// The persisted manifest keeps the same order, and survives a second recovery.
	persisted, err := os.ReadFile(filepath.Join(directory, "proc_a", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := processTimestamp.ReplaceAllLiteralString(string(persisted), "$T"), `{"schema_version":1,"id":"proc_a","state":"lost","cwd":"/w","started_at":"$T","pid":42,"ended_at":"$T","stop_reason":"backend_restarted"}`+"\n"; got != want {
		t.Errorf("persisted manifest = %s, want %s", got, want)
	}
	second := NewProcessManager(directory)
	if err := second.Recover(); err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	snapshot, _ := second.Snapshot("proc_a")
	encoded, _ := json.Marshal(snapshot)
	if got := processTimestamp.ReplaceAllLiteralString(string(encoded), "$T"); got != processTimestamp.ReplaceAllLiteralString(want["proc_a"], "$T") {
		t.Errorf("after a second recovery proc_a encoded as %s", got)
	}
}
