package main

import (
	"testing"

	"jad/backend-go/internal/evalrun"
)

func TestEvalSummaryLineCountsSkippedStopHooksOnlyWhenThereAreSome(t *testing.T) {
	report := evalrun.Report{Suite: "smoke", Summary: evalrun.Summary{Total: 4, Passed: 3, Failed: 1, PassRate: 0.75, InputTokens: 10, OutputTokens: 5}}
	if got, want := evalSummaryLine(report), "smoke: 3/4 passed (75%), 1 failed, 0 errors, 10 input + 5 output tokens"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	report.Summary.StopHooksSkipped = 2
	if got, want := evalSummaryLine(report), "smoke: 3/4 passed (75%), 1 failed, 0 errors, 10 input + 5 output tokens, 2 with stop hooks not run at a cap"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}
