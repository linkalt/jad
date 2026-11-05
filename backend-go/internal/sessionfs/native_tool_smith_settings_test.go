package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func toolSmithSettingsFor(native string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":` + native + `}`)}
}

func normalizeToolSmithForTest(t *testing.T, native string) (*NativeToolSmithSettings, error) {
	t.Helper()
	dir := t.TempDir()
	return normalizeNativeToolSmithSettings(toolSmithSettingsFor(native), dir, filepath.Join(dir, "setting.json"), dir, dir, "", dir)
}

func TestNormalizeNativeToolSmithSettingsAbsent(t *testing.T) {
	for name, setting := range map[string]map[string]json.RawMessage{
		"no tools":      {},
		"no tool_smith": toolSmithSettingsFor(`{}`),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeToolSmithSettings(setting, "", "setting.json", "", "", "", "")
			if err != nil || got != nil {
				t.Fatalf("settings = %+v, %v; want nil", got, err)
			}
		})
	}
	if got, err := normalizeNativeToolSmithSettings(toolSmithSettingsFor(`null`), "", "setting.json", "", "", "", ""); err == nil || got != nil {
		t.Fatalf("null native: got %+v, %v; want refusal", got, err)
	}
}

func TestNormalizeNativeToolSmithSettingsDefaults(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{}}`)
	if err != nil || got == nil {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	if got.FunctionsDir != ".jad/functions" || got.CodeMaxBytes != 262144 || got.ResultMaxBytes != 16384 || got.NestedResultMaxBytes != 16384 || got.NestedRequestMaxBytes != 65536 {
		t.Fatalf("byte and dir defaults = %+v", got)
	}
	if got.MaxNestedCalls != 100 || got.MaxParallelCalls != 4 || got.ListMaxResults != 50 || got.ListMaxFiles != 200 || got.ListHeaderBytes != 2048 || got.FunctionMaxBytes != 65536 {
		t.Fatalf("count defaults = %+v", got)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("tools = %#v, want none", got.Tools)
	}
	if got.Child.TimeoutMs != 120000 || got.Child.MaxTimeoutMs != 600000 {
		t.Fatalf("child defaults = %+v", got.Child)
	}
	if !reflect.DeepEqual(got.Interpreter, []string{"bun", "run", "{file}"}) {
		t.Fatalf("interpreter = %v, want bun run {file}", got.Interpreter)
	}
}

func TestNormalizeNativeToolSmithSettingsInterpreter(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{"interpreter":["node","{file}"]}}`)
	if err != nil || !reflect.DeepEqual(got.Interpreter, []string{"node", "{file}"}) {
		t.Fatalf("interpreter = %+v, %v; want node {file}", got, err)
	}
	null, err := normalizeToolSmithForTest(t, `{"tool_smith":{"interpreter":null}}`)
	if err != nil || !reflect.DeepEqual(null.Interpreter, []string{"bun", "run", "{file}"}) {
		t.Fatalf("null interpreter = %+v, %v; want the default", null, err)
	}
	many := `"{file}"` + strings.Repeat(`,"x"`, 31)
	if _, err := normalizeToolSmithForTest(t, `{"tool_smith":{"interpreter":[`+many+`]}}`); err != nil {
		t.Fatalf("32 elements rejected: %v", err)
	}
	const wantShape = "tools.native.tool_smith.interpreter must be 1 to 32 non-empty strings"
	for name, tc := range map[string]struct{ native, want string }{
		"empty array":   {`{"tool_smith":{"interpreter":[]}}`, wantShape},
		"empty element": {`{"tool_smith":{"interpreter":["node",""]}}`, wantShape},
		"not strings":   {`{"tool_smith":{"interpreter":["node",1]}}`, wantShape},
		"not array":     {`{"tool_smith":{"interpreter":"node {file}"}}`, wantShape},
		"over 32":       {`{"tool_smith":{"interpreter":[` + many + `,"x"]}}`, wantShape},
		"no file":       {`{"tool_smith":{"interpreter":["node","script.js"]}}`, "tools.native.tool_smith.interpreter must contain {file}"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeToolSmithForTest(t, tc.native)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeToolSmithSettingsOverrides(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{"functions_dir":"lib/fn","code_max_bytes":10,"result_max_bytes":20,"nested_result_max_bytes":30,"nested_request_max_bytes":40,"timeout_ms":1000,"max_timeout_ms":2000,"max_nested_calls":5,"max_parallel_calls":2,"list_max_results":7,"list_max_files":8,"list_header_bytes":9,"function_max_bytes":11,"tools":["lsp","read_image"],"standby_pool":2,"standby_idle_ms":5000,"standby_max":3}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := NativeToolSmithSettings{FunctionsDir: "lib/fn", CodeMaxBytes: 10, ResultMaxBytes: 20, NestedResultMaxBytes: 30, NestedRequestMaxBytes: 40, MaxNestedCalls: 5, MaxParallelCalls: 2, ListMaxResults: 7, ListMaxFiles: 8, ListHeaderBytes: 9, FunctionMaxBytes: 11, Tools: []string{"lsp", "read_image"}, Interpreter: []string{"bun", "run", "{file}"}, StandbyPool: 2, StandbyIdleMs: 5000, StandbyMax: 3}
	if got.Child.TimeoutMs != 1000 || got.Child.MaxTimeoutMs != 2000 {
		t.Fatalf("child timeouts = %d, %d", got.Child.TimeoutMs, got.Child.MaxTimeoutMs)
	}
	got.Child = NativeBashSettings{}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("settings = %+v\nwant %+v", *got, want)
	}
	empty, err := normalizeToolSmithForTest(t, `{"tool_smith":{"tools":[],"code_max_bytes":null}}`)
	if err != nil || len(empty.Tools) != 0 || empty.CodeMaxBytes != 262144 {
		t.Fatalf("empty tools = %+v, %v", empty, err)
	}
}

func TestNormalizeNativeToolSmithSettingsRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"not object":         {`{"tool_smith":[]}`, "tools.native.tool_smith must be an object"},
		"unknown key":        {`{"tool_smith":{"nope":1}}`, "tools.native.tool_smith.nope is not supported; supported keys are"},
		"typo":               {`{"tool_smith":{"functions_dr":"x"}}`, "did you mean functions_dir?"},
		"env_file blank":     {`{"tool_smith":{"env_file":" "}}`, "env_file must be a non-empty string"},
		"env_file missing":   {`{"tool_smith":{"env_file":"missing.env"}}`, "tools.native.tool_smith.env_file"},
		"paths":              {`{"tool_smith":{"paths":{}}}`, "tools.native.tool_smith.paths is not supported"},
		"empty dir":          {`{"tool_smith":{"functions_dir":" "}}`, "functions_dir must be a non-empty string"},
		"dir type":           {`{"tool_smith":{"functions_dir":1}}`, "functions_dir must be a non-empty string"},
		"code zero":          {`{"tool_smith":{"code_max_bytes":0}}`, "code_max_bytes must be a safe integer between 1 and 10485760"},
		"code over":          {`{"tool_smith":{"code_max_bytes":10485761}}`, "code_max_bytes must be a safe integer between 1 and 10485760"},
		"result string":      {`{"tool_smith":{"result_max_bytes":"1"}}`, "result_max_bytes must be a safe integer"},
		"nested over":        {`{"tool_smith":{"nested_result_max_bytes":10485761}}`, "nested_result_max_bytes must be a safe integer between 1 and 10485760"},
		"request over":       {`{"tool_smith":{"nested_request_max_bytes":10485761}}`, "nested_request_max_bytes must be a safe integer between 1 and 10485760"},
		"request zero":       {`{"tool_smith":{"nested_request_max_bytes":0}}`, "nested_request_max_bytes must be a safe integer between 1 and 10485760"},
		"nested calls zero":  {`{"tool_smith":{"max_nested_calls":0}}`, "max_nested_calls must be a safe integer between 1 and 10000"},
		"nested calls over":  {`{"tool_smith":{"max_nested_calls":10001}}`, "max_nested_calls must be a safe integer between 1 and 10000"},
		"parallel over":      {`{"tool_smith":{"max_parallel_calls":65}}`, "max_parallel_calls must be a safe integer between 1 and 64"},
		"list results over":  {`{"tool_smith":{"list_max_results":1001}}`, "list_max_results must be a safe integer between 1 and 1000"},
		"list files over":    {`{"tool_smith":{"list_max_files":10001}}`, "list_max_files must be a safe integer between 1 and 10000"},
		"list header over":   {`{"tool_smith":{"list_header_bytes":65537}}`, "list_header_bytes must be a safe integer between 1 and 65536"},
		"list header zero":   {`{"tool_smith":{"list_header_bytes":0}}`, "list_header_bytes must be a safe integer between 1 and 65536"},
		"function max zero":  {`{"tool_smith":{"function_max_bytes":0}}`, "function_max_bytes must be a safe integer between 1 and 10485760"},
		"function max over":  {`{"tool_smith":{"function_max_bytes":10485761}}`, "function_max_bytes must be a safe integer between 1 and 10485760"},
		"timeout zero":       {`{"tool_smith":{"timeout_ms":0}}`, "timeout_ms must be a safe integer"},
		"max below timeout":  {`{"tool_smith":{"timeout_ms":5000,"max_timeout_ms":1000}}`, "max_timeout_ms must be greater than or equal to tools.native.tool_smith.timeout_ms"},
		"tools not array":    {`{"tool_smith":{"tools":"read_image"}}`, "tools must be an array of native tool names"},
		"tools null item":    {`{"tool_smith":{"tools":[null]}}`, "tools[0] must be a native tool name"},
		"tools empty name":   {`{"tool_smith":{"tools":[""]}}`, "tools[0] must be a native tool name"},
		"tools exec second":  {`{"tool_smith":{"tools":["read_image","exec"]}}`, "tools[1] \"exec\" cannot be called from a script"},
		"tools exec":         {`{"tool_smith":{"tools":["exec"]}}`, "\"exec\" cannot be called from a script"},
		"tools async":        {`{"tool_smith":{"tools":["async"]}}`, "\"async\" cannot be called from a script"},
		"tools subagent":     {`{"tool_smith":{"tools":["subagent"]}}`, "\"subagent\" cannot be called from a script"},
		"tools tool_smith":   {`{"tool_smith":{"tools":["tool_smith"]}}`, "\"tool_smith\" cannot be called from a script"},
		"tools task":         {`{"tool_smith":{"tools":["task"]}}`, "\"task\" cannot be called from a script"},
		"tools non native":   {`{"tool_smith":{"tools":["mcp:x"]}}`, "\"mcp:x\" is not a native tool"},
		"tools duplicate":    {`{"tool_smith":{"tools":["read_image","read_image"]}}`, "tools[1] \"read_image\" is listed twice"},
		"approval removed":   {`{"tool_smith":{"approval":"off"}}`, "tools.native.tool_smith.approval was removed"},
		"upper limits valid": {`{"tool_smith":{"code_max_bytes":10485760,"max_nested_calls":10000,"max_parallel_calls":64,"list_max_results":1000,"list_max_files":10000,"list_header_bytes":65536,"function_max_bytes":10485760}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeToolSmithForTest(t, tc.native)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid setting: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeToolSmithSettingsEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ts.env"), []byte("FROM_FILE=file\nOVERRIDDEN=file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	native := `{"tool_smith":{"env_file":"ts.env","env":{"OVERRIDDEN":"inline"}}}`
	got, err := normalizeNativeToolSmithSettings(toolSmithSettingsFor(native), dir, filepath.Join(dir, "setting.json"), dir, dir, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	child := got.Child
	if child.EnvFile != filepath.Join(dir, "ts.env") || child.Env["FROM_FILE"] != "file" || child.Env["OVERRIDDEN"] != "inline" || child.Env["NO_COLOR"] != "1" {
		t.Fatalf("env file = %q env = %v", child.EnvFile, child.Env)
	}
}

func TestNormalizeNativeToolSmithSettingsBashStyleDefaults(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	child := got.Child
	if child.Shell != "/bin/bash" || child.KillGraceMs != 2000 || child.KillDeadlineMs != 2000 || child.DrainAfterExitMs != 500 || child.KillProcessGroup != "always" {
		t.Fatalf("process defaults = %+v", child)
	}
	if child.MaxBytes != 64*1024 || child.MaxLineChars != 2000 || child.StdinMaxBytes != 256*1024 || !child.StripANSI {
		t.Fatalf("output defaults = %+v", child)
	}
	if len(child.EnvDeny) != 0 {
		t.Fatalf("env_deny = %v, want none", child.EnvDeny)
	}
	if !child.Spill.Enabled || child.Spill.Keep != "truncated_only" || child.Spill.CapBytes != 64*1024*1024 || filepath.Base(child.Spill.StorageDir) != ".bash-output" {
		t.Fatalf("spill defaults = %+v", child.Spill)
	}
}

func TestNormalizeNativeToolSmithSettingsBashStyleOverrides(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{"shell":"/bin/sh","kill_grace_ms":0,"kill_deadline_ms":7,"drain_after_exit_ms":9,"kill_process_group":"timeout_only","max_bytes":100000,"max_line_chars":50,"stdin_max_bytes":11,"strip_ansi":false,"env_deny":["SECRET"],"spill":{"enabled":false,"keep":"always","cap_bytes":5}}}`)
	if err != nil {
		t.Fatal(err)
	}
	child := got.Child
	if child.Shell != "/bin/sh" || child.KillGraceMs != 0 || child.KillDeadlineMs != 7 || child.DrainAfterExitMs != 9 || child.KillProcessGroup != "timeout_only" {
		t.Fatalf("process overrides = %+v", child)
	}
	if child.MaxBytes != 100000 || child.MaxLineChars != 50 || child.StdinMaxBytes != 11 || child.StripANSI {
		t.Fatalf("output overrides = %+v", child)
	}
	if !reflect.DeepEqual(child.EnvDeny, []string{"SECRET"}) {
		t.Fatalf("env_deny = %v", child.EnvDeny)
	}
	if child.Spill.Enabled || child.Spill.Keep != "always" || child.Spill.CapBytes != 5 {
		t.Fatalf("spill overrides = %+v", child.Spill)
	}
}

func TestNormalizeNativeToolSmithSettingsRejectsOutOfRangeLimits(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"list results zero":      {`{"tool_smith":{"list_max_results":0}}`, "list_max_results must be a safe integer between 1 and 1000"},
		"list files zero":        {`{"tool_smith":{"list_max_files":0}}`, "list_max_files must be a safe integer between 1 and 10000"},
		"result zero":            {`{"tool_smith":{"result_max_bytes":0}}`, "result_max_bytes must be a safe integer between 1 and 10485760"},
		"result over":            {`{"tool_smith":{"result_max_bytes":10485761}}`, "result_max_bytes must be a safe integer between 1 and 10485760"},
		"nested result zero":     {`{"tool_smith":{"nested_result_max_bytes":0}}`, "nested_result_max_bytes must be a safe integer between 1 and 10485760"},
		"kill deadline zero":     {`{"tool_smith":{"kill_deadline_ms":0}}`, "kill_deadline_ms must be a safe integer between 1 and 60000"},
		"kill deadline over":     {`{"tool_smith":{"kill_deadline_ms":60001}}`, "kill_deadline_ms must be a safe integer between 1 and 60000"},
		"drain zero":             {`{"tool_smith":{"drain_after_exit_ms":0}}`, "drain_after_exit_ms must be a safe integer between 1 and 60000"},
		"drain over":             {`{"tool_smith":{"drain_after_exit_ms":60001}}`, "drain_after_exit_ms must be a safe integer between 1 and 60000"},
		"kill process group bad": {`{"tool_smith":{"kill_process_group":"never"}}`, "kill_process_group must be always or timeout_only"},
		"stdin max zero":         {`{"tool_smith":{"stdin_max_bytes":0}}`, "stdin_max_bytes must be a safe integer"},
		"max line chars too low": {`{"tool_smith":{"max_line_chars":19}}`, "max_line_chars must be a safe integer between 20 and 8000"},
		"max bytes zero":         {`{"tool_smith":{"max_bytes":0}}`, "max_bytes must be a safe integer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeToolSmithForTest(t, tc.native)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNativeToolNamesIncludesToolSmith(t *testing.T) {
	if !containsString(NativeToolNames(), "tool_smith") {
		t.Fatal("tool_smith missing from NativeToolNames")
	}
}

func TestNativeToolSmithExcludedToolsIsACopy(t *testing.T) {
	got := NativeToolSmithExcludedTools()
	if !reflect.DeepEqual(got, nativeToolSmithExcludedTools) {
		t.Fatalf("excluded = %v", got)
	}
	got[0] = "changed"
	if nativeToolSmithExcludedTools[0] == "changed" {
		t.Fatal("the exported list aliases the internal one")
	}
}

func TestNativeToolSmithStandbyDefaultsAndBounds(t *testing.T) {
	got, err := normalizeToolSmithForTest(t, `{"tool_smith":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.StandbyPool != 1 || got.StandbyIdleMs != 300000 || got.StandbyMax != 8 {
		t.Fatalf("standby defaults = pool %d idle %d max %d", got.StandbyPool, got.StandbyIdleMs, got.StandbyMax)
	}
	for native, want := range map[string]string{
		`{"tool_smith":{"standby_pool":17}}`:     "standby_pool must be a safe integer between 0 and 16",
		`{"tool_smith":{"standby_idle_ms":999}}`: "standby_idle_ms must be a safe integer between 1000 and 21600000",
		`{"tool_smith":{"standby_max":0}}`:       "standby_max must be a safe integer between 1 and 256",
		`{"tool_smith":{"standby_pool":"many"}}`: "standby_pool must be a safe integer",
	} {
		if _, err := normalizeToolSmithForTest(t, native); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", native, err, want)
		}
	}
}
