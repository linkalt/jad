package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

func TestBudgetLedgerCountsPerSessionAndPerDay(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	path := filepath.Join(t.TempDir(), "state", "budget.json")
	ledger := &Budget{Ledger: NewBudgetLedger(path), Limits: BudgetLimits{MaxDailyTokens: 100, MaxSessionCostUSD: 1, Location: berlin, KeepDays: 2}}
	// 22:30 UTC on 4 November is 23:30 in Berlin; 23:30 UTC is already 5 November there.
	late := time.Date(2026, 11, 4, 22, 30, 0, 0, time.UTC)
	if err := ledger.Record(90, 0.2, late); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Check(late); err != nil {
		t.Fatalf("Check under the caps = %v", err)
	}
	if err := ledger.Record(10, 0.2, late); err != nil {
		t.Fatal(err)
	}
	var limit *BudgetLimitError
	if err := ledger.Check(late); !errors.As(err, &limit) || limit.Scope != "daily" || limit.Day != "2026-11-04" || limit.Used != 100 {
		t.Fatalf("Check at the daily cap = %v", err)
	}
	nextDay := late.Add(time.Hour)
	if err := ledger.Check(nextDay); err != nil {
		t.Fatalf("Check on the next Berlin day = %v", err)
	}
	// Another process's ledger over the file sees the counts, and the session cost cap holds across days.
	again := &Budget{Ledger: NewBudgetLedger(path), Limits: BudgetLimits{MaxSessionCostUSD: 1, Location: berlin, KeepDays: 2}}
	if err := again.Record(1, 0.6, nextDay); err != nil {
		t.Fatal(err)
	}
	if err := again.Check(nextDay); !errors.As(err, &limit) || limit.Scope != "session" || limit.Unit != "cost" {
		t.Fatalf("Check at the session cost cap = %v", err)
	}
	if !strings.Contains(limit.Error(), "budget.max_session_cost_usd limit of $1.0000") {
		t.Fatalf("message = %q", limit.Error())
	}
	// Keeping two days drops the first once a third is written.
	if err := again.Record(1, 0, nextDay.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored budgetFile
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if _, kept := stored.Days["2026-11-04"]; kept || stored.Session.Tokens != 102 || len(stored.Days) != 1 {
		t.Fatalf("stored ledger = %+v", stored)
	}
}

func TestBudgetLimitErrorTexts(t *testing.T) {
	for _, test := range []struct {
		err  BudgetLimitError
		want string
	}{
		{BudgetLimitError{Scope: "session", Unit: "tokens", Used: 120, Limit: 100}, "the session used 120 tokens, which reached the budget.max_session_tokens limit of 100 tokens."},
		{BudgetLimitError{Scope: "daily", Unit: "cost", Used: 2.5, Limit: 2, Day: "2026-11-05"}, "the session used $2.5000 on 2026-11-05, which reached the budget.max_daily_cost_usd limit of $2.0000."},
	} {
		if got := test.err.Error(); !strings.HasSuffix(got, test.want) {
			t.Errorf("Error() = %q, want it to end with %q", got, test.want)
		}
	}
}

func TestRunStopsAtTheBudgetAndTheNextRunIsRefused(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "budget.json")
	ledger := &Budget{Ledger: NewBudgetLedger(ledgerPath), Limits: BudgetLimits{MaxSessionTokens: 100}}
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		tokenRound(0, 1, seventyTokens),
		tokenRound(1, 1, seventyTokens),
		tokenRound(2, 0, seventyTokens),
	}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{ToolCatalog: newLookupCatalog(t), MaxToolRounds: 10, Budget: ledger}
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), options)
	var limit *BudgetLimitError
	if !errors.As(err, &limit) || limit.Scope != "session" || limit.Used != 140 {
		t.Fatalf("Run() error = %v, want the session token budget after two rounds", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider rounds = %d, want 2", len(streamer.requests))
	}
	var records []string
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if last := records[len(records)-1]; !strings.Contains(last, "budget_limit") {
		t.Fatalf("the refused call has no budget_limit result: %s", last)
	}
	before := len(records)
	_, err = runner.Run(context.Background(), json.RawMessage(`"again"`), options)
	if !errors.As(err, &limit) {
		t.Fatalf("second Run() error = %v, want the budget refusal", err)
	}
	after := 0
	_ = journal.ReadLines(func([]byte) error { after++; return nil })
	if after != before || len(streamer.requests) != 2 {
		t.Fatalf("the refused run stored input or called the provider: records %d -> %d, calls %d", before, after, len(streamer.requests))
	}
}
