package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCompactionSummaryAndOverflowAreOffByDefault(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"summary":null,"overflow":null}`} {
		settings, err := normalizeCompaction(json.RawMessage(raw), "setting.json")
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if settings.Summary.Enabled || !reflect.DeepEqual(settings.Summary, DefaultCompactionSummary()) {
			t.Fatalf("%q: summary = %+v, want the defaults with enabled false", raw, settings.Summary)
		}
		if settings.Overflow.Enabled || settings.Overflow.MaxAttempts != 1 || !reflect.DeepEqual(settings.Overflow.Patterns, defaultCompactionOverflowPatterns) || settings.Overflow.ToolOutputMaxBytes != 8192 {
			t.Fatalf("%q: overflow = %+v", raw, settings.Overflow)
		}
	}
}

func TestCompactionSummaryAndOverflowReadTheirValues(t *testing.T) {
	settings, err := normalizeCompaction(json.RawMessage(`{"summary":{"enabled":true,"prompt":"P","header":"H","role":"user","max_input_bytes":5000,"max_item_bytes":200,"max_output_tokens":0,"timeout_ms":3000,"on_failure":"skip"},"overflow":{"enabled":true,"max_attempts":3,"patterns":["too big"],"tool_output_max_bytes":0}}`), "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	want := CompactionSummary{Enabled: true, Prompt: "P", Header: "H", Role: "user", MaxInputBytes: 5000, MaxItemBytes: 200, MaxOutputTokens: 0, TimeoutMS: 3000, OnFailure: "skip"}
	if settings.Summary != want {
		t.Fatalf("summary = %+v, want %+v", settings.Summary, want)
	}
	if !settings.Overflow.Enabled || settings.Overflow.MaxAttempts != 3 || !reflect.DeepEqual(settings.Overflow.Patterns, []string{"too big"}) || settings.Overflow.ToolOutputMaxBytes != 0 {
		t.Fatalf("overflow = %+v", settings.Overflow)
	}
	for raw, want := range map[string]int64{`{"overflow":{"tool_output_max_bytes":256}}`: 256, `{"overflow":{"tool_output_max_bytes":1000000}}`: 1_000_000} {
		if settings, err := normalizeCompaction(json.RawMessage(raw), "setting.json"); err != nil || settings.Overflow.ToolOutputMaxBytes != want {
			t.Fatalf("%s: %+v, %v", raw, settings.Overflow, err)
		}
	}
}

func TestCompactionSummaryAndOverflowRefuseBadValues(t *testing.T) {
	for _, raw := range []string{
		`{"summary":[]}`,
		`{"summary":{"enabled":"yes"}}`,
		`{"summary":{"prompt":""}}`,
		`{"summary":{"header":"  "}}`,
		`{"summary":{"role":"system"}}`,
		`{"summary":{"max_input_bytes":999}}`,
		`{"summary":{"max_item_bytes":99}}`,
		`{"summary":{"max_output_tokens":-1}}`,
		`{"summary":{"timeout_ms":999}}`,
		`{"summary":{"on_failure":"ignore"}}`,
		`{"summary":{"model":"x"}}`,
		`{"overflow":true}`,
		`{"overflow":{"enabled":1}}`,
		`{"overflow":{"max_attempts":0}}`,
		`{"overflow":{"max_attempts":6}}`,
		`{"overflow":{"patterns":[]}}`,
		`{"overflow":{"patterns":[""]}}`,
		`{"overflow":{"patterns":"context"}}`,
		`{"overflow":{"retries":1}}`,
		`{"overflow":{"tool_output_max_bytes":255}}`,
		`{"overflow":{"tool_output_max_bytes":1000001}}`,
		`{"overflow":{"tool_output_max_bytes":-1}}`,
		`{"overflow":{"tool_output_max_bytes":"8k"}}`,
	} {
		if _, err := normalizeCompaction(json.RawMessage(raw), "setting.json"); err == nil {
			t.Errorf("normalizeCompaction(%s) succeeded, want a config error", raw)
		}
	}
	_, err := normalizeCompaction(json.RawMessage(`{"summary":{"model":"x"}}`), "setting.json")
	if err == nil || !strings.Contains(err.Error(), "compaction.summary") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("unknown key error = %v, want it to name compaction.summary and the key", err)
	}
}

func TestCompactionSummaryAndOverflowSchemaMatchTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	type property struct {
		Type    string          `json:"type"`
		Default json.RawMessage `json:"default"`
		Minimum *int64          `json:"minimum"`
		Maximum *int64          `json:"maximum"`
		Enum    []string        `json:"enum"`
	}
	var schema struct {
		Properties struct {
			Compaction struct {
				Properties map[string]struct {
					Properties           map[string]property `json:"properties"`
					AdditionalProperties *bool               `json:"additionalProperties"`
				} `json:"properties"`
			} `json:"compaction"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	summary, overflow := DefaultCompactionSummary(), DefaultCompactionOverflow()
	checks := map[string]map[string]any{
		"summary": {
			"enabled": false, "prompt": summary.Prompt, "header": summary.Header, "role": summary.Role,
			"max_input_bytes": summary.MaxInputBytes, "max_item_bytes": summary.MaxItemBytes,
			"max_output_tokens": summary.MaxOutputTokens, "timeout_ms": summary.TimeoutMS, "on_failure": summary.OnFailure,
		},
		"overflow": {"enabled": overflow.Enabled, "max_attempts": overflow.MaxAttempts, "patterns": overflow.Patterns, "tool_output_max_bytes": overflow.ToolOutputMaxBytes},
	}
	keys := map[string][]string{"summary": compactionSummaryKeys, "overflow": compactionOverflowKeys}
	for section, defaults := range checks {
		object, ok := schema.Properties.Compaction.Properties[section]
		if !ok {
			t.Fatalf("schema has no compaction.%s", section)
		}
		if object.AdditionalProperties == nil || *object.AdditionalProperties {
			t.Errorf("compaction.%s must refuse unknown keys in the schema, as the loader does", section)
		}
		if len(object.Properties) != len(keys[section]) {
			t.Errorf("compaction.%s schema keys = %d, loader keys = %v", section, len(object.Properties), keys[section])
		}
		for key, want := range defaults {
			field, ok := object.Properties[key]
			if !ok {
				t.Errorf("schema has no compaction.%s.%s", section, key)
				continue
			}
			encoded, _ := json.Marshal(want)
			var got, expected any
			_ = json.Unmarshal(field.Default, &got)
			_ = json.Unmarshal(encoded, &expected)
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("compaction.%s.%s schema default = %s, loader default = %s", section, key, field.Default, encoded)
			}
		}
	}
	bounds := map[string][2]int64{
		"max_input_bytes": {1000, maxCompactionSummaryMaxInputBytes}, "max_item_bytes": {100, maxCompactionSummaryMaxItemBytes},
		"max_output_tokens": {0, maxCompactionSummaryMaxOutputTokens}, "timeout_ms": {1000, maxCompactionSummaryTimeoutMS},
	}
	for key, bound := range bounds {
		field := schema.Properties.Compaction.Properties["summary"].Properties[key]
		if field.Minimum == nil || field.Maximum == nil || *field.Minimum != bound[0] || *field.Maximum != bound[1] {
			t.Errorf("compaction.summary.%s schema bounds = %v..%v, loader %v", key, field.Minimum, field.Maximum, bound)
		}
	}
	attempts := schema.Properties.Compaction.Properties["overflow"].Properties["max_attempts"]
	if attempts.Minimum == nil || *attempts.Minimum != 1 || attempts.Maximum == nil || *attempts.Maximum != maxCompactionOverflowMaxAttempts {
		t.Errorf("compaction.overflow.max_attempts schema bounds = %v..%v", attempts.Minimum, attempts.Maximum)
	}
	cut := schema.Properties.Compaction.Properties["overflow"].Properties["tool_output_max_bytes"]
	if cut.Minimum == nil || *cut.Minimum != 0 || cut.Maximum == nil || *cut.Maximum != maxCompactionOverflowToolOutputMaxBytes {
		t.Errorf("compaction.overflow.tool_output_max_bytes schema bounds = %v..%v", cut.Minimum, cut.Maximum)
	}
}
