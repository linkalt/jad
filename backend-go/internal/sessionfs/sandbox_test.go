package sessionfs

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestSandboxIsOffByDefault(t *testing.T) {
	for _, raw := range []string{"", "null", `{}`, `{"enabled":false,"network":true}`} {
		got, err := normalizeSandbox(json.RawMessage(raw), "s")
		if got != nil || err != nil {
			t.Fatalf("%q: %+v %v", raw, got, err)
		}
	}
}

func TestSandboxDefaultsAndValues(t *testing.T) {
	// sh stands in for the sandbox program: the loader only checks that it can be run.
	got, err := normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["sh","-c","exec \"$@\"","sandbox"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	want := &SandboxSettings{
		Tools: []string{"exec", "tool_smith"}, Backend: "command", Executable: "sh",
		ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{git_common_dir}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}, Command: []string{"sh", "-c", "exec \"$@\"", "sandbox"},
		HidePaths:      defaultSandboxHidePaths,
		CheckTimeoutMs: 10000, ProxyPort: 3128,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %+v", got)
	}
	got, err = normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],"tools":["tool_smith","exec","hooks"],"network":true,"check_timeout_ms":250,"writable_paths":["{cwd}","/var/cache/x"],"plan_writable_paths":["{tmp}","/var/cache/x"],"hide_paths":["{home}/.ssh"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tools, []string{"tool_smith", "exec", "hooks"}) || !got.Network || got.CheckTimeoutMs != 250 || !got.Covers("hooks") || !reflect.DeepEqual(got.WritablePaths, []string{"{cwd}", "/var/cache/x"}) ||
		!reflect.DeepEqual(got.HidePaths, []string{"{home}/.ssh"}) || !got.Covers("exec") || got.Covers("mcp") ||
		!reflect.DeepEqual(got.PlanWritablePaths, []string{"{tmp}", "/var/cache/x"}) {
		t.Fatalf("values = %+v", got)
	}
	var none *SandboxSettings
	if none.Covers("exec") {
		t.Fatal("no sandbox covers nothing")
	}
}

func TestSandboxRefusals(t *testing.T) {
	for name, test := range map[string]struct{ raw, want string }{
		"not an object":       {`[]`, "sandbox must be an object"},
		"unknown key":         {`{"enabled":true,"netwrk":true}`, "did you mean network"},
		"enabled not bool":    {`{"enabled":"yes"}`, "sandbox.enabled must be a boolean"},
		"empty tools":         {`{"tools":[]}`, "tools must be a non-empty array of tool names"},
		"tools not a list":    {`{"tools":"exec"}`, "tools must be a non-empty array of tool names"},
		"blank tool name":     {`{"tools":["exec"," "]}`, "tools[1] must be a non-empty tool name"},
		"backend":             {`{"backend":"docker"}`, `backend must be "bwrap", "seatbelt" or "command"`},
		"relative path":       {`{"writable_paths":["build"]}`, "writable_paths[0] must be an absolute path"},
		"relative plan path":  {`{"plan_writable_paths":["/x","build"]}`, "plan_writable_paths[1] must be an absolute path"},
		"plan paths not list": {`{"plan_writable_paths":"{tmp}"}`, "plan_writable_paths must be an array"},
		"unknown placeholder": {`{"hide_paths":["{project}/x"]}`, "unknown placeholder {project}"},
		"command missing":     {`{"backend":"command"}`, "command is required"},
		"command for bwrap":   {`{"command":["env"]}`, `command is for backend "command"`},
		"executable for cmd":  {`{"backend":"command","command":["env"],"executable":"env"}`, "the program is command[0]"},
		"empty command":       {`{"backend":"command","command":[]}`, "command must be a non-empty array"},
		"missing program":     {`{"enabled":true,"backend":"command","command":["jad-no-such-sandbox-program"]}`, "cannot be run"},
		"missing bwrap":       {`{"enabled":true,"executable":"jad-no-such-bwrap"}`, ""},
		"checked when off":    {`{"enabled":false,"backend":"docker"}`, "backend must be"},
		"check timeout low":   {`{"check_timeout_ms":50}`, "check_timeout_ms must be a safe integer between 100 and 600000"},
		"check timeout text":  {`{"check_timeout_ms":"1s"}`, "check_timeout_ms must be a safe integer"},
	} {
		_, err := normalizeSandbox(json.RawMessage(test.raw), "s")
		if test.want == "" {
			// bwrap is Linux only; elsewhere the platform is named instead of the program.
			test.want = "cannot be run"
			if runtime.GOOS != "linux" {
				test.want = "runs on Linux only"
			}
		}
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

func TestApplySandboxReachesOnlyTheCoveredTools(t *testing.T) {
	sandbox := &SandboxSettings{Tools: []string{"exec"}}
	execSettings, smith, hooks, lsp := &NativeExecSettings{}, &NativeToolSmithSettings{}, &HooksSettings{}, &NativeLSPSettings{}
	applySandbox(sandbox, sandbox, execSettings, smith, hooks, lsp)
	if execSettings.Child.Sandbox != sandbox || smith.Child.Sandbox != nil || hooks.Sandbox != nil || lsp.Sandbox != nil {
		t.Fatalf("exec %v smith %v hooks %v lsp %v", execSettings.Child.Sandbox, smith.Child.Sandbox, hooks.Sandbox, lsp.Sandbox)
	}
	withHooks := &SandboxSettings{Tools: []string{"hooks", "lsp"}}
	applySandbox(withHooks, withHooks, execSettings, smith, hooks, lsp)
	if hooks.Sandbox != withHooks || lsp.Sandbox != withHooks || execSettings.Child.Sandbox != sandbox {
		t.Fatalf("hooks %v lsp %v exec %v", hooks.Sandbox, lsp.Sandbox, execSettings.Child.Sandbox)
	}
	applySandbox(sandbox, sandbox, nil, nil, nil, nil)
	applySandbox(nil, nil, execSettings, smith, hooks, lsp)
	if execSettings.Child.PlanSandbox != nil {
		t.Fatal("exec, covered by a sandbox, got a fallback")
	}
	uncovered := &NativeExecSettings{}
	applySandbox(withHooks, withHooks, uncovered, nil, nil, nil)
	if (uncovered.Child.PlanSandbox != nil) != (runtime.GOOS == "linux" || runtime.GOOS == "darwin") || uncovered.Child.Sandbox != nil {
		t.Fatalf("exec left out of the sandbox: sandbox %v fallback %v", uncovered.Child.Sandbox, uncovered.Child.PlanSandbox)
	}
}

// sandbox.tools is a filter, not a check: a sandbox is shared by profiles and sub-agents with
// different tool sets, so a listed tool the run does not have, or one that starts no child,
// loads and has no effect; a name listed twice counts once.
func TestSandboxToolsAcceptAnyToolName(t *testing.T) {
	got, err := normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],"tools":["read","search","write","exec","lsp","bash","exec"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tools, []string{"read", "search", "write", "exec", "lsp", "bash"}) || !got.Covers("exec") || got.Covers("tool_smith") {
		t.Fatalf("tools = %v", got.Tools)
	}
}

// A profile that shows only task keeps loading with a sandbox listing tools it does not show
// or the session does not register; the registered exec still gets the sandbox.
func TestSandboxToolsAreNotTiedToTheProfileTools(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{
		Native:       map[string]any{"exec": map[string]any{}, "task": map[string]any{}, "async": map[string]any{}},
		ProfileExtra: map[string]any{"tools": map[string]any{"allow": []string{"task"}}},
		Settings: map[string]any{"sandbox": map[string]any{"enabled": true, "backend": "command", "command": []string{"env"},
			"tools": []string{"read", "search", "write", "exec", "tool_smith", "hooks", "mcp", "lsp"}}},
	})
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NativeExec == nil || loaded.NativeExec.Child.Sandbox != loaded.Sandbox || loaded.NativeToolSmith != nil {
		t.Fatalf("exec sandbox %v, tool_smith %v", loaded.NativeExec, loaded.NativeToolSmith)
	}
}

// The sandbox key is a setting of the layout: a session's settings carry it to the tools.
func TestSandboxLoadsFromTheSessionSettings(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{
		Native:   map[string]any{"exec": map[string]any{}, "tool_smith": map[string]any{}, "async": map[string]any{}},
		Settings: map[string]any{"sandbox": map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}, "tools": []string{"exec"}}},
	})
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sandbox == nil || loaded.Sandbox.Backend != "command" || !reflect.DeepEqual(loaded.Sandbox.Tools, []string{"exec"}) {
		t.Fatalf("sandbox = %+v", loaded.Sandbox)
	}
	if loaded.NativeExec == nil || loaded.NativeExec.Child.Sandbox != loaded.Sandbox {
		t.Fatal("native.exec did not get the sandbox")
	}
	if loaded.NativeToolSmith == nil || loaded.NativeToolSmith.Child.Sandbox != nil {
		t.Fatal("native.tool_smith is not in sandbox.tools and must run without it")
	}
	if loaded.Hooks.Sandbox != nil {
		t.Fatal("hooks are not in sandbox.tools and must run without it")
	}
}

// The schema and the loader must take and refuse the same sandbox settings. Only disabled
// sandboxes are compared: an enabled one also needs its program on this machine.
func TestSandboxSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "setting.schema.json")
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{}`, true},
		{`{"enabled":false,"tools":["exec","tool_smith"],"network":true,"writable_paths":["{workspace}","/var/x"],"hide_paths":["{home}/.ssh"]}`, true},
		{`{"backend":"command","command":["sandbox-exec","-f","/p.sb"]}`, true},
		{`{"backend":"bwrap","executable":"/usr/bin/bwrap"}`, true},
		{`{"backend":"command"}`, false},
		{`{"command":["env"]}`, false},
		{`{"backend":"command","command":["env"],"executable":"env"}`, false},
		{`{"backend":"docker"}`, false},
		{`{"tools":["lsp"]}`, true},
		{`{"tools":["read","search","write"]}`, true},
		{`{"tools":["bash"]}`, true},
		{`{"tools":["exec","exec"]}`, true},
		{`{"tools":[]}`, false},
		{`{"tools":[""]}`, false},
		{`{"writable_paths":["build"]}`, false},
		{`{"enabled":"yes"}`, false},
		{`{"netwrk":true}`, false},
		{`{"tools":["hooks"],"check_timeout_ms":5000}`, true},
		{`{"check_timeout_ms":50}`, false},
		{`{"check_timeout_ms":1.5}`, false},
		{`{"read":{"paths":["{system}","{workspace}"],"add_paths":["{home}/.m2"]},"write":{"paths":["{workspace}"],"add_paths":["{tmp}"],"plan_paths":["{tmp}"],"add_plan_paths":["{session_dir}/x"]}}`, true},
		{`{"add_hide_paths":["{workspace}/.env"],"isolation":{"mode":"worktree"}}`, true},
		{`{"read":{"path":["/"]}}`, false},
		{`{"read":{"paths":["src"]}}`, false},
		{`{"write":{"plan":["/"]}}`, false},
		{`{"isolation":{"mode":"overlay"}}`, false},
		{`{"isolation":{"base":"working"}}`, true},
		{`{"isolation":{"base":"tip"}}`, false},
		{`{"isolation":{"materialize":"overlay"}}`, false},
		{`{"tools":["lsp"],"read":{"paths":["/"]}}`, true},
	} {
		if got := schemaAccepts(t, schema, `{"sandbox":`+test.raw+`}`); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := normalizeSandbox(json.RawMessage(test.raw), "s"); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}

// The default covers the tools the model drives; hooks only when listed.
func TestSandboxDefaultLeavesHooksOutAndListedHooksLoad(t *testing.T) {
	for _, test := range []struct {
		tools []string
		hooks bool
	}{{nil, false}, {[]string{"hooks", "exec"}, true}} {
		sandbox := map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}}
		if test.tools != nil {
			sandbox["tools"] = test.tools
		}
		fixture := layouttest.Write(t, layouttest.Spec{
			Native:   map[string]any{"exec": map[string]any{}, "async": map[string]any{}},
			Settings: map[string]any{"sandbox": sandbox, "hooks": map[string]any{"enabled": true}},
		})
		config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		location, err := Resolve(config.Roots, fixture.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadSessionSettings(location, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if (loaded.Hooks.Sandbox != nil) != test.hooks || loaded.NativeExec.Child.Sandbox == nil {
			t.Errorf("tools %v: hooks sandbox %v, exec sandbox %v", test.tools, loaded.Hooks.Sandbox, loaded.NativeExec.Child.Sandbox)
		}
	}
}

// jad check gets each distinct enabled sandbox once, with the first session that uses it.
func TestCheckLayoutCollectsTheEnabledSandbox(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{
		Native:   map[string]any{"exec": map[string]any{}, "async": map[string]any{}},
		Settings: map[string]any{"sandbox": map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}}},
	})
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := CheckLayout(config.Roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Problems) != 0 || len(report.Sandboxes) != 1 || report.Sandboxes[0].SessionID != fixture.SessionID || report.Sandboxes[0].Settings.Command[0] != "env" {
		t.Fatalf("problems %+v sandboxes %+v", report.Problems, report.Sandboxes)
	}
}

// A plan-mode run's sandbox is the bwrap one with the plan paths writable; a command
// backend, the person's own program, and no sandbox are left as they are.
func TestSandboxForPlanMode(t *testing.T) {
	sandbox := &SandboxSettings{Backend: SandboxBackendBwrap, WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}, HidePaths: []string{"{home}/.ssh"}}
	plan := sandbox.ForPlanMode()
	if plan == sandbox || !reflect.DeepEqual(plan.WritablePaths, []string{"{tmp}"}) || !reflect.DeepEqual(plan.HidePaths, sandbox.HidePaths) ||
		!reflect.DeepEqual(sandbox.WritablePaths, []string{"{workspace}", "{tmp}"}) {
		t.Fatalf("plan = %+v, configured = %+v", plan, sandbox)
	}
	command := &SandboxSettings{Backend: SandboxBackendCommand, Command: []string{"firejail"}}
	if command.ForPlanMode() != command {
		t.Fatal("a command backend changed")
	}
	var none *SandboxSettings
	if none.ForPlanMode() != nil {
		t.Fatal("no sandbox became one")
	}
}

func TestSandboxNetworkAllowSeatbeltAndMCP(t *testing.T) {
	// enabled false skips the program and platform checks, so every backend loads here.
	got, err := normalizeSandbox(json.RawMessage(`{"enabled":false,"network_allow":["PyPI.org","www.github.com","pypi.org"],"proxy_port":8080,"tools":["exec","mcp"]}`), "s")
	if err != nil || got != nil {
		t.Fatalf("disabled sandbox = %+v, %v", got, err)
	}
	enabled, err := normalizeSandbox(json.RawMessage(`{"enabled":true,"executable":"sh","network_allow":["PyPI.org","www.github.com","pypi.org"],"proxy_port":8080,"tools":["exec","mcp"]}`), "s")
	if runtime.GOOS == "linux" {
		if err != nil || !reflect.DeepEqual(enabled.NetworkAllow, []string{"pypi.org", "github.com"}) || enabled.ProxyPort != 8080 || !enabled.Covers("mcp") {
			t.Fatalf("network_allow sandbox = %+v, %v", enabled, err)
		}
	}
	for name, test := range map[string]struct{ raw, want string }{
		"allow with network":    {`{"network":true,"network_allow":["a.com"]}`, "set one or the other"},
		"allow with command":    {`{"backend":"command","command":["env"],"network_allow":["a.com"]}`, "needs backend \"bwrap\""},
		"allow with seatbelt":   {`{"backend":"seatbelt","network_allow":["a.com"]}`, "needs backend \"bwrap\""},
		"bad host":              {`{"network_allow":["a b"]}`, "network_allow must be"},
		"allow not a list":      {`{"network_allow":"a.com"}`, "network_allow must be"},
		"proxy port":            {`{"proxy_port":0}`, "proxy_port"},
		"seatbelt with command": {`{"backend":"seatbelt","command":["env"]}`, "command is for backend"},
	} {
		if _, err := normalizeSandbox(json.RawMessage(test.raw), "s"); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
	}
	if runtime.GOOS != "darwin" {
		if _, err := normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"seatbelt"}`), "s"); err == nil || !strings.Contains(err.Error(), "macOS only") {
			t.Errorf("seatbelt off macOS: %v", err)
		}
	}
	plan := (&SandboxSettings{Backend: SandboxBackendSeatbelt, WritablePaths: []string{"{workspace}"}, PlanWritablePaths: []string{"{tmp}"}}).ForPlanMode()
	if !reflect.DeepEqual(plan.WritablePaths, []string{"{tmp}"}) {
		t.Fatalf("seatbelt plan mode = %+v", plan)
	}
}

// sandbox.schema.json and the template loader take and refuse the same template files.
func TestSandboxTemplateSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "sandbox.schema.json")
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{"schema_version":1,"description":"reader","enabled":false,"read":{"paths":["{system}","{workspace}"]},"write":{"paths":["{tmp}"],"plan_paths":["{tmp}"]}}`, true},
		{`{"enabled":false,"writable_paths":["{tmp}"],"add_hide_paths":["{workspace}/.env"]}`, true},
		{`{"enabled":false,"isolation":{"mode":"worktree"},"tools":["exec","lsp"]}`, true},
		{`{"schema_version":1}`, false},
		{`{"enabled":false,"template":"other"}`, false},
		{`{"enabled":false,"schema_version":2}`, false},
		{`{"enabled":false,"netwrk":true}`, false},
		{`{"enabled":false,"backend":"command"}`, false},
		{`{"enabled":false,"read":{"paths":["rel"]}}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := parseSandboxTemplate("t.json", []byte(test.raw)); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
	setting := compileSchema(t, "setting.schema.json")
	for raw, accept := range map[string]bool{`"reader"`: true, `{"template":"reader","network":true}`: true, `null`: true, `"team/reader"`: false, `""`: false} {
		if got := schemaAccepts(t, setting, `{"sandbox":`+raw+`}`); got != accept {
			t.Errorf("setting schema accepts sandbox %s = %v, want %v", raw, got, accept)
		}
	}
}

// §9.7: a profile's sandbox.isolation is kept in SessionConfig.Isolation whether or not the
// sandbox is enabled, so a profile_message target can be isolated without being confined.
func TestSessionIsolationIsKeptWithAndWithoutTheSandbox(t *testing.T) {
	for _, test := range []struct {
		sandbox map[string]any
		enabled bool
	}{
		{map[string]any{"isolation": map[string]any{"mode": "worktree", "base": "working"}}, false},
		{map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}, "isolation": map[string]any{"mode": "worktree", "base": "working"}}, true},
	} {
		fixture := layouttest.Write(t, layouttest.Spec{
			Native:   map[string]any{"exec": map[string]any{}},
			Settings: map[string]any{"sandbox": test.sandbox},
		})
		config := loadFixtureSession(t, fixture)
		if config.Isolation != (SandboxIsolation{Mode: SandboxIsolationWorktree, Base: SandboxIsolationBaseWorking}) {
			t.Fatalf("enabled %v: isolation = %+v", test.enabled, config.Isolation)
		}
		if (config.Sandbox != nil) != test.enabled {
			t.Fatalf("enabled %v: sandbox = %+v", test.enabled, config.Sandbox)
		}
	}
	fixture := layouttest.Write(t, layouttest.Spec{Native: map[string]any{"exec": map[string]any{}}})
	if config := loadFixtureSession(t, fixture); config.Isolation != (SandboxIsolation{}) {
		t.Fatalf("no sandbox: %+v", config.Isolation)
	}
}
