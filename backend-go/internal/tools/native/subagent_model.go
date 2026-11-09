package native

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// modelChoices lists the model IDs a call's model argument may name, sorted.
func modelChoices(models map[string]string) []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// subagentModelText is the part of the tool description that names the models a call may
// pick; empty when model_choice is off.
func subagentModelText(settings sessionfs.NativeSubagentSettings) string {
	if !settings.ModelChoice || len(settings.ProfileModels) == 0 {
		return ""
	}
	return " Models for model: " + strings.Join(modelChoices(settings.ProfileModels), ", ") + "."
}

// optionalProperty is one schema line a tool offers only when a setting turns it on (the model
// argument with model_choice, the sandbox argument with sandbox_choice).
type optionalProperty struct {
	line string
	on   bool
}

// optionalPropertiesSchema is schema as the tool offers it: without each optional property that
// is off, which with all of them off is the schema the tool had before they existed. Each line
// must occur in schema exactly once.
func optionalPropertiesSchema(schema json.RawMessage, properties []optionalProperty) json.RawMessage {
	text := string(schema)
	for _, property := range properties {
		if strings.Count(text, property.line) != 1 {
			panic("optional property is not in the tool schema exactly once")
		}
		if !property.on {
			text = strings.Replace(text, property.line, "", 1)
		}
	}
	return json.RawMessage(text)
}

// withCallModel is the agent with the model a call named in place of its own. An absent model
// keeps the agent's model, which is the calling session's model when the agent names none. The
// model must be one the calling profile lists, optionally with @<effort> the model offers.
func withCallModel(agent sessionfs.NativeSubagentAgent, arguments map[string]any, models map[string]string) (sessionfs.NativeSubagentAgent, string) {
	model, _ := arguments["model"].(string)
	if model == "" {
		return agent, ""
	}
	id, effort, err := sessionfs.SplitModelEffort(model)
	if err != nil {
		return agent, err.Error()
	}
	path, ok := models[id]
	if !ok {
		if len(models) == 0 {
			return agent, "This session offers no model choice for sub-agents"
		}
		return agent, fmt.Sprintf("Model %s is not one this profile lists", id)
	}
	if err := sessionfs.CheckModelEffort(path, effort); err != nil {
		return agent, err.Error()
	}
	agent.Model, agent.ModelPath, agent.ReasoningEffort = model, path, effort
	return agent, ""
}

func modelUnavailable(callID, problem string, models map[string]string) string {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, "model_unavailable", problem,
		tools.Information{Key: "available", Value: strings.Join(modelChoices(models), ", ")})
}
