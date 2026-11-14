package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func writeWorkspaceFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// In the workspace layout a real bwrap sandbox hides G/auth and the other workspace's .jad even
// when the profile's own sandbox sets hide_paths to nothing, and keeps the workspace's
// configuration read-only (docs/design_workspace_layout.md §4.6, design Q4).
func TestWorkspaceSandboxHidesFixedPathsAndKeepsConfigReadOnly(t *testing.T) {
	requireBwrap(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	writeWorkspaceFile(t, filepath.Join(global, "auth", "key"), "hunter2")
	writeWorkspaceFile(t, filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceFile(t, filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	for _, dir := range []string{a, b} {
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "workspace.json"), `{}`)
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
		writeWorkspaceFile(t, filepath.Join(dir, ".jad", "profiles", "dev", "coder.json"),
			`{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap","hide_paths":[]}}}`)
	}
	writeWorkspaceFile(t, filepath.Join(b, ".jad", "secret"), "abc123")
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "a:s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, a)
	settings.Sandbox = config.Sandbox
	useOwnTempDir(t)
	command := `echo "auth=[$(cat ` + global + `/auth/key 2>/dev/null)] other=[$(cat ` + b + `/.jad/secret 2>/dev/null)]"; ` +
		`echo changed > .jad/workspace.json 2>&1; echo changed > .jad/sessions/s1.json 2>&1; touch project.txt && echo wrote-project`
	output := executeSandboxBashTest(t, a, "workspace-call", command, "", settings)
	if !strings.Contains(output, "auth=[] other=[]") || strings.Contains(output, "hunter2") || strings.Contains(output, "abc123") {
		t.Fatalf("a fixed hidden path leaked:\n%s", output)
	}
	if !strings.Contains(output, "Read-only file system") || !strings.Contains(output, "wrote-project") {
		t.Fatalf("config must be read-only and the project writable:\n%s", output)
	}
	for _, path := range []string{filepath.Join(a, ".jad", "workspace.json"), filepath.Join(a, ".jad", "sessions", "s1.json")} {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), "changed") {
			t.Fatalf("%s was changed from the sandbox", path)
		}
	}
}

// Decider fix A for phase 1b: in the workspace layout G and W/.jad are sealed, so a sandboxed
// call cannot change the configuration a later load reads, even when the profile makes $HOME
// and W/.jad/profiles writable: no edited profile, no new toolset, no change under G/models and
// no new G/global.json. The exec helper folder W/.jad/exec stays writable, and so does the
// project (docs/design_workspace_layout.md §4.6).
func TestWorkspaceSandboxSealsTheGlobalAndWorkspaceConfiguration(t *testing.T) {
	requireBwrap(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a := filepath.Join(base, "a")
	model := filepath.Join(global, "models", "openai", "default.json")
	writeWorkspaceFile(t, model, `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "workspace.json"), `{}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	profile := filepath.Join(a, ".jad", "profiles", "dev", "coder.json")
	writeWorkspaceFile(t, profile, `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap",`+
		`"writable_paths":["{workspace}","{tmp}","`+home+`","{workspace}/.jad/profiles"]}}}`)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, a)
	settings.Sandbox = config.Sandbox
	useOwnTempDir(t)
	attempt := func(name, command string) string {
		return `if ( ` + command + ` ) 2>/dev/null; then echo "` + name + `=written"; else echo "` + name + `=refused"; fi; `
	}
	command := attempt("profile", `echo '{"hooks":{}}' > .jad/profiles/dev/coder.json`) +
		attempt("newprofile", `echo '{}' > .jad/profiles/dev/evil.json`) +
		attempt("toolset", `mkdir -p .jad/tools/dev && echo '{}' > .jad/tools/dev/evil.json`) +
		attempt("setting", `echo '{}' > .jad/setting.json`) +
		attempt("gmodel", `echo '{}' > `+model) +
		attempt("gtool", `mkdir -p `+global+`/tools/dev && echo '{}' > `+global+`/tools/dev/evil.json`) +
		attempt("gglobal", `echo '{}' > `+global+`/global.json`) +
		attempt("home", `echo hi > `+home+`/note.txt`) +
		attempt("helper", `mkdir -p .jad/exec && echo 'echo hi' > .jad/exec/hello.sh`) +
		attempt("project", `echo hi > project.txt`)
	output := executeSandboxBashTest(t, a, "seal-call", command, "", settings)
	for _, want := range []string{"profile=refused", "newprofile=refused", "toolset=refused", "setting=refused", "gmodel=refused", "gtool=refused", "gglobal=refused",
		"home=written", "helper=written", "project=written"} {
		if !strings.Contains(output, want) {
			t.Errorf("want %s in:\n%s", want, output)
		}
	}
	for _, path := range []string{filepath.Join(a, ".jad", "profiles", "dev", "evil.json"), filepath.Join(a, ".jad", "tools"), filepath.Join(a, ".jad", "setting.json"), filepath.Join(global, "tools"), filepath.Join(global, "global.json")} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s was made from the sandbox", path)
		}
	}
	if data, _ := os.ReadFile(profile); strings.Contains(string(data), "hooks") {
		t.Error("the profile was changed from the sandbox")
	}
}

// 4c: the tool definition files workspace.json lists are read-only in every sandbox of the
// workspace, also outside W/.jad and even when a writable path covers them; a file of the
// project next to them stays writable. A global.json one outside G is not on this list.
func TestWorkspaceToolDefinitionFilesAreReadOnlyInTheSandbox(t *testing.T) {
	testWorkspaceToolDefinitionFilesAreReadOnlyInTheSandbox(t, false)
}

func TestWorkspaceToolDefinitionGlobsAreReadOnlyInTheSandbox(t *testing.T) {
	testWorkspaceToolDefinitionFilesAreReadOnlyInTheSandbox(t, true)
}

func testWorkspaceToolDefinitionFilesAreReadOnlyInTheSandbox(t *testing.T, globs bool) {
	t.Helper()
	requireBwrap(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	a := filepath.Join(base, "a")
	outside := filepath.Join(base, "defs")
	inProject := filepath.Join(a, "defs", "native.json")
	inJad := filepath.Join(a, ".jad", "tools_defintion", "native.json")
	elsewhere := filepath.Join(outside, "native.json")
	for _, path := range []string{inProject, inJad, elsewhere} {
		writeWorkspaceFile(t, path, `{"tools":[]}`)
	}
	writeWorkspaceFile(t, filepath.Join(a, "defs", "other.txt"), "plain")
	writeWorkspaceFile(t, filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "workspace.json"), `{"libraries":{"tool_definition_paths":["../defs/native.json","tools_defintion/native.json","`+elsewhere+`"]}}`)
	if globs {
		// The project match overlaps globally; the external match is workspace-only.
		writeWorkspaceFile(t, filepath.Join(home, ".jad", "global.json"), `{"libraries":{"tool_definition_paths":["`+filepath.Join(a, "defs", "*.json")+`"]}}`)
		writeWorkspaceFile(t, filepath.Join(a, ".jad", "workspace.json"), `{"libraries":{"tool_definition_paths":["../defs/*.json","tools_defintion/*.json","`+filepath.Join(outside, "*.json")+`"]}}`)
	}
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	writeWorkspaceFile(t, filepath.Join(a, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"settings":{"sandbox":{"enabled":true,"backend":"bwrap","executable":"bwrap",`+
		`"writable_paths":["{workspace}","{tmp}","`+outside+`"]}}}`)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(config.ToolDefinitionPaths); got != 3 {
		t.Fatalf("tool definition paths: %q", config.ToolDefinitionPaths)
	}
	settings := bashTestSettings(t, a)
	settings.Sandbox = config.Sandbox
	useOwnTempDir(t)
	attempt := func(name, command string) string {
		return `if ( ` + command + ` ) 2>/dev/null; then echo "` + name + `=written"; else echo "` + name + `=refused"; fi; `
	}
	command := ""
	for name, path := range map[string]string{"project": inProject, "jad": inJad, "outside": elsewhere} {
		command += attempt(name+"-change", `echo '{"tools":[{"id":"evil"}]}' > `+path) +
			attempt(name+"-delete", `rm -f `+path) +
			attempt(name+"-replace", `echo x > `+path+`.new && mv `+path+`.new `+path)
	}
	command += attempt("project-sibling", `echo ok > `+filepath.Join(a, "defs", "other.txt")) + attempt("outside-sibling", `echo ok > `+filepath.Join(outside, "new.txt"))
	output := executeSandboxBashTest(t, a, "tooldefs-call", command, "", settings)
	for _, name := range []string{"project", "jad", "outside"} {
		for _, kind := range []string{"change", "delete", "replace"} {
			if !strings.Contains(output, name+"-"+kind+"=refused") {
				t.Errorf("%s-%s was not refused:\n%s", name, kind, output)
			}
		}
	}
	for _, want := range []string{"project-sibling=written", "outside-sibling=written"} {
		if !strings.Contains(output, want) {
			t.Errorf("want %s in:\n%s", want, output)
		}
	}
	for _, path := range []string{inProject, inJad, elsewhere} {
		if data, _ := os.ReadFile(path); string(data) != `{"tools":[]}` {
			t.Errorf("%s was changed from the sandbox: %q", path, data)
		}
	}
}

// A sealed folder that does not exist yet becomes an empty read-only folder where a writable
// path covers its parent, so a call cannot make G (and a G/global.json in it); one whose parent
// is missing too, or that no writable path covers, needs no rule.
func TestSealedRulesKeepAMissingFolderFromBeingMade(t *testing.T) {
	home := t.TempDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	global := filepath.Join(home, ".jad")
	rules := sandboxSealedRules([]string{home}, []string{global, filepath.Join(home, "no", "such"), "/elsewhere/.jad"}, []string{filepath.Join(global, "exec")})
	if len(rules) != 1 || rules[0].path != global || !rules[0].missing || !rules[0].dir || len(rules[0].inner) != 0 {
		t.Fatalf("rules = %+v", rules)
	}
	if _, err := os.Lstat(filepath.Join(global, "exec")); err == nil {
		t.Fatal("a helper folder was made inside a missing sealed folder")
	}
}
