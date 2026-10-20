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
}, ignored *[]string) {
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
		if lastToolTurnLacksThinking(out.Messages) {
			*ignored = append(*ignored, "reasoning")
			return
		}
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget, Display: display}
	}
	dropSamplingForThinking(out, ignored)
}

// dropFixedSampling omits temperature and top_p for models that reject them on
// every request. The model's own defaults apply, and the names are reported.
func dropFixedSampling(out *anthropicRequest, profile *ModelProfile, ignored *[]string) {
	if !profile.FixedSampling {
		return
	}
	if out.Temperature != nil {
		out.Temperature = nil
		*ignored = append(*ignored, "temperature")
	}
	if out.TopP != nil {
		out.TopP = nil
		*ignored = append(*ignored, "top_p")
	}
}

// Anthropic rejects temperature (and top_k) while thinking is on, and top_p
// outside 0.95..1. The values are dropped rather than failing the request.
func dropSamplingForThinking(out *anthropicRequest, ignored *[]string) {
	if out.Thinking == nil {
		return
	}
	if out.Temperature != nil {
		out.Temperature = nil
		*ignored = append(*ignored, "temperature")
	}
	if out.TopP != nil && (*out.TopP < 0.95 || *out.TopP > 1) {
		out.TopP = nil
		*ignored = append(*ignored, "top_p")
	}
}

// lastToolTurnLacksThinking reports whether the final assistant turn carries
// tool_use without a leading thinking block. Manual thinking requires that
// block in a tool loop, and the history may come from a client that did not
// replay our reasoning item.
func lastToolTurnLacksThinking(messages []anthropicMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		content := messages[i].Content
		hasTool := false
		for _, block := range content {
			hasTool = hasTool || block.Type == "tool_use"
		}
		return hasTool && (len(content) == 0 || (content[0].Type != "thinking" && content[0].Type != "redacted_thinking"))
	}
	return false
}
