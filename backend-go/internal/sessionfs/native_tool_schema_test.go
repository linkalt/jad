package sessionfs

import (
	"encoding/json"
	"os"
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
	envFile := filepath.Join(t.TempDir(), "tool_smith.env")
	if err := os.WriteFile(envFile, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envFileJSON, _ := json.Marshal(envFile)
	toolSmithChild := `"shell":"/bin/sh","login_shell":true,"kill_grace_ms":5,"kill_deadline_ms":5,"drain_after_exit_ms":5,"max_line_chars":100,"stdin_max_bytes":10,"strip_ansi":false,"env_file":` + string(envFileJSON) + `,"env":{"A":"1"},"env_deny":["B"]`
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
		{"bash every key", "bash", `{` + child + `,` + run + `}`, true},
		{"bash unknown", "bash", `{"nope":1}`, false},
		{"bash removed approval", "bash", `{"approval":"ask"}`, false},
		{"write removed approval", "write", `{"approval":"ask"}`, false},
		{"exec removed approval", "exec", `{"approval":"ask"}`, false},
		{"process removed approval", "process", `{"approval":"ask"}`, false},
		{"exec every bash key", "exec", `{` + child + `,` + run + `,"line_aware":false,"default_lang":"bash","code_max_bytes":10,"result_max_bytes":10}`, true},
		{"exec unknown", "exec", `{"nope":1}`, false},
		{"process child keys", "process", `{` + child + `,"stdout":{"head_bytes":1,"tail_bytes":1},"max_running":2}`, true},
		{"process unknown", "process", `{"nope":1}`, false},
		{"process has no run keys", "process", `{"timeout_ms":1000}`, false},
		{"bash bad type", "bash", `{"timeout_ms":"slow"}`, false},
		{"exec bad type", "exec", `{"timeout_ms":0}`, false},
		{"read_image bad paths", "read_image", `{"paths":3}`, false},
		{"decide paths", "decide", `{"paths":` + paths + `,"max_images":2}`, true},
		{"decide unknown", "decide", `{"nope":1}`, false},
		{"decide bad paths", "decide", `{"paths":3}`, false},
		{"profile_message every key", "profile_message", `{"deny_profiles":["dev/a","exec/fast"],"timeout_ms":1000,"result_max_bytes":100}`, true},
		{"profile_message removed approval", "profile_message", `{"approval":"ask"}`, false},
		{"profile_message unknown", "profile_message", `{"nope":1}`, false},
		{"profile_message old profiles key", "profile_message", `{"profiles":["a"]}`, false},
		{"profile_message empty deny list", "profile_message", `{"deny_profiles":[]}`, true},
		{"profile_message null deny list", "profile_message", `{"deny_profiles":null}`, true},
		{"profile_message no keys", "profile_message", `{}`, true},
		{"profile_message deny not a list", "profile_message", `{"deny_profiles":"a"}`, false},
		{"profile_message deny duplicate", "profile_message", `{"deny_profiles":["dev/a","dev/a"]}`, false},
		{"profile_message deny bare id", "profile_message", `{"deny_profiles":["coder"]}`, false},
		{"profile_message deny bad id", "profile_message", `{"deny_profiles":["a b"]}`, false},
		{"profile_message zero timeout", "profile_message", `{"timeout_ms":0}`, false},
		{"tool_smith every key", "tool_smith", `{` + toolSmithChild + `,` + run + `,"functions_dir":".jad/functions","code_max_bytes":10,"result_max_bytes":10,"nested_result_max_bytes":10,"nested_request_max_bytes":10,"max_nested_calls":5,"max_parallel_calls":2,"list_max_results":3,"list_max_files":4,"list_header_bytes":512,"function_max_bytes":10,"tools":["read","write"],"interpreter":["node","{file}"]}`, true},
		{"tool_smith no keys", "tool_smith", `{}`, true},
		{"tool_smith unknown", "tool_smith", `{"nope":1}`, false},
		{"tool_smith env_file", "tool_smith", `{"env_file":` + string(envFileJSON) + `}`, true},
		{"tool_smith blank env_file", "tool_smith", `{"env_file":" "}`, false},
		{"tool_smith has no paths", "tool_smith", `{"paths":` + paths + `}`, false},
		{"tool_smith removed approval", "tool_smith", `{"approval":"ask"}`, false},
		{"tool_smith zero code max", "tool_smith", `{"code_max_bytes":0}`, false},
		{"tool_smith zero request max", "tool_smith", `{"nested_request_max_bytes":0}`, false},
		{"tool_smith request max too high", "tool_smith", `{"nested_request_max_bytes":10485761}`, false},
		{"tool_smith parallel too high", "tool_smith", `{"max_parallel_calls":65}`, false},
		{"tool_smith excluded tool", "tool_smith", `{"tools":["bash"]}`, false},
		{"tool_smith duplicate tool", "tool_smith", `{"tools":["read","read"]}`, false},
		{"tool_smith empty interpreter", "tool_smith", `{"interpreter":[]}`, false},
		{"tool_smith empty functions_dir", "tool_smith", `{"functions_dir":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config map[string]any
			if err := json.Unmarshal([]byte(tc.config), &config); err != nil {
				t.Fatal(err)
			}
			node := "native"
			if tc.tool == "decide" {
				node = "extension"
			}
			text := `{"` + node + `":{"` + tc.tool + `":` + tc.config + `}}`
			if got := schemaAccepts(t, toolset, `{"schema_version":1,`+text[1:]); got != tc.ok {
				t.Errorf("toolset.schema.json accepts = %v, want %v", got, tc.ok)
			}
			if got := schemaAccepts(t, setting, `{"tools":`+text+`}`); got != tc.ok {
				t.Errorf("setting.schema.json accepts = %v, want %v", got, tc.ok)
			}
			settings := nativeTools(map[string]any{tc.tool: config})
			if node == "extension" {
				settings = map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{tc.tool: config}}}
			}
			_, err := loadSetting(t, settings)
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

	extensionBlock := setting["properties"].(map[string]any)["tools"].(map[string]any)["properties"].(map[string]any)["extension"].(map[string]any)["properties"].(map[string]any)
	decideKeys := flattenSchemaKeys(extensionBlock["decide"].(map[string]any), defs)
	sort.Strings(decideKeys)
	wantDecide := append([]string(nil), extensionDecideKeys...)
	sort.Strings(wantDecide)
	if strings.Join(decideKeys, ",") != strings.Join(wantDecide, ",") {
		t.Errorf("decide schema keys\n  %v\nloader accepts\n  %v", decideKeys, wantDecide)
	}
	for tool, want := range map[string][]string{"exec": nativeExecKeys, "read_image": nativeReadImageKeys, "bash": nativeBashStyleKeys, "profile_message": nativeProfileMessageKeys, "tool_smith": nativeToolSmithKeys} {
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
