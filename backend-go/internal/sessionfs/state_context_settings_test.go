package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const stateContextSettingPath = "/session/setting.json"

func normalizeStateContextRaw(t *testing.T, raw string, process *NativeProcessSettings, task *NativeTaskSettings, memory *NativeMemorySettings, subagent *NativeSubagentSettings) (StateContextSettings, error) {
	t.Helper()
	return normalizeStateContextRawIn(t, t.TempDir(), raw, process, task, memory, subagent)
}

func normalizeStateContextRawIn(t *testing.T, cwd, raw string, process *NativeProcessSettings, task *NativeTaskSettings, memory *NativeMemorySettings, subagent *NativeSubagentSettings) (StateContextSettings, error) {
	t.Helper()
	setting := map[string]json.RawMessage{}
	if raw != "" {
		setting["statecontext"] = json.RawMessage(raw)
	}
	return normalizeStateContextSettings(setting, stateContextSettingPath, cwd, process, task, memory, subagent)
}

func TestNormalizeStateContextSettingsDefaultsAndProcessProvider(t *testing.T) {
	defaults, err := normalizeStateContextRaw(t, "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(defaults) error = %v", err)
	}
	if defaults.Enabled || defaults.Role != "developer" || defaults.Header != DefaultStateContextHeader || len(defaults.Triggers) != 0 {
		t.Fatalf("statecontext defaults = %+v", defaults)
	}

	configured, err := normalizeStateContextRaw(t,
		`{"enabled":true,"role":"system","triggers":{"compaction":{"tools":["process","process"],"max_bytes":2048}}}`,
		&NativeProcessSettings{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(process) error = %v", err)
	}
	want := map[string]StateContextTrigger{"compaction": {Tools: []string{"process"}, MaxBytes: 2048}}
	if !configured.Enabled || configured.Role != "system" || !reflect.DeepEqual(configured.Triggers, want) {
		t.Fatalf("normalized process statecontext = %+v", configured)
	}
}

func TestNormalizeStateContextSettingsTriggersAreIndependent(t *testing.T) {
	configured, err := normalizeStateContextRaw(t,
		`{"enabled":true,"triggers":{"compaction":{"tools":["task","memory"],"max_bytes":4096},"session_start":{"tools":["memory"]}}}`,
		nil, &NativeTaskSettings{}, &NativeMemorySettings{StateContext: []NativeMemoryStateContextSelector{{Type: "project", Limit: 1}}}, nil)
	if err != nil {
		t.Fatalf("two triggers: %v", err)
	}
	want := map[string]StateContextTrigger{
		"compaction":    {Tools: []string{"task", "memory"}, MaxBytes: 4096},
		"session_start": {Tools: []string{"memory"}, MaxBytes: defaultStateContextMaxBytes},
	}
	if !reflect.DeepEqual(configured.Triggers, want) {
		t.Fatalf("triggers = %+v, want %+v", configured.Triggers, want)
	}

	// A trigger that is not listed does not exist, and the unused one needs no provider.
	only, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"session_start":{"tools":["task"]}}}`, nil, &NativeTaskSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("session_start only: %v", err)
	}
	if _, exists := only.Triggers["compaction"]; exists || len(only.Triggers) != 1 {
		t.Fatalf("session_start only = %+v; compaction must not exist", only.Triggers)
	}
}

func TestNormalizeStateContextSettingsRejectsInvalidAndUnavailableProviders(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`{"enabled":"yes"}`,
		`{"enabled":true}`,
		`{"enabled":true,"triggers":{}}`,
		`{"enabled":true,"triggers":null}`,
		`{"enabled":true,"triggers":[]}`,
		`{"triggers":{"timer":{"tools":["task"]}}}`,
		`{"triggers":{"compaction":null}}`,
		`{"triggers":{"compaction":{}}}`,
		`{"triggers":{"compaction":{"tools":[]}}}`,
		`{"triggers":{"compaction":{"tools":null}}}`,
		`{"triggers":{"compaction":{"tools":"task"}}}`,
		`{"triggers":{"compaction":{"tools":["task",3]}}}`,
		`{"triggers":{"compaction":{"tools":["request_draft"]}}}`,
		`{"triggers":{"compaction":{"tools":["task"],"timeout":1}}}`,
		`{"role":"assistant"}`,
		`{"triggers":{"compaction":{"tools":["task"],"max_bytes":1023}}}`,
		`{"triggers":{"compaction":{"tools":["task"],"max_bytes":10485761}}}`,
		`{"header":""}`,
		`{"header":"   "}`,
		`{"header":7}`,
	} {
		_, err := normalizeStateContextRaw(t, raw, nil, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "statecontext") {
			t.Errorf("normalizeStateContextSettings(%s) error = %v, want statecontext config error", raw, err)
		}
	}

	for _, test := range []struct {
		name, raw, want string
		process         *NativeProcessSettings
		task            *NativeTaskSettings
		memory          *NativeMemorySettings
	}{
		{name: "process", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["process"]}}}`, want: "requires tools.native.process"},
		{name: "task", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["task"]}}}`, want: "requires tools.native.task"},
		{name: "memory", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["memory"]}}}`, want: "requires tools.native.memory"},
		{name: "memory without selector", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["memory"]}}}`, memory: &NativeMemorySettings{}, want: "requires at least one tools.native.memory.statecontext selector"},
		{name: "second trigger", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["task"]},"session_start":{"tools":["process"]}}}`, task: &NativeTaskSettings{}, want: "(trigger session_start) requires tools.native.process"},
	} {
		_, err := normalizeStateContextRaw(t, test.raw, test.process, test.task, test.memory, nil)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", test.name, err, test.want)
		}
	}

	configuredTask, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"compaction":{"tools":["task"]}}}`, nil, &NativeTaskSettings{}, nil, nil)
	if err != nil || !configuredTask.Enabled || !reflect.DeepEqual(configuredTask.Triggers["compaction"].Tools, []string{"task"}) {
		t.Fatalf("configured task provider = %+v, error = %v", configuredTask, err)
	}
	memorySettings := &NativeMemorySettings{StateContext: []NativeMemoryStateContextSelector{{Type: "project", Filter: map[string]any{}, Limit: 1}}}
	configuredMemory, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"compaction":{"tools":["memory"]}}}`, nil, nil, memorySettings, nil)
	if err != nil || !configuredMemory.Enabled || !reflect.DeepEqual(configuredMemory.Triggers["compaction"].Tools, []string{"memory"}) {
		t.Fatalf("configured memory provider = %+v, error = %v", configuredMemory, err)
	}
	if _, err := normalizeStateContextRaw(t, `{"triggers":{"compaction":{"tools":["task"]}}}`, nil, nil, nil, nil); err != nil {
		t.Fatalf("disabled task provider should remain inert, error = %v", err)
	}
}

func TestNormalizeStateContextSettingsRemovedFlatKeysNameTheNewShape(t *testing.T) {
	for _, raw := range []string{
		`{"enabled":true,"trigger":["compaction"],"tools":["task"]}`,
		`{"trigger":["compaction"]}`,
		`{"tools":["task"]}`,
		`{"max_bytes":4096}`,
	} {
		_, err := normalizeStateContextRaw(t, raw, nil, &NativeTaskSettings{}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "is no longer supported; configure statecontext.triggers.<name>") || !strings.Contains(err.Error(), "compaction, session_start") {
			t.Errorf("%s: error = %v, want the new shape named", raw, err)
		}
	}
}

func TestNormalizeStateContextSettingsHeader(t *testing.T) {
	configured, err := normalizeStateContextRaw(t, `{"header":"Untrusted data follows."}`, nil, nil, nil, nil)
	if err != nil || configured.Header != "Untrusted data follows." {
		t.Fatalf("custom header = %q, err = %v", configured.Header, err)
	}
	long := strings.Repeat("h", 1025)
	_, err = normalizeStateContextRaw(t, `{"header":"`+long+`","triggers":{"compaction":{"tools":["task"],"max_bytes":1024}}}`, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "statecontext.header is larger than statecontext.triggers.compaction.max_bytes") {
		t.Fatalf("oversized header error = %v", err)
	}
	// Each trigger has its own limit, so the header is checked against each one.
	_, err = normalizeStateContextRaw(t, `{"header":"`+long+`","triggers":{"compaction":{"tools":["task"],"max_bytes":4096},"session_start":{"tools":["task"],"max_bytes":1024}}}`, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "statecontext.triggers.session_start.max_bytes") {
		t.Fatalf("oversized header for the second trigger: error = %v", err)
	}
}

func TestNormalizeStateContextSettingsPromptAndDocuments(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "rules.md"), []byte("From a file."), 0o600); err != nil {
		t.Fatal(err)
	}
	configured, err := normalizeStateContextRawIn(t, cwd,
		`{"triggers":{"compaction":{"tools":["task"],"prompt":"Inline.","prompt_documents":["rules.md"]},"session_start":{"tools":["task"]}}}`,
		nil, &NativeTaskSettings{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.Triggers["compaction"].Prompt; got != "Inline.\n\nFrom a file." {
		t.Fatalf("compaction prompt = %q; want the inline text then the file", got)
	}
	if got := configured.Triggers["session_start"].Prompt; got != "" {
		t.Fatalf("session_start prompt = %q; a trigger without a prompt has none", got)
	}
}

func TestNormalizeStateContextSettingsPromptErrors(t *testing.T) {
	cwd := t.TempDir()
	long := strings.Repeat("p", 1024)
	for raw, want := range map[string]string{
		`{"triggers":{"compaction":{"tools":["task"],"prompt_documents":["missing.md"]}}}`:        "statecontext.triggers.compaction.prompt_documents[0] could not be read",
		`{"triggers":{"compaction":{"tools":["task"],"prompt_documents":[" "]}}}`:                 "prompt_documents[0] must be a non-empty string",
		`{"triggers":{"compaction":{"tools":["task"],"prompt":""}}}`:                              "statecontext.triggers.compaction.prompt must be a non-empty string",
		`{"triggers":{"compaction":{"tools":["task"],"prompt":"` + long + `","max_bytes":1024}}}`: "header plus statecontext.triggers.compaction prompt is larger",
	} {
		_, err := normalizeStateContextRawIn(t, cwd, raw, nil, &NativeTaskSettings{}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", raw, err, want)
		}
	}
}

// The schema and the loader must take and refuse the same statecontext settings.
func TestStateContextSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "setting.schema.json")
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{"enabled":true,"header":"h","role":"system","triggers":{"compaction":{"tools":["task"],"max_bytes":2048},"session_start":{"tools":["memory"]}}}`, true},
		{`{"triggers":{}}`, true},
		{`{"triggers":{"timer":{"tools":["task"]}}}`, false},
		{`{"triggers":{"compaction":{}}}`, false},
		{`{"triggers":{"compaction":{"tools":[]}}}`, false},
		{`{"triggers":{"compaction":{"tools":["request_draft"]}}}`, false},
		{`{"triggers":{"compaction":{"tools":["task"],"prompt":"x"}}}`, true},
		{`{"triggers":{"compaction":{"tools":["task"],"prompt":" "}}}`, false},
		{`{"triggers":{"compaction":{"tools":["task"],"prompt":3}}}`, false},
		{`{"triggers":{"compaction":{"tools":["task"],"prompt_documents":[]}}}`, true},
		{`{"triggers":{"compaction":{"tools":["task"],"prompt_documents":"a.md"}}}`, false},
		{`{"triggers":{"compaction":{"tools":["task"],"unknown":1}}}`, false},
		{`{"triggers":{"compaction":{"tools":["task"],"max_bytes":1023}}}`, false},
		{`{"header":""}`, false},
		{`{"header":"  "}`, false},
		{`{"role":"assistant"}`, false},
	} {
		if got := schemaAccepts(t, schema, `{"statecontext":`+test.raw+`}`); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		_, err := normalizeStateContextRaw(t, test.raw, nil, &NativeTaskSettings{}, &NativeMemorySettings{StateContext: []NativeMemoryStateContextSelector{{Type: "p", Limit: 1}}}, nil)
		if (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}
