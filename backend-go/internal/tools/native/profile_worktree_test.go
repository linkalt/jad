package native

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// A target that ran in a worktree of its own reports its worktree_* lines in the result,
// answered or failed, and its async record starts with the worktree line.
func TestProfileMessageResultCarriesTheTargetsWorktree(t *testing.T) {
	tool := &profileMessageTool{settings: sessionfs.NativeProfileMessageSettings{ResultMaxBytes: 1000}}
	worktree := &DelegatedWorktreeResult{
		Information: []tools.Information{{Key: "worktree_branch", Value: "jad/profile-1"}},
		Text:        "worktree branch jad/profile-1",
	}
	answered, err := tool.renderOutcome("c", "review/reviewer", ProfileMessageOutcome{Text: "done", Worktree: worktree}, nil)
	if err != nil || !strings.Contains(answered, "worktree_branch: jad/profile-1") {
		t.Fatalf("answer = %s, %v", answered, err)
	}
	failed, _ := tool.renderOutcome("c", "review/reviewer", ProfileMessageOutcome{Err: errors.New("x"), Worktree: worktree}, nil)
	if !strings.Contains(failed, "worktree_branch: jad/profile-1") {
		t.Fatalf("failure = %s", failed)
	}
	plain, _ := tool.renderOutcome("c", "review/reviewer", ProfileMessageOutcome{Text: "done"}, nil)
	if strings.Contains(plain, "worktree") {
		t.Fatalf("a target without a worktree: %s", plain)
	}
	for _, outcome := range []ProfileMessageOutcome{{Text: "done", Worktree: worktree}, {Err: ErrProfileNoAnswer, Worktree: worktree}} {
		result := tool.asyncResult("review/reviewer", outcome)
		if !strings.HasPrefix(result.Summary, worktree.Text) || !strings.HasPrefix(result.Detail, worktree.Text) {
			t.Fatalf("async result = %+v", result)
		}
	}
	if result := tool.asyncResult("review/reviewer", ProfileMessageOutcome{Text: "done"}); result.Summary != "done" || result.State != asyncmgr.StateDone {
		t.Fatalf("plain async result = %+v", result)
	}
}

// A delegated run's sandbox writes in the worktree, never in the session's folder: absolute
// paths inside the folder move, a write path that holds it becomes the worktree, hidden and
// read-only paths are kept and moved, placeholders and paths elsewhere stay.
func TestDelegatedWorktreeSandboxMovesTheSessionFolder(t *testing.T) {
	origin := t.TempDir()
	d := &DelegatedWorktree{tree: &worktree.Tree{Workspace: "/wt/ws"}}
	sandbox := &sessionfs.SandboxSettings{
		WritablePaths:     []string{"{workspace}", "{tmp}", origin + "/build", filepath.Dir(origin), "/elsewhere"},
		PlanWritablePaths: []string{"{tmp}", origin},
		ReadPaths:         []string{"/", origin + "/src"},
		HidePaths:         []string{origin + "/.env", "/secret"},
		ReadOnlyPaths:     []string{origin + "/.git/hooks"},
	}
	moved := d.Sandbox(sandbox, origin)
	for name, test := range map[string]struct{ got, want []string }{
		"write":     {moved.WritablePaths, []string{"{workspace}", "{tmp}", "/wt/ws/build", "/wt/ws", "/elsewhere"}},
		"plan":      {moved.PlanWritablePaths, []string{"{tmp}", "/wt/ws"}},
		"read":      {moved.ReadPaths, []string{"/", origin + "/src", "/wt/ws/src"}},
		"hide":      {moved.HidePaths, []string{origin + "/.env", "/wt/ws/.env", "/secret"}},
		"read-only": {moved.ReadOnlyPaths, []string{origin + "/.git/hooks", "/wt/ws/.git/hooks"}},
	} {
		if strings.Join(test.got, ",") != strings.Join(test.want, ",") {
			t.Errorf("%s = %v, want %v", name, test.got, test.want)
		}
	}
	if len(sandbox.WritablePaths) != 5 || sandbox.WritablePaths[2] != origin+"/build" {
		t.Fatalf("the sandbox it was given changed: %v", sandbox.WritablePaths)
	}
	if d.Sandbox(nil, origin) != nil {
		t.Fatal("no sandbox became one")
	}
}
