package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestResolverSharesOneSubagentManagerPerStorageDirAndClosesIt(t *testing.T) {
	workspace := t.TempDir()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)

	configFor := func(dir string) sessionfs.SessionConfig {
		return sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: filepath.Join(workspace, dir)}}
	}
	first := resolver.sharedSubagentManager(configFor("kids"))
	if first == nil || first != resolver.sharedSubagentManager(configFor("kids/")) {
		t.Fatal("the resolver did not reuse the manager for the same storage directory")
	}
	if other := resolver.sharedSubagentManager(configFor("other")); other == nil || other == first {
		t.Fatal("a different storage directory shared the manager")
	}
	if resolver.sharedSubagentManager(sessionfs.SessionConfig{}) != nil {
		t.Fatal("a session without the subagent tool got a manager")
	}

	resolver.Close()
	if !first.Closed() {
		t.Fatal("closing the resolver left the sub-agent manager open")
	}
	if resolver.sharedSubagentManager(configFor("late")) != nil {
		t.Fatal("a closed resolver created a manager")
	}
}

// seedFinishedRuns writes three finished runs, oldest first, each with a transcript
// folder directly under storage, as an earlier server would have left them.
func seedFinishedRuns(t *testing.T, storage string) []string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(storage, ".runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	var folders []string
	base := time.Now().Add(-time.Hour)
	for index, id := range []string{"sub_00000001", "sub_00000002", "sub_00000003"} {
		folder := filepath.Join(storage, "call-"+id)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "messages.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, folder)
		ended := base.Add(time.Duration(index) * time.Minute)
		manifest, _ := json.Marshal(map[string]any{
			"version": 1, "id": id, "agent": "a", "task": "t", "state": "done",
			"started_at": ended.Add(-time.Second), "ended_at": ended, "dir": folder,
		})
		if err := os.WriteFile(filepath.Join(storage, ".runs", id+".json"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return folders
}

func TestResolverPassesDeleteTranscriptsToTheManager(t *testing.T) {
	for _, deleteTranscripts := range []bool{false, true} {
		workspace := t.TempDir()
		resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
		if err != nil {
			t.Fatal(err)
		}
		storage := filepath.Join(workspace, "kids")
		folders := seedFinishedRuns(t, storage)
		manager := resolver.sharedSubagentManager(sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{
			StorageDir: storage, PersistRuns: true, KeepFinished: 1, DeleteTranscripts: deleteTranscripts,
		}})
		if manager == nil {
			t.Fatal("no manager")
		}
		for index, folder := range folders {
			_, err := os.Stat(folder)
			kept := err == nil
			wantKept := !deleteTranscripts || index == 2
			if kept != wantKept {
				t.Errorf("delete_transcripts=%v: folder %d kept = %v, want %v", deleteTranscripts, index, kept, wantKept)
			}
		}
		resolver.Close()
	}
}
