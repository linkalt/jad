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
		Tools: []string{"bash", "exec", "process", "tool_smith"}, Backend: "command", Executable: "sh",
		WritablePaths: []string{"{workspace}", "{git_common_dir}", "{tmp}"}, Command: []string{"sh", "-c", "exec \"$@\"", "sandbox"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %+v", got)
	}
	got, err = normalizeSandbox(json.RawMessage(`{"enabled":true,"backend":"command","command":["env"],"tools":["process","bash"],"network":true,"writable_paths":["{cwd}","/var/cache/x"],"hide_paths":["{home}/.ssh"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tools, []string{"process", "bash"}) || !got.Network || !reflect.DeepEqual(got.WritablePaths, []string{"{cwd}", "/var/cache/x"}) ||
		!reflect.DeepEqual(got.HidePaths, []string{"{home}/.ssh"}) || !got.Covers("bash") || got.Covers("exec") {
		t.Fatalf("values = %+v", got)
	}
	var none *SandboxSettings
	if none.Covers("bash") {
		t.Fatal("no sandbox covers nothing")
	}
}

func TestSandboxRefusals(t *testing.T) {
	for name, test := range map[string]struct{ raw, want string }{
		"not an object":       {`[]`, "sandbox must be an object"},
		"unknown key":         {`{"enabled":true,"netwrk":true}`, "did you mean network"},
		"enabled not bool":    {`{"enabled":"yes"}`, "sandbox.enabled must be a boolean"},
		"unknown tool":        {`{"tools":["grep"]}`, "tools[0] must be one of"},
		"empty tools":         {`{"tools":[]}`, "tools must list one or more"},
		"tool twice":          {`{"tools":["bash","bash"]}`, "lists bash twice"},
		"backend":             {`{"backend":"docker"}`, `backend must be "bwrap" or "command"`},
		"relative path":       {`{"writable_paths":["build"]}`, "writable_paths[0] must be an absolute path"},
		"unknown placeholder": {`{"hide_paths":["{project}/x"]}`, "unknown placeholder {project}"},
		"command missing":     {`{"backend":"command"}`, "command is required"},
		"command for bwrap":   {`{"command":["env"]}`, `command is for backend "command"`},
		"executable for cmd":  {`{"backend":"command","command":["env"],"executable":"env"}`, "the program is command[0]"},
		"empty command":       {`{"backend":"command","command":[]}`, "command must be a non-empty array"},
		"missing program":     {`{"enabled":true,"backend":"command","command":["jad-no-such-sandbox-program"]}`, "cannot be run"},
		"missing bwrap":       {`{"enabled":true,"executable":"jad-no-such-bwrap"}`, ""},
		"checked when off":    {`{"enabled":false,"backend":"docker"}`, "backend must be"},
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
	sandbox := &SandboxSettings{Tools: []string{"bash", "tool_smith"}}
	bash, execSettings, smith, process := &NativeBashSettings{}, &NativeExecSettings{}, &NativeToolSmithSettings{}, &NativeProcessSettings{}
	applySandbox(sandbox, bash, execSettings, smith, process)
	if bash.Sandbox != sandbox || smith.Child.Sandbox != sandbox || execSettings.Child.Sandbox != nil || process.Sandbox != nil {
		t.Fatalf("bash %v exec %v smith %v process %v", bash.Sandbox, execSettings.Child.Sandbox, smith.Child.Sandbox, process.Sandbox)
	}
	applySandbox(sandbox, nil, nil, nil, nil)
	applySandbox(nil, bash, execSettings, smith, process)
}

// The sandbox key is a setting of the layout: a session's settings carry it to the tools.
func TestSandboxLoadsFromTheSessionSettings(t *testing.T) {
	fixture := layouttest.Write(t, layouttest.Spec{
		Native:   map[string]any{"bash": map[string]any{}, "process": map[string]any{}, "async": map[string]any{}},
		Settings: map[string]any{"sandbox": map[string]any{"enabled": true, "backend": "command", "command": []string{"env"}, "tools": []string{"bash"}}},
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
	if loaded.Sandbox == nil || loaded.Sandbox.Backend != "command" || !reflect.DeepEqual(loaded.Sandbox.Tools, []string{"bash"}) {
		t.Fatalf("sandbox = %+v", loaded.Sandbox)
	}
	if loaded.NativeBash == nil || loaded.NativeBash.Sandbox != loaded.Sandbox {
		t.Fatal("native.bash did not get the sandbox")
	}
	if loaded.NativeProcess == nil || loaded.NativeProcess.Sandbox != nil {
		t.Fatal("native.process is not in sandbox.tools and must run without it")
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
		{`{"enabled":false,"tools":["bash","process"],"network":true,"writable_paths":["{workspace}","/var/x"],"hide_paths":["{home}/.ssh"]}`, true},
		{`{"backend":"command","command":["sandbox-exec","-f","/p.sb"]}`, true},
		{`{"backend":"bwrap","executable":"/usr/bin/bwrap"}`, true},
		{`{"backend":"command"}`, false},
		{`{"command":["env"]}`, false},
		{`{"backend":"command","command":["env"],"executable":"env"}`, false},
		{`{"backend":"docker"}`, false},
		{`{"tools":["grep"]}`, false},
		{`{"tools":[]}`, false},
		{`{"tools":["bash","bash"]}`, false},
		{`{"writable_paths":["build"]}`, false},
		{`{"enabled":"yes"}`, false},
		{`{"netwrk":true}`, false},
	} {
		if got := schemaAccepts(t, schema, `{"sandbox":`+test.raw+`}`); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := normalizeSandbox(json.RawMessage(test.raw), "s"); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}
