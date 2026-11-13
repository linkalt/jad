package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// {profile_own} is the profile's own private folder in the workspace layout.
func TestProfileOwnPromptDocumentIsTheProfilesFolder(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"prompt_documents":["{profile_own}/documents/me.md"]}`)
	wsPut(t, filepath.Join(w, ".jad", "profile-own", "dev", "coder", "documents", "me.md"), "mine")
	config := wsMustLoad(t, f, w)
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Model.PromptText != "mine" {
		t.Fatalf("prompt = %q", session.Model.PromptText)
	}
	if want := filepath.Join(w, ".jad", "profile-own", "dev", "coder"); session.ProfileOwnDir != want {
		t.Fatalf("own dir = %s, want %s", session.ProfileOwnDir, want)
	}
	if want := filepath.Join(w, ".jad", "data", "profiles", "dev", "coder"); session.ProfileDataDir != want {
		t.Fatalf("data dir = %s, want %s", session.ProfileDataDir, want)
	}
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"prompt_documents":["{profile_own}documents/me.md"]}`)
	if _, err := wsSession(t, config, "s1", ""); err == nil || !strings.Contains(err.Error(), "{profile_own} may only be followed by a /") {
		t.Fatalf("{profile_own} glued to a name = %v", err)
	}
}

// In v1 {profile_own} is no placeholder: the text is a path relative to the cwd as before.
func TestProfileOwnIsLiteralInV1(t *testing.T) {
	layout, location, root := sampleLayout(t)
	var cwd string
	editJSON(t, location.File, func(m map[string]any) { cwd, _ = m["cwd"].(string) })
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["prompt_documents"] = []string{"{profile_own}/x.md"}
	})
	wsPut(t, filepath.Join(cwd, "{profile_own}", "x.md"), "literal")
	config, err := LoadLayoutSessionConfig(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.Model.PromptText, "literal") || config.ProfileOwnDir != "" || config.ProfileDataDir != "" {
		t.Fatalf("prompt = %q, own = %q, data = %q", config.Model.PromptText, config.ProfileOwnDir, config.ProfileDataDir)
	}
}

// A profile may not put another profile's private document in its prompt, by its path, by a
// relative path, through a link or through its own folder linked to the other's; selected it
// fails, unselected it is left out with a warning.
func TestAnotherProfilesPrivateDocumentIsRefused(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	b := f.workspace(t, "other", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"proj","path":"`+w+`"},{"id":"other","path":"`+b+`"}]}`)
	secret := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "documents", "secret.md")
	wsPut(t, secret, "secret")
	if err := os.Symlink(secret, filepath.Join(w, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w, ".jad", "profile-own", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(w, ".jad", "profile-own", "dev", "coder"), filepath.Join(w, ".jad", "profile-own", "dev", "linked")); err != nil {
		t.Fatal(err)
	}
	config := wsMustLoad(t, f, w)
	for name, document := range map[string]string{
		"direct":   "{workspace}/.jad/profile-own/dev/coder/documents/secret.md",
		"relative": "link.md",
		"link":     filepath.Join(w, "link.md"),
		"linked":   "{profile_own}/documents/secret.md",
	} {
		profile := "dev/" + name
		wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", name+".json"), `{"models":["openai/default"],"prompt_documents":["`+document+`"]}`)
		if _, err := wsSession(t, config, "proj:s1", profile); err == nil || !strings.Contains(err.Error(), "another profile's private folder") {
			t.Fatalf("%s selected = %v", name, err)
		}
	}
	own, err := wsSession(t, config, "proj:s1", "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dev/direct", "dev/relative", "dev/link", "dev/linked"} {
		if slices.Contains(own.Profiles, name) {
			t.Fatalf("%s is offered: %v", name, own.Profiles)
		}
	}
	if !slices.ContainsFunc(own.Warnings, func(w string) bool { return strings.Contains(w, "dev/direct") }) {
		t.Fatalf("no warning names the left-out profile: %q", own.Warnings)
	}
	// The same id in another workspace is another profile.
	wsPut(t, filepath.Join(b, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"prompt_documents":["`+secret+`"]}`)
	otherConfig := wsMustLoad(t, f, b)
	if _, err := wsSession(t, otherConfig, "other:s1", "dev/coder"); err == nil || !strings.Contains(err.Error(), "another profile's private folder") {
		t.Fatalf("other workspace's dev/coder = %v", err)
	}
}

// The check after the load also covers the documents of statecontext triggers and agents.
func TestPrivateDocumentCheckCoversTriggersAndAgentToolsets(t *testing.T) {
	w := t.TempDir()
	layout := Layout{Workspace: w, ProfileOwnRoots: []string{filepath.Join(w, ".jad", profileOwnFolder)}}
	secret := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "secret.md")
	wsPut(t, secret, "secret")
	trigger := SessionConfig{StateContext: StateContextSettings{Triggers: map[string]StateContextTrigger{"compaction": {PromptDocuments: []string{secret}}}}}
	if err := layout.checkPrivateDocuments("dev/coder", "p.json", trigger); err != nil {
		t.Fatalf("own document refused: %v", err)
	}
	if err := layout.checkPrivateDocuments("dev/other", "p.json", trigger); err == nil {
		t.Fatal("a trigger's private document of another profile passes")
	}
	nested := SessionConfig{AgentToolsets: map[string]SessionConfig{"x": trigger}}
	if err := layout.checkPrivateDocuments("dev/other", "p.json", nested); err == nil {
		t.Fatal("an agent toolset's private document of another profile passes")
	}
}

// profile config's effective settings are the composed ones, and each says which layers set it.
func TestProfileConfigSaysWhereEachSettingComesFrom(t *testing.T) {
	f := newWSFixture(t)
	f.globalJSON(t, `{"defaults":{"max_tool_rounds":5,"compaction":{"backup_mode":"replace","trigger_tokens":1000}}}`)
	w := f.workspace(t, "proj", `{"defaults":{"compaction":{"trigger_tokens":2000},"max_parallel_tool_calls":3}}`)
	wsPut(t, filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1,"settings":{"max_parallel_tool_calls":4}}`)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"settings":{"compaction":{"backup_mode":"append"},"max_tool_rounds":7}}`)
	config := wsMustLoad(t, f, w)
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := ReadProfileConfig(session, ProfileRun{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"cwd":                       {SettingLayerBuiltIn},
		"max_tool_rounds":           {SettingLayerProfile},
		"max_parallel_tool_calls":   {SettingLayerSession},
		"compaction.backup_mode":    {SettingLayerProfile},
		"compaction.trigger_tokens": {SettingLayerWorkspace},
	}
	for key, layers := range want {
		if !slices.Equal(view.SetBy[key], layers) {
			t.Fatalf("set_by[%s] = %v, want %v (all %v)", key, view.SetBy[key], layers, view.SetBy)
		}
	}
	if session.MaxToolRounds != 7 || session.MaxParallelToolCalls != 4 {
		t.Fatalf("composed rounds %d parallel %d", session.MaxToolRounds, session.MaxParallelToolCalls)
	}
	var compaction struct {
		BackupMode    string `json:"backup_mode"`
		TriggerTokens int64  `json:"trigger_tokens"`
	}
	if err := json.Unmarshal(view.Effective["compaction"], &compaction); err != nil || compaction.BackupMode != session.Compaction.BackupMode || compaction.TriggerTokens != int64(session.Compaction.TriggerTokens) || compaction.TriggerTokens != 2000 || compaction.BackupMode != "append" {
		t.Fatalf("effective compaction = %s, composed %+v", view.Effective["compaction"], session.Compaction)
	}
	if !slices.Equal(view.Layers, []string{SettingLayerBuiltIn, SettingLayerGlobal, SettingLayerWorkspace, SettingLayerSession, SettingLayerProfile}) {
		t.Fatalf("layers = %v", view.Layers)
	}
	contents, _ := os.ReadFile(session.ProfilePath)
	if view.Revision != CatalogETag(contents) || !json.Valid(view.Contents) || view.OwnDir != session.ProfileOwnDir || view.DataDir != session.ProfileDataDir {
		t.Fatalf("view = %+v", view)
	}
	for _, key := range []string{"server", "triggers"} {
		if _, found := view.Effective[key]; found {
			t.Fatalf("effective shows %s", key)
		}
	}
}

// A value a later layer replaces with a value that is not an object drops the earlier layers,
// and a composed value that is not the merge names every layer that has the key.
func TestSetByFollowsTheMergeRules(t *testing.T) {
	sources := SettingSources{
		Layers: []SettingLayer{
			{Name: "a", Values: map[string]json.RawMessage{"x": json.RawMessage(`{"y":1,"z":2}`), "s": json.RawMessage(`{"enabled":true}`), "l": json.RawMessage(`[1]`), "o": json.RawMessage(`"text"`)}},
			{Name: "b", Values: map[string]json.RawMessage{"x": json.RawMessage(`"replaced"`), "s": json.RawMessage(`{"write":{}}`), "l": json.RawMessage(`[2]`), "o": json.RawMessage(`{"k":1}`)}},
		},
		Effective: map[string]json.RawMessage{"x": json.RawMessage(`"replaced"`), "s": json.RawMessage(`{"enabled":true,"write":{"paths":["/x"]}}`), "l": json.RawMessage(`[2]`), "o": json.RawMessage(`{"k":1}`), "hooks": json.RawMessage(`{}`)},
	}
	got := sources.SetBy()
	want := map[string][]string{"x": {"b"}, "s": {"a", "b"}, "l": {"b"}, "o.k": {"b"}, "hooks": {SettingLayerToolset}}
	if len(got) != len(want) {
		t.Fatalf("set_by = %v", got)
	}
	for key, layers := range want {
		if !slices.Equal(got[key], layers) {
			t.Fatalf("set_by[%s] = %v, want %v", key, got[key], layers)
		}
	}
}

// The load checks the documents of statecontext triggers too, which judging does not see.
func TestTriggerPrivateDocumentOfAnotherProfileFailsTheLoad(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	secret := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "secret.md")
	wsPut(t, secret, "secret")
	wsPut(t, filepath.Join(w, ".jad", "tools", "dev", "t.json"), `{"native":{"task":{}}}`)
	trigger := `"settings":{"statecontext":{"triggers":{"compaction":{"tools":["task"],"prompt_documents":["` + secret + `"]}}}}`
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"],"toolset":["dev/t"],`+trigger+`}`)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":["dev/t"],`+trigger+`}`)
	config := wsMustLoad(t, f, w)
	if _, err := wsSession(t, config, "s1", "dev/coder"); err != nil {
		t.Fatalf("own trigger document: %v", err)
	}
	if _, err := wsSession(t, config, "s1", "dev/other"); err == nil || !strings.Contains(err.Error(), "another profile's private folder") {
		t.Fatalf("another profile's trigger document = %v", err)
	}
}

// The loader gives the session sandbox the private folders to hide, the own ones to show and
// its data folder; the plan-mode fallback and plan mode get no data folder.
func TestWorkspaceSandboxKnowsTheProfilesFolders(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", wsSandboxDefaults(f, ""))
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":["dev/t"]}`)
	wsPut(t, filepath.Join(w, ".jad", "tools", "dev", "t.json"), `{"native":{"exec":{}}}`)
	session, err := wsSession(t, wsMustLoad(t, f, w), "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	jad := filepath.Join(w, ".jad")
	sandbox := session.Sandbox
	if sandbox == nil {
		t.Fatal("no sandbox")
	}
	if want := []string{filepath.Join(jad, "profile-own"), filepath.Join(jad, "data", "profiles")}; !slices.Equal(sandbox.ProfileHidden, want) {
		t.Fatalf("hidden = %q, want %q", sandbox.ProfileHidden, want)
	}
	if want := []string{session.ProfileOwnDir, session.ProfileDataDir}; !slices.Equal(sandbox.ProfileShown, want) || sandbox.ProfileData != session.ProfileDataDir {
		t.Fatalf("shown = %q, data = %q", sandbox.ProfileShown, sandbox.ProfileData)
	}
	if plan := sandbox.ForPlanMode(); plan.ProfileData != "" || len(plan.ProfileShown) != 2 {
		t.Fatalf("plan mode = %+v", plan)
	}
	if fallback := session.NativeExec.Child.PlanSandbox; fallback != nil && fallback.ProfileData != "" {
		t.Fatalf("plan fallback has a data folder: %+v", fallback)
	}
}

// NarrowSandbox and InheritSandbox never pass a data folder on; the shown folders are those
// both sides show, and an inherited sandbox shows none and drops {profile_data} paths.
func TestNarrowedAndInheritedSandboxesWriteNoDataFolder(t *testing.T) {
	parent := &SandboxSettings{Backend: SandboxBackendBwrap, WritablePaths: []string{"/w", "{profile_data}"}, ReadPaths: []string{"/"},
		ProfileHidden: []string{"/w/.jad/profile-own"}, ProfileShown: []string{"/w/.jad/profile-own/dev/a"}, ProfileData: "/w/.jad/data/profiles/dev/a"}
	child := &SandboxSettings{Backend: SandboxBackendBwrap, WritablePaths: []string{"/w"}, ReadPaths: []string{"/"},
		ProfileHidden: []string{"/w/.jad/data/profiles"}, ProfileShown: []string{"/w/.jad/profile-own/dev/a", "/w/.jad/profile-own/dev/b"}, ProfileData: "/w/.jad/data/profiles/dev/b"}
	narrowed, err := NarrowSandbox(parent, child, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.ProfileData != "" || !slices.Equal(narrowed.ProfileShown, []string{"/w/.jad/profile-own/dev/a"}) || len(narrowed.ProfileHidden) != 2 {
		t.Fatalf("narrowed = %+v", narrowed)
	}
	place := SandboxPlace{Workspace: "/w", CWD: "/w", ProfileData: parent.ProfileData}
	inherited := InheritSandbox(parent, place.Expander())
	if inherited.ProfileData != "" || inherited.ProfileShown != nil || !slices.Equal(inherited.WritablePaths, []string{"/w"}) || !slices.Equal(inherited.ProfileHidden, parent.ProfileHidden) {
		t.Fatalf("inherited = %+v", inherited)
	}
	if parent.ProfileData == "" || len(parent.WritablePaths) != 2 {
		t.Fatal("the parent was changed")
	}
}

// profile config says when the data folder is writable in this run, and why not.
func TestProfileDataAccessFollowsTheRun(t *testing.T) {
	config := SessionConfig{WorkingDir: "/w", ProfileDataDir: "/w/.jad/data/profiles/dev/a",
		Sandbox: &SandboxSettings{Backend: SandboxBackendBwrap, Tools: []string{"exec"}, WritablePaths: []string{"{workspace}"}, ProfileData: "/w/.jad/data/profiles/dev/a"}}
	// No writable path covers the data folder: the own top-level run writes it anyway (F2).
	unlisted := config
	unlistedSandbox := *config.Sandbox
	unlistedSandbox.WritablePaths = []string{"{tmp}"}
	unlisted.Sandbox = &unlistedSandbox
	for name, test := range map[string]struct {
		config SessionConfig
		run    ProfileRun
		want   bool
		reason string
	}{
		"own":      {config, ProfileRun{}, true, "writable"},
		"unlisted": {unlisted, ProfileRun{}, true, "own top-level run"},
		"worktree": {config, ProfileRun{HasEnvironment: true, Sandbox: config.Sandbox.WithoutProfileData()}, false, "worktree"},
		"plan":     {config, ProfileRun{PlanMode: true}, false, "plan mode"},
		"confined": {config, ProfileRun{HasEnvironment: true, Sandbox: config.Sandbox.WithoutProfileData()}, false, "confined"},
		"none":     {SessionConfig{ProfileDataDir: config.ProfileDataDir}, ProfileRun{}, true, "every other profile's"},
		"v1":       {SessionConfig{}, ProfileRun{}, false, "only the workspace layout"},
	} {
		writable, reason := ProfileDataAccess(test.config, test.run)
		if writable != test.want || !strings.Contains(reason, test.reason) {
			t.Errorf("%s = %v %q", name, writable, reason)
		}
	}
}

// An except folder shows only what is inside it and inside its hidden folder; a hidden folder
// inside it, or one it names whole, stays hidden.
func TestHiddenExceptShowsOnlyItsOwnFolder(t *testing.T) {
	policy := NativePathPolicy{Hidden: []string{"/w/.jad/profile-own", "/w/.jad/profile-own/dev/a/secret", "/g/auth"}, HiddenExcept: []string{"/w/.jad/profile-own/dev/a", "/g/auth"}}
	for path, hidden := range map[string]bool{
		"/w/.jad/profile-own/dev/a/doc.md":   false,
		"/w/.jad/profile-own/dev/a/secret/x": true,
		"/w/.jad/profile-own/dev/b/doc.md":   true,
		"/w/.jad/profile-own":                true,
		"/g/auth/token":                      true,
		"/w/src/main.go":                     false,
	} {
		if policy.Hides(path) != hidden {
			t.Errorf("Hides(%s) = %v", path, !hidden)
		}
	}
}
