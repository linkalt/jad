package sessionfs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// nativeDirectPathRefusals records the direct-path result of each native section normalizer
// for the probe inputs. It pins that a literal tools.native null is refused by every section,
// so a change to null handling shows up as an exact diff. Each line is "<section> native=<input> <outcome>".
const nativeDirectPathRefusals = `async native=string err: setting.json: tools.native must be a JSON object
async native=number err: setting.json: tools.native must be a JSON object
async native=array err: setting.json: tools.native must be a JSON object
async native=null err: setting.json: tools.native must be a JSON object
async native=missing nil
async native=tools-null err: setting.json: tools must be a JSON object
async native=valid-empty-object nil
bash native=string err: setting.json: tools.native must be a JSON object
bash native=number err: setting.json: tools.native must be a JSON object
bash native=array err: setting.json: tools.native must be a JSON object
bash native=null err: setting.json: tools.native must be a JSON object
bash native=missing nil
bash native=tools-null err: setting.json: tools must be a JSON object
bash native=valid-empty-object nil
exec native=string err: setting.json: tools.native must be a JSON object
exec native=number err: setting.json: tools.native must be a JSON object
exec native=array err: setting.json: tools.native must be a JSON object
exec native=null err: setting.json: tools.native must be a JSON object
exec native=missing nil
exec native=tools-null err: setting.json: tools must be a JSON object
exec native=valid-empty-object nil
fetch native=string err: setting.json: tools.native must be a JSON object
fetch native=number err: setting.json: tools.native must be a JSON object
fetch native=array err: setting.json: tools.native must be a JSON object
fetch native=null err: setting.json: tools.native must be a JSON object
fetch native=missing nil
fetch native=tools-null err: setting.json: tools must be a JSON object
fetch native=valid-empty-object nil
glob native=string err: setting.json: tools.native must be a JSON object
glob native=number err: setting.json: tools.native must be a JSON object
glob native=array err: setting.json: tools.native must be a JSON object
glob native=null err: setting.json: tools.native must be a JSON object
glob native=missing nil
glob native=tools-null err: setting.json: tools must be a JSON object
glob native=valid-empty-object nil
grep native=string err: setting.json: tools.native must be a JSON object
grep native=number err: setting.json: tools.native must be a JSON object
grep native=array err: setting.json: tools.native must be a JSON object
grep native=null err: setting.json: tools.native must be a JSON object
grep native=missing nil
grep native=tools-null err: setting.json: tools must be a JSON object
grep native=valid-empty-object nil
memory native=string err: setting.json: tools.native must be a JSON object
memory native=number err: setting.json: tools.native must be a JSON object
memory native=array err: setting.json: tools.native must be a JSON object
memory native=null err: setting.json: tools.native must be a JSON object
memory native=missing nil
memory native=tools-null err: setting.json: tools must be a JSON object
memory native=valid-empty-object nil
mutation native=string err: setting.json: tools.native must be a JSON object
mutation native=number err: setting.json: tools.native must be a JSON object
mutation native=array err: setting.json: tools.native must be a JSON object
mutation native=null err: setting.json: tools.native must be a JSON object
mutation native=missing nil
mutation native=tools-null err: setting.json: tools must be a JSON object
mutation native=valid-empty-object nil
process native=string err: setting.json: tools.native must be a JSON object
process native=number err: setting.json: tools.native must be a JSON object
process native=array err: setting.json: tools.native must be a JSON object
process native=null err: setting.json: tools.native must be a JSON object
process native=missing nil
process native=tools-null err: setting.json: tools must be a JSON object
process native=valid-empty-object nil
profile_message native=string err: setting.json: tools.native must be a JSON object
profile_message native=number err: setting.json: tools.native must be a JSON object
profile_message native=array err: setting.json: tools.native must be a JSON object
profile_message native=null err: setting.json: tools.native must be a JSON object
profile_message native=missing nil
profile_message native=tools-null err: setting.json: tools must be a JSON object
profile_message native=valid-empty-object nil
read_image native=string err: setting.json: tools.native must be a JSON object
read_image native=number err: setting.json: tools.native must be a JSON object
read_image native=array err: setting.json: tools.native must be a JSON object
read_image native=null err: setting.json: tools.native must be a JSON object
read_image native=missing nil
read_image native=tools-null err: setting.json: tools must be a JSON object
read_image native=valid-empty-object nil
request_draft native=string err: setting.json: tools.native must be a JSON object
request_draft native=number err: setting.json: tools.native must be a JSON object
request_draft native=array err: setting.json: tools.native must be a JSON object
request_draft native=null err: setting.json: tools.native must be a JSON object
request_draft native=missing nil
request_draft native=tools-null err: setting.json: tools must be a JSON object
request_draft native=valid-empty-object nil
skill native=string err: setting.json: tools.native must be a JSON object
skill native=number err: setting.json: tools.native must be a JSON object
skill native=array err: setting.json: tools.native must be a JSON object
skill native=null err: setting.json: tools.native must be a JSON object
skill native=missing nil
skill native=tools-null err: setting.json: tools must be a JSON object
skill native=valid-empty-object nil
subagent native=string err: setting.json: tools.native must be a JSON object
subagent native=number err: setting.json: tools.native must be a JSON object
subagent native=array err: setting.json: tools.native must be a JSON object
subagent native=null err: setting.json: tools.native must be a JSON object
subagent native=missing nil
subagent native=tools-null err: setting.json: tools must be a JSON object
subagent native=valid-empty-object nil
task native=string err: setting.json: tools.native must be a JSON object
task native=number err: setting.json: tools.native must be a JSON object
task native=array err: setting.json: tools.native must be a JSON object
task native=null err: setting.json: tools.native must be a JSON object
task native=missing nil
task native=tools-null err: setting.json: tools must be a JSON object
task native=valid-empty-object nil
tool_smith native=string err: setting.json: tools.native must be a JSON object
tool_smith native=number err: setting.json: tools.native must be a JSON object
tool_smith native=array err: setting.json: tools.native must be a JSON object
tool_smith native=null err: setting.json: tools.native must be a JSON object
tool_smith native=missing nil
tool_smith native=tools-null err: setting.json: tools must be a JSON object
tool_smith native=valid-empty-object nil
transcript native=string err: setting.json: tools.native must be a JSON object
transcript native=number err: setting.json: tools.native must be a JSON object
transcript native=array err: setting.json: tools.native must be a JSON object
transcript native=null err: setting.json: tools.native must be a JSON object
transcript native=missing nil
transcript native=tools-null err: setting.json: tools must be a JSON object
transcript native=valid-empty-object nil
transcripts native=string err: setting.json: tools.native must be a JSON object
transcripts native=number err: setting.json: tools.native must be a JSON object
transcripts native=array err: setting.json: tools.native must be a JSON object
transcripts native=null err: setting.json: tools.native must be a JSON object
transcripts native=missing nil
transcripts native=tools-null err: setting.json: tools must be a JSON object
transcripts native=valid-empty-object nil
read_settings native=string err: setting.json: tools.native must be a JSON object
read_settings native=number err: setting.json: tools.native must be a JSON object
read_settings native=array err: setting.json: tools.native must be a JSON object
read_settings native=null err: setting.json: tools.native must be a JSON object
read_settings native=missing nil
read_settings native=tools-null err: setting.json: tools must be a JSON object
read_settings native=valid-empty-object nil
read_policy native=string err: setting.json: tools.native must be a JSON object
read_policy native=number err: setting.json: tools.native must be a JSON object
read_policy native=array err: setting.json: tools.native must be a JSON object
read_policy native=null err: setting.json: tools.native must be a JSON object
read_policy native=missing non-nil
read_policy native=tools-null err: setting.json: tools must be a JSON object
read_policy native=valid-empty-object non-nil`

func TestNativeDirectPathRefusesNullNative(t *testing.T) {
	const settingPath = "setting.json"
	inputs := []struct {
		name    string
		setting string
	}{
		{"string", `{"tools":{"native":"x"}}`},
		{"number", `{"tools":{"native":1}}`},
		{"array", `{"tools":{"native":[]}}`},
		{"null", `{"tools":{"native":null}}`},
		{"missing", `{"tools":{}}`},
		{"tools-null", `{"tools":null}`},
		{"valid-empty-object", `{"tools":{"native":{}}}`},
	}
	sections := []struct {
		name string
		run  func(setting map[string]json.RawMessage) (bool, error)
	}{
		{"async", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeAsyncSettings(s, settingPath)
			return v != nil, err
		}},
		{"bash", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeBashSettings(s, "/tmp/jad", settingPath, "/tmp/cwd", "/tmp/session", "/tmp/tools", "", NativePathPolicy{}, NativePathPolicy{})
			return v != nil, err
		}},
		{"exec", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeExecSettings(s, "/tmp/jad", settingPath, "/tmp/cwd", "/tmp/session", "/tmp/tools", "", NativePathPolicy{}, NativePathPolicy{})
			return v != nil, err
		}},
		{"fetch", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeFetchSettings(s, settingPath, "/tmp/cwd", "/tmp/tools")
			return v != nil, err
		}},
		{"glob", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeGlobSettings(s, "/tmp/jad", settingPath)
			return v != nil, err
		}},
		{"grep", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeGrepSettings(s, "/tmp/jad", settingPath)
			return v != nil, err
		}},
		{"memory", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeMemorySettings(s, settingPath, "/tmp/cwd", "/tmp/tools")
			return v != nil, err
		}},
		{"mutation", func(s map[string]json.RawMessage) (bool, error) {
			w, e, err := normalizeNativeWriteEditSettings(s, "/tmp/jad", settingPath, "/tmp/cwd", "/tmp/session", "/tmp/tools")
			return w != nil && e != nil, err
		}},
		{"process", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeProcessSettings(s, "/tmp/jad", settingPath, "/tmp/cwd", "/tmp/session", "/tmp/tools", "")
			return v != nil, err
		}},
		{"profile_message", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeProfileMessageSettings(s, settingPath)
			return v != nil, err
		}},
		{"read_image", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeReadImageSettings(s, "/tmp/jad", settingPath)
			return v != nil, err
		}},
		{"request_draft", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeRequestDraftSettings(s, settingPath)
			return v != nil, err
		}},
		{"skill", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeSkillSettings(s, settingPath)
			return v != nil, err
		}},
		{"subagent", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeSubagentSettings(s, settingPath, "/tmp/cwd", "/tmp/tools")
			return v != nil, err
		}},
		{"task", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeTaskSettings(s, settingPath, "/tmp/cwd", "/tmp/tools")
			return v != nil, err
		}},
		{"tool_smith", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeToolSmithSettings(s, "/tmp/jad", settingPath, "/tmp/cwd", "/tmp/session", "/tmp/tools", "", NativePathPolicy{}, NativePathPolicy{})
			return v != nil, err
		}},
		{"transcript", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeTranscriptSettings(s, settingPath, NativePathPolicy{})
			return v != nil, err
		}},
		{"transcripts", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeTranscriptsSettings(s, settingPath, NativePathPolicy{})
			return v != nil, err
		}},
		{"read_settings", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeReadSettings(s, "/tmp/jad", settingPath)
			return v != nil, err
		}},
		{"read_policy", func(s map[string]json.RawMessage) (bool, error) {
			v, err := normalizeNativeReadPolicy(s, "/tmp/jad", settingPath)
			return !reflect.ValueOf(v).IsZero(), err
		}},
	}

	var got []string
	for _, sec := range sections {
		for _, in := range inputs {
			var setting map[string]json.RawMessage
			if err := json.Unmarshal([]byte(in.setting), &setting); err != nil {
				t.Fatalf("unmarshal %s: %v", in.name, err)
			}
			ok, err := sec.run(setting)
			outcome := "nil"
			switch {
			case err != nil:
				outcome = "err: " + err.Error()
			case ok:
				outcome = "non-nil"
			}
			got = append(got, fmt.Sprintf("%s native=%s %s", sec.name, in.name, outcome))
		}
	}
	gotText := strings.Join(got, "\n")
	if gotText != nativeDirectPathRefusals {
		t.Fatalf("direct-path refusal table mismatch\n--- got ---\n%s", gotText)
	}
}
