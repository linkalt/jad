package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func modelChoiceSpec(subagent, profileMessage map[string]any) layouttest.Spec {
	subagent["agents"] = map[string]any{"finder": map[string]any{"description": "Finds facts.", "tools": []string{"read"}}}
	return layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/default": {"model": "default-model"},
			"openai/other":   {"model": "other-model"},
		},
		Native: map[string]any{"read": map[string]any{}, "subagent": subagent, "profile_message": profileMessage},
		Profiles: map[string]map[string]any{
			"dev/coder":    {"models": []string{"openai/default", "openai/other"}, "model_default": "openai/default", "toolset": "coding/default"},
			"dev/reviewer": {"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": "coding/default"},
		},
	}
}

// model_choice is off by default on both tools: the loader then leaves the profile's models
// out of the subagent settings, so the tool offers no model argument.
func TestModelChoiceIsOffByDefaultAndTurnsOnPerTool(t *testing.T) {
	config, err := loadLayoutSpec(t, modelChoiceSpec(map[string]any{}, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeSubagent.ModelChoice || config.NativeSubagent.ProfileModels != nil {
		t.Fatalf("default subagent: model_choice %v, profile models %v", config.NativeSubagent.ModelChoice, config.NativeSubagent.ProfileModels)
	}
	if config.NativeProfileMessage.ModelChoice {
		t.Fatal("default profile_message has model_choice on")
	}

	config, err = loadLayoutSpec(t, modelChoiceSpec(map[string]any{"model_choice": true}, map[string]any{"model_choice": true}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.NativeSubagent.ModelChoice || len(config.NativeSubagent.ProfileModels) != 2 || config.NativeSubagent.ProfileModels["openai/other"] == "" {
		t.Fatalf("model_choice on: profile models %v", config.NativeSubagent.ProfileModels)
	}
	if !config.NativeProfileMessage.ModelChoice {
		t.Fatal("profile_message model_choice true was not read")
	}

	for name, spec := range map[string]layouttest.Spec{
		"subagent":        modelChoiceSpec(map[string]any{"model_choice": "yes"}, map[string]any{}),
		"profile_message": modelChoiceSpec(map[string]any{}, map[string]any{"model_choice": 1}),
	} {
		if _, err := loadLayoutSpec(t, spec); err == nil || !strings.Contains(err.Error(), "tools.native."+name+".model_choice must be a boolean") {
			t.Fatalf("%s: a non-boolean model_choice gave %v", name, err)
		}
	}
}
