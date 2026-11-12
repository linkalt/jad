package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
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
