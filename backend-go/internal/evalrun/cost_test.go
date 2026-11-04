package evalrun

import (
	"math"
	"testing"

	"jad/backend-go/internal/headless"
)

func TestSummaryAddsUpThePricedAttempts(t *testing.T) {
	first, second := 0.25, 0.5
	summary := summarize([]Attempt{
		{Outcome: OutcomePass, Run: &headless.Result{CostUSD: &first}},
		{Outcome: OutcomeFail, Run: &headless.Result{}},
		{Outcome: OutcomePass, Run: &headless.Result{CostUSD: &second}},
		{Outcome: OutcomeError},
	})
	if summary.CostUSD == nil || math.Abs(*summary.CostUSD-0.75) > 1e-12 {
		t.Fatalf("cost = %v", summary.CostUSD)
	}
	if none := summarize([]Attempt{{Outcome: OutcomePass, Run: &headless.Result{}}}); none.CostUSD != nil {
		t.Fatalf("unpriced cost = %v", *none.CostUSD)
	}
}
