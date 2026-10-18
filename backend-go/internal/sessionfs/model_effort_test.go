package sessionfs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func effortSpec() layouttest.Spec {
	return layouttest.Spec{Models: map[string]map[string]any{
		"openai/gpt": {
			"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY",
			"efforts": []string{"low", "high"},
			"request": map[string]any{"reasoning": map[string]any{"effort": "medium", "summary": "auto"}, "parallel_tool_calls": true},
		},
		"openai/plain": {"provider": "openai", "model": "plain-test", "api_key_env": "FIXTURE_KEY"},
	}}
}

func requestEffort(t *testing.T, config SessionConfig) string {
	t.Helper()
	var request struct {
		Reasoning map[string]any `json:"reasoning"`
	}
	if err := json.Unmarshal(config.Model.Request, &request); err != nil {
		t.Fatal(err)
	}
	effort, _ := request.Reasoning["effort"].(string)
	return effort
}

func TestSplitModelEffort(t *testing.T) {
	for _, test := range []struct{ in, id, effort string }{
		{"jad/sonet", "jad/sonet", ""},
		{"jad/sonet@xhigh", "jad/sonet", "xhigh"},
		{"jad/sonet@x.1_a-b", "jad/sonet", "x.1_a-b"},
	} {
		id, effort, err := SplitModelEffort(test.in)
		if err != nil || id != test.id || effort != test.effort {
			t.Errorf("SplitModelEffort(%q) = %q, %q, %v", test.in, id, effort, err)
		}
	}
	for _, bad := range []string{"jad/sonet@", "jad/sonet@a b", "jad/sonet@a@b", "jad/sonet@../x"} {
		if _, _, err := SplitModelEffort(bad); err == nil {
			t.Errorf("SplitModelEffort(%q) accepted", bad)
		}
	}
}

func TestRequestedModelEffortOverridesRequest(t *testing.T) {
	location, _ := layoutLocation(t, effortSpec())
	config, err := LoadSessionConfig(location, "", "openai/gpt@high")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestEffort(t, config); got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}
	if config.Model.Effort != "high" || !strings.Contains(string(config.Model.Request), `"parallel_tool_calls":true`) || !strings.Contains(string(config.Model.Request), `"summary":"auto"`) {
		t.Fatalf("request lost its other fields: %s", config.Model.Request)
	}
	plain, err := LoadSessionConfig(location, "", "openai/gpt")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestEffort(t, plain); got != "medium" {
		t.Fatalf("without @effort the file's own effort must stay, got %q", got)
	}
	if len(plain.Model.Efforts) != 2 || plain.Model.Efforts[1] != "high" {
		t.Fatalf("efforts = %v", plain.Model.Efforts)
	}
}

func TestRequestedModelEffortIsRefusedWhenNotOffered(t *testing.T) {
	location, _ := layoutLocation(t, effortSpec())
	for _, test := range []struct{ model, want string }{
		{"openai/gpt@max", "does not offer the reasoning effort"},
		{"openai/plain@high", "does not offer a reasoning effort choice"},
		{"openai/gpt@", "must not be empty"},
		{"@high", "must not be empty"},
		{"openai/missing@high", "not available"},
	} {
		_, err := LoadSessionConfig(location, "", test.model)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: err = %v, want %q", test.model, err, test.want)
		}
		var client *ClientError
		if !errors.As(err, &client) {
			t.Fatalf("%s: %T is not a ClientError", test.model, err)
		}
	}
}

func TestModelFileEffortsAreValidated(t *testing.T) {
	for name, efforts := range map[string]any{"not an array": "high", "bad name": []string{"a b"}, "empty": []string{""}, "duplicate": []string{"high", "high"}} {
		spec := effortSpec()
		spec.Models["openai/gpt"]["efforts"] = efforts
		if _, err := loadLayoutConfig(t, spec); err == nil || !strings.Contains(err.Error(), "efforts") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSubagentModelCarriesEffort(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	setAgentModel(t, location, "cheap/haiku@low")
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	agent := subagentX(t, config)
	if agent.ReasoningEffort != "low" || !strings.HasSuffix(agent.ModelPath, "haiku.json") {
		t.Fatalf("agent = %+v", agent)
	}
	setAgentModel(t, location, "cheap/haiku@")
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "agent x") {
		t.Fatalf("empty effort accepted: %v", err)
	}
}

func TestSubagentModelFilePathCannotTakeEffort(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	setAgentModel(t, location, "own.json@high")
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "model file path cannot take") {
		t.Fatalf("err = %v", err)
	}
}

func TestEffortIsCheckedWhenTheSettingsLoad(t *testing.T) {
	location, _ := layoutLocation(t, effortSpec())
	_, err := LoadSessionSettings(location, "", "openai/gpt@max")
	var client *ClientError
	if !errors.As(err, &client) {
		t.Fatalf("settings load did not refuse an unoffered effort: %v", err)
	}
	if _, err := LoadSessionSettings(location, "", "openai/gpt@high"); err != nil {
		t.Fatal(err)
	}
}

func TestWithEffortReasoningShapes(t *testing.T) {
	for name, request := range map[string]string{"absent": `{}`, "null": `{"reasoning":null}`} {
		got, err := Model{Name: "m", Efforts: []string{"high"}, Request: json.RawMessage(request)}.withEffort("high")
		if err != nil || !strings.Contains(string(got.Request), `"effort":"high"`) {
			t.Errorf("%s: %s %v", name, got.Request, err)
		}
	}
	for _, request := range []string{`{"reasoning":"x"}`, `{"reasoning":[1]}`} {
		if _, err := (Model{Name: "m", Efforts: []string{"high"}, Request: json.RawMessage(request)}).withEffort("high"); err == nil {
			t.Errorf("%s accepted", request)
		}
	}
}
