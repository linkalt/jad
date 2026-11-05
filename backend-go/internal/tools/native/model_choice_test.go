package native

import (
	"context"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// The schemas the two tools had before their model argument existed. With model_choice off
// (the default) the tools must offer exactly these, so a provider's prompt cache and the
// model's view of the tools are what they were.
const (
	subagentSchemaWithoutModel = `{
  "type":"object",
  "properties":{
    "task":{"type":"string","minLength":1,"description":"Self-contained instructions. The agent sees nothing of this conversation. Omit to list the agents."},
    "agent":{"type":"string","description":"Agent name. Optional when only one is configured."},
    "resume":{"type":"string","description":"Run id (sub_...) of a finished run of this session to continue from its transcript. The new run is a copy: the old run is not changed, and the result names it as resumed_from."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time this call blocks for the answer; 0 returns a run id at once. Clamped to the async policy. An agent marked [can change files; runs to its end] ignores it and runs to its end."}
  },
  "additionalProperties":false
}`
	profileMessageSchemaWithoutModel = `{
  "type":"object",
  "properties":{
    "profile":{"type":"string","minLength":1,"description":"Profile to message: one of the available profiles, or <session>/<category>/<profile> for a profile of another session this session may reach."},
    "message":{"type":"string","minLength":1,"description":"The message to send. The profile sees nothing of this conversation."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the answer before an id is returned; collect it with the async tool."}
  },
  "required":["profile","message"],
  "additionalProperties":false
}`
)

func TestModelChoiceOffKeepsTheToolSchemasTheyHadBefore(t *testing.T) {
	if got := string(modelChoiceSchema(subagentInputSchema, subagentModelProperty, false)); got != subagentSchemaWithoutModel {
		t.Fatalf("subagent schema with model_choice off:\n%s", got)
	}
	if got := string(modelChoiceSchema(profileMessageInputSchema, profileMessageModelProperty, false)); got != profileMessageSchemaWithoutModel {
		t.Fatalf("profile_message schema with model_choice off:\n%s", got)
	}
	if got := modelChoiceSchema(subagentInputSchema, subagentModelProperty, true); string(got) != string(subagentInputSchema) {
		t.Fatalf("subagent schema with model_choice on:\n%s", got)
	}
}

func TestProfileMessageWithModelChoiceOffHasNoModelArgument(t *testing.T) {
	tool, err := NewProfileMessageTool(profileMessageSettings(), profileMessageTargets, &fakeMessenger{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(tool.Definition.InputSchema) != profileMessageSchemaWithoutModel {
		t.Fatalf("schema = %s", tool.Definition.InputSchema)
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	output := callProfileMessage(t, profileMessageSettings(), messenger, `{"profile":"reviewer","message":"check it","model":"openai/large"}`)
	if messenger.request.Message != "" || messenger.request.Model != "" {
		t.Fatalf("a call naming a model reached the messenger with model_choice off: %+v", messenger.request)
	}
	if !strings.Contains(output, "model") {
		t.Fatalf("output = %s", output)
	}
}

func TestSubagentModelTextNeedsModelChoice(t *testing.T) {
	models := map[string]string{"openai/b": "b.json", "openai/a": "a.json"}
	if text := subagentModelText(sessionfs.NativeSubagentSettings{ProfileModels: models}); text != "" {
		t.Fatalf("model_choice off lists models: %q", text)
	}
	if text := subagentModelText(sessionfs.NativeSubagentSettings{ModelChoice: true, ProfileModels: models}); text != " Models for model: openai/a, openai/b." {
		t.Fatalf("model_choice on: %q", text)
	}
}
