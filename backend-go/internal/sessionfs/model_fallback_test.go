package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestModelFallbackDefaultsAndValues(t *testing.T) {
	if got, err := normalizeModelFallback(nil, "s"); got != nil || err != nil {
		t.Fatalf("absent: %+v %v", got, err)
	}
	got, err := normalizeModelFallback(json.RawMessage(`{"models":["openai/large"]}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	want := &ModelFallbackSettings{Models: []ModelFallbackEntry{{ID: "openai/large"}}, OnStatus: []int{429, 500, 502, 503, 504, 529}, OnNetworkError: true, CooldownMS: 60_000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %+v", got)
	}
	got, err = normalizeModelFallback(json.RawMessage(`{"models":["openai/large@low"," anthropic/haiku "],"on_status":[503],"on_network_error":false,"cooldown_ms":0}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	want = &ModelFallbackSettings{Models: []ModelFallbackEntry{{ID: "openai/large@low"}, {ID: "anthropic/haiku"}}, OnStatus: []int{503}, CooldownMS: 0}
	if !reflect.DeepEqual(got, want) || got.Falls(429) || !got.Falls(503) {
		t.Fatalf("values = %+v", got)
	}
}

func TestModelFallbackRefusals(t *testing.T) {
	for name, test := range map[string]struct{ raw, want string }{
		"not an object":    {`[]`, "model_fallback must be an object"},
		"unknown key":      {`{"models":["a/b"],"retry":1}`, "retry"},
		"no models":        {`{"models":[]}`, "must list 1 to 8 model IDs"},
		"models missing":   {`{"cooldown_ms":1}`, "must list 1 to 8 model IDs"},
		"too many":         {`{"models":["a/1","a/2","a/3","a/4","a/5","a/6","a/7","a/8","a/9"]}`, "must list 1 to 8"},
		"a file path":      {`{"models":["models/x.json"]}`, "must be a model ID"},
		"blank":            {`{"models":[" "]}`, "must be a model ID"},
		"twice":            {`{"models":["a/b","a/b"]}`, "lists a/b twice"},
		"status not error": {`{"models":["a/b"],"on_status":[200]}`, "on_status[0] must be an HTTP error status"},
		"status not list":  {`{"models":["a/b"],"on_status":503}`, "on_status must be an array"},
		"cooldown range":   {`{"models":["a/b"],"cooldown_ms":-1}`, "cooldown_ms"},
		"network not bool": {`{"models":["a/b"],"on_network_error":"yes"}`, "on_network_error"},
	} {
		_, err := normalizeModelFallback(json.RawMessage(test.raw), "s")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// A session's model_fallback names models of the catalog by ID: the loader resolves each to
// its file, also one the profile does not list, and refuses an unknown one.
func TestModelFallbackResolvesCatalogModels(t *testing.T) {
	models := map[string]map[string]any{
		"openai/small": {"provider": "openai", "model": "small"},
		"openai/large": {"provider": "openai", "model": "large", "efforts": []string{"low", "high"}},
	}
	fixture := layouttest.Write(t, layouttest.Spec{
		Models: models, ProfileModels: []string{"openai/small"},
		Settings: map[string]any{"model_fallback": map[string]any{"models": []string{"openai/large@low"}}},
	})
	config, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.ModelFallback.Models[0]
	if entry.ModelPath != filepath.Join(fixture.ModelsDir, "openai", "large.json") || entry.ReasoningEffort != "low" {
		t.Fatalf("entry = %+v", entry)
	}

	for name, fallback := range map[string]string{"unknown model": "openai/missing", "unknown effort": "openai/large@max"} {
		fixture := layouttest.Write(t, layouttest.Spec{
			Models: models, ProfileModels: []string{"openai/small"},
			Settings: map[string]any{"model_fallback": map[string]any{"models": []string{fallback}}},
		})
		config, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		location, _ := Resolve(config.Roots, fixture.SessionID)
		if _, err := LoadSessionSettings(location, "", ""); err == nil || !strings.Contains(err.Error(), "model_fallback.models[0]") {
			t.Errorf("%s: error %v", name, err)
		}
	}
}
