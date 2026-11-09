package sessionfs

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func sandboxChoiceSpec(subagent, profileMessage map[string]any) layouttest.Spec {
	subagent["agents"] = map[string]any{"finder": map[string]any{"description": "Finds facts.", "tools": []string{"read_image"}}}
	spec := modelChoiceSpec(subagent, profileMessage)
	spec.SandboxTemplates = map[string]map[string]any{
		"reader": envSandbox(map[string]any{"description": "Reader:\n  writes nothing but scratch", "write": map[string]any{"paths": []string{"{tmp}"}, "plan_paths": []string{"{tmp}"}}}),
		"off":    {"enabled": false, "isolation": map[string]any{"mode": "worktree", "base": "working"}},
	}
	return spec
}

// sandbox_choice is off by default; set, the loader resolves each name to its template with the
// template's description on one line and the session's configuration read-only.
func TestSandboxChoiceResolvesTheTemplates(t *testing.T) {
	config, err := loadLayoutSpec(t, sandboxChoiceSpec(map[string]any{}, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeSubagent.SandboxChoice != nil || config.NativeSubagent.SandboxChoices != nil || config.NativeProfileMessage.SandboxChoices != nil {
		t.Fatal("sandbox_choice is on by default")
	}
	config, err = loadLayoutSpec(t, sandboxChoiceSpec(map[string]any{"sandbox_choice": []string{"reader", "off"}}, map[string]any{"sandbox_choice": []string{"off"}}))
	if err != nil {
		t.Fatal(err)
	}
	choices := config.NativeSubagent.SandboxChoices
	if !reflect.DeepEqual(SandboxChoiceNames(choices), []string{"reader", "off"}) {
		t.Fatalf("choices = %+v", choices)
	}
	reader := choices[0]
	if reader.Description != "Reader: writes nothing but scratch" || reader.Sandbox == nil || reader.Sandbox.Template != "reader" || !reflect.DeepEqual(reader.Sandbox.WritablePaths, []string{"{tmp}"}) {
		t.Fatalf("reader = %+v %+v", reader, reader.Sandbox)
	}
	if len(reader.Sandbox.ReadOnlyPaths) == 0 || !slices.Contains(reader.Sandbox.ReadOnlyPaths, config.SettingPath) {
		t.Fatalf("reader read-only paths = %v (session file %s)", reader.Sandbox.ReadOnlyPaths, config.SettingPath)
	}
	off := choices[1]
	if off.Sandbox != nil || off.Isolation.Mode != SandboxIsolationWorktree || off.Isolation.Base != SandboxIsolationBaseWorking {
		t.Fatalf("off = %+v", off)
	}
	if got := SandboxChoiceNames(config.NativeProfileMessage.SandboxChoices); !reflect.DeepEqual(got, []string{"off"}) {
		t.Fatalf("profile_message choices = %v", got)
	}
}

func TestSandboxChoiceRefusals(t *testing.T) {
	for name, test := range map[string]struct {
		value any
		want  string
	}{
		"not a list":     {"reader", "sandbox_choice must be an array of sandbox template names"},
		"empty":          {[]string{}, "sandbox_choice is empty"},
		"duplicate":      {[]string{"reader", "reader"}, `lists "reader" more than once`},
		"unknown":        {[]string{"readr"}, "did you mean reader?"},
		"sub-folder":     {[]string{"team/reader"}, "a sub-folder is not allowed"},
		"bad identifier": {[]string{"-reader"}, "must start with a letter or digit"},
	} {
		for _, tool := range []string{"subagent", "profile_message"} {
			t.Run(name+" "+tool, func(t *testing.T) {
				subagent, profileMessage := map[string]any{}, map[string]any{}
				if tool == "subagent" {
					subagent["sandbox_choice"] = test.value
				} else {
					profileMessage["sandbox_choice"] = test.value
				}
				_, err := loadLayoutSpec(t, sandboxChoiceSpec(subagent, profileMessage))
				if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "tools.native."+tool) {
					t.Fatalf("err = %v, want %q", err, test.want)
				}
			})
		}
	}
	// null is off, like absent.
	config, err := loadLayoutSpec(t, sandboxChoiceSpec(map[string]any{"sandbox_choice": nil}, map[string]any{}))
	if err != nil || config.NativeSubagent.SandboxChoice != nil {
		t.Fatalf("null: %v %v", err, config.NativeSubagent.SandboxChoice)
	}
}
