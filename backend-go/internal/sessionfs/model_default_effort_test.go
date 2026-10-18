package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func defaultEffortSpec(modelDefault string) layouttest.Spec {
	spec := effortSpec()
	spec.Models["openai/other"] = map[string]any{
		"provider": "openai", "model": "other-test", "api_key_env": "FIXTURE_KEY",
		"efforts": []string{"low", "high"},
		"request": map[string]any{"reasoning": map[string]any{"effort": "low"}},
	}
	spec.Profiles = map[string]map[string]any{"dev/coder": {
		"models":        []string{"openai/gpt", "openai/plain", "openai/other"},
		"model_default": modelDefault,
	}}
	return spec
}

func TestProfileModelDefaultTakesAnEffort(t *testing.T) {
	location, _ := layoutLocation(t, defaultEffortSpec("openai/gpt@high"))
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.ReasoningEffort != "high" || config.ModelDefault != "openai/gpt@high" || !strings.HasSuffix(config.ModelPath, "gpt.json") {
		t.Fatalf("config = effort %q default %q path %q", config.ReasoningEffort, config.ModelDefault, config.ModelPath)
	}
	if got := requestEffort(t, config); got != "high" {
		t.Fatalf("request effort = %q, want high", got)
	}
	for _, id := range config.Models {
		if strings.Contains(id, "@") {
			t.Fatalf("models must stay plain: %v", config.Models)
		}
	}
}

func TestRunModelAgainstTheDefaultEffort(t *testing.T) {
	location, _ := layoutLocation(t, defaultEffortSpec("openai/gpt@high"))
	for _, test := range []struct{ model, wantEffort string }{
		{"", "high"},
		{"openai/gpt@low", "low"},
		{"openai/gpt", "high"},
		{"openai/other", "low"},
		{"openai/plain", ""},
	} {
		config, err := LoadSessionConfig(location, "", test.model)
		if err != nil {
			t.Fatalf("%q: %v", test.model, err)
		}
		if test.model == "openai/other" || test.model == "openai/plain" {
			if config.ReasoningEffort != "" {
				t.Errorf("%q: the default's effort leaked to another model: %q", test.model, config.ReasoningEffort)
			}
			if test.model == "openai/other" && requestEffort(t, config) != "low" {
				t.Errorf("%q: request effort = %q, want the file's low", test.model, requestEffort(t, config))
			}
			continue
		}
		if config.ReasoningEffort != test.wantEffort || requestEffort(t, config) != test.wantEffort {
			t.Errorf("%q: effort %q request %q, want %q", test.model, config.ReasoningEffort, requestEffort(t, config), test.wantEffort)
		}
	}
}

func TestProfileModelDefaultEffortIsRefused(t *testing.T) {
	for _, test := range []struct{ modelDefault, want string }{
		{"openai/gpt@max", "does not offer the reasoning effort"},
		{"openai/plain@high", "does not offer a reasoning effort choice"},
		{"openai/gpt@a b", "may only hold"},
		{"openai/gpt@", "must not be empty"},
		{"openai/missing@high", "openai/missing"},
	} {
		_, err := loadLayoutSpec(t, defaultEffortSpec(test.modelDefault))
		if err == nil || !strings.Contains(err.Error(), "model_default:") || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: err = %v, want model_default: ... %q", test.modelDefault, err, test.want)
		}
	}
}

func TestProfileModelDefaultWithoutEffortIsUnchanged(t *testing.T) {
	location, _ := layoutLocation(t, defaultEffortSpec("openai/gpt"))
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.ReasoningEffort != "" || config.ModelDefault != "openai/gpt" || requestEffort(t, config) != "medium" {
		t.Fatalf("effort %q default %q request %q", config.ReasoningEffort, config.ModelDefault, requestEffort(t, config))
	}
}

func TestSetLayoutModelDefaultPersistsTheEffort(t *testing.T) {
	location, fixture := layoutLocation(t, defaultEffortSpec("openai/gpt"))
	profileFile := filepath.Join(fixture.Root, "profiles", "dev", "coder.json")

	update, err := SetLayoutModelDefault(location, "dev/coder", "openai/gpt@high", "")
	if err != nil {
		t.Fatal(err)
	}
	if update.ModelDefault != "openai/gpt@high" || !update.Changed {
		t.Fatalf("update = %+v", update)
	}
	stored, err := os.ReadFile(profileFile)
	if err != nil || !strings.Contains(string(stored), `"model_default": "openai/gpt@high"`) {
		t.Fatalf("profile file: err=%v\n%s", err, stored)
	}
	again, err := SetLayoutModelDefault(location, "dev/coder", "openai/gpt@high", "")
	if err != nil || again.Changed {
		t.Fatalf("repeat = %+v, %v", again, err)
	}

	for _, model := range []string{"openai/gpt@max", "openai/plain@high", "openai/gpt@", "openai/missing@high"} {
		_, err := SetLayoutModelDefault(location, "dev/coder", model, "")
		var client *ClientError
		if !errors.As(err, &client) {
			t.Errorf("%q: err = %v, want a ClientError", model, err)
		}
	}
	if after, _ := os.ReadFile(profileFile); string(after) != string(stored) {
		t.Fatalf("a refused update changed the file:\n%s", after)
	}
}
