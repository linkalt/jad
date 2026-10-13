package app

import (
	"testing"

	"jad/backend-go/internal/webui"
)

func int64Pointer(value int64) *int64 { return &value }

// Compaction keeps the cumulative totals and the turn count but zeroes the
// latest response's own token counts, as the TypeScript usage cache does.
func TestResetLatestResponseUsageKeepsTotalsAndZeroesCurrent(t *testing.T) {
	summary := webui.UsageSummary{
		TokenUsage:         webui.TokenUsage{InputTokens: int64Pointer(90), OutputTokens: int64Pointer(21), TotalTokens: int64Pointer(111)},
		CompletedResponses: 3,
		ResponsesWithUsage: 3,
		TurnCount:          3,
		Current: &webui.CurrentResponseUsage{
			TokenUsage: webui.TokenUsage{InputTokens: int64Pointer(30), OutputTokens: int64Pointer(7), TotalTokens: int64Pointer(37), CachedInputTokens: int64Pointer(4)},
			Turn:       3,
			Step:       1,
			ResponseID: "resp-3",
		},
	}
	reset := resetLatestResponseUsage(summary)

	if reset.TurnCount != 3 || reset.CompletedResponses != 3 || *reset.InputTokens != 90 || *reset.TotalTokens != 111 {
		t.Fatalf("cumulative totals changed: %+v", reset)
	}
	current := reset.Current
	if current == nil || current.Turn != 3 || current.Step != 1 || current.ResponseID != "resp-3" {
		t.Fatalf("current identity changed: %+v", current)
	}
	for name, value := range map[string]*int64{"input": current.InputTokens, "output": current.OutputTokens, "total": current.TotalTokens, "cached": current.CachedInputTokens} {
		if value == nil || *value != 0 {
			t.Fatalf("current %s tokens = %v, want 0", name, value)
		}
	}
	if *summary.Current.InputTokens != 30 {
		t.Fatal("resetLatestResponseUsage mutated its input")
	}
}

func TestResetLatestResponseUsageWithoutCurrentIsUnchanged(t *testing.T) {
	summary := webui.UsageSummary{TurnCount: 2, CompletedResponses: 2}
	if reset := resetLatestResponseUsage(summary); reset.Current != nil || reset.TurnCount != 2 {
		t.Fatalf("summary without a current response changed: %+v", reset)
	}
}
