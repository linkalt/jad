package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/worktree"
)

// writeLostWorktreeRun stores the manifest of a run that was going, in a worktree, when the
// server stopped.
func writeLostWorktreeRun(t *testing.T, root, id string, record subagentWorktree) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, subagentManifestDir), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"version": 1, "id": id, "owner": "alpha", "agent": "fixer", "task": "fix it", "state": "running", "started_at": "2025-01-01T00:00:00Z", "worktree": record}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, subagentManifestDir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A run lost while it worked in a worktree has that work committed to its branch at the next
// start, and the folder removed; before, the folder stayed with uncommitted work nobody knew of.
func TestRestartFinishesTheWorktreeOfALostRun(t *testing.T) {
	repo := gitRepository(t)
	options := worktreeOptions(sessionfs.NativeSubagentWorktreeSettings{})
	tree, err := worktree.Create(context.Background(), options, repo, "sub_0000eeee", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Workspace, "left.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeLostWorktreeRun(t, root, "sub_0000eeee", subagentWorktree{Branch: tree.Branch, Base: tree.Base, Path: tree.Path})
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	run := manager.byID("sub_0000eeee")
	if run == nil || run.state != subagentRunLost {
		t.Fatalf("restored run = %+v, want a lost run", run)
	}
	record := run.view().worktree
	if record == nil || !record.Changed || record.Error != "" || record.Path != "" || record.Commit == "" {
		t.Fatalf("worktree record = %+v, want a changed branch and no folder", record)
	}
	if _, err := os.Stat(tree.Path); !os.IsNotExist(err) {
		t.Fatalf("the lost run's worktree folder is still there: %v", err)
	}
	if files := gitCommand(t, repo, "show", "--name-only", "--format=", tree.Branch); !strings.Contains(files, "left.txt") {
		t.Fatalf("the branch does not hold the lost run's file: %q", files)
	}
	if saved := manifestOf(t, root, "sub_0000eeee"); saved.Worktree == nil || !saved.Worktree.Changed {
		t.Fatalf("the manifest does not record the finished worktree: %+v", saved.Worktree)
	}
}

// A lost run whose worktree folder is gone keeps its branch name and says so.
func TestRestartRecordsAGoneWorktree(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	writeLostWorktreeRun(t, root, "sub_0000ffff", subagentWorktree{Branch: "jad/sub_0000ffff", Base: "abc", Path: gone})
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	record := manager.byID("sub_0000ffff").view().worktree
	if record == nil || !strings.Contains(record.Error, "gone") || record.Path != "" || record.Branch != "jad/sub_0000ffff" {
		t.Fatalf("worktree record = %+v, want the gone folder reported", record)
	}
}
