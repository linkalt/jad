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

// modelChoiceSchema is schema as the tool offers it: with its model property when model choice
// is on, and without it otherwise, which is the schema the tool had before the property
// existed. property must occur in schema exactly once.
func modelChoiceSchema(schema json.RawMessage, property string, choice bool) json.RawMessage {
	if choice {
		return append(json.RawMessage(nil), schema...)
	}
	if strings.Count(string(schema), property) != 1 {
		panic("model property is not in the tool schema exactly once")
	}
	return json.RawMessage(strings.Replace(string(schema), property, "", 1))
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
