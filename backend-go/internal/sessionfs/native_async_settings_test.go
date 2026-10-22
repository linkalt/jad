package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
)

func normalizeAsync(t *testing.T, tools string) (*NativeAsyncSettings, error) {
	t.Helper()
	return normalizeNativeAsyncSettings(map[string]json.RawMessage{"tools": json.RawMessage(tools)}, "setting.json")
}

func TestNativeAsyncSettingsAbsentIsNil(t *testing.T) {
	for _, tools := range []string{`{}`, `{"native":null}`, `{"native":{"process":{}}}`} {
		settings, err := normalizeAsync(t, tools)
		if err != nil || settings != nil {
			t.Errorf("%s: settings = %+v, err = %v, want nil", tools, settings, err)
		}
	}
	if settings, err := normalizeNativeAsyncSettings(map[string]json.RawMessage{}, "setting.json"); settings != nil || err != nil {
		t.Fatalf("no tools: %+v, %v", settings, err)
	}
}

func TestNativeAsyncSettingsEmptyObjectUsesDefaults(t *testing.T) {
	settings, err := normalizeAsync(t, `{"native":{"async":{}}}`)
	if err != nil || settings == nil {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
	if settings.Config != asyncmgr.DefaultConfig() {
		t.Fatalf("config = %+v, want defaults %+v", settings.Config, asyncmgr.DefaultConfig())
	}
}

func TestNativeAsyncSettingsEveryKeyOverridesAndNullKeepsDefault(t *testing.T) {
	settings, err := normalizeAsync(t, `{"native":{"async":{"max_handles":3,"max_handles_total":9,"default_block_ms":5,"max_block_ms":50,"default_wait_ms":6,"max_wait_ms":60,"wait_max_signals":4,"list_default_limit":5,"list_max_limit":25,"summary_max_bytes":100,"status_summary_max_bytes":200,"response_max_bytes":4000,"keep_finished":7,"retention_days":0,"unread_max":8,"unread_ttl_days":0,"stop_wait_ms":0}}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := asyncmgr.Config{MaxHandles: 3, MaxHandlesTotal: 9, DefaultBlockMs: 5, MaxBlockMs: 50, DefaultWaitMs: 6, MaxWaitMs: 60,
		WaitMaxSignals: 4, ListDefaultLimit: 5, ListMaxLimit: 25, SummaryMaxBytes: 100, StatusSummaryBytes: 200, ResponseMaxBytes: 4000,
		KeepFinished: 7, RetentionDays: 0, UnreadMax: 8, UnreadTTLDays: 0, StopWaitMs: 0}
	if settings.Config != want {
		t.Fatalf("config = %+v, want %+v", settings.Config, want)
	}
	nulled, err := normalizeAsync(t, `{"native":{"async":{"max_handles":null}}}`)
	if err != nil || nulled.Config.MaxHandles != 16 {
		t.Fatalf("null key: %+v, %v", nulled, err)
	}
}

func TestNativeAsyncSettingsRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":        `{"native":{"async":{"max_handle":3}}}`,
		"not an object":      `{"native":{"async":[]}}`,
		"string value":       `{"native":{"async":{"max_handles":"3"}}}`,
		"zero handles":       `{"native":{"async":{"max_handles":0}}}`,
		"negative retention": `{"native":{"async":{"retention_days":-1}}}`,
		"huge":               `{"native":{"async":{"keep_finished":99999999999}}}`,
		"wait order":         `{"native":{"async":{"default_wait_ms":500,"max_wait_ms":100}}}`,
		"block order":        `{"native":{"async":{"default_block_ms":500,"max_block_ms":100}}}`,
		"budget too small":   `{"native":{"async":{"response_max_bytes":600}}}`,
		"unread below wait":  `{"native":{"async":{"unread_max":2}}}`,
	}
	for name, tools := range cases {
		if _, err := normalizeAsync(t, tools); err == nil {
			t.Errorf("%s: accepted %s", name, tools)
		}
	}
	_, err := normalizeAsync(t, `{"native":{"async":{"max_handle":3}}}`)
	if err == nil || !strings.Contains(err.Error(), "max_handles") {
		t.Fatalf("typo error = %v, want a suggestion", err)
	}
}

func TestNativeAsyncIsAnAllowListableTool(t *testing.T) {
	if !containsString(NativeToolNames(), "async") {
		t.Fatal("async missing from NativeToolNames")
	}
}
