package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestHooksDefaultToOffWithNothingToRun(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := HooksSettings{TimeoutMS: 60_000, MaxOutputBytes: 8192}
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
	want := HooksSettings{Enabled: true, TimeoutMS: 5000, MaxOutputBytes: 1024, PostEdit: []PostEditHook{
		{Name: "go-vet", Ext: []string{".go"}, Argv: []string{"go", "vet", "./{reldir}"}, Cwd: "nearest:go.mod", OnFailureOnly: true},
		{Name: "json", Ext: []string{".json", ".JSONC"}, Argv: []string{"jq", "empty", "{file}"}, Cwd: "file_dir"},
	}}
	if !reflect.DeepEqual(config.Hooks, want) || !config.Hooks.Active() {
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
