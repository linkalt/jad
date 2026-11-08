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

func normalizeStateContextRaw(t *testing.T, raw string, task *NativeTaskSettings, records *NativeRecordsSettings, subagent *NativeSubagentSettings) (StateContextSettings, error) {
	t.Helper()
	return normalizeStateContextRawIn(t, t.TempDir(), raw, task, records, subagent)
}

func normalizeStateContextRawIn(t *testing.T, cwd, raw string, task *NativeTaskSettings, records *NativeRecordsSettings, subagent *NativeSubagentSettings) (StateContextSettings, error) {
	t.Helper()
	setting := map[string]json.RawMessage{}
	if raw != "" {
		setting["statecontext"] = json.RawMessage(raw)
	}
	return normalizeStateContextSettings(setting, stateContextSettingPath, cwd, task, records, subagent, nil)
}

func TestNormalizeStateContextSettingsDefaultsAndRemovedProcessProvider(t *testing.T) {
	defaults, err := normalizeStateContextRaw(t, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("normalizeStateContextSettings(defaults) error = %v", err)
	}
	if defaults.Enabled || defaults.Role != "developer" || defaults.Header != DefaultStateContextHeader || len(defaults.Triggers) != 0 {
		t.Fatalf("statecontext defaults = %+v", defaults)
	}

	// The process provider went with tools.native.process: it is refused by name, also when
	// a task provider that is available stands next to it.
	for _, raw := range []string{
		`{"enabled":true,"role":"system","triggers":{"compaction":{"tools":["process"],"max_bytes":2048}}}`,
		`{"enabled":true,"triggers":{"compaction":{"tools":["task"]},"session_start":{"tools":["task","process"]}}}`,
	} {
		_, err := normalizeStateContextRaw(t, raw, &NativeTaskSettings{}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "the process provider was removed") || !strings.Contains(err.Error(), "async provider") {
			t.Fatalf("process provider %s: error = %v, want a removal error naming the async provider", raw, err)
		}
	}
}

func TestNormalizeStateContextSettingsTriggersAreIndependent(t *testing.T) {
	configured, err := normalizeStateContextRaw(t,
		`{"enabled":true,"triggers":{"compaction":{"tools":["task","records"],"max_bytes":4096},"session_start":{"tools":["records"]}}}`, &NativeTaskSettings{}, &NativeRecordsSettings{StateContext: []NativeRecordsStateContextSelector{{Type: "project", Limit: 1}}}, nil)
	if err != nil {
		t.Fatalf("two triggers: %v", err)
	}
	want := map[string]StateContextTrigger{
		"compaction":    {Tools: []string{"task", "records"}, MaxBytes: 4096},
		"session_start": {Tools: []string{"records"}, MaxBytes: defaultStateContextMaxBytes},
	}
	if !reflect.DeepEqual(configured.Triggers, want) {
		t.Fatalf("triggers = %+v, want %+v", configured.Triggers, want)
	}

	// A trigger that is not listed does not exist, and the unused one needs no provider.
	only, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"session_start":{"tools":["task"]}}}`, &NativeTaskSettings{}, nil, nil)
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
		_, err := normalizeStateContextRaw(t, raw, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "statecontext") {
			t.Errorf("normalizeStateContextSettings(%s) error = %v, want statecontext config error", raw, err)
		}
	}

	for _, test := range []struct {
		name, raw, want string
		task            *NativeTaskSettings
		records         *NativeRecordsSettings
	}{
		{name: "task", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["task"]}}}`, want: "requires tools.native.task"},
		{name: "records", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["records"]}}}`, want: "requires tools.native.records"},
		{name: "records without selector", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["records"]}}}`, records: &NativeRecordsSettings{}, want: "requires at least one tools.native.records.statecontext selector"},
		{name: "second trigger", raw: `{"enabled":true,"triggers":{"compaction":{"tools":["task"]},"session_start":{"tools":["records"]}}}`, task: &NativeTaskSettings{}, want: "(trigger session_start) requires tools.native.records"},
	} {
		_, err := normalizeStateContextRaw(t, test.raw, test.task, test.records, nil)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", test.name, err, test.want)
		}
	}

	configuredTask, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"compaction":{"tools":["task"]}}}`, &NativeTaskSettings{}, nil, nil)
	if err != nil || !configuredTask.Enabled || !reflect.DeepEqual(configuredTask.Triggers["compaction"].Tools, []string{"task"}) {
		t.Fatalf("configured task provider = %+v, error = %v", configuredTask, err)
	}
	recordsSettings := &NativeRecordsSettings{StateContext: []NativeRecordsStateContextSelector{{Type: "project", Filter: map[string]any{}, Limit: 1}}}
	configuredRecords, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"compaction":{"tools":["records"]}}}`, nil, recordsSettings, nil)
	if err != nil || !configuredRecords.Enabled || !reflect.DeepEqual(configuredRecords.Triggers["compaction"].Tools, []string{"records"}) {
		t.Fatalf("configured records provider = %+v, error = %v", configuredRecords, err)
	}
	if _, err := normalizeStateContextRaw(t, `{"triggers":{"compaction":{"tools":["task"]}}}`, nil, nil, nil); err != nil {
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
		_, err := normalizeStateContextRaw(t, raw, &NativeTaskSettings{}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "is no longer supported; configure statecontext.triggers.<name>") || !strings.Contains(err.Error(), "compaction, session_start") {
			t.Errorf("%s: error = %v, want the new shape named", raw, err)
		}
	}
}

func TestNormalizeStateContextSettingsHeader(t *testing.T) {
	configured, err := normalizeStateContextRaw(t, `{"header":"Untrusted data follows."}`, nil, nil, nil)
	if err != nil || configured.Header != "Untrusted data follows." {
		t.Fatalf("custom header = %q, err = %v", configured.Header, err)
	}
	long := strings.Repeat("h", 1025)
	_, err = normalizeStateContextRaw(t, `{"header":"`+long+`","triggers":{"compaction":{"tools":["task"],"max_bytes":1024}}}`, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "statecontext.header is larger than statecontext.triggers.compaction.max_bytes") {
		t.Fatalf("oversized header error = %v", err)
	}
	// Each trigger has its own limit, so the header is checked against each one.
	_, err = normalizeStateContextRaw(t, `{"header":"`+long+`","triggers":{"compaction":{"tools":["task"],"max_bytes":4096},"session_start":{"tools":["task"],"max_bytes":1024}}}`, nil, nil, nil)
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
		`{"triggers":{"compaction":{"tools":["task"],"prompt":"Inline.","prompt_documents":["rules.md"]},"session_start":{"tools":["task"]}}}`, &NativeTaskSettings{}, nil, nil)
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
		_, err := normalizeStateContextRawIn(t, cwd, raw, &NativeTaskSettings{}, nil, nil)
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
		{`{"enabled":true,"header":"h","role":"system","triggers":{"compaction":{"tools":["task"],"max_bytes":2048},"session_start":{"tools":["records"]}}}`, true},
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
		_, err := normalizeStateContextRaw(t, test.raw, &NativeTaskSettings{}, &NativeRecordsSettings{StateContext: []NativeRecordsStateContextSelector{{Type: "p", Limit: 1}}}, nil)
		if (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}

func TestNormalizeStateContextSettingsUserTriggerMaxPromptBytes(t *testing.T) {
	records := &NativeRecordsSettings{StateContext: []NativeRecordsStateContextSelector{{Type: "project", Limit: 1}}}
	configured, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"user":{"tools":["records"]}}}`, nil, records, nil)
	if err != nil {
		t.Fatalf("user trigger: %v", err)
	}
	if got := configured.Triggers["user"].MaxPromptBytes; got != defaultStateContextMaxPromptBytes {
		t.Fatalf("default max_prompt_bytes = %d, want %d", got, defaultStateContextMaxPromptBytes)
	}
	custom, err := normalizeStateContextRaw(t, `{"enabled":true,"triggers":{"user":{"tools":["records"],"max_prompt_bytes":100}}}`, nil, records, nil)
	if err != nil || custom.Triggers["user"].MaxPromptBytes != 100 {
		t.Fatalf("custom max_prompt_bytes = %+v, %v", custom.Triggers["user"], err)
	}
	for name, raw := range map[string]string{
		"zero":              `{"enabled":true,"triggers":{"user":{"tools":["records"],"max_prompt_bytes":0}}}`,
		"above the cap":     `{"enabled":true,"triggers":{"user":{"tools":["records"],"max_prompt_bytes":65537}}}`,
		"not on compaction": `{"enabled":true,"triggers":{"compaction":{"tools":["records"],"max_prompt_bytes":10}}}`,
		"exceeds max_bytes": `{"enabled":true,"triggers":{"user":{"tools":["records"],"max_bytes":1024,"max_prompt_bytes":2000}}}`,
	} {
		if _, err := normalizeStateContextRaw(t, raw, nil, records, nil); err == nil {
			t.Fatalf("%s: want a config error", name)
		}
	}
	if configured.Triggers["user"].MaxBytes < configured.Triggers["user"].MaxPromptBytes {
		t.Fatal("max_prompt_bytes must fit inside max_bytes")
	}
}

func TestNormalizeStateContextSettingsAcceptsRunStart(t *testing.T) {
	configured, err := normalizeStateContextRaw(t,
		`{"enabled":true,"triggers":{"run_start":{"tools":["task"],"max_bytes":4096}}}`, &NativeTaskSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("run_start: %v", err)
	}
	want := map[string]StateContextTrigger{"run_start": {Tools: []string{"task"}, MaxBytes: 4096}}
	if !reflect.DeepEqual(configured.Triggers, want) {
		t.Fatalf("run_start triggers = %+v; want %+v", configured.Triggers, want)
	}
	if _, err := normalizeStateContextRaw(t,
		`{"enabled":true,"triggers":{"run_start":{"tools":["task"],"max_prompt_bytes":10}}}`, &NativeTaskSettings{}, nil, nil); err == nil || !strings.Contains(err.Error(), "max_prompt_bytes is not supported") {
		t.Fatalf("max_prompt_bytes on run_start error = %v; want it rejected", err)
	}
}

func TestStateContextPromptDocumentsRefuseDuplicatePath(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.md"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := normalizeStateContextRawIn(t, cwd, `{"triggers":{"compaction":{"tools":["task"],"prompt_documents":["a.md","./a.md"]}}}`, &NativeTaskSettings{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "prompt_documents[1] repeats") {
		t.Fatalf("duplicate document error = %v", err)
	}
}

func TestStateContextPromptDocumentsStopAtMaxBytesBeforeReading(t *testing.T) {
	cwd := t.TempDir()
	write := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(strings.Repeat("x", size)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("small.md", 1000)
	write("fits.md", 1000)
	write("big.md", 3500)
	config := func(docs string) string {
		return `{"triggers":{"compaction":{"tools":["task"],"max_bytes":4096,"prompt_documents":` + docs + `}}}`
	}
	if _, err := normalizeStateContextRawIn(t, cwd, config(`["small.md","fits.md"]`), &NativeTaskSettings{}, nil, nil); err != nil {
		t.Fatalf("within budget: %v", err)
	}
	_, err := normalizeStateContextRawIn(t, cwd, config(`["small.md","big.md","fits.md"]`), &NativeTaskSettings{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "prompt_documents[1]") || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("over budget error = %v", err)
	}
}
