package sessionfs

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestModelPricingValuesAndRefusals(t *testing.T) {
	model, err := hostedToolsModel(t, `,"pricing":{"input_per_mtok":2,"output_per_mtok":8}`)
	if err != nil || model.Pricing == nil || *model.Pricing != (ModelPricing{InputPerMTok: 2, CachedInputPerMTok: 2, OutputPerMTok: 8}) {
		t.Fatalf("pricing = %+v, %v", model.Pricing, err)
	}
	if model, err = hostedToolsModel(t, ``); err != nil || model.Pricing != nil {
		t.Fatalf("absent: %+v %v", model.Pricing, err)
	}
	for name, test := range map[string]struct{ extra, want string }{
		"not an object":  {`,"pricing":1`, "pricing must be an object"},
		"unknown key":    {`,"pricing":{"input_per_mtok":1,"output_per_mtok":1,"per_call":1}`, "per_call"},
		"no output":      {`,"pricing":{"input_per_mtok":1}`, "needs input_per_mtok and output_per_mtok"},
		"no input":       {`,"pricing":{"output_per_mtok":1}`, "needs input_per_mtok and output_per_mtok"},
		"negative":       {`,"pricing":{"input_per_mtok":-1,"output_per_mtok":1}`, "pricing.input_per_mtok must be"},
		"too large":      {`,"pricing":{"input_per_mtok":1,"output_per_mtok":100001}`, "pricing.output_per_mtok must be"},
		"cached as text": {`,"pricing":{"input_per_mtok":1,"output_per_mtok":1,"cached_input_per_mtok":"0.1"}`, "pricing.cached_input_per_mtok must be"},
	} {
		if _, err := hostedToolsModel(t, test.extra); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

func TestModelPricingCost(t *testing.T) {
	pricing := ModelPricing{InputPerMTok: 2, CachedInputPerMTok: 0.5, OutputPerMTok: 8}
	// 1M input of which 400k cached, 100k output: 600k*2 + 400k*0.5 + 100k*8 per million.
	if cost := pricing.CostUSD(1_000_000, 400_000, 100_000); math.Abs(cost-(1.2+0.2+0.8)) > 1e-9 {
		t.Fatalf("cost = %v", cost)
	}
	// More cached than input (a provider quirk) counts all input as cached; negatives are 0.
	if cost := pricing.CostUSD(100, 500, -5); math.Abs(cost-100*0.5/1e6) > 1e-12 {
		t.Fatalf("odd usage cost = %v", cost)
	}
}

func TestMaxRunCostValuesAndRefusals(t *testing.T) {
	if value, err := normalizeMaxRunCost(nil, "s"); value != 0 || err != nil {
		t.Fatalf("absent: %v %v", value, err)
	}
	if value, err := normalizeMaxRunCost(json.RawMessage(`0.25`), "s"); value != 0.25 || err != nil {
		t.Fatalf("value: %v %v", value, err)
	}
	for _, raw := range []string{`0`, `-1`, `"1"`, `1000001`, `{}`} {
		if _, err := normalizeMaxRunCost(json.RawMessage(raw), "s"); err == nil || !strings.Contains(err.Error(), "max_run_cost_usd must be") {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
