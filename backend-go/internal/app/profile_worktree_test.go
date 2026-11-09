package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// isolatedTargetFixture is the layout fixture with its working directory a git repository and
// the target profile review/reviewer given exec and sandbox.isolation (isolation is the JSON
// object, e.g. {"mode":"worktree"}). The target's model writes made.txt with exec, then answers.
func isolatedTargetFixture(t *testing.T, isolation string) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitIn(t, f.cwd, "init", "--quiet")
	// The fixture's state folder is inside the repository; git must not see it as a change.
	writeFile(t, filepath.Join(f.cwd, ".gitignore"), []byte(".jad/\n"))
	gitIn(t, f.cwd, "add", ".gitignore")
	gitIn(t, f.cwd, "commit", "--quiet", "-m", "start")
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}}},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/exec","settings":{"sandbox":{"isolation":`+isolation+`}}}`)
	write := scriptFunctionCall("exec", map[string]any{"code": "printf 'by the target' > made.txt"})
	f.script = func(model, body string) string {
		if model == "model-large" {
			return write(model, body)
		}
		return ""
	}
	return f
}

// sendToReviewer sends one message to review/reviewer and returns its outcome.
func sendToReviewer(t *testing.T, f *layoutFixture) native.ProfileMessageOutcome {
	t.Helper()
	messenger := profileMessenger{resolver: f.service.resolver}
	ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
	defer cancel()
	plan, err := messenger.Prepare(ctx, native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "write it"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("the message did not end")
	}
	return run.Outcome()
}

func worktreeValue(outcome native.ProfileMessageOutcome, key string) string {
	if outcome.Worktree == nil {
		return ""
	}
	for _, item := range outcome.Worktree.Information {
		if item.Key == key {
			if text, ok := item.Value.(string); ok {
				return text
			}
		}
	}
	return ""
}

// §9.7: a profile_message target whose sandbox.isolation.mode is worktree works in a git
// worktree of its own. What it writes lands on a branch, never in the session's folder, the
// worktree is removed, and the caller's result carries the worktree_* lines.
func TestProfileMessageTargetWithIsolationWorksInAWorktree(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	head := gitIn(t, f.cwd, "rev-parse", "HEAD")
	outcome := sendToReviewer(t, f)
	if outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the target wrote into the session's folder: %v", err)
	}
	branch := worktreeValue(outcome, "worktree_branch")
	if !strings.HasPrefix(branch, "jad/profile-") {
		t.Fatalf("worktree lines = %+v", outcome.Worktree)
	}
	if got := gitIn(t, f.cwd, "show", branch+":made.txt"); got != "by the target" {
		t.Fatalf("made.txt on %s = %q", branch, got)
	}
	if got := worktreeValue(outcome, "worktree_base"); got != head {
		t.Fatalf("worktree_base = %q, want %s", got, head)
	}
	if got := worktreeValue(outcome, "worktree_conflicts"); !strings.HasPrefix(got, "none") {
		t.Fatalf("worktree_conflicts = %q", got)
	}
	if list := gitIn(t, f.cwd, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("the run's worktree was not removed:\n%s", list)
	}
	if !strings.Contains(outcome.Worktree.Text, branch) {
		t.Fatalf("worktree text = %q", outcome.Worktree.Text)
	}
	// The commit names the run; the caller's own profile runs are untouched.
	if message := gitIn(t, f.cwd, "log", "-1", "--format=%B", branch); !strings.Contains(message, "jad profile run review/reviewer (message from dev/coder)") || !strings.Contains(message, "write it") {
		t.Fatalf("commit message = %q", message)
	}
}

// D7: a user's own run of the same profile works in the session's folder.
func TestAUsersRunOfAnIsolatedProfileIsNotIsolated(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	runner := boundRunnerOf(t, f, "review/reviewer")
	if runner.config.Isolation.Mode != sessionfs.SandboxIsolationWorktree {
		t.Fatalf("isolation = %+v", runner.config.Isolation)
	}
	if runner.delegation != nil {
		t.Fatal("a resolved runner is marked delegated")
	}
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(f.cwd, "made.txt")); err != nil || string(data) != "by the target" {
		t.Fatalf("made.txt in the session folder: %v %q", err, data)
	}
	if branches := gitIn(t, f.cwd, "branch", "--list", "jad/*"); branches != "" {
		t.Fatalf("a user's run made a branch: %s", branches)
	}
}

// A target without isolation, or a non-repository folder: the target works in its folder, or
// the run fails and says why.
func TestProfileMessageTargetWithoutIsolationOrRepository(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"none"}`)
	outcome := sendToReviewer(t, f)
	if outcome.Err != nil || outcome.Worktree != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); err != nil {
		t.Fatalf("made.txt: %v", err)
	}

	f = isolatedTargetFixture(t, `{"mode":"worktree"}`)
	if err := os.RemoveAll(filepath.Join(f.cwd, ".git")); err != nil {
		t.Fatal(err)
	}
	outcome = sendToReviewer(t, f)
	var failed *native.ProfileFailedError
	if outcome.Err == nil || !errors.As(outcome.Err, &failed) || !strings.Contains(failed.Message, "worktree") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the target ran in the session's folder: %v", err)
	}
}

// The Confine hook marks only bound runners; a test's runner passes through, and the
// caller's confinement still applies to a bound one.
func TestTargetRunnerMarksTheDelegatedRun(t *testing.T) {
	f := newLayoutFixture(t)
	resolved, err := f.service.resolver.Resolve(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	hook := targetRunner(nil, "dev/coder", "m", func(native.DelegatedWorktreeResult) { reported = true })
	runner, err := hook(resolved.Runner)
	if err != nil {
		t.Fatal(err)
	}
	bound := runner.(boundRunner)
	if bound.delegation == nil || bound.delegation.report == nil || reported {
		t.Fatalf("delegation = %+v", bound.delegation)
	}
	var other httpapi.RunSession = fakeRunSession{}
	if passed, err := hook(other); err != nil || passed != other {
		t.Fatalf("a test runner: %v %v", passed, err)
	}
	confined := false
	withConfine := targetRunner(func(runner httpapi.RunSession) (httpapi.RunSession, error) {
		confined = true
		return runner, nil
	}, "dev/coder", "m", nil)
	if runner, err = withConfine(resolved.Runner); err != nil || !confined || runner.(boundRunner).delegation == nil {
		t.Fatalf("confined %v, err %v", confined, err)
	}
}

type fakeRunSession struct{}

func (fakeRunSession) Run(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
	return session.RunResult{}, nil
}

// Work the isolated run left going in its worktree (a handle it started: a background
// sub-agent, a message, an exec) is stopped before the worktree is finished; the profile's
// work from before the run is not. (An exec's own process is also swept with the run.)
func TestIsolatedTargetRunStopsWhatItStarted(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}},"async":{}},"mcp":{}}`)
	// A first run builds the session's async manager.
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	target := boundRunnerOf(t, f, "review/reviewer")
	manager := f.service.resolver.sessionAsyncManager(target.config)
	if manager == nil {
		t.Fatal("the target has no async manager")
	}
	driver := &recordingDriver{}
	if err := manager.AddDriver(driver); err != nil {
		t.Fatal(err)
	}
	owner := sessionKey(target.reference, target.config.Profile)
	if err := manager.Register(asyncmgr.Registration{ID: "earlier", Owner: owner, Kind: driver.Kind()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	target.delegation = &profileDelegation{isolation: target.config.Isolation, commitMessage: "m"}
	options := session.RunOptions{}
	options.ToolExecution.WorkspaceRoot = f.cwd
	options.ToolExecution.SessionKey = owner
	opened, err := target.openDelegatedWorktree(context.Background(), &options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(asyncmgr.Registration{ID: "during", Owner: owner, Kind: driver.Kind()}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(asyncmgr.Registration{ID: "elsewhere", Owner: owner + "x", Kind: driver.Kind()}); err != nil {
		t.Fatal(err)
	}
	opened.close(context.Background())
	if got := driver.stopped(); strings.Join(got, ",") != "during:profile_run_ended" {
		t.Fatalf("stopped = %v", got)
	}
}

type recordingDriver struct {
	mu    sync.Mutex
	stops []string
}

func (d *recordingDriver) Kind() asyncmgr.Kind { return "test_delegated" }

func (d *recordingDriver) Stop(id, reason string) {
	d.mu.Lock()
	d.stops = append(d.stops, id+":"+reason)
	d.mu.Unlock()
}

func (d *recordingDriver) stopped() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stops...)
}

// The isolated run writes no file of the session, so it takes no checkpoint; its tools work
// in the worktree.
func TestOpenDelegatedWorktreeDropsTheCheckpoint(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree","base":"working"}`)
	runner := boundRunnerOf(t, f, "review/reviewer")
	runner.delegation = &profileDelegation{isolation: runner.config.Isolation, commitMessage: "m"}
	writeFile(t, filepath.Join(f.cwd, "dirty.txt"), []byte("uncommitted\n"))
	options := session.RunOptions{Checkpoint: &checkpoint.Service{}}
	options.ToolExecution.WorkspaceRoot = f.cwd
	opened, err := runner.openDelegatedWorktree(context.Background(), &options)
	if err != nil {
		t.Fatal(err)
	}
	var reported *native.DelegatedWorktreeResult
	runner.delegation.report = func(result native.DelegatedWorktreeResult) { reported = &result }
	opened.runner = runner
	if options.Checkpoint != nil || options.ToolExecution.WorkspaceRoot == f.cwd || options.ToolExecution.WorkspaceOrigin != f.cwd {
		t.Fatalf("checkpoint %v, root %s, origin %s", options.Checkpoint, options.ToolExecution.WorkspaceRoot, options.ToolExecution.WorkspaceOrigin)
	}
	// base "working": the run sees the session's uncommitted file.
	if data, err := os.ReadFile(filepath.Join(options.ToolExecution.WorkspaceRoot, "dirty.txt")); err != nil || string(data) != "uncommitted\n" {
		t.Fatalf("dirty.txt in the worktree: %v %q", err, data)
	}
	opened.close(context.Background())
	opened.close(context.Background())
	if reported == nil || !strings.HasPrefix(reported.Text, "worktree: no changes") {
		t.Fatalf("reported = %+v", reported)
	}
}

// D20 with §9.7: a target that inherits its sandboxed caller's sandbox gets the caller's
// session folder as an absolute write path. In a worktree that path is moved, so the run writes
// its worktree (and its work reaches the branch), never the session's folder.
func TestIsolatedTargetInASandboxedCallersSandboxWritesItsWorktree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap runs on Linux only")
	}
	if out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot run here: %v %s", err, out)
	}
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	// Besides made.txt in its folder, the run tries the session's own folder by its path.
	escape := scriptFunctionCall("exec", map[string]any{"code": "printf 'by the target' > made.txt; printf x > '" + filepath.Join(f.cwd, "escaped.txt") + "' || true"})
	f.script = func(model, body string) string {
		if model == "model-large" {
			return escape(model, body)
		}
		return ""
	}
	caller := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"},
		WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	messenger := profileMessenger{resolver: f.service.resolver}
	ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
	defer cancel()
	plan, err := messenger.Prepare(ctx, native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "write it",
		Environment: &tools.Environment{Sandbox: caller}, CallerPlace: sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()
	outcome := run.Outcome()
	if outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	for _, name := range []string{"made.txt", "escaped.txt"} {
		if _, err := os.Stat(filepath.Join(f.cwd, name)); !os.IsNotExist(err) {
			t.Fatalf("the target wrote %s into the session's folder: %v", name, err)
		}
	}
	branch := worktreeValue(outcome, "worktree_branch")
	if branch == "" || worktreeValue(outcome, "worktree_error") != "" {
		t.Fatalf("worktree: %+v", outcome.Worktree)
	}
	if files := gitIn(t, f.cwd, "show", "--name-only", "--format=", branch); files != "made.txt" {
		data, _ := os.ReadFile(boundRunnerOf(t, f, "review/reviewer").config.MessagesPath)
		t.Fatalf("files on %s = %q; transcript:\n%s", branch, files, data)
	}
}

// §10.3: the worktree of a delegated run the last server lost is finished the first time the
// next server resolves the target's session: the run's work is on its branch, the folder gone.
func TestResolveFinishesALostDelegatedWorktree(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	resolved, err := f.service.resolver.Resolve(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := resolved.Runner.(boundRunner).config.StateDir
	execution := tools.ExecutionContext{WorkspaceRoot: f.cwd}
	if _, err := native.OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, stateDir, "jad profile run review/reviewer (message from dev/coder)"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(execution.WorkspaceRoot, "made.txt"), []byte("lost\n"))
	records, _ := filepath.Glob(filepath.Join(stateDir, "delegated-worktrees", "*.json"))
	if len(records) != 1 {
		t.Fatalf("records = %v", records)
	}
	// The server that wrote the record has stopped.
	data, _ := os.ReadFile(records[0])
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["server"] = map[string]any{"pid": 1 << 30}
	data, _ = json.Marshal(record)
	writeFile(t, records[0], data)
	branch, _ := record["branch"].(string)
	// This resolver already looked at the folder; a new server would look once.
	f.service.resolver.delegatedRecovered.Delete(filepath.Clean(stateDir))
	if _, err := f.service.resolver.Resolve(context.Background(), "local:demo", "dev/coder", ""); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, f.cwd, "show", branch+":made.txt"); got != "lost" {
		t.Fatalf("made.txt on %s = %q", branch, got)
	}
	if list := gitIn(t, f.cwd, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("the lost worktree was not removed:\n%s", list)
	}
	if _, err := os.Stat(records[0]); !os.IsNotExist(err) {
		t.Fatalf("the record is still there: %v", err)
	}
}

// A message from another server's profile (remote API) to a profile whose sandbox.isolation.mode
// is worktree runs it in a worktree too, and the run's result carries the worktree lines.
func TestRemoteMessageToAnIsolatedProfileWorksInAWorktree(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	backend := remoteBackend{resolver: f.service.resolver, runTimeout: time.Minute, runs: &remoteRunIndex{}}
	run, err := backend.SendMessage(context.Background(), remoteapi.MessageRequest{Session: "demo", Profile: "review/reviewer", Message: "write it", Caller: "remote:peer"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-time.After(time.Minute):
		t.Fatal("the message did not end")
	}
	result := run.Result()
	if result.State != "done" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the target wrote into the session's folder: %v", err)
	}
	lines := map[string]string{}
	for _, line := range result.Worktree {
		lines[line.Key] = line.Value
	}
	branch := lines["worktree_branch"]
	if !strings.HasPrefix(branch, "jad/profile-") || !strings.HasPrefix(lines["worktree_conflicts"], "none") {
		t.Fatalf("worktree lines = %+v", result.Worktree)
	}
	if got := gitIn(t, f.cwd, "show", branch+":made.txt"); got != "by the target" {
		t.Fatalf("made.txt on %s = %q", branch, got)
	}
	if message := gitIn(t, f.cwd, "log", "-1", "--format=%B", branch); !strings.Contains(message, "(message from remote:peer)") {
		t.Fatalf("commit message = %q", message)
	}
}
