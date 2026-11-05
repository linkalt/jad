package sessionfs

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	defaultBudgetTimeZone       = "UTC"
	defaultBudgetKeepDays int64 = 35
	maxBudgetKeepDays     int64 = 3660
	maxBudgetCostUSD            = 1_000_000_000
	budgetSection               = "budget"
)

var budgetKeys = []string{"max_session_tokens", "max_session_cost_usd", "max_daily_tokens", "max_daily_cost_usd", "time_zone", "keep_days"}

// BudgetSettings are the normalized `budget` settings: caps on what all the runs of a
// session use, in total and per calendar day, on top of the per-run max_run_tokens and
// max_run_cost_usd. A zero cap is no cap; with every cap zero the budget is off and
// nothing is counted.
type BudgetSettings struct {
	MaxSessionTokens  int64
	MaxSessionCostUSD float64
	MaxDailyTokens    int64
	MaxDailyCostUSD   float64
	// TimeZone is the IANA name whose midnight starts a new day.
	TimeZone string
	Location *time.Location
	// KeepDays is how many past days the ledger keeps.
	KeepDays int64
}

// Active reports whether any cap is set.
func (settings BudgetSettings) Active() bool {
	return settings.MaxSessionTokens > 0 || settings.MaxSessionCostUSD > 0 || settings.MaxDailyTokens > 0 || settings.MaxDailyCostUSD > 0
}

// CostCapped reports whether a cap is in US dollars, which needs the model's pricing.
func (settings BudgetSettings) CostCapped() bool {
	return settings.MaxSessionCostUSD > 0 || settings.MaxDailyCostUSD > 0
}

func defaultBudgetSettings() BudgetSettings {
	return BudgetSettings{TimeZone: defaultBudgetTimeZone, Location: time.UTC, KeepDays: defaultBudgetKeepDays}
}

func normalizeBudget(raw json.RawMessage, settingPath string) (BudgetSettings, error) {
	settings := defaultBudgetSettings()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": "+budgetSection)
	if err != nil {
		return BudgetSettings{}, err
	}
	for key := range fields {
		if !slices.Contains(budgetKeys, key) {
			return BudgetSettings{}, configError("%s: %s.%s is not supported", settingPath, budgetSection, key)
		}
	}
	if settings.MaxSessionTokens, err = configuredInteger(fields, "max_session_tokens", 0, 1, maxConfiguredRunTokens, settingPath, budgetSection); err != nil {
		return BudgetSettings{}, err
	}
	if settings.MaxDailyTokens, err = configuredInteger(fields, "max_daily_tokens", 0, 1, maxConfiguredRunTokens, settingPath, budgetSection); err != nil {
		return BudgetSettings{}, err
	}
	if settings.MaxSessionCostUSD, err = budgetCost(fields, "max_session_cost_usd", settingPath); err != nil {
		return BudgetSettings{}, err
	}
	if settings.MaxDailyCostUSD, err = budgetCost(fields, "max_daily_cost_usd", settingPath); err != nil {
		return BudgetSettings{}, err
	}
	if settings.KeepDays, err = configuredInteger(fields, "keep_days", defaultBudgetKeepDays, 1, maxBudgetKeepDays, settingPath, budgetSection); err != nil {
		return BudgetSettings{}, err
	}
	if settings.TimeZone, err = bashString(fields, "time_zone", defaultBudgetTimeZone, settingPath+": "+budgetSection, true); err != nil {
		return BudgetSettings{}, err
	}
	settings.TimeZone = strings.TrimSpace(settings.TimeZone)
	location, err := time.LoadLocation(settings.TimeZone)
	if err != nil || settings.TimeZone == "" || strings.EqualFold(settings.TimeZone, "local") {
		return BudgetSettings{}, configError("%s: %s.time_zone must be an IANA time zone name such as UTC or Europe/Berlin", settingPath, budgetSection)
	}
	settings.Location = location
	return settings, nil
}

func budgetCost(fields map[string]json.RawMessage, name, settingPath string) (float64, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return 0, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || value <= 0 || value > maxBudgetCostUSD {
		return 0, configError("%s: %s.%s must be a number of US dollars above 0 and at most %d", settingPath, budgetSection, name, maxBudgetCostUSD)
	}
	return value, nil
}
