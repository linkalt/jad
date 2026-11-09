package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// envSandbox is an enabled command sandbox (env runs everywhere) plus extra keys.
func envSandbox(extra map[string]any) map[string]any {
	sandbox := map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}}
	for key, value := range extra {
		sandbox[key] = value
	}
	return sandbox
}

func loadSandboxFixture(t *testing.T, fixture layouttest.Fixture) (SessionConfig, error) {
	t.Helper()
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		return SessionConfig{}, err
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return LoadSessionSettings(location, "", "")
}

func mustLoadFixtureSession(t *testing.T, fixture layouttest.Fixture) SessionConfig {
	t.Helper()
	config, err := loadSandboxFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestSandboxReadWriteKeysAddPathsAndWriteImpliesRead(t *testing.T) {
	got, err := normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],
		"read":{"paths":["{system}","{workspace}"],"add_paths":["{home}/.m2","{workspace}"]},
		"write":{"paths":["{workspace}/build"],"add_paths":["{tmp}"],"plan_paths":["{tmp}"],"add_plan_paths":["{session_dir}/notes"]},
		"hide_paths":["{workspace}/.env"],"add_hide_paths":["{workspace}/secrets"],"isolation":{"mode":"worktree"}}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	want := &SandboxSettings{
		Tools: []string{"exec", "tool_smith"}, Backend: "command", Executable: "env", Command: []string{"env"}, ProxyPort: 3128, CheckTimeoutMs: 10000,
		ReadPaths:         []string{"{system}", "{workspace}", "{home}/.m2", "{workspace}/build", "{tmp}", "{session_dir}/notes"},
		WritablePaths:     []string{"{workspace}/build", "{tmp}"},
		PlanWritablePaths: []string{"{tmp}", "{session_dir}/notes"},
		HidePaths:         []string{"{workspace}/.env", "{workspace}/secrets"},
		Isolation:         SandboxIsolation{Mode: SandboxIsolationWorktree},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	// add_hide_paths adds to the default credential list.
	got, err = normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],"add_hide_paths":["{workspace}/.env"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]string(nil), defaultSandboxHidePaths...), "{workspace}/.env"); !reflect.DeepEqual(got.HidePaths, want) || !got.WholeFileSystemReadable() {
		t.Fatalf("hide %v read %v", got.HidePaths, got.ReadPaths)
	}
	// The older spellings still load.
	got, err = normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],"writable_paths":["/x"],"plan_writable_paths":["/y"],"write":{"add_paths":["/z"]}}`), "s")
	if err != nil || !reflect.DeepEqual(got.WritablePaths, []string{"/x", "/z"}) || !reflect.DeepEqual(got.PlanWritablePaths, []string{"/y"}) {
		t.Fatalf("older spelling: %+v %v", got, err)
	}
}

func TestSandboxNewKeyRefusals(t *testing.T) {
	for name, test := range map[string]struct{ raw, want string }{
		"both spellings":        {`{"writable_paths":["/x"],"write":{"paths":["/y"]}}`, "older spelling of sandbox.write.paths"},
		"both plan spellings":   {`{"plan_writable_paths":["/x"],"write":{"plan_paths":["/y"]}}`, "older spelling of sandbox.write.plan_paths"},
		"read not object":       {`{"read":["/"]}`, "sandbox.read must be an object"},
		"unknown read key":      {`{"read":{"path":["/"]}}`, "did you mean paths"},
		"unknown write key":     {`{"write":{"plan":["/"]}}`, "sandbox.write"},
		"system in write":       {`{"write":{"paths":["{system}"]}}`, "{system} is a read path of its own"},
		"system inside a path":  {`{"read":{"paths":["{system}/x"]}}`, "{system} is a read path of its own"},
		"system in hide":        {`{"hide_paths":["{system}"]}`, "{system} is a read path of its own"},
		"relative read":         {`{"read":{"paths":["src"]}}`, "read.paths[0] must be an absolute path"},
		"isolation mode":        {`{"isolation":{"mode":"overlay"}}`, `isolation.mode must be "none" or "worktree"`},
		"isolation materialize": {`{"isolation":{"materialize":"copy"}}`, `isolation.materialize must be "auto", "checkout" or "overlay"`},
		"isolation unknown key": {`{"isolation":{"shared":["x"]}}`, "sandbox.isolation"},
		"share with checkout":   {`{"isolation":{"materialize":"checkout","share_paths":["node_modules"]}}`, `share_paths needs the overlay materialiser`},
		"share not array":       {`{"isolation":{"share_paths":"node_modules"}}`, "isolation.share_paths must be an array"},
		"share absolute":        {`{"isolation":{"share_paths":["/opt/x"]}}`, `share_paths entry "/opt/x"`},
		"share outside":         {`{"isolation":{"share_paths":["../x"]}}`, `share_paths entry "../x"`},
		"share placeholder":     {`{"isolation":{"share_paths":["{workspace}/x"]}}`, `share_paths entry "{workspace}/x"`},
		"share dot":             {`{"isolation":{"share_paths":["."]}}`, `share_paths entry "."`},
		"isolation base":        {`{"isolation":{"base":"tip"}}`, `isolation.base must be "head" or "working"`},
		"blank template":        {`{"template":" "}`, "template must be the name"},
		"string":                {`"workspace-writer"`, "needs the catalog layout"},
	} {
		_, err := normalizeSandbox(json.RawMessage(test.raw), "s")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

func TestSandboxTemplateOnEachLayer(t *testing.T) {
	writer := envSandbox(map[string]any{"schema_version": 1, "description": "writer", "network": false,
		"write": map[string]any{"paths": []string{"{workspace}", "{tmp}"}}, "hide_paths": []string{"{workspace}/.env"}})
	for name, test := range map[string]struct {
		spec  layouttest.Spec
		check func(*testing.T, *SandboxSettings)
	}{
		"name on the session": {
			layouttest.Spec{Settings: map[string]any{"sandbox": "writer"}},
			func(t *testing.T, got *SandboxSettings) {
				if got.Template != "writer" || !reflect.DeepEqual(got.WritablePaths, []string{"{workspace}", "{tmp}"}) {
					t.Fatalf("%+v", got)
				}
			},
		},
		"name on the defaults, overridden by the profile's object": {
			layouttest.Spec{Defaults: map[string]any{"sandbox": "writer"}, ProfileExtra: map[string]any{"settings": map[string]any{"sandbox": map[string]any{"network": true, "hide_paths": []string{}}}}},
			func(t *testing.T, got *SandboxSettings) {
				if got.Template != "writer" || !got.Network || len(got.HidePaths) != 0 || !reflect.DeepEqual(got.WritablePaths, []string{"{workspace}", "{tmp}"}) {
					t.Fatalf("%+v", got)
				}
			},
		},
		"template with overrides on the profile": {
			layouttest.Spec{ProfileExtra: map[string]any{"settings": map[string]any{"sandbox": map[string]any{"template": "writer", "add_hide_paths": []string{"{workspace}/secrets"}, "read": map[string]any{"add_paths": []string{"{home}/.m2"}}}}}},
			func(t *testing.T, got *SandboxSettings) {
				if !reflect.DeepEqual(got.HidePaths, []string{"{workspace}/.env", "{workspace}/secrets"}) || !reflect.DeepEqual(got.ReadPaths, []string{"/", "{home}/.m2"}) {
					t.Fatalf("%+v", got)
				}
			},
		},
		"add_paths from every layer": {
			layouttest.Spec{
				Defaults:     map[string]any{"sandbox": map[string]any{"template": "writer", "write": map[string]any{"add_paths": []string{"/d"}}}},
				Settings:     map[string]any{"sandbox": map[string]any{"write": map[string]any{"add_paths": []string{"/s"}}}},
				ProfileExtra: map[string]any{"settings": map[string]any{"sandbox": map[string]any{"write": map[string]any{"add_paths": []string{"/p"}}}}},
			},
			func(t *testing.T, got *SandboxSettings) {
				if !reflect.DeepEqual(got.WritablePaths, []string{"{workspace}", "{tmp}", "/d", "/s", "/p"}) {
					t.Fatalf("%v", got.WritablePaths)
				}
			},
		},
		"a session's paths drop the defaults' add_paths but keep the profile's": {
			layouttest.Spec{
				Defaults:     map[string]any{"sandbox": map[string]any{"template": "writer", "write": map[string]any{"add_paths": []string{"/d"}}}},
				Settings:     map[string]any{"sandbox": map[string]any{"write": map[string]any{"paths": []string{"{workspace}"}}}},
				ProfileExtra: map[string]any{"settings": map[string]any{"sandbox": map[string]any{"write": map[string]any{"add_paths": []string{"/p"}}}}},
			},
			func(t *testing.T, got *SandboxSettings) {
				if !reflect.DeepEqual(got.WritablePaths, []string{"{workspace}", "/p"}) {
					t.Fatalf("%v", got.WritablePaths)
				}
			},
		},
		"a write add_path of a restricted read is readable": {
			layouttest.Spec{Settings: map[string]any{"sandbox": map[string]any{"template": "writer", "read": map[string]any{"paths": []string{"{system}"}}, "write": map[string]any{"add_paths": []string{"{home}/.m2/repository"}}}}},
			func(t *testing.T, got *SandboxSettings) {
				if !reflect.DeepEqual(got.ReadPaths, []string{"{system}", "{workspace}", "{tmp}", "{home}/.m2/repository"}) {
					t.Fatalf("%v", got.ReadPaths)
				}
			},
		},
		"null on the profile drops the template": {
			layouttest.Spec{Settings: map[string]any{"sandbox": "writer"}, ProfileExtra: map[string]any{"settings": map[string]any{"sandbox": nil}}},
			func(t *testing.T, got *SandboxSettings) {
				if got != nil {
					t.Fatalf("%+v", got)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			test.spec.SandboxTemplates = map[string]map[string]any{"writer": writer}
			config := mustLoadFixtureSession(t, layouttest.Write(t, test.spec))
			test.check(t, config.Sandbox)
		})
	}
}

func TestSandboxTemplateRefusals(t *testing.T) {
	for name, test := range map[string]struct {
		templates map[string]map[string]any
		value     any
		want      string
	}{
		"missing, with a suggestion": {map[string]map[string]any{"writer": envSandbox(nil)}, "writr", "did you mean writer?"},
		"missing, folder empty":      {nil, "writer", "holds no template"},
		"without enabled":            {map[string]map[string]any{"writer": {"tools": []string{"exec"}}}, "writer", "must set enabled"},
		"naming another":             {map[string]map[string]any{"writer": envSandbox(map[string]any{"template": "other"})}, "writer", "a template cannot name another"},
		"unknown key in the file":    {map[string]map[string]any{"writer": envSandbox(map[string]any{"netwrk": true})}, "writer", "writer.json"},
		"in a sub-folder":            {nil, "team/writer", "a sub-folder is not allowed"},
		"bad name":                   {nil, "-writer", "must start with a letter or digit"},
		"template key not a string":  {nil, map[string]any{"template": 3}, "template must be the name"},
		"wrong schema_version":       {map[string]map[string]any{"writer": envSandbox(map[string]any{"schema_version": 2})}, "writer", "unsupported schema_version 2"},
		// The two halves come from different layers; the merged object is what is checked.
		"share_paths over a checkout template": {map[string]map[string]any{"writer": envSandbox(map[string]any{"isolation": map[string]any{"materialize": "checkout"}})}, map[string]any{"template": "writer", "isolation": map[string]any{"share_paths": []string{"node_modules"}}}, "share_paths needs the overlay materialiser"},
		"checkout over a share_paths template": {map[string]map[string]any{"writer": envSandbox(map[string]any{"isolation": map[string]any{"share_paths": []string{"node_modules"}}})}, map[string]any{"template": "writer", "isolation": map[string]any{"materialize": "checkout"}}, "share_paths needs the overlay materialiser"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: test.templates, Settings: map[string]any{"sandbox": test.value}})
			_, err := loadSandboxFixture(t, fixture)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func TestSandboxCatalogForms(t *testing.T) {
	template := envSandbox(map[string]any{"schema_version": 1})
	put := func(t *testing.T, dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(template)
		if err := os.WriteFile(filepath.Join(dir, "box.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	absolute := t.TempDir()
	put(t, absolute)
	cwd := t.TempDir()
	put(t, filepath.Join(cwd, "project-boxes"))
	for name, catalog := range map[string]string{"absolute": absolute, "{cwd}": "{cwd}/project-boxes", "relative": "relative-boxes"} {
		t.Run(name, func(t *testing.T) {
			fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd, SandboxCatalog: catalog, Settings: map[string]any{"sandbox": "box"}})
			if catalog == "relative-boxes" {
				put(t, filepath.Join(fixture.Root, "relative-boxes"))
			}
			config := mustLoadFixtureSession(t, fixture)
			if config.Sandbox == nil || config.Sandbox.Template != "box" {
				t.Fatalf("sandbox = %+v", config.Sandbox)
			}
			want := map[string]string{"absolute": absolute, "{cwd}": filepath.Join(cwd, "project-boxes"), "relative": filepath.Join(fixture.Root, "relative-boxes")}[name]
			if !containsString(config.Sandbox.ReadOnlyPaths, want) || !containsString(config.Sandbox.ReadOnlyPaths, fixture.SessionFile) || !containsString(config.Sandbox.ReadOnlyPaths, fixture.StateDir) {
				t.Fatalf("read-only paths %v lack %s, the session file or the state folder", config.Sandbox.ReadOnlyPaths, want)
			}
		})
	}
	for _, bad := range []string{"{home}/boxes", "x/{cwd}/boxes", "{cwd}/{tmp}"} {
		fixture := layouttest.Write(t, layouttest.Spec{SandboxCatalog: bad})
		if _, err := loadSandboxFixture(t, fixture); err == nil || !strings.Contains(err.Error(), "catalogs.sandbox") {
			t.Errorf("%s: error %v", bad, err)
		}
	}
}

// An edited template reloads the sessions that use it, like an edited toolset.
func TestEditedSandboxTemplateChangesTheStamp(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: map[string]any{"sandbox": "box"}})
	backdateTree(t, fixture.Root, fixture.CWD)
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, stamp, err := LoadSessionSettingsStamped(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !stamp.Current() {
		t.Fatal("a fresh stamp is not current")
	}
	path := filepath.Join(fixture.Root, ".jad", "sandbox", "box.json")
	data, _ := json.Marshal(envSandbox(map[string]any{"network": true, "description": "edited, a longer file"}))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if stamp.Current() {
		t.Fatal("the stamp did not notice the edited template")
	}
}

func TestAgentSandboxIsNarrowedByTheSession(t *testing.T) {
	session := envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{workspace}", "{tmp}"}}, "hide_paths": []string{"{home}/.ssh"}})
	reader := envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{tmp}"}}, "tools": []string{"exec", "lsp"}, "isolation": map[string]any{"mode": "none"}})
	wide := envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{home}"}}})
	agents := func(sandbox any, extra map[string]any) map[string]any {
		agent := map[string]any{"description": "a", "sandbox": sandbox}
		for key, value := range extra {
			agent[key] = value
		}
		return map[string]any{"subagent": map[string]any{"agents": map[string]any{"explore": agent}}, "async": map[string]any{}, "exec": map[string]any{}}
	}
	templates := map[string]map[string]any{"reader": reader, "wide": wide}
	fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: templates, Settings: map[string]any{"sandbox": session}, Native: agents("reader", nil)})
	config := mustLoadFixtureSession(t, fixture)
	agent, _ := config.NativeSubagent.Agent("explore")
	if agent.Sandbox == nil || agent.Sandbox.Template != "reader" || !reflect.DeepEqual(agent.Sandbox.WritablePaths, []string{"{tmp}"}) ||
		!reflect.DeepEqual(agent.Sandbox.Tools, []string{"exec", "tool_smith", "lsp"}) || !containsString(agent.Sandbox.HidePaths, "{home}/.ssh") ||
		len(agent.Sandbox.ReadOnlyPaths) == 0 || agent.Isolation != "" {
		t.Fatalf("agent sandbox = %+v", agent.Sandbox)
	}
	for name, test := range map[string]struct {
		sandbox any
		extra   map[string]any
		want    string
	}{
		"wider template":       {"wide", nil, `agent explore: sandbox (template wide) widens: write.paths entry "{home}"`},
		"wider inline":         {envSandbox(map[string]any{"network": true, "write": map[string]any{"paths": []string{"{tmp}"}}}), nil, "widens: network is on"},
		"both isolation forms": {map[string]any{"template": "reader", "isolation": map[string]any{"mode": "worktree"}}, map[string]any{"isolation": "worktree"}, "isolation and sandbox.isolation.mode are both set"},
		"missing template":     {"nope", nil, "sandbox template \"nope\" is not in"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: templates, Settings: map[string]any{"sandbox": session}, Native: agents(test.sandbox, test.extra)})
			_, err := loadSandboxFixture(t, fixture)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
	// sandbox.isolation.mode worktree is the agent key isolation; a disabled sandbox still sets it.
	isolated := map[string]any{"enabled": false, "isolation": map[string]any{"mode": "worktree"}}
	fixture = layouttest.Write(t, layouttest.Spec{Settings: map[string]any{"sandbox": session}, Native: agents(isolated, nil)})
	config = mustLoadFixtureSession(t, fixture)
	if agent, _ := config.NativeSubagent.Agent("explore"); agent.Isolation != SubagentIsolationWorktree || agent.Sandbox != nil || agent.IsolationBase != "" {
		t.Fatalf("agent = %+v", agent)
	}
	// sandbox.isolation.base working is the agent's worktree base, with either form of isolation.
	for _, extra := range []map[string]any{nil, {"isolation": "worktree"}} {
		working := map[string]any{"enabled": false, "isolation": map[string]any{"base": "working"}}
		if extra == nil {
			working["isolation"] = map[string]any{"mode": "worktree", "base": "working"}
		}
		fixture = layouttest.Write(t, layouttest.Spec{Settings: map[string]any{"sandbox": session}, Native: agents(working, extra)})
		config = mustLoadFixtureSession(t, fixture)
		if agent, _ := config.NativeSubagent.Agent("explore"); agent.Isolation != SubagentIsolationWorktree || agent.IsolationBase != SandboxIsolationBaseWorking {
			t.Fatalf("agent = %+v", agent)
		}
	}
}

func TestNarrowSandbox(t *testing.T) {
	base := func() *SandboxSettings {
		return &SandboxSettings{Backend: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}, HidePaths: []string{"{home}/.ssh"}, NetworkAllow: []string{"example.com"}}
	}
	parent := base()
	if got, _ := NarrowSandbox(nil, parent, nil, nil); got != parent {
		t.Fatal("no parent: the child's own")
	}
	if got, _ := NarrowSandbox(parent, nil, nil, nil); got != parent {
		t.Fatal("no child: the parent's")
	}
	accept := map[string]func(*SandboxSettings){
		"same":                  func(*SandboxSettings) {},
		"write inside":          func(c *SandboxSettings) { c.WritablePaths = []string{"{workspace}/src"} },
		"read restricted":       func(c *SandboxSettings) { c.ReadPaths = []string{"{system}", "{workspace}"} },
		"subdomain":             func(c *SandboxSettings) { c.NetworkAllow = []string{"api.example.com"} },
		"no network":            func(c *SandboxSettings) { c.NetworkAllow = nil },
		"isolated":              func(c *SandboxSettings) { c.Isolation.Mode = SandboxIsolationWorktree },
		"more tools and hidden": func(c *SandboxSettings) { c.Tools = []string{"lsp"}; c.HidePaths = []string{"{workspace}/.env"} },
	}
	for name, change := range accept {
		child := base()
		change(child)
		got, err := NarrowSandbox(parent, child, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !containsString(got.HidePaths, "{home}/.ssh") || !containsString(got.Tools, "exec") {
			t.Fatalf("%s: hidden or tools not joined: %+v", name, got)
		}
		if name == "no network" && got.NetworkAllow != nil {
			t.Fatalf("no network: %v", got.NetworkAllow)
		}
	}
	refuse := map[string]struct {
		change func(*SandboxSettings)
		want   string
	}{
		"backend":       {func(c *SandboxSettings) { c.Backend = "seatbelt" }, "backend"},
		"write outside": {func(c *SandboxSettings) { c.WritablePaths = []string{"{home}"} }, `write.paths entry "{home}"`},
		"plan wider":    {func(c *SandboxSettings) { c.PlanWritablePaths = []string{"{workspace}"} }, "write.plan_paths"},
		"network":       {func(c *SandboxSettings) { c.Network = true }, "network is on"},
		"other host":    {func(c *SandboxSettings) { c.NetworkAllow = []string{"evil.test"} }, `host "evil.test"`},
		"command":       {func(c *SandboxSettings) { c.Backend, c.Command = "command", []string{"x"} }, "backend"},
	}
	for name, test := range refuse {
		child := base()
		test.change(child)
		_, err := NarrowSandbox(parent, child, nil, nil)
		if err == nil || !strings.HasPrefix(err.Error(), SandboxWidensPrefix) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	// A restricted parent read: the child may read only what the parent reads or writes.
	restricted := base()
	restricted.ReadPaths = []string{"{system}", "{workspace}", "{tmp}"}
	child := base()
	child.ReadPaths = []string{"{system}", "{workspace}/src"}
	if _, err := NarrowSandbox(restricted, child, nil, nil); err != nil {
		t.Fatal(err)
	}
	child.ReadPaths = []string{"/"}
	if _, err := NarrowSandbox(restricted, child, nil, nil); err == nil || !strings.Contains(err.Error(), "read.paths") {
		t.Fatalf("whole read under a restricted parent: %v", err)
	}
	// Across sessions paths compare expanded: the caller's {workspace} is not the target's.
	caller := SandboxPlace{Workspace: "/work/a", CWD: "/work/a"}.Expander()
	target := SandboxPlace{Workspace: "/work/b", CWD: "/work/b"}.Expander()
	child = base()
	if _, err := NarrowSandbox(parent, child, caller, target); err == nil || !strings.Contains(err.Error(), "{workspace}") {
		t.Fatalf("another workspace was accepted: %v", err)
	}
	inside := SandboxPlace{Workspace: "/work/a/sub", CWD: "/work/a/sub"}.Expander()
	if _, err := NarrowSandbox(parent, child, caller, inside); err != nil {
		t.Fatalf("a target inside the caller's workspace: %v", err)
	}
	inherited := InheritSandbox(parent, caller)
	if !reflect.DeepEqual(inherited.WritablePaths, []string{"/work/a", "{tmp}"}) {
		t.Fatalf("inherited = %v", inherited.WritablePaths)
	}
}

// The catalog file API serves sandbox templates by flat name: a save is checked by the template
// loader, and a delete is refused while a profile, a session or the defaults name the template.
func TestSandboxTemplateCatalogFiles(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: map[string]any{"sandbox": "box"}})
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	layout := *config.Roots[0].Layout
	listed, err := ListCatalogFiles(layout, CatalogSandboxes)
	if err != nil || len(listed) != 1 || listed[0].ID != "box" {
		t.Fatalf("listed %+v %v", listed, err)
	}
	if _, _, err := WriteCatalogFile(layout, config.Roots, CatalogSandboxes, "reader", []byte(`{"tools":["exec"]}`), WriteCondition{IfNoneMatch: "*"}, false, nil); err == nil || !strings.Contains(err.Error(), "must set enabled") {
		t.Fatalf("a template without enabled was saved: %v", err)
	}
	if _, _, err := WriteCatalogFile(layout, config.Roots, CatalogSandboxes, "team/reader", []byte(`{"enabled":false}`), WriteCondition{IfNoneMatch: "*"}, false, nil); err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("a sub-folder id was accepted: %v", err)
	}
	info, created, err := WriteCatalogFile(layout, config.Roots, CatalogSandboxes, "reader", []byte(`{"enabled":false}`), WriteCondition{IfNoneMatch: "*"}, false, nil)
	if err != nil || !created {
		t.Fatalf("save: %v", err)
	}
	if err := DeleteCatalogFile(layout, config.Roots, CatalogSandboxes, "reader", info.ETag); err != nil {
		t.Fatalf("delete an unused template: %v", err)
	}
	box, err := ReadCatalogFile(layout, CatalogSandboxes, "box")
	if err != nil {
		t.Fatal(err)
	}
	err = DeleteCatalogFile(layout, config.Roots, CatalogSandboxes, "box", box.ETag)
	var refused *CatalogError
	if !errors.As(err, &refused) || refused.Kind != CatalogInUse || !strings.Contains(err.Error(), "session") {
		t.Fatalf("deleting a used template: %v", err)
	}
}

// The sample templates load and follow sandbox.schema.json.
func TestSampleSandboxTemplates(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil || runtime.GOOS != "linux" {
		t.Skip("the sample templates use bwrap, which an enabled template must be able to run")
	}
	schema := compileSchema(t, "sandbox.schema.json")
	dir := filepath.Join("..", "..", "..", "sample", "jad", "sandbox")
	names := ListSandboxTemplates(dir)
	if len(names) == 0 {
		t.Fatalf("no template in %s", dir)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseSandboxTemplate(name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !schemaAccepts(t, schema, string(data)) {
			t.Errorf("%s does not follow sandbox.schema.json", name)
		}
	}
}
