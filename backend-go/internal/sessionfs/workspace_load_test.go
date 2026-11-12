package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// wsFixture is a home with the global folder G = <home>/.jad and workspaces beside it.
type wsFixture struct{ base, home, global string }

func newWSFixture(t *testing.T) wsFixture {
	t.Helper()
	base := t.TempDir()
	f := wsFixture{base: base, home: filepath.Join(base, "home")}
	f.global = filepath.Join(f.home, ".jad")
	wsPut(t, filepath.Join(f.global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	return f
}

func wsPut(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// workspace makes base/<name> a workspace with workspace.json, profile dev/coder and session s1.
func (f wsFixture) workspace(t *testing.T, name, workspaceJSON string) string {
	t.Helper()
	dir := filepath.Join(f.base, name)
	wsPut(t, filepath.Join(dir, ".jad", "workspace.json"), workspaceJSON)
	wsPut(t, filepath.Join(dir, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"model_default":"openai/default"}`)
	wsPut(t, filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	return dir
}

func (f wsFixture) globalJSON(t *testing.T, text string) {
	t.Helper()
	wsPut(t, filepath.Join(f.global, "global.json"), text)
}

func (f wsFixture) load(cwd string) (Config, error) {
	return Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{"HOME": f.home}})
}

func wsMustLoad(t *testing.T, f wsFixture, cwd string) Config {
	t.Helper()
	config, err := f.load(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func wsSession(t *testing.T, config Config, reference, profile string) (SessionConfig, error) {
	t.Helper()
	location, err := Resolve(config.Roots, reference)
	if err != nil {
		t.Fatal(err)
	}
	return LoadSessionSettings(location, profile, "")
}

func TestWorkspaceLayoutKeepsSessionsProfilesAndStateInTheWorkspace(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{"schema_version":2}`)
	config := wsMustLoad(t, f, w)
	if len(config.Roots) != 1 || config.Roots[0].ID != "proj" || config.GlobalDir != f.global {
		t.Fatalf("roots = %+v, global = %s", config.Roots, config.GlobalDir)
	}
	if config.Roots[0].Directory != filepath.Join(w, ".jad", "sessions") {
		t.Fatalf("sessions = %s", config.Roots[0].Directory)
	}
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkingDir != w {
		t.Fatalf("cwd = %s, want the workspace %s", session.WorkingDir, w)
	}
	if want := filepath.Join(w, ".jad", "state", "s1", "dev", "coder", "messages.jsonl"); session.MessagesPath != want {
		t.Fatalf("messages = %s, want %s", session.MessagesPath, want)
	}
	if want := filepath.Join(f.global, "models", "openai", "default.json"); session.ModelPath != want {
		t.Fatalf("model = %s, want the global %s", session.ModelPath, want)
	}
	if !slices.ContainsFunc(config.Warnings, func(w string) bool { return strings.Contains(w, "do not enable a sandbox") }) {
		t.Fatalf("no warning that there is no sandbox: %q", config.Warnings)
	}
}

// A v1 start never reads G/global.json, however broken it is, and a v2 start never reads a
// setting.json in G or W.
func TestGlobalJSONIsIgnoredByAV1StartAndSettingJSONByAV2Start(t *testing.T) {
	f := newWSFixture(t)
	f.globalJSON(t, `{"not":"a v1 key"`)
	v1 := filepath.Join(f.base, "old")
	wsPut(t, filepath.Join(v1, ".jad", "setting.json"), `{"session_roots":[{"id":"old","path":"s"}]}`)
	config := wsMustLoad(t, f, v1)
	if config.Roots[0].ID != "old" || config.Workspaces != nil {
		t.Fatalf("v1 start: %+v", config.Roots)
	}
	bare := filepath.Join(f.base, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if config := wsMustLoad(t, f, bare); config.Roots[0].ID != defaultRootID {
		t.Fatalf("no setting: %+v", config.Roots)
	}

	f.globalJSON(t, `{"schema_version":2}`)
	wsPut(t, filepath.Join(f.global, "setting.json"), `{"broken"`)
	w := f.workspace(t, "proj", `{}`)
	wsPut(t, filepath.Join(w, ".jad", "setting.json"), `{"broken"`)
	config = wsMustLoad(t, f, w)
	notes := strings.Join(config.Warnings, "\n")
	for _, ignored := range []string{filepath.Join(f.global, "setting.json"), filepath.Join(w, ".jad", "setting.json")} {
		if !strings.Contains(notes, ignored+" is not read") {
			t.Errorf("no warning about %s: %s", ignored, notes)
		}
	}
	// A named root setting keeps v1 even in a workspace.
	named, err := Load(LoadOptions{WorkingDir: w, RootSettingPath: filepath.Join(v1, ".jad", "setting.json"), Environment: map[string]string{"HOME": f.home}})
	if err != nil || named.Roots[0].ID != "old" {
		t.Fatalf("named setting: %+v, %v", named.Roots, err)
	}
}

func wsMustRefuse(t *testing.T, f wsFixture, cwd string, wants ...string) {
	t.Helper()
	_, err := f.load(cwd)
	if err == nil {
		t.Fatalf("start in %s was not refused", cwd)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
}

// Design Q2, row 1: W/.jad is G, or W holds G.
func TestWorkspaceThatHoldsTheGlobalFolderIsRefused(t *testing.T) {
	f := newWSFixture(t)
	wsPut(t, filepath.Join(f.home, ".jad", "workspace.json"), `{}`)
	wsMustRefuse(t, f, f.home, "contains the global folder")

	f = newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	inside := filepath.Join(w, "sub", "g")
	_, err := Load(LoadOptions{WorkingDir: w, GlobalDir: inside, Environment: map[string]string{"HOME": f.home}})
	if err == nil || !strings.Contains(err.Error(), "contains the global folder") {
		t.Fatalf("G inside W: %v", err)
	}
	// Through a symlink too.
	link := filepath.Join(f.base, "link")
	if err := os.Symlink(w, link); err != nil {
		t.Fatal(err)
	}
	_, err = Load(LoadOptions{WorkingDir: w, GlobalDir: filepath.Join(link, ".jad"), Environment: map[string]string{"HOME": f.home}})
	if err == nil || !strings.Contains(err.Error(), "contains the global folder") {
		t.Fatalf("G as W/.jad through a link: %v", err)
	}
	// A listed workspace that holds G is refused as well.
	f = newWSFixture(t)
	w = f.workspace(t, "proj", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"home","path":"~"}]}`)
	wsMustRefuse(t, f, w, "contains the global folder")
}

// Design Q2, row 2: the same id or the same directory twice.
func TestDuplicateWorkspacesAreRefused(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"x","path":"`+a+`"},{"id":"x","path":"`+b+`"}]}`)
	wsMustRefuse(t, f, a, "listed twice")
	f.globalJSON(t, `{"workspaces":[{"id":"x","path":"`+a+`"},{"id":"y","path":"`+a+`/."}]}`)
	wsMustRefuse(t, f, b, "same directory")
}

// Design Q2, row 3: a relative path.
func TestRelativeWorkspacePathIsRefused(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"b","path":"../b"}]}`)
	wsMustRefuse(t, f, a, "must be absolute or start with ~")
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"~/../a"}]}`)
	config := wsMustLoad(t, f, a)
	if config.Roots[0].ID != "a" {
		t.Fatalf("a ~ path: %+v", config.Roots)
	}
}

// Design Q2, row 4: the start workspace's workspace.json is broken or has an unknown key.
func TestBrokenStartWorkspaceIsRefused(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"catalogs":{}}`)
	wsMustRefuse(t, f, a, "catalogs is not a key")
	f.workspace(t, "a", `{"schema_version":1}`)
	wsMustRefuse(t, f, a, "unsupported schema_version")
	f.workspace(t, "a", `{`)
	wsMustRefuse(t, f, a, "invalid JSON")
	f.workspace(t, "a", `{}`)
	f.globalJSON(t, `{"session_roots":[]}`)
	wsMustRefuse(t, f, a, "session_roots is not a key")
	f.globalJSON(t, `{"libraries":{"agent_dirs":["p"]}}`)
	wsMustRefuse(t, f, a, "libraries.agent_dirs is not a key here")
}

// Design Q2, row 5: a listed workspace that is missing or broken is skipped with a warning and
// reported as unavailable.
func TestMissingOrBrokenListedWorkspaceIsSkipped(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	broken := f.workspace(t, "broken", `{"unknown":1}`)
	missing := filepath.Join(f.base, "missing")
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"broken","path":"`+broken+`"},{"id":"missing","path":"`+missing+`"}]}`)
	config := wsMustLoad(t, f, a)
	if len(config.Roots) != 1 || config.Roots[0].ID != "a" {
		t.Fatalf("roots = %+v", config.Roots)
	}
	if len(config.Workspaces) != 3 || config.Workspaces[1].Unavailable == "" || config.Workspaces[2].Unavailable == "" || config.Workspaces[0].Unavailable != "" {
		t.Fatalf("workspaces = %+v", config.Workspaces)
	}
	notes := strings.Join(config.Warnings, "\n")
	if !strings.Contains(notes, "workspace broken is skipped") || !strings.Contains(notes, "workspace missing is skipped") {
		t.Fatalf("warnings = %s", notes)
	}
	// Its .jad stays hidden from the served one.
	if !slices.Contains(config.Roots[0].Layout.FixedHidePaths, filepath.Join(broken, ".jad")) {
		t.Fatalf("fixed hidden = %q", config.Roots[0].Layout.FixedHidePaths)
	}
}

// Design Q2, row 6: a start directory that is not listed takes its folder name as id, and a
// clash with a listed id is refused.
func TestUnlistedStartTakesItsFolderNameUnlessTaken(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "my proj", `{}`)
	config := wsMustLoad(t, f, a)
	if config.Roots[0].ID != "my-proj" {
		t.Fatalf("id = %s", config.Roots[0].ID)
	}
	other := f.workspace(t, "other", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"my-proj","path":"`+other+`"}]}`)
	wsMustRefuse(t, f, a, "is not listed", "taken by a listed workspace")
	// Listed under its own id it works, and the other is served too.
	f.globalJSON(t, `{"workspaces":[{"id":"other","path":"`+other+`"},{"id":"mine","path":"`+a+`","label":"Mine"}]}`)
	config = wsMustLoad(t, f, a)
	if config.Roots[0].ID != "mine" || config.Roots[0].Label != "Mine" || len(config.Roots) != 2 || config.Roots[1].ID != "other" {
		t.Fatalf("roots = %+v", config.Roots)
	}
}

// Design Q2, row 7: a workspace inside another is allowed; each hides the other's .jad.
func TestNestedWorkspacesHideEachOthersJadFolder(t *testing.T) {
	f := newWSFixture(t)
	outer := f.workspace(t, "outer", `{}`)
	inner := f.workspace(t, filepath.Join("outer", "inner"), `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"outer","path":"`+outer+`"},{"id":"inner","path":"`+inner+`"}]}`)
	config := wsMustLoad(t, f, outer)
	if len(config.Roots) != 2 {
		t.Fatalf("roots = %+v", config.Roots)
	}
	hidden := map[string][]string{}
	for _, root := range config.Roots {
		hidden[root.ID] = root.Layout.FixedHidePaths
	}
	if !slices.Contains(hidden["outer"], filepath.Join(inner, ".jad")) || !slices.Contains(hidden["inner"], filepath.Join(outer, ".jad")) {
		t.Fatalf("hidden = %q", hidden)
	}
	for id, paths := range hidden {
		if !slices.Contains(paths, filepath.Join(f.global, "auth")) {
			t.Fatalf("%s does not hide G/auth: %q", id, paths)
		}
	}
}

// The same session id in two workspaces shares no state, needs a qualifier, and may be created
// in the second workspace.
func TestSameSessionIDInTwoWorkspacesIsSeparate(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	config := wsMustLoad(t, f, a)
	if _, err := Resolve(config.Roots, "s1"); err == nil {
		t.Fatal("an unqualified id in two workspaces must be ambiguous")
	}
	first, err := wsSession(t, config, "a:s1", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := wsSession(t, config, "b:s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.MessagesPath == second.MessagesPath || !strings.HasPrefix(second.MessagesPath, b) {
		t.Fatalf("messages %s and %s", first.MessagesPath, second.MessagesPath)
	}
	if err := os.Remove(filepath.Join(b, ".jad", "sessions", "s1.json")); err != nil {
		t.Fatal(err)
	}
	if _, created, err := WriteSessionFile(config.Roots, "b:s1", []byte(`{"schema_version":1}`), WriteCondition{IfNoneMatch: "*"}, false); err != nil || !created {
		t.Fatalf("creating s1 in b while a has one: %v created=%v", err, created)
	}
	// Tools see only their own workspace (design Q6): another workspace's session and a
	// missing one give the same error.
	foreign, foreignErr := ResolveFrom(config.Roots, "a:s1", "b:s1")
	_, missingErr := ResolveFrom(config.Roots, "a:s1", "a:nope")
	if foreignErr == nil || missingErr == nil || foreignErr.Error() != missingErr.Error() || foreign.File != "" {
		t.Fatalf("foreign %+v %v, missing %v", foreign, foreignErr, missingErr)
	}
	if own, err := ResolveFrom(config.Roots, "a:s1", "s1"); err != nil || own.Root.ID != "a" || own.Reference != "a:s1" {
		t.Fatalf("own unqualified: %+v, %v", own, err)
	}
	if got := VisibleFrom(config.Roots, "a:s1", []string{"a:s1", "b:s1"}); !slices.Equal(got, []string{"a:s1"}) {
		t.Fatalf("visible = %q", got)
	}
}

// A model or toolset name is looked up in the global and the workspace folder; a name in both
// is an error naming both files, never a silent pick. A private model is not seen by another
// workspace.
func TestScopedCatalogNamesAreNeverPickedSilently(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	wsPut(t, filepath.Join(a, ".jad", "models", "local", "mine.json"), `{"provider":"openai","model":"gpt-mine","api_key_env":"FIXTURE_KEY"}`)
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "private.json"), `{"models":["local/mine"],"model_default":"local/mine"}`)
	wsPut(t, filepath.Join(b, ".jad", "profiles", "dev", "private.json"), `{"models":["local/mine"],"model_default":"local/mine"}`)
	config := wsMustLoad(t, f, a)
	session, err := wsSession(t, config, "a:s1", "dev/private")
	if err != nil || session.ModelPath != filepath.Join(a, ".jad", "models", "local", "mine.json") {
		t.Fatalf("private model: %s, %v", session.ModelPath, err)
	}
	if _, err := wsSession(t, config, "b:s1", "dev/private"); err == nil || !strings.Contains(err.Error(), "local/mine") {
		t.Fatalf("b reached a's private model: %v", err)
	}
	wsPut(t, filepath.Join(a, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-shadow","api_key_env":"FIXTURE_KEY"}`)
	_, err = wsSession(t, config, "a:s1", "dev/coder")
	if err == nil || !strings.Contains(err.Error(), "more than one scope") || !strings.Contains(err.Error(), filepath.Join(f.global, "models")) || !strings.Contains(err.Error(), filepath.Join(a, ".jad", "models")) {
		t.Fatalf("a name in both scopes: %v", err)
	}
	// The other workspace still loads the global model.
	if session, err := wsSession(t, config, "b:s1", "dev/coder"); err != nil || session.ModelPath != filepath.Join(f.global, "models", "openai", "default.json") {
		t.Fatalf("b: %s, %v", session.ModelPath, err)
	}
}

func wsSandboxDefaults(f wsFixture, extra string) string {
	return `{"defaults":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"` + wsTrue() + `"` + extra + `}}}`
}

// wsTrue is an executable that exists, standing in for bwrap: the loader only looks it up.
func wsTrue() string {
	for _, path := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "true"
}

// The fixed hidden paths survive a profile's hide_paths and a template; the workspace config
// folders are read-only in the sandbox.
func TestFixedHiddenAndReadOnlyPathsOfAWorkspaceSandbox(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],`+strings.TrimPrefix(wsSandboxDefaults(f, ""), "{"))
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "open.json"), `{"models":["openai/default"],"settings":{"sandbox":{"hide_paths":[]}}}`)
	config := wsMustLoad(t, f, a)
	session, err := wsSession(t, config, "a:s1", "dev/open")
	if err == nil {
		t.Fatal("a profile that drops the default hidden paths loosened the sandbox")
	}
	if !strings.Contains(err.Error(), "stop hiding") {
		t.Fatalf("error = %v", err)
	}
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "open.json"), `{"models":["openai/default"],"settings":{"sandbox":{"add_hide_paths":["/x"]}}}`)
	session, err = wsSession(t, config, "a:s1", "dev/open")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(f.global, "auth"), filepath.Join(b, ".jad"), "/x"} {
		if !slices.Contains(session.Sandbox.HidePaths, want) {
			t.Errorf("hidden %q lacks %s", session.Sandbox.HidePaths, want)
		}
	}
	// G and the own W/.jad are sealed; the helper folders stay writable for the tools covered.
	for _, want := range []string{f.global, filepath.Join(a, ".jad")} {
		if !slices.Contains(session.Sandbox.SealedPaths, want) {
			t.Errorf("sealed %q lacks %s", session.Sandbox.SealedPaths, want)
		}
	}
	if slices.Contains(session.Sandbox.SealedPaths, filepath.Join(b, ".jad")) {
		t.Errorf("sealed %q holds the other workspace", session.Sandbox.SealedPaths)
	}
	if !slices.Equal(session.Sandbox.SealedInner, []string{filepath.Join(a, ".jad", "exec"), filepath.Join(a, ".jad", "functions")}) {
		t.Errorf("sealed inner = %q", session.Sandbox.SealedInner)
	}
	// A profile that turns a sandbox on itself may set hide_paths, but not drop the fixed ones.
	wsPut(t, filepath.Join(b, ".jad", "profiles", "dev", "own.json"), `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"`+wsTrue()+`","hide_paths":[]}}}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	unsandboxed := wsMustLoad(t, f, a)
	own, err := wsSession(t, unsandboxed, "b:s1", "dev/own")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(own.Sandbox.HidePaths, []string{filepath.Join(f.global, "auth"), filepath.Join(a, ".jad")}) {
		t.Fatalf("own sandbox hidden = %q", own.Sandbox.HidePaths)
	}
	for _, want := range []string{filepath.Join(a, ".jad", "workspace.json"), filepath.Join(a, ".jad", "sessions"), filepath.Join(a, ".jad", "models"), filepath.Join(a, ".jad", "tools"), filepath.Join(a, ".jad", "profiles"), filepath.Join(a, ".jad", "state"), filepath.Join(f.global, "global.json"), filepath.Join(f.global, "models")} {
		if !slices.Contains(session.Sandbox.ReadOnlyPaths, want) {
			t.Errorf("read-only %q lacks %s", session.Sandbox.ReadOnlyPaths, want)
		}
	}
}

// Design Q7: an ordered key set below a profile can only be tightened by it. An unselected
// profile that loosens one is left out with a warning; a selected one fails to load.
func TestProfileCannotLoosenOrderedKeysInAWorkspace(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"defaults":{"max_run_tokens":1000,"budget":{"max_daily_tokens":5000}}}`)
	f.globalJSON(t, wsSandboxDefaults(f, `,"network_allow":["example.com"]`))
	profile := func(name, settings string) {
		wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", name+".json"), `{"models":["openai/default"],"settings":`+settings+`}`)
	}
	profile("tight", `{"max_run_tokens":500,"budget":{"max_daily_tokens":100},"sandbox":{"network_allow":[]}}`)
	profile("tokens", `{"max_run_tokens":2000}`)
	profile("budget", `{"budget":{"max_daily_tokens":null}}`)
	profile("off", `{"sandbox":{"enabled":false}}`)
	profile("net", `{"sandbox":{"network":true,"network_allow":[]}}`)
	profile("hosts", `{"sandbox":{"network_allow":["example.com","evil.test"]}}`)
	profile("write", `{"sandbox":{"writable_paths":["{workspace}","/etc"]}}`)
	config := wsMustLoad(t, f, a)
	if _, err := wsSession(t, config, "s1", "dev/tight"); err != nil {
		t.Fatalf("tightening: %v", err)
	}
	session, err := wsSession(t, config, "s1", "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(session.Warnings, "\n")
	for name, want := range map[string]string{
		"tokens": "max_run_tokens may not be raised", "budget": "budget.max_daily_tokens may not be raised", "off": "turn the sandbox off",
		"net": "turn the network on", "hosts": "network host evil.test", "write": "make /etc writable",
	} {
		if slices.Contains(session.Profiles, "dev/"+name) {
			t.Errorf("profile %s is offered", name)
		}
		if !strings.Contains(notes, want) {
			t.Errorf("no warning %q for %s in:\n%s", want, name, notes)
		}
		if _, err := wsSession(t, config, "s1", "dev/"+name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("selected %s: %v", name, err)
		}
	}
}

// v1 layouts are never checked for ceilings: a v1 profile may still raise a default.
func TestV1ProfileMayStillRaiseADefault(t *testing.T) {
	layout := Layout{Defaults: map[string]json.RawMessage{"max_run_tokens": json.RawMessage("10")}}
	if err := checkProfileCeilings(layout, layoutSessionFile{}, "p.json", map[string]json.RawMessage{"max_run_tokens": json.RawMessage("99")}, nil); err != nil {
		t.Fatal(err)
	}
	layout.Workspace = "/w"
	if err := checkProfileCeilings(layout, layoutSessionFile{}, "p.json", map[string]json.RawMessage{"max_run_tokens": json.RawMessage("99")}, nil); err == nil {
		t.Fatal("the workspace layout must refuse it")
	}
}
