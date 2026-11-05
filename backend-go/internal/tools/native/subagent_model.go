package native

import (
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

// subagentModelText is the part of the tool description that names the models a call may pick.
func subagentModelText(models map[string]string) string {
	if len(models) == 0 {
		return ""
	}
	return " Models for model: " + strings.Join(modelChoices(models), ", ") + "."
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
