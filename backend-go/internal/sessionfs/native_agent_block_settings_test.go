package sessionfs

import (
	"strings"
	"testing"
)

// TestSubagentAndProfileMessageDefaultBlock: both tools wait 120 s for an answer by default,
// take their own default_block_ms, and refuse one outside 1..6 hours.
func TestSubagentAndProfileMessageDefaultBlock(t *testing.T) {
	subagent, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"explore":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || subagent.DefaultBlockMs != 120_000 {
		t.Fatalf("subagent default = %+v, %v", subagent, err)
	}
	subagent, err = normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"explore":{}},"default_block_ms":3000}`), "/s/setting.json", "/w", "")
	if err != nil || subagent.DefaultBlockMs != 3000 {
		t.Fatalf("subagent set = %+v, %v", subagent, err)
	}
	message, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"default_block_ms":4000}}`), "setting.json")
	if err != nil || message.DefaultBlockMs != 4000 {
		t.Fatalf("profile_message set = %+v, %v", message, err)
	}
	for _, value := range []string{"0", "21600001", "1.5"} {
		if _, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"explore":{}},"default_block_ms":`+value+`}`), "/s/setting.json", "/w", ""); err == nil || !strings.Contains(err.Error(), "default_block_ms") {
			t.Errorf("subagent default_block_ms %s: %v", value, err)
		}
		if _, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"default_block_ms":`+value+`}}`), "setting.json"); err == nil || !strings.Contains(err.Error(), "default_block_ms") {
			t.Errorf("profile_message default_block_ms %s: %v", value, err)
		}
	}
}

// TestNativeAsyncDefaultWaitFollowsALowerMaxWait: the default wait is 120 s, the most
// max_wait_ms allows by default; a max_wait_ms set lower on its own lowers the default with
// it instead of making the configuration invalid. A default set above the max is still refused.
func TestNativeAsyncDefaultWaitFollowsALowerMaxWait(t *testing.T) {
	settings, err := normalizeAsync(t, `{"native":{"async":{}}}`)
	if err != nil || settings.Config.DefaultWaitMs != 120_000 {
		t.Fatalf("default = %+v, %v", settings, err)
	}
	settings, err = normalizeAsync(t, `{"native":{"async":{"max_wait_ms":30000}}}`)
	if err != nil || settings.Config.DefaultWaitMs != 30_000 || settings.Config.MaxWaitMs != 30_000 {
		t.Fatalf("lower max = %+v, %v", settings, err)
	}
	settings, err = normalizeAsync(t, `{"native":{"async":{"max_wait_ms":30000,"default_wait_ms":5000}}}`)
	if err != nil || settings.Config.DefaultWaitMs != 5000 {
		t.Fatalf("both set = %+v, %v", settings, err)
	}
	if _, err := normalizeAsync(t, `{"native":{"async":{"max_wait_ms":30000,"default_wait_ms":60000}}}`); err == nil {
		t.Fatal("a default above the max was accepted")
	}
}
