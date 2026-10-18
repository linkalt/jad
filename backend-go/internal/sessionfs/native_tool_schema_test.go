package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join("..", "..", "..", "sample", "schemes", name))
	if err != nil {
		t.Fatalf("%s does not compile: %v", name, err)
	}
	return schema
}

func schemaAccepts(t *testing.T, schema *jsonschema.Schema, text string) bool {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return schema.Validate(value) == nil
}

// The shared setting shapes (read, edit, bash style) are defined once in the schema. The
// schema and the loader must take and refuse the same tool settings, in the toolset file
// and in the inline tools.native block.
func TestNativeToolSchemasAgreeWithTheLoader(t *testing.T) {
	toolset := compileSchema(t, "toolset.schema.json")
	setting := compileSchema(t, "setting.schema.json")
	paths := `{"allow":["a"],"deny":["b"]}`
	child := `"shell":"/bin/sh","login_shell":true,"kill_grace_ms":5,"max_line_chars":100,"stdin_max_bytes":10,"strip_ansi":false,"env":{"A":"1"},"env_deny":["B"],"paths":` + paths
	run := `"timeout_ms":1000,"max_timeout_ms":2000,"kill_process_group":"always","max_bytes":65536,"stdout":{"head_bytes":1,"tail_bytes":1},"stderr":{"head_bytes":1,"tail_bytes":1},"spill":{"enabled":false}`
	cases := []struct {
		name   string
		tool   string
		config string
		ok     bool
	}{
		{"read paths", "read", `{"paths":` + paths + `,"max_bytes":100}`, true},
		{"read unknown", "read", `{"path":"x"}`, false},
		{"grep paths", "grep", `{"paths":` + paths + `,"max_results":3}`, true},
		{"grep unknown", "grep", `{"nope":1}`, false},
		{"glob paths", "glob", `{"paths":` + paths + `}`, true},
		{"glob unknown", "glob", `{"nope":1}`, false},
		{"read_image paths", "read_image", `{"paths":` + paths + `,"max_image_bytes":100}`, true},
		{"read_image unknown", "read_image", `{"nope":1}`, false},
		{"write backups", "write", `{"backup_max_count":2,"backup_max_total_bytes":3,"fsync":false,"backup_dir":null,"paths":` + paths + `}`, true},
		{"write unknown", "write", `{"nope":1}`, false},
		{"edit backups", "edit", `{"backup_max_count":2,"backup_max_total_bytes":3,"fsync":false,"backup_dir":"b","paths":` + paths + `,"max_bytes":9}`, true},
		{"edit unknown", "edit", `{"nope":1}`, false},
		{"bash every key", "bash", `{"approval":"ask",` + child + `,` + run + `}`, true},
		{"bash unknown", "bash", `{"nope":1}`, false},
		{"exec every bash key", "exec", `{"approval":"ask",` + child + `,` + run + `,"line_aware":false,"default_lang":"bash","code_max_bytes":10,"result_max_bytes":10}`, true},
		{"exec unknown", "exec", `{"nope":1}`, false},
		{"process child keys", "process", `{"approval":"ask",` + child + `,"stdout":{"head_bytes":1,"tail_bytes":1},"max_running":2}`, true},
		{"process unknown", "process", `{"nope":1}`, false},
		{"process has no run keys", "process", `{"timeout_ms":1000}`, false},
		{"bash bad type", "bash", `{"timeout_ms":"slow"}`, false},
		{"exec bad type", "exec", `{"timeout_ms":0}`, false},
		{"read_image bad paths", "read_image", `{"paths":3}`, false},
		{"decide paths", "decide", `{"paths":` + paths + `,"max_images":2}`, true},
		{"decide unknown", "decide", `{"nope":1}`, false},
		{"decide bad paths", "decide", `{"paths":3}`, false},
		{"profile_message every key", "profile_message", `{"profiles":["a","exec/fast"],"timeout_ms":1000,"result_max_bytes":100,"approval":"ask"}`, true},
		{"profile_message bad approval", "profile_message", `{"profiles":["a"],"approval":"on"}`, false},
		{"profile_message unknown", "profile_message", `{"profiles":["a"],"nope":1}`, false},
		{"profile_message empty list", "profile_message", `{"profiles":[]}`, false},
		{"profile_message no profiles", "profile_message", `{}`, false},
		{"profile_message duplicate", "profile_message", `{"profiles":["a","a"]}`, false},
		{"profile_message bad id", "profile_message", `{"profiles":["a b"]}`, false},
		{"profile_message zero timeout", "profile_message", `{"profiles":["a"],"timeout_ms":0}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config map[string]any
			if err := json.Unmarshal([]byte(tc.config), &config); err != nil {
				t.Fatal(err)
			}
			text := `{"native":{"` + tc.tool + `":` + tc.config + `}}`
			if got := schemaAccepts(t, toolset, `{"schema_version":1,`+text[1:]); got != tc.ok {
				t.Errorf("toolset.schema.json accepts = %v, want %v", got, tc.ok)
			}
			if got := schemaAccepts(t, setting, `{"tools":`+text+`}`); got != tc.ok {
				t.Errorf("setting.schema.json accepts = %v, want %v", got, tc.ok)
			}
			_, err := loadApprovalSetting(t, nativeTools(map[string]any{tc.tool: config}))
			if (err == nil) != tc.ok {
				t.Errorf("loader accepts = %v, want %v (err: %v)", err == nil, tc.ok, err)
			}
		})
	}
}

// The toolset schema takes its native tool block from setting.schema.json by reference, so
// the tool settings are written once. The schema lists the same keys for exec as the loader
// accepts, so a key added to one is caught in the other.
func TestExecAndReadImageSchemaKeysMatchTheLoader(t *testing.T) {
	setting := readSchemaJSON(t, "setting.schema.json")
	defs, _ := setting["$defs"].(map[string]any)
	nativeBlock := setting["properties"].(map[string]any)["tools"].(map[string]any)["properties"].(map[string]any)["native"].(map[string]any)
	native := nativeBlock["properties"].(map[string]any)

	toolsetNative := readSchemaJSON(t, "toolset.schema.json")["properties"].(map[string]any)["native"].(map[string]any)
	if ref := toolsetNative["$ref"]; ref != "setting.schema.json#/properties/tools/properties/native" {
		t.Errorf("toolset.schema.json native $ref = %v, want the tools.native block of setting.schema.json", ref)
	}
	if _, copied := toolsetNative["properties"]; copied {
		t.Error("toolset.schema.json native must reference setting.schema.json, not repeat the tool settings")
	}

	for tool, want := range map[string][]string{"exec": nativeExecKeys, "read_image": nativeReadImageKeys, "bash": nativeBashStyleKeys, "decide": nativeDecideKeys, "profile_message": nativeProfileMessageKeys} {
		got := flattenSchemaKeys(native[tool].(map[string]any), defs)
		sort.Strings(got)
		want = append([]string(nil), want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s schema keys\n  %v\nloader accepts\n  %v", tool, got, want)
		}
	}
}

func readSchemaJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	var value map[string]any
	readJSONFile(t, filepath.Join("..", "..", "..", "sample", "schemes", name), &value)
	return value
}

func flattenSchemaKeys(node map[string]any, defs map[string]any) []string {
	seen := map[string]bool{}
	var walk func(map[string]any)
	walk = func(n map[string]any) {
		if props, ok := n["properties"].(map[string]any); ok {
			for key := range props {
				seen[key] = true
			}
		}
		if all, ok := n["allOf"].([]any); ok {
			for _, item := range all {
				ref, _ := item.(map[string]any)["$ref"].(string)
				if def, ok := defs[ref[strings.LastIndex(ref, "/")+1:]].(map[string]any); ok {
					walk(def)
				}
			}
		}
	}
	walk(node)
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	return out
}
