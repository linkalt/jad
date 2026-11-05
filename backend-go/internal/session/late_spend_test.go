package session

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/transcript"
)

func ledgerCost(t *testing.T, path string) float64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored budgetFile
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored.Session.CostUSD
}

// A child that spends during the caller's run is recorded by the caller's next round; one
// that spends after the caller's last round is recorded when it ends; nothing is recorded
// twice.
func TestBackgroundChildSpendAfterTheCallersLastRoundReachesTheBudget(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "budget.json")
	budget := &Budget{Ledger: NewBudgetLedger(ledgerPath)}
	var callerMeter *CostMeter
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: "native.lookup", Source: "native", Name: "lookup", Description: "Look up.", InputSchema: schema},
		Validator:  validator,
		Execute: func(ctx context.Context, _ map[string]any, _ tools.ExecutionContext) (string, error) {
			callerMeter = CostMeterFrom(ctx)
			// A foreground child's spend, which the next round records.
			NewCostMeter(callerMeter).Add(0.25)
			return "started", nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{tokenRound(0, 1, seventyTokens), tokenRound(1, 0, seventyTokens)}}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{ToolCatalog: catalog, MaxToolRounds: 4, Budget: budget}); err != nil {
		t.Fatal(err)
	}
	if callerMeter == nil {
		t.Fatal("the tool saw no meter")
	}
	if cost := ledgerCost(t, ledgerPath); math.Abs(cost-0.25) > 1e-9 {
		t.Fatalf("ledger after the run = %v, want 0.25", cost)
	}
	// A background child, whose meter adds into the caller's, spends after the run ended.
	late := NewCostMeter(callerMeter)
	late.Add(0.5)
	if err := late.RecordUnrecordedSpend(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := late.RecordUnrecordedSpend(time.Now()); err != nil {
		t.Fatal(err)
	}
	if cost := ledgerCost(t, ledgerPath); math.Abs(cost-0.75) > 1e-9 {
		t.Fatalf("ledger after the late spend = %v, want 0.75", cost)
	}
	if err := NewCostMeter(nil).RecordUnrecordedSpend(time.Now()); err != nil {
		t.Fatalf("a meter with no budget in its chain: %v", err)
	}
}
