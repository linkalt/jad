package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

const hooksToolsetText = `{"native":{"read_image":{}},"hooks":{"enabled":true,"timeout_ms":5000,"post_tool":[{"name":"audit","argv":["true"]}]}}`

func loadHooksToolset(t *testing.T, spec layouttest.Spec) (SessionConfig, error) {
	t.Helper()
	return loadToolsetSession(t, spec, nil)
}

func TestHooksComeFromTheToolset(t *testing.T) {
	config, err := loadHooksToolset(t, layouttest.Spec{ToolsetRaw: hooksToolsetText})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Hooks.Enabled || config.Hooks.TimeoutMS != 5000 || len(config.Hooks.PostTool) != 1 || config.Hooks.PostTool[0].Name != "audit" {
		t.Fatalf("hooks = %+v", config.Hooks)
	}
}

func TestHooksInToolsetAndSettingsAreAnError(t *testing.T) {
	spec := layouttest.Spec{ToolsetRaw: hooksToolsetText, Settings: map[string]any{"hooks": map[string]any{"enabled": false}}}
	_, err := loadHooksToolset(t, spec)
	if err == nil || !strings.Contains(err.Error(), "hooks is set here and in the settings") {
		t.Fatalf("err = %v, want the both-places error", err)
	}
}

func TestToolsetHooksAreValidatedLikeTheSettingsKey(t *testing.T) {
	_, err := loadHooksToolset(t, layouttest.Spec{ToolsetRaw: `{"native":{"read_image":{}},"hooks":{"timeout_ms":1}}`})
	if err == nil || !strings.Contains(err.Error(), "hooks.timeout_ms") {
		t.Fatalf("err = %v, want a hooks.timeout_ms error", err)
	}
	_, err = loadHooksToolset(t, layouttest.Spec{ToolsetRaw: `{"native":{"read_image":{}},"hooks":{"nope":1}}`})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want an unknown-key error", err)
	}
}

func TestToolsetWithoutHooksKeepsTheSettingsKey(t *testing.T) {
	spec := layouttest.Spec{Native: map[string]any{"read_image": map[string]any{}}, Settings: map[string]any{"hooks": map[string]any{"enabled": true, "timeout_ms": 7000}}}
	config, err := loadHooksToolset(t, spec)
	if err != nil {
		t.Fatal(err)
	}
	if config.Hooks.TimeoutMS != 7000 {
		t.Fatalf("hooks = %+v", config.Hooks)
	}
}

// A sub-agent's toolset is composed on its own, so its hooks belong to that child only.
func TestSubagentToolsetCarriesItsOwnHooks(t *testing.T) {
	spec := toolsetSpec(map[string]any{"toolset": "extra/search"})
	config, err := loadToolsetSession(t, spec, map[string]string{"extra/search": hooksToolsetText})
	if err != nil {
		t.Fatal(err)
	}
	derived := config.AgentToolsets["extra/search"]
	if len(derived.Hooks.PostTool) != 1 {
		t.Fatalf("child hooks = %+v", derived.Hooks)
	}
	if config.Hooks.ToolEventsActive() {
		t.Fatalf("the session's own hooks = %+v, want none", config.Hooks)
	}
}

func TestSubagentToolsetHooksClashWithSessionHooks(t *testing.T) {
	spec := toolsetSpec(map[string]any{"toolset": "extra/search"})
	spec.Settings = map[string]any{"hooks": map[string]any{"enabled": true}}
	_, err := loadToolsetSession(t, spec, map[string]string{"extra/search": hooksToolsetText})
	if err == nil || !strings.Contains(err.Error(), "hooks is set here and in the settings") {
		t.Fatalf("err = %v, want the both-places error", err)
	}
}
