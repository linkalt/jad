package sessionfs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBudgetSettingsDefaultIsOff(t *testing.T) {
	settings, err := normalizeBudget(nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Active() || settings.CostCapped() || settings.TimeZone != "UTC" || settings.KeepDays != 35 || settings.Location == nil {
		t.Fatalf("default budget = %+v", settings)
	}
	settings, err = normalizeBudget(json.RawMessage(`{"max_daily_tokens":5000,"max_session_cost_usd":2.5,"time_zone":"Europe/Berlin","keep_days":7}`), "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Active() || !settings.CostCapped() || settings.MaxDailyTokens != 5000 || settings.MaxSessionCostUSD != 2.5 || settings.Location.String() != "Europe/Berlin" || settings.KeepDays != 7 {
		t.Fatalf("budget = %+v", settings)
	}
}

func TestBudgetSettingsRefuseBadValues(t *testing.T) {
	for raw, want := range map[string]string{
		`{"max_session_tokens":0}`:     "max_session_tokens",
		`{"max_daily_tokens":1.5}`:     "max_daily_tokens",
		`{"max_daily_cost_usd":0}`:     "max_daily_cost_usd",
		`{"max_session_cost_usd":"1"}`: "max_session_cost_usd",
		`{"time_zone":"Mars/Olympus"}`: "time_zone",
		`{"time_zone":"Local"}`:        "time_zone",
		`{"keep_days":0}`:              "keep_days",
		`{"max_weekly_tokens":10}`:     "max_weekly_tokens is not supported",
		`[]`:                           "budget",
	} {
		if _, err := normalizeBudget(json.RawMessage(raw), "setting.json"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to contain %q", raw, err, want)
		}
	}
}

func TestBudgetSettingsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties["budget"].Properties
	if len(properties) != len(budgetKeys) {
		t.Fatalf("schema budget keys = %d, loader keys = %v", len(properties), budgetKeys)
	}
	for _, key := range budgetKeys {
		if properties[key] == nil {
			t.Errorf("schema has no budget.%s", key)
		}
	}
	if properties["keep_days"]["default"] != float64(defaultBudgetKeepDays) || properties["keep_days"]["maximum"] != float64(maxBudgetKeepDays) || properties["time_zone"]["default"] != defaultBudgetTimeZone {
		t.Errorf("schema defaults differ from the loader: %v %v", properties["keep_days"], properties["time_zone"])
	}
	for _, key := range []string{"max_session_tokens", "max_daily_tokens"} {
		if properties[key]["maximum"] != float64(maxConfiguredRunTokens) {
			t.Errorf("budget.%s maximum = %v", key, properties[key]["maximum"])
		}
	}
	for _, key := range []string{"max_session_cost_usd", "max_daily_cost_usd"} {
		if properties[key]["maximum"] != float64(maxBudgetCostUSD) {
			t.Errorf("budget.%s maximum = %v", key, properties[key]["maximum"])
		}
	}
	if _, listed := layoutSettingKeys["budget"]; !listed {
		t.Error("budget is not a layout settings key")
	}
}
