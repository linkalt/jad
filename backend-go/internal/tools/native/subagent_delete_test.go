package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// deletingManager is a persistent manager with delete_transcripts on or off.
func deletingManager(t *testing.T, root string, keep int, deleteTranscripts bool) *SubagentManager {
	t.Helper()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: keep, DeleteTranscripts: deleteTranscripts})
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// runOne runs one fast child whose transcript folder is name under root, and returns
// the folder.
func runOne(t *testing.T, root, name string, manager *SubagentManager, keep int) string {
	t.Helper()
	dir := writeTranscript(t, root, name, "answer "+name)
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(dir)
		return SubagentResult{Text: "answer " + name, Rounds: 1, Tokens: 1}, nil
	}}
	run, err := manager.start(subagentStart{
		launcher: launcher, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "t " + name},
		limit: 4, maxQueued: 4, keep: keep, resultMax: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-run.done
	return dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSubagentDeleteTranscriptsRemovesTheFolderOfADroppedRun(t *testing.T) {
	root := t.TempDir()
	manager := deletingManager(t, root, 2, true)
	var dirs []string
	for _, name := range []string{"call-a", "call-b", "call-c", "call-d"} {
		dirs = append(dirs, runOne(t, root, name, manager, 2))
	}
	for _, dir := range dirs[:2] {
		if exists(dir) {
			t.Errorf("transcript folder of a dropped run was kept: %s", dir)
		}
	}
	for _, dir := range dirs[2:] {
		if !exists(dir) {
			t.Errorf("transcript folder of a kept run was deleted: %s", dir)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, subagentManifestDir))
	if err != nil || len(entries) != 2 {
		t.Errorf("manifests = %d, %v; want 2", len(entries), err)
	}
}

func TestSubagentDeleteTranscriptsIsOffByDefault(t *testing.T) {
	root := t.TempDir()
	manager := deletingManager(t, root, 1, false)
	var dirs []string
	for _, name := range []string{"call-a", "call-b", "call-c"} {
		dirs = append(dirs, runOne(t, root, name, manager, 1))
	}
	for _, dir := range dirs {
		if !exists(dir) {
			t.Errorf("a transcript folder was deleted with delete_transcripts off: %s", dir)
		}
	}
}

func TestSubagentDeleteTranscriptsNeverTouchesARunThatIsStillGoing(t *testing.T) {
	root := t.TempDir()
	manager := deletingManager(t, root, 1, true)
	running := writeTranscript(t, root, "call-running", "partial")
	release := make(chan struct{})
	slow := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(running)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "late"}, nil
	}}
	slowRun, err := manager.start(subagentStart{
		launcher: slow, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "slow"},
		limit: 4, maxQueued: 4, keep: 1, resultMax: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"call-a", "call-b", "call-c"} {
		runOne(t, root, name, manager, 1)
	}
	if !exists(running) {
		t.Error("the folder of a run that is still going was deleted")
	}
	close(release)
	<-slowRun.done
}

func TestSubagentDeleteTranscriptsOnlyDeletesAFolderDirectlyUnderTheStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	for _, dir := range []string{outside, nested, filepath.Join(root, subagentManifestDir), root} {
		if transcriptFolderUnder(root, dir) {
			t.Errorf("transcriptFolderUnder(%q) = true, want false", dir)
		}
	}
	if transcriptFolderUnder(root, "") || transcriptFolderUnder("", filepath.Join(root, "x")) {
		t.Error("an empty root or folder must never be deletable")
	}
	if !transcriptFolderUnder(root, filepath.Join(root, "call-1-abcd1234")) {
		t.Error("a child folder directly under the storage directory must be deletable")
	}
}

func TestSubagentDeleteTranscriptsIgnoresAFolderOutsideTheStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := writeTranscript(t, t.TempDir(), "elsewhere", "keep me")
	manager := deletingManager(t, root, 1, true)
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(outside)
		return SubagentResult{Text: "x"}, nil
	}}
	for range 3 {
		run, err := manager.start(subagentStart{
			launcher: launcher, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "t"},
			limit: 4, maxQueued: 4, keep: 1, resultMax: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		<-run.done
	}
	if !exists(outside) {
		t.Error("a folder outside the storage directory was deleted")
	}
}

func TestSubagentDeleteTranscriptsAlsoPrunesAtStartup(t *testing.T) {
	root := t.TempDir()
	first := deletingManager(t, root, 10, false)
	var dirs []string
	for _, name := range []string{"call-a", "call-b", "call-c"} {
		dirs = append(dirs, runOne(t, root, name, first, 10))
		time.Sleep(2 * time.Millisecond)
	}
	_ = first.Close()
	for _, dir := range dirs {
		if !exists(dir) {
			t.Fatalf("setup: %s should still exist", dir)
		}
	}
	_ = deletingManager(t, root, 1, true)
	if exists(dirs[0]) || exists(dirs[1]) {
		t.Error("startup pruning kept the transcript folder of a dropped run")
	}
	if !exists(dirs[2]) {
		t.Error("startup pruning deleted the folder of the newest run")
	}
}

func TestSubagentDeleteTranscriptsWorksForAMemoryOnlyManager(t *testing.T) {
	root := t.TempDir()
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	manager.DeleteTranscriptsUnder(root)
	first := runOne(t, root, "call-a", manager, 1)
	second := runOne(t, root, "call-b", manager, 1)
	if exists(first) {
		t.Error("the dropped run's folder was kept")
	}
	if !exists(second) {
		t.Error("the kept run's folder was deleted")
	}
}
