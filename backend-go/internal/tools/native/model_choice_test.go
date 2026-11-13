package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	off := func(model, sandbox string) []optionalProperty {
		return []optionalProperty{{model, false}, {sandbox, false}}
	}
	if got := string(optionalPropertiesSchema(subagentInputSchema, off(subagentModelProperty, subagentSandboxProperty))); got != subagentSchemaWithoutModel {
		t.Fatalf("subagent schema with model_choice and sandbox_choice off:\n%s", got)
	}
	if got := string(optionalPropertiesSchema(profileMessageInputSchema, off(profileMessageModelProperty, profileMessageSandboxProperty))); got != profileMessageSchemaWithoutModel {
		t.Fatalf("profile_message schema with model_choice and sandbox_choice off:\n%s", got)
	}
	modelOnly := optionalPropertiesSchema(subagentInputSchema, []optionalProperty{{subagentModelProperty, true}, {subagentSandboxProperty, false}})
	if want := strings.Replace(string(subagentInputSchema), subagentSandboxProperty, "", 1); string(modelOnly) != want || strings.Contains(string(modelOnly), `"sandbox"`) || !strings.Contains(string(modelOnly), `"model"`) {
		t.Fatalf("subagent schema with model_choice on:\n%s", modelOnly)
	}
	both := optionalPropertiesSchema(subagentInputSchema, []optionalProperty{{subagentModelProperty, true}, {subagentSandboxProperty, true}})
	if string(both) != string(subagentInputSchema) {
		t.Fatalf("subagent schema with both on:\n%s", both)
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

// A subagent.json or profile_message.json copied before the model argument existed still
// loads, with model_choice off or on: the tools build the schema the model sees themselves.
// Any other schema is still refused.
func TestDefinitionFilesFromBeforeModelChoiceStillLoad(t *testing.T) {
	directory := t.TempDir()
	write := func(name, id, schema string) string {
		path := filepath.Join(directory, name+".json")
		document, _ := json.Marshal(map[string]any{"tools": []map[string]any{{"id": id, "source": "native", "name": name, "description": name + " tool", "inputSchema": json.RawMessage(schema)}}})
		if err := os.WriteFile(path, document, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	build := func(choice bool, subagentSchema string) error {
		agent := agentNamed("explore")
		agent.Tools, agent.HasTools = []string{"read_image"}, true
		subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{agent}, ModelChoice: choice}
		config := sessionfs.SessionConfig{
			SettingPath: filepath.Join(directory, "setting.json"),
			ToolDefinitionPaths: []string{
				filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"),
				write("subagent", subagentToolID, subagentSchema),
				write("profile_message", profileMessageToolID, profileMessageSchemaWithoutModel),
			},
			NativeReadImage:      &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
			NativeSubagent:       &subagent,
			NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{Caller: sessionfs.MessageCaller{Workspace: "app", Session: "release", Profile: "dev/main"}, TimeoutMs: 5000, ResultMaxBytes: 1024, ModelChoice: choice},
			Profile:              "main",
			Profiles:             []string{"main", "reviewer"},
		}
		_, err := BuildCatalogWithMessenger(config, nil, nil, nil, &fakeLauncher{}, nil, nil, nil, &fakeMessenger{})
		return err
	}
	for _, choice := range []bool{false, true} {
		if err := build(choice, subagentSchemaWithoutModel); err != nil {
			t.Fatalf("model_choice %v: definition files from before the model argument refused: %v", choice, err)
		}
	}
	changed := strings.Replace(subagentSchemaWithoutModel, `"minLength":1`, `"minLength":2`, 1)
	if err := build(false, changed); err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("a changed subagent schema: error %v, want the mismatch", err)
	}
}
