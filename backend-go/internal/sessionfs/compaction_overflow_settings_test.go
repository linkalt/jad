package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestCompactionOverflowAreOffByDefault(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"overflow":null}`} {
		settings, err := normalizeCompaction(json.RawMessage(raw), "setting.json")
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if settings.Overflow.Enabled || settings.Overflow.MaxAttempts != 1 || !reflect.DeepEqual(settings.Overflow.Patterns, defaultCompactionOverflowPatterns) || settings.Overflow.ToolOutputMaxBytes != 8192 {
			t.Fatalf("%q: overflow = %+v", raw, settings.Overflow)
		}
	}
}

func TestCompactionOverflowReadTheirValues(t *testing.T) {
	settings, err := normalizeCompaction(json.RawMessage(`{"overflow":{"enabled":true,"max_attempts":3,"patterns":["too big"],"tool_output_max_bytes":0}}`), "setting.json")
	if err != nil {
		t.Fatal(err)
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

func TestCompactionOverflowRefuseBadValues(t *testing.T) {
	for _, raw := range []string{
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
}

func TestCompactionOverflowSchemaMatchesTheLoader(t *testing.T) {
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
	overflow := DefaultCompactionOverflow()
	checks := map[string]map[string]any{
		"overflow": {"enabled": overflow.Enabled, "max_attempts": overflow.MaxAttempts, "patterns": overflow.Patterns, "tool_output_max_bytes": overflow.ToolOutputMaxBytes},
	}
	keys := map[string][]string{"overflow": compactionOverflowKeys}
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
	attempts := schema.Properties.Compaction.Properties["overflow"].Properties["max_attempts"]
	if attempts.Minimum == nil || *attempts.Minimum != 1 || attempts.Maximum == nil || *attempts.Maximum != maxCompactionOverflowMaxAttempts {
		t.Errorf("compaction.overflow.max_attempts schema bounds = %v..%v", attempts.Minimum, attempts.Maximum)
	}
	cut := schema.Properties.Compaction.Properties["overflow"].Properties["tool_output_max_bytes"]
	if cut.Minimum == nil || *cut.Minimum != 0 || cut.Maximum == nil || *cut.Maximum != maxCompactionOverflowToolOutputMaxBytes {
		t.Errorf("compaction.overflow.tool_output_max_bytes schema bounds = %v..%v", cut.Minimum, cut.Maximum)
	}
}
