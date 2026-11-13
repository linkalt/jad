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

// The shared setting shapes (paths, child-process style) are defined once in the schema. The
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
		{"read_image paths", "read_image", `{"paths":` + paths + `,"max_image_bytes":100}`, true},
		{"read_image unknown", "read_image", `{"nope":1}`, false},
		{"exec removed approval", "exec", `{"approval":"ask"}`, false},
		{"exec every bash key", "exec", `{` + child + `,` + run + `,"line_aware":false,"default_lang":"bash","code_max_bytes":10,"result_max_bytes":10,"input_max_bytes":10,"arg_max_bytes":10,"functions_dir":"f","list_max_results":1,"list_max_files":1,"list_header_bytes":1,"function_max_bytes":1}`, true},
		{"exec unknown", "exec", `{"nope":1}`, false},
		{"exec bad type", "exec", `{"timeout_ms":0}`, false},
		{"exec removed spill allow_unreadable", "exec", `{"spill":{"allow_unreadable":true}}`, false},
		{"read_image bad paths", "read_image", `{"paths":3}`, false},
		{"task every limit key", "task", `{"max_tasks":5,"list_limit":2,"max_list_limit":3,"max_title_chars":10,"max_tags":2,"max_tag_chars":5,"max_documents":2,"max_document_chars":9,"max_storage_bytes":1000,"max_list_bytes":100,"max_batch_ops":50,"storage_dir":"t"}`, true},
		{"task max_batch_ops at the cap", "task", `{"max_batch_ops":200}`, true},
		{"task max_batch_ops over the cap", "task", `{"max_batch_ops":201}`, false},
		{"task max_batch_ops zero", "task", `{"max_batch_ops":0}`, false},
		{"task max_batch_ops text", "task", `{"max_batch_ops":"20"}`, false},
		{"task unknown", "task", `{"nope":1}`, false},
		{"removed read", "read", `{}`, false},
		{"removed write", "write", `{}`, false},
		{"removed edit", "edit", `{}`, false},
		{"removed apply_patch", "apply_patch", `{}`, false},
		{"removed notebook", "notebook", `{}`, false},
		{"removed grep", "grep", `{}`, false},
		{"removed glob", "glob", `{}`, false},
		{"removed bash", "bash", `{}`, false},
		{"removed process", "process", `{}`, false},
		{"removed fetch", "fetch", `{}`, false},
		{"decide_systemone keys", "decide_systemone", `{"max_questions":2}`, true},
		{"decide_systemone unknown", "decide_systemone", `{"nope":1}`, false},
		{"profile_message every key", "profile_message", `{"deny":[{"to":"local:*/*/dev/a"},{"to":"local:*/*/exec/fast","actions":["reset"]}],"timeout_ms":1000,"result_max_bytes":100}`, true},
		{"profile_message removed approval", "profile_message", `{"approval":"ask"}`, false},
		{"profile_message unknown", "profile_message", `{"nope":1}`, false},
		{"profile_message old profiles key", "profile_message", `{"profiles":["a"]}`, false},
		{"profile_message empty deny list", "profile_message", `{"deny":[]}`, true},
		{"profile_message no keys", "profile_message", `{}`, true},
		{"profile_message deny not a list", "profile_message", `{"deny_profiles":"a"}`, false},
		{"profile_message deny duplicate", "profile_message", `{"deny_profiles":["dev/a","dev/a"]}`, false},
		{"profile_message deny bare id", "profile_message", `{"deny_profiles":["coder"]}`, false},
		{"profile_message deny bad id", "profile_message", `{"deny_profiles":["a b"]}`, false},
		{"profile_message zero timeout", "profile_message", `{"timeout_ms":0}`, false},
		{"tool_smith every key", "tool_smith", `{` + toolSmithChild + `,` + run + `,"functions_dir":".jad/functions","code_max_bytes":10,"result_max_bytes":10,"nested_result_max_bytes":10,"nested_request_max_bytes":10,"max_nested_calls":5,"max_parallel_calls":2,"list_max_results":3,"list_max_files":4,"list_header_bytes":512,"function_max_bytes":10,"tools":["read_image","lsp"],"interpreter":["node","{file}"]}`, true},
		{"tool_smith no keys", "tool_smith", `{}`, true},
		{"transcript every key", "transcript", `{"response_max_bytes":100,"preview_bytes":10,"value_bytes":10,"max_list_entries":5,"max_detail_bytes":100,"line_max_bytes":1000,"scan_max_bytes":1000,"suggest_max":3,"timeout_ms":1000,"allow_other_sessions":false,"allow_other_reasoning":true,"allow_other_profiles":false,"allow_children":false,"allow_path":true}`, true},
		{"transcript bad allow_other_reasoning", "transcript", `{"allow_other_reasoning":"yes"}`, false},
		{"transcript no keys", "transcript", `{}`, true},
		{"transcript unknown", "transcript", `{"nope":1}`, false},
		{"transcript has no paths", "transcript", `{"paths":` + paths + `}`, false},
		{"transcript timeout over six hours", "transcript", `{"timeout_ms":21600001}`, false},
		{"transcript zero response max", "transcript", `{"response_max_bytes":0}`, false},
		{"transcript bad allow_path", "transcript", `{"allow_path":"yes"}`, false},
		{"transcripts every key", "transcripts", `{"max_entries":5,"scan_max_entries":10,"timeout_ms":1000,"allow_other_sessions":false,"allow_other_profiles":false,"allow_children":false}`, true},
		{"transcripts no keys", "transcripts", `{}`, true},
		{"transcripts unknown", "transcripts", `{"nope":1}`, false},
		{"transcripts has no paths", "transcripts", `{"paths":` + paths + `}`, false},
		{"transcripts has no allow_path", "transcripts", `{"allow_path":true}`, false},
		{"transcripts timeout over six hours", "transcripts", `{"timeout_ms":21600001}`, false},
		{"transcripts zero max_entries", "transcripts", `{"max_entries":0}`, false},
		{"transcripts max_entries over cap", "transcripts", `{"max_entries":501}`, false},
		{"transcripts bad switch", "transcripts", `{"allow_children":"yes"}`, false},
		{"tool_smith unknown", "tool_smith", `{"nope":1}`, false},
		{"tool_smith env_file", "tool_smith", `{"env_file":` + string(envFileJSON) + `}`, true},
		{"tool_smith blank env_file", "tool_smith", `{"env_file":" "}`, false},
		{"tool_smith has no paths", "tool_smith", `{"paths":` + paths + `}`, false},
		{"tool_smith removed approval", "tool_smith", `{"approval":"ask"}`, false},
		{"tool_smith zero code max", "tool_smith", `{"code_max_bytes":0}`, false},
		{"tool_smith zero request max", "tool_smith", `{"nested_request_max_bytes":0}`, false},
		{"tool_smith request max too high", "tool_smith", `{"nested_request_max_bytes":10485761}`, false},
		{"tool_smith parallel too high", "tool_smith", `{"max_parallel_calls":65}`, false},
		{"tool_smith excluded tool", "tool_smith", `{"tools":["exec"]}`, false},
		{"tool_smith removed tool", "tool_smith", `{"tools":["read"]}`, false},
		{"tool_smith duplicate tool", "tool_smith", `{"tools":["lsp","lsp"]}`, false},
		{"tool_smith empty interpreter", "tool_smith", `{"interpreter":[]}`, false},
		{"tool_smith empty functions_dir", "tool_smith", `{"functions_dir":""}`, false},
		{"load_tool no keys", "load_tool", `{}`, true},
		{"load_tool every key", "load_tool", `{"lazy":["web_search","decide_systemone","mcp:docs:search","mcp:docs:*"],"lazy_summary_bytes":64,"lazy_load_max":3}`, true},
		{"load_tool empty lazy", "load_tool", `{"lazy":[]}`, true},
		{"load_tool unknown", "load_tool", `{"nope":1}`, false},
		{"load_tool lazy native tool", "load_tool", `{"lazy":["exec"]}`, false},
		{"load_tool lazy itself", "load_tool", `{"lazy":["load_tool"]}`, false},
		{"load_tool lazy duplicate", "load_tool", `{"lazy":["web_search","web_search"]}`, false},
		{"load_tool lazy bad entry", "load_tool", `{"lazy":["mcp:docs"]}`, false},
		{"load_tool lazy whitespace", "load_tool", `{"lazy":["mcp:docs:a b"]}`, false},
		{"load_tool lazy not a list", "load_tool", `{"lazy":"web_search"}`, false},
		{"load_tool summary at the minimum", "load_tool", `{"lazy_summary_bytes":1}`, true},
		{"load_tool summary at the maximum", "load_tool", `{"lazy_summary_bytes":4096}`, true},
		{"load_tool summary zero", "load_tool", `{"lazy_summary_bytes":0}`, false},
		{"load_tool summary over the maximum", "load_tool", `{"lazy_summary_bytes":4097}`, false},
		{"load_tool summary text", "load_tool", `{"lazy_summary_bytes":"256"}`, false},
		{"load_tool load max at the maximum", "load_tool", `{"lazy_load_max":100}`, true},
		{"load_tool load max zero", "load_tool", `{"lazy_load_max":0}`, false},
		{"load_tool load max over the maximum", "load_tool", `{"lazy_load_max":101}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config map[string]any
			if err := json.Unmarshal([]byte(tc.config), &config); err != nil {
				t.Fatal(err)
			}
			node := "native"
			if tc.tool == "decide_systemone" {
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
	decideSystemoneKeys := flattenSchemaKeys(extensionBlock["decide_systemone"].(map[string]any), defs)
	sort.Strings(decideSystemoneKeys)
	wantDecideSystemone := append([]string(nil), extensionDecideSystemoneKeys...)
	sort.Strings(wantDecideSystemone)
	if strings.Join(decideSystemoneKeys, ",") != strings.Join(wantDecideSystemone, ",") {
		t.Errorf("decide_systemone schema keys\n  %v\nloader accepts\n  %v", decideSystemoneKeys, wantDecideSystemone)
	}
	for tool, want := range map[string][]string{"exec": nativeExecKeys, "load_tool": nativeLoadToolKeys, "read_image": nativeReadImageKeys, "profile_message": nativeProfileMessageKeys, "tool_smith": nativeToolSmithKeys, "transcript": nativeTranscriptKeys, "transcripts": nativeTranscriptsKeys} {
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
