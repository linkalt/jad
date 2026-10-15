package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedCompletedProcesses(tb testing.TB, directory string, count int) {
	tb.Helper()
	for index := 0; index < count; index++ {
		manifest := ProcessManifest{
			SchemaVersion: 1,
			ID:            fmt.Sprintf("proc_old_%05d", index),
			State:         ProcessExited,
			CWD:           "/workspace",
			StartedAt:     fmt.Sprintf("2025-01-01T00:%02d:%02d.000Z", index/60%60, index%60),
			EndedAt:       fmt.Sprintf("2025-01-01T00:%02d:%02d.500Z", index/60%60, index%60),
		}
		processDirectory := filepath.Join(directory, manifest.ID)
		if err := os.MkdirAll(processDirectory, 0o755); err != nil {
			tb.Fatal(err)
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			tb.Fatal(err)
		}
		for name, content := range map[string][]byte{"manifest.json": data, "stdout.log": []byte("old output\n"), "stderr.log": nil} {
			if err := os.WriteFile(filepath.Join(processDirectory, name), content, 0o600); err != nil {
				tb.Fatal(err)
			}
		}
	}
}

func countProcessDirectories(tb testing.TB, directory string) int {
	tb.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		tb.Fatal(err)
	}
	return len(entries)
}

// BenchmarkProcessEndWithPrunableHistory is the time from starting a trivial process to
// Wait returning, when ending it prunes 2000 completed processes.
func BenchmarkProcessEndWithPrunableHistory(b *testing.B) {
	workspace := b.TempDir()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		directory := filepath.Join(b.TempDir(), "processes")
		seedCompletedProcesses(b, directory, 2000)
		manager := NewProcessManager(directory)
		if err := manager.Recover(); err != nil {
			b.Fatal(err)
		}
		request := processTestRequest(workspace, "true", 1024)
		request.Policy.MaxCompleted = 1
		b.StartTimer()
		snapshot, err := manager.Start(request)
		if err != nil {
			b.Fatal(err)
		}
		if result := manager.Wait(snapshot.ID, 5000); !result.Found || result.Snapshot.State == ProcessRunning {
			b.Fatalf("Wait() = %+v", result)
		}
		b.StopTimer()
		deadline := time.Now().Add(30 * time.Second)
		for countProcessDirectories(b, directory) > 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := countProcessDirectories(b, directory); got != 1 {
			b.Fatalf("%d process directories left after prune, want 1", got)
		}
		_ = manager.Close()
	}
}

// Pruning still happens, and the process that just ended is the one that is kept.
func TestProcessEndPrunesOldHistoryAndKeepsTheNewestProcess(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(t.TempDir(), "processes")
	seedCompletedProcesses(t, directory, 30)
	manager := NewProcessManager(directory)
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	request := processTestRequest(workspace, "printf done", 1024)
	request.Policy.MaxCompleted = 3
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	result := manager.Wait(snapshot.ID, 5000)
	if !result.Found || result.Snapshot.State != ProcessExited || result.Snapshot.Output.text != "done" {
		t.Fatalf("Wait() = %+v", result)
	}
	deadline := time.Now().Add(10 * time.Second)
	for countProcessDirectories(t, directory) > 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := countProcessDirectories(t, directory); got != 3 {
		t.Fatalf("%d process directories after prune, want 3", got)
	}
	if _, err := os.Stat(filepath.Join(directory, snapshot.ID)); err != nil {
		t.Fatalf("the process that just ended was pruned: %v", err)
	}
	if again := manager.Wait(snapshot.ID, 100); !again.Found || again.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() after prune = %+v", again)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}
