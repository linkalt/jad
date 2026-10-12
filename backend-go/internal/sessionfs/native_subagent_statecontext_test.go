package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeSubagentStateContextDefaultsToLiveRuns(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.StateContext.States, []string{"queued", "running"}) || got.StateContext.Limit != 20 {
		t.Fatalf("defaults = %+v, want queued+running, limit 20", got.StateContext)
	}
	// The default list is a copy, so one session cannot change the next one's.
	got.StateContext.States[0] = "done"
	again, _ := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if again.StateContext.States[0] != "queued" {
		t.Fatal("the default states are shared between sessions")
	}
}

func TestNormalizeNativeSubagentStateContextReadsStatesAndLimit(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(
		`{"agents":{"a":{}},"statecontext":{"states":["done","failed","done","timed_out"],"limit":3}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.StateContext.States, []string{"done", "failed", "timed_out"}) || got.StateContext.Limit != 3 {
		t.Fatalf("statecontext = %+v", got.StateContext)
	}
	for name, raw := range map[string]string{
		"null":         `{"agents":{"a":{}},"statecontext":null}`,
		"empty object": `{"agents":{"a":{}},"statecontext":{}}`,
		"null states":  `{"agents":{"a":{}},"statecontext":{"states":null,"limit":null}}`,
	} {
		got, err := normalizeNativeSubagentSettings(subagentSettingsFor(raw), "/s/setting.json", "/w", "")
		if err != nil || got.StateContext.Limit != 20 || len(got.StateContext.States) != 2 {
			t.Errorf("%s: got %+v, %v; want the defaults", name, got.StateContext, err)
		}
	}
	empty, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}},"statecontext":{"states":[]}}`), "/s/setting.json", "/w", "")
	if err != nil || len(empty.StateContext.States) != 0 {
		t.Errorf("an empty states list is allowed and lists nothing: got %+v, %v", empty.StateContext, err)
	}
}

func TestNormalizeNativeSubagentStateContextRejectsBadValues(t *testing.T) {
	for name, test := range map[string]struct{ raw, want string }{
		"not an object": {`[]`, "statecontext"},
		"unknown key":   {`{"states":["running"],"order":"old"}`, "order"},
		"unknown state": {`{"states":["sleeping"]}`, "run states"},
		"state type":    {`{"states":[1]}`, "run states"},
		"states type":   {`{"states":"running"}`, "run states"},
		"limit zero":    {`{"limit":0}`, "limit"},
		"limit huge":    {`{"limit":100001}`, "limit"},
		"limit type":    {`{"limit":"x"}`, "limit"},
	} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}},"statecontext":`+test.raw+`}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want mention of %q", name, err, test.want)
		}
	}
}

func TestStateContextSubagentProviderNeedsTheSubagentTool(t *testing.T) {
	const settingPath = "/session/setting.json"
	enabled := map[string]json.RawMessage{"statecontext": json.RawMessage(`{"enabled":true,"tools":["subagent"]}`)}
	if _, err := normalizeStateContextSettings(enabled, settingPath, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "requires tools.native.subagent") {
		t.Fatalf("missing subagent provider error = %v", err)
	}
	configured, err := normalizeStateContextSettings(enabled, settingPath, nil, nil, nil, &NativeSubagentSettings{}, nil)
	if err != nil || !reflect.DeepEqual(configured.Tools, []string{"subagent"}) {
		t.Fatalf("configured subagent provider = %+v, error = %v", configured, err)
	}
	// A disabled statecontext stays inert, as for the other providers.
	inert := map[string]json.RawMessage{"statecontext": json.RawMessage(`{"tools":["subagent"]}`)}
	if _, err := normalizeStateContextSettings(inert, settingPath, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("disabled subagent provider should remain inert, error = %v", err)
	}
	for _, raw := range []string{`{"enabled":true,"tools":["agent"]}`, `{"enabled":true,"tools":["subagent",3]}`} {
		_, err := normalizeStateContextSettings(map[string]json.RawMessage{"statecontext": json.RawMessage(raw)}, settingPath, nil, nil, nil, &NativeSubagentSettings{}, nil)
		if err == nil || !strings.Contains(err.Error(), "task, memory, process, subagent, or request_draft") {
			t.Errorf("%s: error = %v, want the provider list", raw, err)
		}
	}
}
