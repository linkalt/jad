package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// defaultHooksWant is what an absent or empty hooks node loads as: off, with the child-process
// defaults the hooks own (they match bash's defaults but are not read from it).
func defaultHooksWant() HooksSettings {
	return HooksSettings{
		TimeoutMS: 60_000, MaxOutputBytes: 8192, MaxPayloadBytes: 65_536,
		KillGraceMs: 2000, KillDeadlineMs: 2000, DrainAfterExitMs: 500, KillProcessGroup: "always", MaxLineChars: 2000, StripANSI: true,
		Env: map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"}, EnvDeny: []string{},
	}
}

func TestHooksTakeTheirOwnChildProcessKeys(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{
		"enabled": true, "kill_grace_ms": 500, "kill_deadline_ms": 900, "drain_after_exit_ms": 70, "kill_process_group": "timeout_only", "max_line_chars": 120, "strip_ansi": false,
		"env": map[string]any{"CHECK_MODE": "strict", "NO_COLOR": "0"}, "env_deny": []any{"SECRET_A"},
	}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	h := config.Hooks
	if h.KillGraceMs != 500 || h.KillDeadlineMs != 900 || h.DrainAfterExitMs != 70 || h.KillProcessGroup != "timeout_only" || h.MaxLineChars != 120 || h.StripANSI {
		t.Fatalf("child keys = %+v", h)
	}
	if h.Env["CHECK_MODE"] != "strict" || h.Env["NO_COLOR"] != "0" || h.Env["GIT_TERMINAL_PROMPT"] != "0" || !reflect.DeepEqual(h.EnvDeny, []string{"SECRET_A"}) {
		t.Fatalf("env = %v deny = %v", h.Env, h.EnvDeny)
	}
}

func TestHooksReadNothingFromTheBashTool(t *testing.T) {
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"bash": map[string]any{"kill_grace_ms": 7000, "max_line_chars": 77, "strip_ansi": false, "env": map[string]any{"FROM_BASH": "1"}, "env_deny": []any{"X"}},
	}))
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if want := defaultHooksWant(); !reflect.DeepEqual(config.Hooks, want) {
		t.Fatalf("hooks picked up bash settings: %+v, want %+v", config.Hooks, want)
	}
}

func TestHooksEnvFileResolvesAndLoads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hooks.env"), []byte("FROM_FILE=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{"env_file": filepath.Join(dir, "hooks.env"), "env": map[string]any{"INLINE": "1"}}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if config.Hooks.Env["FROM_FILE"] != "yes" || config.Hooks.Env["INLINE"] != "1" || config.Hooks.EnvFile == "" {
		t.Fatalf("env file not applied: %+v", config.Hooks)
	}
	_, err = loadApprovalSetting(t, map[string]any{"hooks": map[string]any{"env_file": filepath.Join(dir, "missing.env")}})
	if err == nil || !strings.Contains(err.Error(), "hooks.env_file") {
		t.Fatalf("error = %v, want one naming hooks.env_file", err)
	}
}

func TestHooksDefaultToOffWithNothingToRun(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := defaultHooksWant()
	if !reflect.DeepEqual(config.Hooks, want) || config.Hooks.Active() {
		t.Fatalf("default hooks = %+v, want %+v and inactive", config.Hooks, want)
	}
}

func TestHooksAcceptEveryKey(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{
		"enabled": true, "timeout_ms": 5000, "max_output_bytes": 1024,
		"post_edit": []any{
			map[string]any{"name": "go-vet", "ext": []any{".go"}, "argv": []any{"go", "vet", "./{reldir}"}, "cwd": "nearest:go.mod", "on_failure_only": true},
			map[string]any{"name": "json", "ext": []any{".json", ".JSONC"}, "argv": []any{"jq", "empty", "{file}"}},
		},
	}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := defaultHooksWant()
	want.Enabled, want.TimeoutMS, want.MaxOutputBytes = true, 5000, 1024
	want.PostEdit = []PostEditHook{
		{Name: "go-vet", Ext: []string{".go"}, Argv: []string{"go", "vet", "./{reldir}"}, Cwd: "nearest:go.mod", OnFailureOnly: true},
		{Name: "json", Ext: []string{".json", ".JSONC"}, Argv: []string{"jq", "empty", "{file}"}, Cwd: "file_dir"},
	}
	if !reflect.DeepEqual(config.Hooks, want) || !config.Hooks.Active() {
		t.Fatalf("hooks = %+v, want %+v", config.Hooks, want)
	}
}

func TestHooksToolEventsLoadWithDefaults(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{
		"enabled":           true,
		"max_payload_bytes": 2048,
		"pre_tool":          []any{map[string]any{"name": "gate", "argv": []any{"./gate.sh", "--strict"}}},
		"post_tool": []any{
			map[string]any{"name": "audit", "match": "mcp:docs:*", "argv": []any{"audit"}, "on_failure_only": true},
			map[string]any{"name": "grep-log", "match": "grep", "argv": []any{"log"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := defaultHooksWant()
	want.Enabled, want.MaxPayloadBytes = true, 2048
	want.PreTool = []ToolEventHook{{Name: "gate", Match: "*", Argv: []string{"./gate.sh", "--strict"}}}
	want.PostTool = []ToolEventHook{
		{Name: "audit", Match: "mcp:docs:*", Argv: []string{"audit"}, OnFailureOnly: true},
		{Name: "grep-log", Match: "grep", Argv: []string{"log"}},
	}
	if !reflect.DeepEqual(config.Hooks, want) || !config.Hooks.ToolEventsActive() || config.Hooks.PostEditActive() || !config.Hooks.Active() {
		t.Fatalf("hooks = %+v, want %+v", config.Hooks, want)
	}
}

func TestHooksEnabledWithoutChecksIsInactive(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{"enabled": true}})
	if err != nil || config.Hooks.Active() {
		t.Fatalf("err=%v active=%v", err, config.Hooks.Active())
	}
}

func TestHooksRejectBadValuesWithTheKeyInTheMessage(t *testing.T) {
	good := func(mutate func(map[string]any)) map[string]any {
		entry := map[string]any{"name": "lint", "ext": []any{".go"}, "argv": []any{"go", "vet"}}
		if mutate != nil {
			mutate(entry)
		}
		return map[string]any{"post_edit": []any{entry}}
	}
	cases := []struct {
		name  string
		hooks any
		want  string
	}{
		{"not an object", "on", "hooks must be an object"},
		{"array", []any{}, "hooks must be an object"},
		{"misspelt key", map[string]any{"enable": true}, "hooks.enable is not supported; did you mean enabled?"},
		{"enabled string", map[string]any{"enabled": "yes"}, "hooks.enabled must be a boolean"},
		{"enabled null", map[string]any{"enabled": nil}, "hooks.enabled must be a boolean"},
		{"timeout low", map[string]any{"timeout_ms": 999}, "hooks.timeout_ms must be a safe integer between 1000 and 600000"},
		{"timeout high", map[string]any{"timeout_ms": 600001}, "hooks.timeout_ms must be a safe integer between 1000 and 600000"},
		{"timeout fraction", map[string]any{"timeout_ms": 1500.5}, "hooks.timeout_ms must be a safe integer"},
		{"output low", map[string]any{"max_output_bytes": 255}, "hooks.max_output_bytes must be a safe integer between 256 and 1048576"},
		{"output null", map[string]any{"max_output_bytes": nil}, "hooks.max_output_bytes must be a safe integer"},
		{"kill grace high", map[string]any{"kill_grace_ms": 60001}, "hooks.kill_grace_ms must be a safe integer between 0 and 60000"},
		{"kill deadline zero", map[string]any{"kill_deadline_ms": 0}, "hooks.kill_deadline_ms must be a safe integer between 1 and 60000"},
		{"drain high", map[string]any{"drain_after_exit_ms": 60001}, "hooks.drain_after_exit_ms must be a safe integer between 1 and 60000"},
		{"kill group word", map[string]any{"kill_process_group": "never"}, "hooks.kill_process_group must be always or timeout_only"},
		{"line chars low", map[string]any{"max_line_chars": 19}, "hooks.max_line_chars must be a safe integer between 20 and 8000"},
		{"strip ansi string", map[string]any{"strip_ansi": "yes"}, "hooks.strip_ansi must be a boolean"},
		{"env array", map[string]any{"env": []any{}}, "hooks.env must be an object"},
		{"env bad name", map[string]any{"env": map[string]any{"1BAD": "x"}}, "hooks.env.1BAD is not a valid environment variable name"},
		{"env number", map[string]any{"env": map[string]any{"A": 1}}, "hooks.env.A must be a string"},
		{"env_deny object", map[string]any{"env_deny": map[string]any{}}, "hooks.env_deny must be an array"},
		{"env_deny bad name", map[string]any{"env_deny": []any{"a b"}}, "hooks.env_deny[0] must be an environment variable name"},
		{"env_file blank", map[string]any{"env_file": " "}, "hooks: env_file must be a non-empty string"},
		{"post_edit object", map[string]any{"post_edit": map[string]any{}}, "hooks.post_edit must be a list"},
		{"post_edit null", map[string]any{"post_edit": nil}, "hooks.post_edit must be a list"},
		{"entry string", map[string]any{"post_edit": []any{"go vet"}}, "hooks.post_edit[0] must be an object"},
		{"entry unknown key", good(func(e map[string]any) { e["shell"] = true }), "hooks.post_edit[0].shell is not supported"},
		{"name missing", good(func(e map[string]any) { delete(e, "name") }), "hooks.post_edit[0].name must match"},
		{"name upper", good(func(e map[string]any) { e["name"] = "Lint" }), "hooks.post_edit[0].name must match"},
		{"ext missing", good(func(e map[string]any) { delete(e, "ext") }), "hooks.post_edit[0].ext must be 1 to 32 extensions"},
		{"ext empty", good(func(e map[string]any) { e["ext"] = []any{} }), "hooks.post_edit[0].ext must be 1 to 32 extensions"},
		{"ext no dot", good(func(e map[string]any) { e["ext"] = []any{"go"} }), `hooks.post_edit[0].ext entry "go"`},
		{"ext too long", good(func(e map[string]any) { e["ext"] = []any{".abcdefghi"} }), "hooks.post_edit[0].ext entry"},
		{"argv missing", good(func(e map[string]any) { delete(e, "argv") }), "hooks.post_edit[0].argv must be 1 to 32 non-empty strings"},
		{"argv empty", good(func(e map[string]any) { e["argv"] = []any{} }), "hooks.post_edit[0].argv must be 1 to 32 non-empty strings"},
		{"argv blank item", good(func(e map[string]any) { e["argv"] = []any{"go", " "} }), "hooks.post_edit[0].argv must be 1 to 32 non-empty strings"},
		{"argv number", good(func(e map[string]any) { e["argv"] = []any{"go", 7} }), "hooks.post_edit[0].argv must be 1 to 32 non-empty strings"},
		{"argv program placeholder", good(func(e map[string]any) { e["argv"] = []any{"{file}"} }), "hooks.post_edit[0].argv[0] is the program"},
		{"argv unknown placeholder", good(func(e map[string]any) { e["argv"] = []any{"go", "{path}"} }), "unknown placeholder {path}"},
		{"cwd word", good(func(e map[string]any) { e["cwd"] = "repo" }), "hooks.post_edit[0].cwd must be"},
		{"cwd bare nearest", good(func(e map[string]any) { e["cwd"] = "nearest:" }), "hooks.post_edit[0].cwd must be"},
		{"cwd marker dot", good(func(e map[string]any) { e["cwd"] = "nearest:." }), "hooks.post_edit[0].cwd must be"},
		{"cwd marker dotdot", good(func(e map[string]any) { e["cwd"] = "nearest:.." }), "hooks.post_edit[0].cwd must be"},
		{"cwd marker with slash", good(func(e map[string]any) { e["cwd"] = "nearest:../go.mod" }), "hooks.post_edit[0].cwd must be"},
		{"failure only string", good(func(e map[string]any) { e["on_failure_only"] = "yes" }), "hooks.post_edit[0].on_failure_only must be a boolean"},
		{"failure only null", good(func(e map[string]any) { e["on_failure_only"] = nil }), "hooks.post_edit[0].on_failure_only must be a boolean"},
		{"pre_tool object", map[string]any{"pre_tool": map[string]any{}}, "hooks.pre_tool must be a list"},
		{"post_tool null", map[string]any{"post_tool": nil}, "hooks.post_tool must be a list"},
		{"pre_tool entry string", map[string]any{"pre_tool": []any{"echo"}}, "hooks.pre_tool[0] must be an object"},
		{"pre_tool unknown key", map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "ext": []any{".go"}}}}, "hooks.pre_tool[0].ext is not supported"},
		{"pre_tool name", map[string]any{"pre_tool": []any{map[string]any{"name": "A b", "argv": []any{"a"}}}}, "hooks.pre_tool[0].name must match"},
		{"pre_tool argv missing", map[string]any{"pre_tool": []any{map[string]any{"name": "a"}}}, "hooks.pre_tool[0].argv must be 1 to 32"},
		{"pre_tool argv blank", map[string]any{"pre_tool": []any{map[string]any{"name": "a", "argv": []any{"a", " "}}}}, "hooks.pre_tool[0].argv must be 1 to 32"},
		{"post_tool placeholder", map[string]any{"post_tool": []any{map[string]any{"name": "a", "argv": []any{"a", "{file}"}}}}, "hooks.post_tool[0].argv[1] has a placeholder"},
		{"post_tool match path", map[string]any{"post_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "match": "../x"}}}, "hooks.post_tool[0].match must be"},
		{"post_tool match number", map[string]any{"post_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "match": 7}}}, "hooks.post_tool[0].match must be"},
		{"post_tool failure only", map[string]any{"post_tool": []any{map[string]any{"name": "a", "argv": []any{"a"}, "on_failure_only": "yes"}}}, "hooks.post_tool[0].on_failure_only must be a boolean"},
		{"pre_tool duplicate", map[string]any{"pre_tool": []any{
			map[string]any{"name": "a", "argv": []any{"a"}},
			map[string]any{"name": "a", "argv": []any{"b"}},
		}}, `hooks.pre_tool[1].name "a" is used twice`},
		{"payload too small", map[string]any{"max_payload_bytes": 10}, "hooks.max_payload_bytes must be a safe integer between 1024 and 1048576"},
		{"duplicate name", map[string]any{"post_edit": []any{
			map[string]any{"name": "lint", "ext": []any{".go"}, "argv": []any{"a"}},
			map[string]any{"name": "lint", "ext": []any{".ts"}, "argv": []any{"b"}},
		}}, `hooks.post_edit[1].name "lint" is used twice`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadApprovalSetting(t, map[string]any{"hooks": test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestHooksAllowMoreThanOneCheckForTheSameExtension(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"hooks": map[string]any{"enabled": true, "post_edit": []any{
		map[string]any{"name": "a", "ext": []any{".go"}, "argv": []any{"gofmt", "-l", "{file}"}},
		map[string]any{"name": "b", "ext": []any{".go"}, "argv": []any{"go", "vet"}},
	}}})
	if err != nil || len(config.Hooks.PostEdit) != 2 {
		t.Fatalf("err=%v hooks=%+v", err, config.Hooks)
	}
}

// The sample file is what a user copies, so the loader must accept it and the schema must
// describe every key it uses.
func TestHooksSampleFileLoadsAndMatchesTheSchema(t *testing.T) {
	data, err := os.ReadFile("../../../sample/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample map[string]any
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	config, err := loadApprovalSetting(t, sample)
	if err != nil {
		t.Fatalf("the sample does not load: %v", err)
	}
	if !config.Hooks.Active() || len(config.Hooks.PostEdit) != 4 {
		t.Fatalf("sample hooks = %+v", config.Hooks)
	}

	schemaData, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Hooks struct {
				Properties map[string]struct {
					Items struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"hooks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatal(err)
	}
	var topKeys, entryKeys []string
	for key := range schema.Properties.Hooks.Properties {
		topKeys = append(topKeys, key)
	}
	for key := range schema.Properties.Hooks.Properties["post_edit"].Items.Properties {
		entryKeys = append(entryKeys, key)
	}
	for list, loader := range map[string][]string{
		"pre_tool":    preToolHookKeys,
		"post_tool":   toolEventHookKeys,
		"run_start":   runEventHookKeys,
		"run_end":     runEventHookKeys,
		"pre_compact": runEventHookKeys,
	} {
		got := sortedItemKeys(schema.Properties.Hooks.Properties[list].Items.Properties)
		want := append([]string(nil), loader...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("schema %s entry keys %v, loader accepts %v", list, got, want)
		}
	}
	sort.Strings(topKeys)
	sort.Strings(entryKeys)
	wantTop := append([]string(nil), hooksKeys...)
	wantEntry := append([]string(nil), postEditHookKeys...)
	sort.Strings(wantTop)
	sort.Strings(wantEntry)
	if !reflect.DeepEqual(topKeys, wantTop) || !reflect.DeepEqual(entryKeys, wantEntry) {
		t.Fatalf("schema keys %v / %v, loader accepts %v / %v", topKeys, entryKeys, wantTop, wantEntry)
	}
}

func sortedItemKeys(properties map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
