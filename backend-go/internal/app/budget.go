package app

import (
	"path/filepath"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
)

// budgetFileName is the session's budget ledger in its state directory, shared by its
// profiles.
const budgetFileName = "budget.json"

// sessionBudget returns the session's budget with the caps of config, or nil when the
// budget sets no cap.
func (r *Resolver) sessionBudget(config sessionfs.SessionConfig) *session.Budget {
	settings := config.Budget
	if !settings.Active() {
		return nil
	}
	path := filepath.Join(config.StateDir, budgetFileName)
	r.mu.Lock()
	if r.budgetLedgers == nil {
		r.budgetLedgers = make(map[string]*session.BudgetLedger)
	}
	ledger := r.budgetLedgers[path]
	if ledger == nil {
		ledger = session.NewBudgetLedger(path)
		r.budgetLedgers[path] = ledger
	}
	r.mu.Unlock()
	return &session.Budget{Ledger: ledger, Limits: session.BudgetLimits{
		MaxSessionTokens:  settings.MaxSessionTokens,
		MaxSessionCostUSD: settings.MaxSessionCostUSD,
		MaxDailyTokens:    settings.MaxDailyTokens,
		MaxDailyCostUSD:   settings.MaxDailyCostUSD,
		Location:          settings.Location,
		KeepDays:          int(settings.KeepDays),
	}}
}
