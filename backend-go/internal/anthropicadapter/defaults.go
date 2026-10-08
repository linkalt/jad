// Package anthropicadapter serves the OpenAI Responses API on top of the
// Anthropic Messages API. It is stateless: every request carries the full
// history, as the agent backends send it.
package anthropicadapter

// Defaults for every configurable key. The config schema documents the same
// values; config_test.go keeps the two in step.
const (
	DefaultListen                    = "127.0.0.1:8788"
	DefaultUpstreamBaseURL           = "https://api.anthropic.com"
	DefaultAnthropicVersion          = "2023-06-01"
	DefaultRequestTimeoutMS          = 600_000
	DefaultMaxTokens                 = 8192
	DefaultMaxRequestBytes           = 64 * 1024 * 1024
	DefaultUnsupportedFields         = UnsupportedIgnore
	DefaultSynthesizeToolResults     = true
	DefaultInvalidArgumentsKey       = "_invalid_arguments"
	DefaultMissingToolResultText     = "No result was recorded for this tool call."
	MaxRequestTimeoutMS              = 60 * 60 * 1000
	DefaultStreamRetryMax            = 0
	DefaultStreamRetryDelayMS        = 1000
	MaxStreamRetryMax                = 10
	MaxStreamRetryDelayMS            = 60_000
	MinThinkingBudgetTokens          = 1024
	UnsupportedReject                = "reject"
	UnsupportedIgnore                = "ignore"
	ThinkingNone                     = "none"
	ThinkingAdaptive                 = "adaptive"
	ThinkingBudget                   = "budget"
	DisplayDefault                   = ""
	DisplaySummarized                = "summarized"
	DisplayOmitted                   = "omitted"
	defaultEffortBudgetMinimalTokens = 1024
	defaultEffortBudgetLowTokens     = 2048
	defaultEffortBudgetMediumTokens  = 8192
	defaultEffortBudgetHighTokens    = 16384
	defaultEffortBudgetXHighTokens   = 24576
	defaultEffortBudgetMaxTokens     = 32000
)

// efforts lists the Responses reasoning.effort values the adapter accepts.
var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// anthropicEfforts lists the output_config.effort values Anthropic documents.
var anthropicEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func defaultEffortMap() map[string]string {
	return map[string]string{
		"minimal": "low", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "xhigh", "max": "max",
	}
}

func defaultEffortBudget() map[string]int {
	return map[string]int{
		"minimal": defaultEffortBudgetMinimalTokens,
		"low":     defaultEffortBudgetLowTokens,
		"medium":  defaultEffortBudgetMediumTokens,
		"high":    defaultEffortBudgetHighTokens,
		"xhigh":   defaultEffortBudgetXHighTokens,
		"max":     defaultEffortBudgetMaxTokens,
	}
}
