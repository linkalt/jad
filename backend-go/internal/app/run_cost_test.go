package app

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// costFixture is the layout fixture whose model is priced (when pricing is set), whose
// provider reports usage, and whose toolset has read; its first answer reads a file and
// the second answers. settings go into the coder profile.
func costFixture(t *testing.T, pricing, settings map[string]any) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	f.usage = `"usage":{"input_tokens":1000,"output_tokens":500,"total_tokens":1500,"input_tokens_details":{"cached_tokens":200}},`
	f.script = func(_, body string) string {
		if strings.Contains(body, `"function_call_output"`) {
			return ""
		}
		return `{"type":"function_call","call_id":"call_r","name":"read","arguments":"{\"path\":\"hello.txt\"}"}`
	}
	modelPath := filepath.Join(f.root, "models", "openai", "small.json")
	var model map[string]any
	data, err := os.ReadFile(modelPath)
	if err != nil || json.Unmarshal(data, &model) != nil {
		t.Fatalf("read the model file: %v", err)
	}
	if pricing != nil {
		model["pricing"] = pricing
	}
	files := map[string]any{
		"models/openai/small.json": model,
		"tools/coding/none.json":   map[string]any{"native": map[string]any{"read": map[string]any{}}, "mcp": map[string]any{}},
		"profiles/dev/coder.json":  map[string]any{"models": []string{"openai/small", "openai/large"}, "model_default": "openai/small", "toolset": "coding/none", "settings": settings},
	}
	for rel, value := range files {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(f.root, rel), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.cwd, "hello.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

var testPricing = map[string]any{"input_per_mtok": 2, "cached_input_per_mtok": 0.5, "output_per_mtok": 8}

// Each response: 800 fresh input at $2, 200 cached at $0.50, 500 output at $8 per million.
const testResponseCost = (800*2 + 200*0.5 + 500*8) / 1e6

func runCostCoder(t *testing.T, f *layoutFixture) headless.Result {
	t.Helper()
	result, err := headlessFor(t, f).Run(context.Background(), headless.Request{Session: "local:demo", Profile: "dev/coder", Input: "read hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPricedRunReportsItsCost(t *testing.T) {
	result := runCostCoder(t, costFixture(t, testPricing, map[string]any{}))
	if result.Status != headless.StatusCompleted || result.CostUSD == nil || math.Abs(*result.CostUSD-2*testResponseCost) > 1e-12 {
		t.Fatalf("result = %+v, cost %v, want %v", result, result.CostUSD, 2*testResponseCost)
	}
	// Without pricing nothing is reported.
	if unpriced := runCostCoder(t, costFixture(t, nil, map[string]any{})); unpriced.Status != headless.StatusCompleted || unpriced.CostUSD != nil {
		t.Fatalf("unpriced result = %+v", unpriced)
	}
}

func TestMaxRunCostStopsTheNextRound(t *testing.T) {
	f := costFixture(t, testPricing, map[string]any{"max_run_cost_usd": 0.005})
	result := runCostCoder(t, f)
	if result.Status != headless.StatusFailed || !strings.Contains(result.Error, "max_run_cost_usd limit of $0.0050") || !strings.Contains(result.Error, "$0.0057") {
		t.Fatalf("result = %+v", result)
	}
	transcript, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl"))
	if err != nil || !strings.Contains(string(transcript), "run_cost_limit") {
		t.Fatalf("the refused call has no run_cost_limit result: %v\n%s", err, transcript)
	}
	// A cap above what the run costs lets it finish.
	if done := runCostCoder(t, costFixture(t, testPricing, map[string]any{"max_run_cost_usd": 1})); done.Status != headless.StatusCompleted {
		t.Fatalf("under the cap: %+v", done)
	}
}

func TestMaxRunCostNeedsPricing(t *testing.T) {
	// The refusal's text stays in the server log, as for a missing key; the run fails
	// before the provider is asked anything.
	f := costFixture(t, nil, map[string]any{"max_run_cost_usd": 1})
	result := runCostCoder(t, f)
	f.mu.Lock()
	requests := len(f.models)
	f.mu.Unlock()
	if result.Status != headless.StatusFailed || requests != 0 {
		t.Fatalf("result = %+v, provider requests %d", result, requests)
	}
}

// The Anthropic translation reports cache writes; they are priced as writes, and a total
// without the 5-minute/1-hour split counts as 5-minute writes.
func TestResponseCostPricesCacheWrites(t *testing.T) {
	streamer := &pricedStreamer{pricing: sessionfs.ModelPricing{InputPerMTok: 3, CachedInputPerMTok: 0.3, CacheWritePerMTok: 3.75, CacheWrite1hPerMTok: 6, OutputPerMTok: 15}}
	for _, test := range []struct {
		details string
		want    float64
	}{
		{`{"cached_tokens":500000,"cache_creation_tokens":300000,"cache_creation_5m_tokens":200000,"cache_creation_1h_tokens":100000}`, 0.2*3 + 0.5*0.3 + 0.2*3.75 + 0.1*6},
		{`{"cached_tokens":500000,"cache_creation_tokens":300000}`, 0.2*3 + 0.5*0.3 + 0.3*3.75},
		{`{"cached_tokens":500000}`, 0.5*3 + 0.5*0.3},
	} {
		raw := `{"response":{"usage":{"input_tokens":1000000,"output_tokens":0,"input_tokens_details":` + test.details + `}}}`
		got, ok := streamer.responseCost(json.RawMessage(raw))
		if !ok || math.Abs(got-test.want) > 1e-9 {
			t.Errorf("%s: cost %v (%v), want %v", test.details, got, ok, test.want)
		}
	}
}
