package sessionfs

import (
	"strings"
	"testing"
)

func TestSubagentPersistRunsDefaultsToOnAndCanBeTurnedOff(t *testing.T) {
	on, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !on.PersistRuns {
		t.Fatalf("default persist_runs = %v, %v; want true", on != nil && on.PersistRuns, err)
	}
	off, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":false,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || off.PersistRuns {
		t.Fatalf("persist_runs false = %v, %v", off != nil && off.PersistRuns, err)
	}
	explicit, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":null,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !explicit.PersistRuns {
		t.Fatalf("persist_runs null = %v, %v; want the default true", explicit != nil && explicit.PersistRuns, err)
	}
}

func TestSubagentPersistRunsRejectsANonBoolean(t *testing.T) {
	for _, value := range []string{`"yes"`, `1`, `{}`} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":`+value+`,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.persist_runs must be a boolean") {
			t.Errorf("persist_runs %s: err = %v", value, err)
		}
	}
}

func TestSubagentStateContextAcceptsTheLostState(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"statecontext":{"states":["running","lost"]},"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || strings.Join(got.StateContext.States, ",") != "running,lost" {
		t.Fatalf("states = %+v, %v", got, err)
	}
}
