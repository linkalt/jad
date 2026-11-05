package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPlanModeDefaults(t *testing.T) {
	config, err := loadSetting(t, map[string]any{})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if !reflect.DeepEqual(config.PlanMode, DefaultPlanModeSettings()) {
		t.Fatalf("default plan_mode = %+v", config.PlanMode)
	}
	if config.PlanMode.Role != "developer" || config.PlanMode.Instructions != DefaultPlanModeInstructions || len(config.PlanMode.AllowTools) != 0 {
		t.Fatalf("defaults = %+v", config.PlanMode)
	}
}

func TestPlanModeAcceptsEveryKey(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"plan_mode": map[string]any{
		"instructions": "Plan only.", "role": "system", "allow_tools": []string{"task", "mcp:docs:*"}, "execute_prompt": "Go.",
	}})
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	want := PlanModeSettings{Instructions: "Plan only.", Role: "system", AllowTools: []string{"task", "mcp:docs:*"}, ExecutePrompt: "Go."}
	if !reflect.DeepEqual(config.PlanMode, want) {
		t.Fatalf("plan_mode = %+v, want %+v", config.PlanMode, want)
	}
}

func TestPlanModeRejectsBadValues(t *testing.T) {
	cases := []struct {
		name     string
		planMode any
		message  string
	}{
		{"not an object", "on", "plan_mode must be an object"},
		{"misspelt key", map[string]any{"instruction": "x"}, "plan_mode.instruction is not supported; did you mean instructions?"},
		{"empty instructions", map[string]any{"instructions": "  "}, "plan_mode.instructions must be a non-empty string"},
		{"long instructions", map[string]any{"instructions": strings.Repeat("x", 64*1024+1)}, "plan_mode.instructions must be a non-empty string of at most 65536 bytes"},
		{"null instructions", map[string]any{"instructions": nil}, "plan_mode.instructions must be a non-empty string"},
		{"empty execute_prompt", map[string]any{"execute_prompt": ""}, "plan_mode.execute_prompt must be a non-empty string"},
		{"long execute_prompt", map[string]any{"execute_prompt": strings.Repeat("x", 64*1024+1)}, "plan_mode.execute_prompt must be a non-empty string of at most 65536 bytes"},
		{"user role", map[string]any{"role": "user"}, `plan_mode.role must be "developer" or "system"`},
		{"allow string", map[string]any{"allow_tools": "read"}, "plan_mode.allow_tools must be an array of tool names"},
		{"allow empty entry", map[string]any{"allow_tools": []any{""}}, "plan_mode.allow_tools[0] must be a non-empty string"},
		{"allow unknown tool", map[string]any{"allow_tools": []any{"rm"}}, `plan_mode.allow_tools[0] "rm" is not a native tool`},
		{"allow bad mcp", map[string]any{"allow_tools": []any{"mcp:docs"}}, `plan_mode.allow_tools[0] "mcp:docs" must be mcp:<server>:<tool> or mcp:<server>:*`},
		{"allow repeated", map[string]any{"allow_tools": []any{"task", "task"}}, `plan_mode.allow_tools[1] repeats "task"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSetting(t, map[string]any{"plan_mode": tc.planMode})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want one containing %q", err, tc.message)
			}
		})
	}
}

// The schema's defaults and limits are the loader's.
func TestPlanModeSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			PlanMode struct {
				Properties struct {
					Instructions struct {
						Default   string `json:"default"`
						MaxLength int    `json:"maxLength"`
					} `json:"instructions"`
					Role struct {
						Default string   `json:"default"`
						Enum    []string `json:"enum"`
					} `json:"role"`
					AllowTools    json.RawMessage `json:"allow_tools"`
					ExecutePrompt struct {
						Default   string `json:"default"`
						MaxLength int    `json:"maxLength"`
					} `json:"execute_prompt"`
				} `json:"properties"`
				AdditionalProperties bool `json:"additionalProperties"`
			} `json:"plan_mode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.PlanMode.Properties
	if properties.Instructions.Default != DefaultPlanModeInstructions || properties.Instructions.MaxLength != maxPlanModeInstructionBytes {
		t.Errorf("instructions schema = %+v", properties.Instructions)
	}
	if properties.Role.Default != defaultPlanModeRole || strings.Join(properties.Role.Enum, ",") != "developer,system" {
		t.Errorf("role schema = %+v", properties.Role)
	}
	if properties.ExecutePrompt.Default != DefaultPlanModeExecutePrompt || properties.ExecutePrompt.MaxLength != maxPlanModeInstructionBytes {
		t.Errorf("execute_prompt schema = %+v", properties.ExecutePrompt)
	}
	if len(properties.AllowTools) == 0 || schema.Properties.PlanMode.AdditionalProperties {
		t.Error("plan_mode schema has no allow_tools or allows other keys")
	}
}
