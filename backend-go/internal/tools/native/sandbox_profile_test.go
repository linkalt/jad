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
	"jad/backend-go/internal/worktree"
)

// profileSandboxFixture is a workspace a with profiles dev/coder (sandboxed, the one loaded)
// and dev/other, each with a private document and a data file.
type profileSandboxFixture struct {
	a, home string
	config  sessionfs.SessionConfig
}

func newProfileSandboxFixture(t *testing.T, writable string) profileSandboxFixture {
	t.Helper()
	return newProfileSandboxFixtureWith(t, writable, `"{tmp}"`)
}

func newProfileSandboxFixtureWith(t *testing.T, writable, planWritable string) profileSandboxFixture {
	t.Helper()
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	home := filepath.Join(base, "home")
	a := filepath.Join(base, "a")
	writeWorkspaceFile(t, filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "workspace.json"), `{}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap",`+
		`"writable_paths":[`+writable+`],"plan_writable_paths":[`+planWritable+`]}}}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"]}`)
	for _, name := range []string{"coder", "other"} {
		writeWorkspaceFile(t, filepath.Join(a, ".jad", "profile-own", "dev", name, "documents", "doc.md"), name+"-doc")
		writeWorkspaceFile(t, filepath.Join(a, ".jad", "data", "profiles", "dev", name, "notes.md"), name+"-notes")
	}
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	return profileSandboxFixture{a: a, home: home, config: config}
}

// run runs command in the profile's sandbox (or the run's own, environment) with execution.
func (f profileSandboxFixture) run(t *testing.T, command string, execution tools.ExecutionContext, sandbox *sessionfs.SandboxSettings) string {
	t.Helper()
	settings := bashTestSettings(t, f.a)
	settings.Sandbox = sandbox
	execution.CallID, execution.WorkspaceRoot, execution.SessionDir = "profile-call", f.a, t.TempDir()
	return executeNativeBash(context.Background(), map[string]any{"command": command}, settings, "", execution)
}

func profileAttempt(name, command string) string {
	return `if ( ` + command + ` ) 2>/dev/null; then echo "` + name + `=yes"; else echo "` + name + `=no"; fi; `
}

func wantProfileOutput(t *testing.T, label, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("%s: want %s in:\n%s", label, want, output)
		}
	}
}

// In its own top-level run a profile writes its data folder, reads its own private folder,
// cannot change it, cannot change the configuration, and does not see another profile's
// private or data folder.
func TestProfileSandboxOwnDataIsWritableAndOthersAreHidden(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	own := filepath.Join(f.a, ".jad", "data", "profiles", "dev", "coder")
	if f.config.Sandbox == nil || f.config.Sandbox.ProfileData != own {
		t.Fatalf("sandbox = %+v", f.config.Sandbox)
	}
	command := profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`) +
		profileAttempt("readown", `grep -q coder-doc .jad/profile-own/dev/coder/documents/doc.md`) +
		profileAttempt("writeown", `echo x > .jad/profile-own/dev/coder/documents/doc.md`) +
		profileAttempt("newprofile", `echo '{}' > .jad/profiles/dev/x.json`) +
		profileAttempt("readother", `cat .jad/profile-own/dev/other/documents/doc.md`) +
		profileAttempt("readotherdata", `cat .jad/data/profiles/dev/other/notes.md`) +
		profileAttempt("writeotherdata", `mkdir -p .jad/data/profiles/dev/other && echo x > .jad/data/profiles/dev/other/notes.md`) +
		profileAttempt("newprofiledata", `mkdir -p .jad/data/profiles/dev/x`)
	output := f.run(t, command, tools.ExecutionContext{}, f.config.Sandbox)
	wantProfileOutput(t, "own run", output, "data=yes", "readown=yes", "writeown=no", "newprofile=no", "readother=no", "readotherdata=no", "writeotherdata=no", "newprofiledata=no")
	if data, err := os.ReadFile(filepath.Join(own, "new.md")); err != nil || string(data) != "hi\n" {
		t.Errorf("own data file = %q, %v", data, err)
	}
	for _, path := range []string{filepath.Join(f.a, ".jad", "profiles", "dev", "x.json"), filepath.Join(f.a, ".jad", "data", "profiles", "dev", "x")} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s was made from the sandbox", path)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.a, ".jad", "data", "profiles", "dev", "other", "notes.md")); string(data) != "other-notes" {
		t.Errorf("other profile's data = %q", data)
	}
}

// Plan mode, a sub-agent, an inherited sandbox and a narrowed one read the data folder but do
// not write it, and never write the caller's.
func TestProfileSandboxDataIsReadOnlyOutsideTheProfilesOwnRun(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	command := profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`) +
		profileAttempt("readdata", `grep -q coder-notes .jad/data/profiles/dev/coder/notes.md`)
	output := f.run(t, command, tools.ExecutionContext{PlanMode: true}, f.config.Sandbox)
	wantProfileOutput(t, "plan mode", output, "data=no")
	output = f.run(t, command, tools.ExecutionContext{AgentDepth: 1}, f.config.Sandbox)
	wantProfileOutput(t, "sub-agent", output, "data=no", "readdata=yes")

	callerPlace := sessionfs.SandboxPlace{Workspace: f.a, CWD: f.a, SessionDir: f.config.StateDir, ProfileData: f.config.ProfileDataDir}
	inherited := sessionfs.InheritSandbox(f.config.Sandbox, callerPlace.Expander())
	if inherited.ProfileData != "" || len(inherited.ProfileShown) != 0 {
		t.Fatalf("inherited = %+v", inherited)
	}
	output = f.run(t, command, tools.ExecutionContext{Environment: &tools.Environment{Sandbox: inherited}}, nil)
	wantProfileOutput(t, "inherited", output, "data=no", "readdata=no")

	target := *f.config.Sandbox
	target.ProfileShown = []string{filepath.Join(f.a, ".jad", "profile-own", "dev", "other"), filepath.Join(f.a, ".jad", "data", "profiles", "dev", "other")}
	target.ProfileData = target.ProfileShown[1]
	narrowed, err := sessionfs.NarrowSandbox(f.config.Sandbox, &target, callerPlace.Expander(), callerPlace.Expander())
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.ProfileData != "" || len(narrowed.ProfileShown) != 0 {
		t.Fatalf("narrowed = %+v", narrowed)
	}
	output = f.run(t, command+profileAttempt("targetdata", `echo hi > .jad/data/profiles/dev/other/new.md`), tools.ExecutionContext{Environment: &tools.Environment{Sandbox: narrowed}}, nil)
	wantProfileOutput(t, "narrowed", output, "data=no", "targetdata=no")
}

// {profile_data} in writable paths names the run's own data folder, and nothing for a sub-agent.
func TestProfileDataPlaceholderFollowsTheRun(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{profile_data}","{tmp}"`)
	useOwnTempDir(t)
	command := profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`) + profileAttempt("project", `echo hi > project.txt`)
	wantProfileOutput(t, "own run", f.run(t, command, tools.ExecutionContext{}, f.config.Sandbox), "data=yes", "project=no")
	wantProfileOutput(t, "sub-agent", f.run(t, command, tools.ExecutionContext{AgentDepth: 1}, f.config.Sandbox), "data=no", "project=no")
}

// A link anywhere below W/.jad on the way to the data folder refuses the call.
func TestProfileDataBehindALinkRefusesTheCall(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	elsewhere := t.TempDir()
	dev := filepath.Join(f.a, ".jad", "data", "profiles", "dev")
	if err := os.RemoveAll(dev); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dev); err != nil {
		t.Fatal(err)
	}
	output := f.run(t, `echo hi > .jad/data/profiles/dev/coder/new.md && echo wrote`, tools.ExecutionContext{}, f.config.Sandbox)
	if strings.Contains(output, "wrote") || !strings.Contains(output, "is a link or not a folder") {
		t.Fatalf("output = %s", output)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("the link target got %v", entries)
	}
}

// Without a workspace layout nothing changes, and ensureProfileData refuses other paths.
func TestEnsureProfileDataRefusesOtherPaths(t *testing.T) {
	if err := ensureProfileData(filepath.Join(t.TempDir(), "x", "y")); err == nil {
		t.Fatal("a path that is no profile data folder was accepted")
	}
	w := t.TempDir()
	if err := os.MkdirAll(filepath.Join(w, ".jad"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(w, ".jad", "data", "profiles", "dev", "coder")
	if err := ensureProfileData(data); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(data); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("data folder = %v, %v", info, err)
	}
}

// Plan mode never writes the data folder, also where its writable paths cover the workspace.
func TestProfileSandboxPlanModeWithAWritableWorkspace(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixtureWith(t, `"{workspace}","{tmp}"`, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	command := profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`) + profileAttempt("project", `echo hi > project.txt`)
	wantProfileOutput(t, "plan mode", f.run(t, command, tools.ExecutionContext{PlanMode: true}, f.config.Sandbox), "data=no", "project=yes")
}

// A link on the way to the own private folder (here its category folder) shows nothing: the
// link could lead to another profile's folder.
func TestProfileSandboxDoesNotShowAnOwnFolderBehindALink(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	own := filepath.Join(f.a, ".jad", "profile-own")
	writeWorkspaceFile(t, filepath.Join(own, "devx", "coder", "documents", "doc.md"), "elsewhere-doc")
	if err := os.RemoveAll(filepath.Join(own, "dev")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(own, "devx"), filepath.Join(own, "dev")); err != nil {
		t.Fatal(err)
	}
	command := profileAttempt("readown", `cat .jad/profile-own/dev/coder/documents/doc.md`) + profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`)
	wantProfileOutput(t, "linked own folder", f.run(t, command, tools.ExecutionContext{}, f.config.Sandbox), "readown=no", "data=yes")
}

// The seatbelt profile (text only; seatbelt itself is not run here) hides the private profile
// folders and allows writing only the own data folder.
func TestSeatbeltProfileHidesOtherProfilesFolders(t *testing.T) {
	w := t.TempDir()
	if real, err := filepath.EvalSymlinks(w); err == nil {
		w = real
	}
	own := filepath.Join(w, ".jad", "profile-own", "dev", "a")
	data := filepath.Join(w, ".jad", "data", "profiles", "dev", "a")
	for _, dir := range []string{own, data, filepath.Join(w, ".jad", "profile-own", "dev", "b")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendSeatbelt, ReadPaths: []string{"/"}, WritablePaths: []string{w},
		ProfileHidden: []string{filepath.Join(w, ".jad", "profile-own"), filepath.Join(w, ".jad", "data", "profiles")}, ProfileShown: []string{own, data}, ProfileData: data}
	place := sandboxPlace{workspace: w, cwd: w}
	text := seatbeltProfile(sandbox, place, sandboxExpander(place))
	for _, want := range []string{
		`(deny file-read* file-write* (subpath "` + filepath.Join(w, ".jad", "profile-own") + `"))`,
		`(allow file-read* (subpath "` + own + `"))`,
		`(allow file-write* (subpath "` + data + `"))`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("want %s in:\n%s", want, text)
		}
	}
	if strings.Contains(text, `(allow file-write* (subpath "`+own+`"))`) {
		t.Errorf("the own private folder is writable:\n%s", text)
	}
}

// The own data folder is writable in the profile's own top-level run with no writable path
// that covers it (decider S1, F2); plan mode, a sub-agent and a worktree run read it only, and
// the profile's private folder and the project stay read-only.
func TestProfileSandboxOwnDataIsWritableWithoutAWritablePath(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{tmp}"`)
	useOwnTempDir(t)
	if writable, reason := sessionfs.ProfileDataAccess(f.config, sessionfs.ProfileRun{}); !writable {
		t.Fatalf("profile config says %q", reason)
	}
	command := profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`) +
		profileAttempt("readdata", `grep -q coder-notes .jad/data/profiles/dev/coder/notes.md`) +
		profileAttempt("writeown", `echo x > .jad/profile-own/dev/coder/documents/doc.md`) +
		profileAttempt("project", `echo hi > project.txt`)
	wantProfileOutput(t, "own run", f.run(t, command, tools.ExecutionContext{}, f.config.Sandbox), "data=yes", "readdata=yes", "writeown=no", "project=no")
	wantProfileOutput(t, "plan mode", f.run(t, command, tools.ExecutionContext{PlanMode: true}, f.config.Sandbox), "data=no", "readdata=yes")
	wantProfileOutput(t, "sub-agent", f.run(t, command, tools.ExecutionContext{AgentDepth: 1}, f.config.Sandbox), "data=no", "readdata=yes")

	d := &DelegatedWorktree{tree: &worktree.Tree{Workspace: t.TempDir()}}
	moved := d.Sandbox(f.config.Sandbox, f.a)
	if moved.ProfileData != "" {
		t.Fatalf("worktree sandbox keeps the data folder %s", moved.ProfileData)
	}
	if writable, _ := sessionfs.ProfileDataAccess(f.config, sessionfs.ProfileRun{HasEnvironment: true, Sandbox: moved}); writable {
		t.Fatal("profile config says a worktree run writes the data folder")
	}
	wantProfileOutput(t, "worktree", f.run(t, command, tools.ExecutionContext{Environment: &tools.Environment{Sandbox: moved}}, nil), "data=no", "readdata=yes")
	if data, err := os.ReadFile(filepath.Join(f.config.ProfileDataDir, "new.md")); err != nil || string(data) != "hi\n" {
		t.Errorf("own data file = %q, %v", data, err)
	}
}

// With both profile folder roots missing and a writable path that covers W/.jad/data (so
// W/.jad is not sealed), the engine makes the roots before the sandbox starts and hides them:
// no call can make another profile's private or data folder (decider F1).
func TestProfileSandboxMakesMissingRootsSoOthersCannotBePlanted(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}/.jad/data","{tmp}"`)
	useOwnTempDir(t)
	ownRoot, dataRoot := filepath.Join(f.a, ".jad", "profile-own"), filepath.Join(f.a, ".jad", "data", "profiles")
	for _, root := range []string{ownRoot, dataRoot} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}
	command := profileAttempt("plantdata", `mkdir -p .jad/data/profiles/dev/other && echo x > .jad/data/profiles/dev/other/notes.md`) +
		profileAttempt("plantown", `mkdir -p .jad/profile-own/dev/other/documents && echo x > .jad/profile-own/dev/other/documents/doc.md`) +
		profileAttempt("data", `echo hi > .jad/data/profiles/dev/coder/new.md`)
	// The sub-agent first: it has no data folder of its own, so only the roots are made for it.
	for _, run := range []struct {
		name      string
		execution tools.ExecutionContext
	}{{"sub-agent", tools.ExecutionContext{AgentDepth: 1}}, {"own run", tools.ExecutionContext{}}} {
		name, execution := run.name, run.execution
		wantProfileOutput(t, name, f.run(t, command, execution, f.config.Sandbox), "plantdata=no", "plantown=no")
		for _, planted := range []string{filepath.Join(dataRoot, "dev", "other"), filepath.Join(ownRoot, "dev", "other")} {
			if _, err := os.Lstat(planted); err == nil {
				t.Fatalf("%s: %s was made from the sandbox", name, planted)
			}
		}
	}
	for _, root := range []string{ownRoot, dataRoot} {
		if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("root %s = %v, %v", root, info, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(f.config.ProfileDataDir, "new.md")); err != nil || string(data) != "hi\n" {
		t.Errorf("own data file = %q, %v", data, err)
	}
}

// ensureProfileRoots refuses a path that is no profile folder root and a link in a root's
// place, leaves out a root whose W/.jad does not exist, and takes a W/.jad behind a link.
func TestEnsureProfileRootsRefusesLinksAndOtherPaths(t *testing.T) {
	w := t.TempDir()
	if err := ensureProfileRoots([]string{filepath.Join(w, "x", "profiles")}); err == nil {
		t.Fatal("a path that is no profile folder root was accepted")
	}
	if err := ensureProfileRoots([]string{filepath.Join(w, "missing", ".jad", "profile-own")}); err != nil {
		t.Fatalf("a missing W/.jad: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(w, "missing")); err == nil {
		t.Fatal("a missing workspace was made")
	}
	jad := filepath.Join(w, ".jad")
	if err := os.MkdirAll(jad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(jad, "profile-own")); err != nil {
		t.Fatal(err)
	}
	err := ensureProfileRoots([]string{filepath.Join(jad, "data", "profiles"), filepath.Join(jad, "profile-own")})
	if err == nil || !strings.Contains(err.Error(), "is a link or not a folder") {
		t.Fatalf("a linked root: %v", err)
	}
	if info, err := os.Stat(filepath.Join(jad, "data", "profiles")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("data root = %v, %v", info, err)
	}

	// W/.jad behind a link: the roots are given as written and below its target, whose name
	// is not .jad; both forms are made once.
	w2 := t.TempDir()
	target := filepath.Join(w2, "config")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(w2, ".jad")); err != nil {
		t.Fatal(err)
	}
	linked := []string{filepath.Join(w2, ".jad", "profile-own"), filepath.Join(w2, ".jad", "data", "profiles"), filepath.Join(target, "profile-own"), filepath.Join(target, "data", "profiles")}
	if err := ensureProfileRoots(linked); err != nil {
		t.Fatalf("a linked W/.jad: %v", err)
	}
	for _, root := range linked[2:] {
		if info, err := os.Lstat(root); err != nil || !info.IsDir() {
			t.Fatalf("root %s = %v, %v", root, info, err)
		}
	}
}

// The history a self-edit keeps (W/.jad/profile-own/<cat>/<name>/history) cannot be changed by
// the profile's own sandboxed calls, even with all of W writable, and another profile does not
// see it (decider S6). The edit itself is the engine's write, outside the sandbox.
func TestProfileSandboxKeepsTheHistoryReadOnlyAndPrivate(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.a, Environment: map[string]string{"HOME": f.home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := sessionfs.ReadProfileConfig(f.config, sessionfs.ProfileRun{})
	if err != nil {
		t.Fatal(err)
	}
	var contents map[string]json.RawMessage
	if err := json.Unmarshal(view.Contents, &contents); err != nil {
		t.Fatal(err)
	}
	contents["description"] = json.RawMessage(`"edited"`)
	encoded, _ := json.Marshal(contents)
	result, err := sessionfs.EditProfile(location, "dev/coder", sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit, IfMatch: view.Revision, Contents: encoded})
	if err != nil {
		t.Fatal(err)
	}
	historyDir := filepath.Join(f.a, ".jad", "profile-own", "dev", "coder", "history")
	entryPath := filepath.Join(historyDir, result.HistoryID+".json")
	kept, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatal(err)
	}
	history := ".jad/profile-own/dev/coder/history"
	command := profileAttempt("read", `grep -q '"action"' `+history+`/`+result.HistoryID+`.json`) +
		profileAttempt("add", `echo '{}' > `+history+`/000099-0123456789ab.json`) +
		profileAttempt("change", `echo x > `+history+`/`+result.HistoryID+`.json`) +
		profileAttempt("remove", `rm -f `+history+`/`+result.HistoryID+`.json && test ! -e `+history+`/`+result.HistoryID+`.json`) +
		profileAttempt("replace", `mv `+history+` .jad/profile-own/dev/coder/old`) +
		profileAttempt("profile", `echo '{}' > .jad/profiles/dev/coder.json`)
	config, err := sessionfs.LoadSessionSettings(location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	wantProfileOutput(t, "own run", f.run(t, command, tools.ExecutionContext{}, config.Sandbox), "read=yes", "add=no", "change=no", "remove=no", "replace=no", "profile=no")
	if now, err := os.ReadFile(entryPath); err != nil || string(now) != string(kept) {
		t.Fatalf("the history entry changed on the host: %v", err)
	}
	if entries, _ := os.ReadDir(historyDir); len(entries) != 1 {
		t.Fatalf("history folder holds %d entries", len(entries))
	}

	// Another profile of the workspace, sandboxed the same way, does not see the history.
	other := filepath.Join(f.a, ".jad", "profiles", "dev", "other.json")
	writeWorkspaceFile(t, other, `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","writable_paths":["{workspace}","{tmp}"]}}}`)
	otherConfig, err := sessionfs.LoadSessionSettings(location, "dev/other", "")
	if err != nil {
		t.Fatal(err)
	}
	wantProfileOutput(t, "other profile", f.run(t, profileAttempt("read", `cat `+history+`/`+result.HistoryID+`.json`)+profileAttempt("list", `ls `+history), tools.ExecutionContext{}, otherConfig.Sandbox), "read=no", "list=no")
}

// The own documents an edit writes are read-only to the profile's own sandboxed exec (all of W
// writable): it cannot create, change, delete or move them; another profile does not see them.
func TestProfileSandboxKeepsTheDocumentsReadOnlyAndPrivate(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.a, Environment: map[string]string{"HOME": f.home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := sessionfs.ReadProfileConfig(f.config, sessionfs.ProfileRun{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionfs.EditProfile(location, "dev/coder", sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit, IfMatch: view.Revision,
		Documents: []sessionfs.ProfileDocumentChange{{Path: "notes.md", Content: []byte("own-notes")}}}); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(f.a, ".jad", "profile-own", "dev", "coder", "documents", "notes.md")
	documents := ".jad/profile-own/dev/coder/documents"
	command := profileAttempt("read", `grep -q own-notes `+documents+`/notes.md`) +
		profileAttempt("create", `echo x > `+documents+`/new.md`) +
		profileAttempt("change", `echo x > `+documents+`/notes.md`) +
		profileAttempt("remove", `rm -f `+documents+`/notes.md && test ! -e `+documents+`/notes.md`) +
		profileAttempt("folder", `mkdir `+documents+`/sub`) +
		profileAttempt("replace", `mv `+documents+` .jad/profile-own/dev/coder/old`)
	config, err := sessionfs.LoadSessionSettings(location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	wantProfileOutput(t, "own run", f.run(t, command, tools.ExecutionContext{}, config.Sandbox), "read=yes", "create=no", "change=no", "remove=no", "folder=no", "replace=no")
	if got, err := os.ReadFile(host); err != nil || string(got) != "own-notes" {
		t.Fatalf("the document changed on the host: %q, %v", got, err)
	}
	// The fixture's own doc.md and the edit's notes.md, nothing the exec call made.
	entries, _ := os.ReadDir(filepath.Dir(host))
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "doc.md,notes.md" {
		t.Fatalf("documents folder holds %v", names)
	}

	other := filepath.Join(f.a, ".jad", "profiles", "dev", "other.json")
	writeWorkspaceFile(t, other, `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","writable_paths":["{workspace}","{tmp}"]}}}`)
	otherConfig, err := sessionfs.LoadSessionSettings(location, "dev/other", "")
	if err != nil {
		t.Fatal(err)
	}
	wantProfileOutput(t, "other profile", f.run(t, profileAttempt("read", `cat `+documents+`/notes.md`)+profileAttempt("list", `ls `+documents)+profileAttempt("write", `echo x > `+documents+`/planted.md`), tools.ExecutionContext{}, otherConfig.Sandbox), "read=no", "list=no", "write=no")
	if _, err := os.Stat(filepath.Join(filepath.Dir(host), "planted.md")); err == nil {
		t.Fatal("another profile planted a document")
	}
}

// The run's own models folder is read-only to its own exec and hidden from another profile's;
// the loader still composes the own model from it (3c.1).
func TestProfileSandboxKeepsTheOwnModelsReadOnlyAndPrivate(t *testing.T) {
	requireBwrap(t)
	f := newProfileSandboxFixture(t, `"{workspace}","{tmp}"`)
	useOwnTempDir(t)
	host := filepath.Join(f.a, ".jad", "profile-own", "dev", "coder", "models", "openai", "fast.json")
	const ownModel = `{"based_on":"global:openai/default","set":{"name":"own-fast"}}`
	writeWorkspaceFile(t, host, ownModel)
	writeWorkspaceFile(t, filepath.Join(f.a, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default","profile:openai/fast"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","writable_paths":["{workspace}","{tmp}"]}}}`)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.a, Environment: map[string]string{"HOME": f.home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/coder", "openai/fast")
	if err != nil || config.ModelPath != host || !config.OwnModel(host) {
		t.Fatalf("own model = %s, %v", config.ModelPath, err)
	}
	models := ".jad/profile-own/dev/coder/models"
	command := profileAttempt("read", `grep -q own-fast `+models+`/openai/fast.json`) +
		profileAttempt("create", `echo x > `+models+`/openai/new.json`) +
		profileAttempt("change", `echo x > `+models+`/openai/fast.json`) +
		profileAttempt("remove", `rm -f `+models+`/openai/fast.json && test ! -e `+models+`/openai/fast.json`) +
		profileAttempt("folder", `mkdir `+models+`/local`) +
		profileAttempt("replace", `mv `+models+` .jad/profile-own/dev/coder/old`)
	wantProfileOutput(t, "own run", f.run(t, command, tools.ExecutionContext{}, config.Sandbox), "read=yes", "create=no", "change=no", "remove=no", "folder=no", "replace=no")
	if got, err := os.ReadFile(host); err != nil || string(got) != ownModel {
		t.Fatalf("the own model changed on the host: %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(host)); len(entries) != 1 {
		t.Fatalf("the own models folder holds %d files", len(entries))
	}

	other := filepath.Join(f.a, ".jad", "profiles", "dev", "other.json")
	writeWorkspaceFile(t, other, `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","writable_paths":["{workspace}","{tmp}"]}}}`)
	otherConfig, err := sessionfs.LoadSessionSettings(location, "dev/other", "")
	if err != nil {
		t.Fatal(err)
	}
	wantProfileOutput(t, "other profile", f.run(t, profileAttempt("read", `cat `+models+`/openai/fast.json`)+profileAttempt("write", `echo x > `+models+`/openai/planted.json`), tools.ExecutionContext{}, otherConfig.Sandbox), "read=no", "write=no")
	if _, err := os.Stat(filepath.Join(filepath.Dir(host), "planted.json")); err == nil {
		t.Fatal("another profile planted an own model")
	}
}

// A writable path a profile lists into a profile-own folder (its own documents, another
// profile's, the profile-own root, through .. or a link, or the whole W/.jad) opens nothing: the
// fixed read-only and hidden rules are bound after it and win (3b.1).
func TestProfileSandboxWritablePathsDoNotOpenProfileOwn(t *testing.T) {
	requireBwrap(t)
	for _, writable := range []string{
		`"{workspace}/.jad/profile-own/dev/coder/documents","{workspace}/.jad/profile-own/dev/other/documents"`,
		`"{workspace}/.jad/profile-own"`,
		`"{workspace}/.jad/profile-own/dev/../dev/coder/documents"`,
		`"{workspace}/own","{workspace}/theirs"`,
		`"{workspace}/.jad"`,
	} {
		f := newProfileSandboxFixture(t, writable+`,"{tmp}"`)
		useOwnTempDir(t)
		for link, target := range map[string]string{"own": "coder", "theirs": "other"} {
			if err := os.Symlink(filepath.Join(f.a, ".jad", "profile-own", "dev", target, "documents"), filepath.Join(f.a, link)); err != nil {
				t.Fatal(err)
			}
		}
		own, theirs := ".jad/profile-own/dev/coder/documents", ".jad/profile-own/dev/other/documents"
		command := profileAttempt("read", `grep -q coder-doc `+own+`/doc.md`) +
			profileAttempt("create", `echo x > `+own+`/new.md`) +
			profileAttempt("change", `echo x > `+own+`/doc.md`) +
			profileAttempt("history", `mkdir -p .jad/profile-own/dev/coder/history/x`) +
			profileAttempt("linkwrite", `echo x > own/new.md`) +
			profileAttempt("other", `cat `+theirs+`/doc.md`) +
			profileAttempt("plant", `echo x > `+theirs+`/planted.md`) +
			profileAttempt("linkplant", `echo x > theirs/planted.md`)
		wantProfileOutput(t, writable, f.run(t, command, tools.ExecutionContext{}, f.config.Sandbox),
			"read=yes", "create=no", "change=no", "history=no", "linkwrite=no", "other=no", "plant=no", "linkplant=no")
		for _, path := range []string{filepath.Join(f.a, own, "new.md"), filepath.Join(f.a, theirs, "planted.md"), filepath.Join(f.a, ".jad", "profile-own", "dev", "coder", "history")} {
			if _, err := os.Stat(path); err == nil {
				t.Errorf("%s: %s was made on the host", writable, path)
			}
		}
		if got, err := os.ReadFile(filepath.Join(f.a, own, "doc.md")); err != nil || string(got) != "coder-doc" {
			t.Errorf("%s: the own document changed: %q, %v", writable, got, err)
		}
	}
}
