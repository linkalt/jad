package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// childConfig is the configuration base of an ephemeral run through phase R5 (DECISIONS, unified
// agent runs R0 and R3): the calling session's whole SessionConfig, with only the agent's model
// file, its reasoning effort and its prompt text swapped in. The design's section 3.2 order (root,
// session, template) replaces it in R6; until then these tests pin that nothing else changes.
func TestChildConfigCopiesTheCallersConfigAndSwapsOnlyModelEffortAndPrompt(t *testing.T) {
	dir := t.TempDir()
	modelFile := func(name, model string) string {
		path := filepath.Join(dir, name)
		text := `{"provider":"openai","model":"` + model + `","efforts":["low","high"],"request":{"reasoning":{"effort":"low"}}}`
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	parent := sessionfs.SessionConfig{
		ModelPath:            modelFile("parent.json", "parent-model"),
		ReasoningEffort:      "high",
		MessagesPath:         filepath.Join(dir, "messages.jsonl"),
		MaxParallelToolCalls: 3,
		Profile:              "dev/coder",
		Warnings:             []string{"kept"},
	}
	parent.Model.PromptText = "the parent's prompt"
	parent, err := parent.WithModel()
	if err != nil {
		t.Fatal(err)
	}
	childModel := modelFile("child.json", "child-model")

	for name, test := range map[string]struct {
		agent      sessionfs.NativeSubagentAgent
		wantModel  string
		wantPath   string
		wantEffort string
	}{
		// No model of its own: the caller's model file and effort.
		"inherits the model": {sessionfs.NativeSubagentAgent{Name: "a", PromptText: "child prompt"}, "parent-model", parent.ModelPath, "high"},
		// A model of its own takes the agent's effort, also an empty one, never the caller's.
		"own model, no effort":  {sessionfs.NativeSubagentAgent{Name: "b", ModelPath: childModel}, "child-model", childModel, ""},
		"own model, own effort": {sessionfs.NativeSubagentAgent{Name: "c", ModelPath: childModel, ReasoningEffort: "low", PromptText: "p"}, "child-model", childModel, "low"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := childConfig(RunTemplate{Base: parent, Agent: test.agent})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model.Name != test.wantModel || got.ModelPath != test.wantPath || got.ReasoningEffort != test.wantEffort {
				t.Fatalf("model %q path %q effort %q, want %q %q %q", got.Model.Name, got.ModelPath, got.ReasoningEffort, test.wantModel, test.wantPath, test.wantEffort)
			}
			if got.Model.PromptText != test.agent.PromptText {
				t.Fatalf("prompt = %q, want the agent's %q (never the caller's)", got.Model.PromptText, test.agent.PromptText)
			}
			// Every other field is the caller's, as it is.
			want := parent
			want.ModelPath, want.ReasoningEffort, want.Model = got.ModelPath, got.ReasoningEffort, got.Model
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("child config differs from the caller's beyond model, effort and prompt:\n got %+v\nwant %+v", got, want)
			}
			if parent.Model.PromptText != "the parent's prompt" || parent.ModelPath == childModel {
				t.Fatal("childConfig changed the caller's config")
			}
		})
	}
}

func TestChildConfigNamesTheAgentWhenItsModelCannotLoad(t *testing.T) {
	_, err := childConfig(RunTemplate{Agent: sessionfs.NativeSubagentAgent{Name: "broken", ModelPath: filepath.Join(t.TempDir(), "missing.json")}})
	if err == nil || !strings.Contains(err.Error(), "load the model of agent broken") {
		t.Fatalf("error = %v", err)
	}
}

// An agent that names one of the profile's own model files by its path runs it as a whole
// model, so it fails: the own model is never composed for a sub-agent (decider 3c C2). The
// caller's own selection of it still composes.
func TestChildConfigNeverComposesAnOwnModelForAnAgent(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	own := filepath.Join(dir, "own.json")
	for path, text := range map[string]string{
		base: `{"provider":"openai","model":"base-model","api_key_env":"FIXTURE_KEY"}`,
		own:  `{"based_on":"global:openai/default","set":{"name":"Own"}}`,
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parent := sessionfs.SessionConfig{ModelPath: own, ModelBases: map[string]sessionfs.ModelBase{own: {BasedOn: "global:openai/default", Path: base}}}
	if _, err := parent.WithModel(); err != nil {
		t.Fatalf("the caller's own model: %v", err)
	}
	_, err := childConfig(RunTemplate{Base: parent, Agent: sessionfs.NativeSubagentAgent{Name: "a", ModelPath: own}})
	if err == nil || !strings.Contains(err.Error(), "based_on") {
		t.Fatalf("agent naming the own file = %v", err)
	}
	if _, inherits := childConfig(RunTemplate{Base: parent, Agent: sessionfs.NativeSubagentAgent{Name: "b"}}); inherits != nil {
		t.Fatalf("agent inheriting the run's own model = %v", inherits)
	}
	if !parent.OwnModel(own) {
		t.Fatal("childConfig changed the caller's own models")
	}
}
