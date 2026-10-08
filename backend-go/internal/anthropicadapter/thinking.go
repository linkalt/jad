package anthropicadapter

import (
	"encoding/json"
	"slices"
)

// applyThinking sets thinking and output_config from reasoning.effort and the
// model profile.
//
//   - none: nothing is sent, so the model keeps its own default.
//   - adaptive: thinking {type: adaptive} plus output_config.effort from
//     effort_map.
//   - budget: thinking {type: enabled, budget_tokens} from effort_budget, only
//     when an effort is known and the budget fits below max_tokens.
func applyThinking(out *anthropicRequest, profile *ModelProfile, reasoning *struct {
	Effort  string          `json:"effort"`
	Summary json.RawMessage `json:"summary"`
}) {
	if profile.Thinking == ThinkingNone {
		return
	}
	effort := profile.DefaultEffort
	var summary string
	if reasoning != nil {
		if reasoning.Effort != "" {
			effort = reasoning.Effort
		}
		summary = stringOf(reasoning.Summary)
	}
	if effort == "none" {
		return
	}
	display := profile.Display
	if display == DisplayDefault && summary != "" {
		display = DisplaySummarized
	}
	switch profile.Thinking {
	case ThinkingAdaptive:
		out.Thinking = &anthropicThinking{Type: "adaptive", Display: display}
		if mapped := profile.EffortMap[effort]; mapped != "" {
			out.OutputConfig = &anthropicOutputConfig{Effort: mapped}
		}
	case ThinkingBudget:
		budget := profile.EffortBudget[effort]
		if effort == "" || budget == 0 {
			return
		}
		// Manual thinking rejects forced tool use, and needs budget < max_tokens.
		if out.ToolChoice != nil && slices.Contains([]string{"any", "tool"}, out.ToolChoice.Type) {
			return
		}
		if budget >= out.MaxTokens {
			budget = out.MaxTokens - 1
		}
		if budget < MinThinkingBudgetTokens {
			return
		}
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget, Display: display}
	}
}
