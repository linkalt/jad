package sessionfs

import (
	"strings"
	"testing"
)

func TestSubagentAgentCompactionAndStateContextKeys(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{
		"plain":{},
		"cheap":{"compaction_trigger_tokens":32000,"state_context":true},
		"off":{"auto_compaction":false},
		"on":{"auto_compaction":true},
		"nulls":{"auto_compaction":null,"compaction_trigger_tokens":null,"state_context":null}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	agent := func(name string) NativeSubagentAgent {
		found, ok := got.Agent(name)
		if !ok {
			t.Fatalf("agent %s missing", name)
		}
		return found
	}
	for _, name := range []string{"plain", "nulls"} {
		if a := agent(name); a.HasAutoCompaction || a.AutoCompaction || a.CompactionTriggerTokens != 0 || a.StateContext {
			t.Errorf("%s = %+v, want every new key unset", name, a)
		}
	}
	if a := agent("cheap"); a.CompactionTriggerTokens != 32000 || !a.StateContext || a.HasAutoCompaction {
		t.Errorf("cheap = %+v", a)
	}
	if a := agent("off"); !a.HasAutoCompaction || a.AutoCompaction {
		t.Errorf("off = %+v", a)
	}
	if a := agent("on"); !a.HasAutoCompaction || !a.AutoCompaction {
		t.Errorf("on = %+v", a)
	}
}

func TestSubagentAgentCompactionAndStateContextKeysAreChecked(t *testing.T) {
	for name, tc := range map[string]struct{ setting, want string }{
		"auto not a bool":     {`{"agents":{"a":{"auto_compaction":"yes"}}}`, "agents.a.auto_compaction must be a boolean"},
		"state not a bool":    {`{"agents":{"a":{"state_context":1}}}`, "agents.a.state_context must be a boolean"},
		"trigger zero":        {`{"agents":{"a":{"compaction_trigger_tokens":0}}}`, "agents.a.compaction_trigger_tokens must be a safe integer between 1 and 10000000"},
		"trigger too big":     {`{"agents":{"a":{"compaction_trigger_tokens":10000001}}}`, "agents.a.compaction_trigger_tokens must be a safe integer between 1 and 10000000"},
		"trigger not integer": {`{"agents":{"a":{"compaction_trigger_tokens":"big"}}}`, "agents.a.compaction_trigger_tokens must be a safe integer"},
		"trigger while off":   {`{"agents":{"a":{"auto_compaction":false,"compaction_trigger_tokens":5000}}}`, "compaction_trigger_tokens has no effect when auto_compaction is false"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeSubagentSettings(subagentSettingsFor(tc.setting), "setting.json", "/w", "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
