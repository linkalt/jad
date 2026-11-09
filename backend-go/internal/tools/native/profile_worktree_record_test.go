package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// openRecordedDelegatedWorktree opens a delegated worktree of repo recorded in a new state
// folder, and returns it with the state folder and the record's path.
func openRecordedDelegatedWorktree(t *testing.T, repo string) (*DelegatedWorktree, tools.ExecutionContext, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	tree, err := OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, stateDir, "jad profile run review/reviewer (message from dev/coder)\n\nwrite it")
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, delegatedWorktreeRecordDir))
	if tree.record == "" || len(entries) != 1 || filepath.Join(stateDir, delegatedWorktreeRecordDir, entries[0].Name()) != tree.record {
		t.Fatalf("record %q, folder %v", tree.record, entries)
	}
	return tree, execution, stateDir, tree.record
}

// lose makes the record's server one that has stopped.
func lose(t *testing.T, record string) {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["server"] = map[string]any{"pid": 1 << 30}
	data, _ = json.Marshal(fields)
	if err := os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// §10.3: a delegated run's worktree is recorded in the target session's state folder while
// the run goes and the record is removed at its end.
func TestDelegatedWorktreeIsRecordedWhileTheRunGoes(t *testing.T) {
	repo := gitRepository(t)
	tree, execution, stateDir, record := openRecordedDelegatedWorktree(t, repo)
	// The run's own server is still going: recovery leaves the run alone.
	if got := RecoverDelegatedWorktrees(context.Background(), stateDir, nil, nil); len(got) != 0 {
		t.Fatalf("recovered a running run: %+v", got)
	}
	if _, err := os.Stat(tree.tree.Path); err != nil {
		t.Fatalf("the running run's worktree is gone: %v", err)
	}
	tree.Close(context.Background(), execution, "done")
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the record outlived the run: %v", err)
	}
}

// A server that stops while a delegated run goes leaves its record; the next server finishes
// the worktree from it: the run's work lands on its branch and the folder is removed.
func TestLostDelegatedWorktreeIsFinishedAtTheNextStart(t *testing.T) {
	repo := gitRepository(t)
	tree, _, stateDir, record := openRecordedDelegatedWorktree(t, repo)
	if err := os.WriteFile(filepath.Join(tree.tree.Workspace, "made.txt"), []byte("by the target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgetIsolatedRun(tree.tree)
	lose(t, record)
	got := RecoverDelegatedWorktrees(context.Background(), stateDir, nil, nil)
	if len(got) != 1 || !got[0].Changed || got[0].Error != "" || got[0].Branch != tree.tree.Branch {
		t.Fatalf("recovered = %+v", got)
	}
	if text := gitCommand(t, repo, "show", tree.tree.Branch+":made.txt"); strings.TrimSpace(text) != "by the target" {
		t.Fatalf("made.txt on the branch = %q", text)
	}
	if message := gitCommand(t, repo, "log", "-1", "--format=%B", tree.tree.Branch); !strings.Contains(message, "(message from dev/coder) (finished after the server stopped)") || !strings.Contains(message, "write it") {
		t.Fatalf("commit message = %q", message)
	}
	for _, gone := range []string{tree.tree.Path, tree.tree.Admin, record} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
	// Each record is tried once.
	if got := RecoverDelegatedWorktrees(context.Background(), stateDir, nil, nil); len(got) != 0 {
		t.Fatalf("second recovery = %+v", got)
	}
}

// A record that is not one jad wrote (unreadable, a relative or foreign admin folder) is
// dropped without running git on what it names.
func TestDelegatedWorktreeRecordIsChecked(t *testing.T) {
	repo := gitRepository(t)
	tree, _, stateDir, record := openRecordedDelegatedWorktree(t, repo)
	forgetIsolatedRun(tree.tree)
	lose(t, record)
	data, _ := os.ReadFile(record)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	fields["admin"] = filepath.Join(t.TempDir(), "evil")
	data, _ = json.Marshal(fields)
	if err := os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(stateDir, delegatedWorktreeRecordDir, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RecoverDelegatedWorktrees(context.Background(), stateDir, nil, nil); len(got) != 0 {
		t.Fatalf("recovered = %+v", got)
	}
	for _, gone := range []string{record, broken} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s was kept: %v", gone, err)
		}
	}
	// The worktree itself is left as it was (git worktree list shows it).
	if _, err := os.Stat(tree.tree.Path); err != nil {
		t.Fatal(err)
	}
	// No state folder, no record.
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	plain, err := OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, "", "m")
	if err != nil || plain.record != "" {
		t.Fatalf("record %q, err %v", plain.record, err)
	}
	plain.Close(context.Background(), execution, "m")
}
