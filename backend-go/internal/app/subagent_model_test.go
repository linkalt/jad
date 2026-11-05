package app

import (
	"strings"
	"testing"
)

// A call's model runs the child on that model, one the profile lists, in place of the
// agent's own (here the session's model, since the agent names none).
func TestSubagentCallModelRunsTheChildOnThatModel(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"description": "Finds facts.", "__profile_models": []string{"openai/parent", "openai/child"}, "__subagent_settings": map[string]any{"model_choice": true}})
	f.provider.parentSteps = []subagentStep{{"subagent", `{"task":"find the answer","model":"openai/child"}`}}
	body := f.run(t)
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if len(f.provider.forModel("child-model")) == 0 {
		t.Fatalf("the child did not run on the model the call named; parent requests %d", len(parent))
	}
	if !strings.Contains(parent[0].Descriptions["subagent"], "Models for model: openai/child, openai/parent.") {
		t.Fatalf("the description does not list the models:\n%s", parent[0].Descriptions["subagent"])
	}
	if !strings.Contains(string(parent[0].Parameters["subagent"]), `"model"`) {
		t.Fatalf("the schema has no model: %s", parent[0].Parameters["subagent"])
	}
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "the answer is 42") {
		t.Fatalf("the parent did not get the child's answer:\n%s", last)
	}
}

// A model the profile does not list is refused before any child starts.
func TestSubagentCallModelTheProfileDoesNotListIsRefused(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"description": "Finds facts.", "__subagent_settings": map[string]any{"model_choice": true}})
	f.provider.parentSteps = []subagentStep{{"subagent", `{"task":"find the answer","model":"openai/child"}`}}
	f.run(t)
	if requests := f.provider.forModel("child-model"); len(requests) != 0 {
		t.Fatalf("a child ran on a model the profile does not list: %d requests", len(requests))
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if !strings.Contains(last, "model_unavailable") || !strings.Contains(last, "Model openai/child is not one this profile lists") {
		t.Fatalf("the refusal did not reach the parent:\n%s", last)
	}
}

// With model_choice at its default (off) the tool has no model argument and lists no models,
// so its definition is the one it had before the argument existed, and a call that names a
// model is refused by the schema before any child starts.
func TestSubagentModelChoiceIsOffByDefault(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"description": "Finds facts.", "__profile_models": []string{"openai/parent", "openai/child"}})
	f.provider.parentSteps = []subagentStep{{"subagent", `{"task":"find the answer","model":"openai/child"}`}}
	f.run(t)
	if requests := f.provider.forModel("child-model"); len(requests) != 0 {
		t.Fatalf("a child ran on a model the call named with model_choice off: %d requests", len(requests))
	}
	parent := f.provider.forModel("parent-model")
	if strings.Contains(parent[0].Descriptions["subagent"], "Models for model") {
		t.Fatalf("the description lists models with model_choice off:\n%s", parent[0].Descriptions["subagent"])
	}
	if strings.Contains(string(parent[0].Parameters["subagent"]), `"model"`) {
		t.Fatalf("the schema has a model property with model_choice off: %s", parent[0].Parameters["subagent"])
	}
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "model") {
		t.Fatalf("the refusal of the model argument did not reach the parent:\n%s", last)
	}
}
