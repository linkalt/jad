package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// BudgetLimits caps what all the runs of a session use: in total and per calendar day in
// Location. A zero cap is no cap.
type BudgetLimits struct {
	MaxSessionTokens  int64
	MaxSessionCostUSD float64
	MaxDailyTokens    int64
	MaxDailyCostUSD   float64
	Location          *time.Location
	// KeepDays is how many days the ledger keeps; older days are dropped when it is written.
	KeepDays int
}

// BudgetLimitError is returned when a run would start, or start another round, after a
// budget cap was reached. Its text holds only numbers the harness counted, so a client
// may read it.
type BudgetLimitError struct {
	// Scope is "session" or "daily".
	Scope string
	// Unit is "tokens" or "cost".
	Unit  string
	Used  float64
	Limit float64
	// Day is the day of a daily cap, as YYYY-MM-DD in the budget's time zone.
	Day string
}

func (e *BudgetLimitError) Error() string {
	used, limit := strconv.FormatInt(int64(e.Used), 10)+" tokens", strconv.FormatInt(int64(e.Limit), 10)+" tokens"
	if e.Unit == "cost" {
		used, limit = fmt.Sprintf("$%.4f", e.Used), fmt.Sprintf("$%.4f", e.Limit)
	}
	if e.Scope == "daily" {
		return fmt.Sprintf("The run stopped before its next round: the session used %s on %s, which reached the budget.max_daily_%s limit of %s.", used, e.Day, budgetKeyUnit(e.Unit), limit)
	}
	return fmt.Sprintf("The run stopped before its next round: the session used %s, which reached the budget.max_session_%s limit of %s.", used, budgetKeyUnit(e.Unit), limit)
}

// ClientMessage is the error's text, which names only counted numbers and the limit.
func (e *BudgetLimitError) ClientMessage() string { return e.Error() }

func budgetKeyUnit(unit string) string {
	if unit == "cost" {
		return "cost_usd"
	}
	return "tokens"
}

// budgetUsage is what was used in one scope.
type budgetUsage struct {
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
}

// budgetFile is the ledger as stored.
type budgetFile struct {
	SchemaVersion int                    `json:"schema_version"`
	Session       budgetUsage            `json:"session"`
	Days          map[string]budgetUsage `json:"days"`
}

// BudgetLedger holds the counts of one session, stored as JSON at its path. The process
// keeps one ledger per path, so the runs of every profile of a session add to the same
// counts under one lock; the file is read again when another process changed it. It is
// safe for concurrent use.
type BudgetLedger struct {
	mu     sync.Mutex
	path   string
	state  budgetFile
	stamp  time.Time
	size   int64
	loaded bool
}

// NewBudgetLedger returns the ledger stored at path.
func NewBudgetLedger(path string) *BudgetLedger {
	return &BudgetLedger{path: path}
}

// Budget is a ledger with the caps one run applies to it.
type Budget struct {
	Ledger *BudgetLedger
	Limits BudgetLimits
}

func (b *Budget) location() *time.Location {
	if b.Limits.Location == nil {
		return time.UTC
	}
	return b.Limits.Location
}

// Check returns a *BudgetLimitError when a cap is reached at now, and nil otherwise. A
// nil budget checks nothing.
func (b *Budget) Check(now time.Time) error {
	if b == nil || b.Ledger == nil {
		return nil
	}
	l := b.Ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	limits := b.Limits
	day := now.In(b.location()).Format(time.DateOnly)
	today := l.state.Days[day]
	switch {
	case limits.MaxSessionTokens > 0 && l.state.Session.Tokens >= limits.MaxSessionTokens:
		return &BudgetLimitError{Scope: "session", Unit: "tokens", Used: float64(l.state.Session.Tokens), Limit: float64(limits.MaxSessionTokens)}
	case limits.MaxSessionCostUSD > 0 && l.state.Session.CostUSD >= limits.MaxSessionCostUSD:
		return &BudgetLimitError{Scope: "session", Unit: "cost", Used: l.state.Session.CostUSD, Limit: limits.MaxSessionCostUSD}
	case limits.MaxDailyTokens > 0 && today.Tokens >= limits.MaxDailyTokens:
		return &BudgetLimitError{Scope: "daily", Unit: "tokens", Used: float64(today.Tokens), Limit: float64(limits.MaxDailyTokens), Day: day}
	case limits.MaxDailyCostUSD > 0 && today.CostUSD >= limits.MaxDailyCostUSD:
		return &BudgetLimitError{Scope: "daily", Unit: "cost", Used: today.CostUSD, Limit: limits.MaxDailyCostUSD, Day: day}
	}
	return nil
}

// Record adds what one response used and stores the ledger.
func (b *Budget) Record(tokens int64, costUSD float64, now time.Time) error {
	if b == nil || b.Ledger == nil || (tokens <= 0 && costUSD <= 0) {
		return nil
	}
	tokens, costUSD = max(tokens, 0), max(costUSD, 0)
	l := b.Ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.load(); err != nil {
		return err
	}
	location := b.location()
	day := now.In(location).Format(time.DateOnly)
	l.state.Session.Tokens += tokens
	l.state.Session.CostUSD += costUSD
	today := l.state.Days[day]
	today.Tokens += tokens
	today.CostUSD += costUSD
	l.state.Days[day] = today
	l.prune(now.In(location), b.Limits.KeepDays)
	return l.store()
}

// load reads the file when it changed since it was last read. A missing file is an
// empty ledger.
func (l *BudgetLedger) load() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		if !l.loaded {
			l.state = budgetFile{SchemaVersion: 1, Days: map[string]budgetUsage{}}
			l.loaded = true
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the budget ledger: %w", err)
	}
	if l.loaded && info.ModTime().Equal(l.stamp) && info.Size() == l.size {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read the budget ledger: %w", err)
	}
	var state budgetFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("read the budget ledger %s: %w", l.path, err)
	}
	if state.Days == nil {
		state.Days = map[string]budgetUsage{}
	}
	l.state, l.stamp, l.size, l.loaded = state, info.ModTime(), info.Size(), true
	return nil
}

func (l *BudgetLedger) prune(now time.Time, keep int) {
	if keep <= 0 || len(l.state.Days) <= keep {
		return
	}
	oldest := now.AddDate(0, 0, -(keep - 1)).Format(time.DateOnly)
	days := make([]string, 0, len(l.state.Days))
	for day := range l.state.Days {
		days = append(days, day)
	}
	sort.Strings(days)
	for _, day := range days {
		if day < oldest {
			delete(l.state.Days, day)
		}
	}
}

// store writes the ledger through a temporary file and a rename.
func (l *BudgetLedger) store() error {
	l.state.SchemaVersion = 1
	data, err := json.MarshalIndent(l.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("store the budget ledger: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("store the budget ledger: %w", err)
	}
	_, writeErr := temporary.Write(append(data, '\n'))
	closeErr := temporary.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(temporary.Name(), l.path)
	}
	if writeErr != nil {
		_ = os.Remove(temporary.Name())
		return fmt.Errorf("store the budget ledger: %w", writeErr)
	}
	if info, err := os.Stat(l.path); err == nil {
		l.stamp, l.size = info.ModTime(), info.Size()
	}
	return nil
}
