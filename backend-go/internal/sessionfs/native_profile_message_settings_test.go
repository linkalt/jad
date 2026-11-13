package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestNormalizeNativeProfileMessageSettings(t *testing.T) {
	absent, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{}`), "setting.json", MessageCaller{})
	if err != nil || absent != nil {
		t.Fatalf("absent = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeNativeProfileMessageSettings(map[string]json.RawMessage{}, "setting.json", MessageCaller{})
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   NativeProfileMessageSettings
	}{
		"defaults":                 {`{"profile_message":{}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"nulls default":            {`{"profile_message":{"timeout_ms":null,"busy_wait_ms":null,"result_max_bytes":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"every key":                {`{"profile_message":{"timeout_ms":50,"busy_wait_ms":5,"result_max_bytes":7}}`, NativeProfileMessageSettings{TimeoutMs: 50, DefaultBlockMs: DefaultNativeAgentBlockMs, BusyWaitMs: 5, ResultMaxBytes: 7, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"busy wait equals timeout": {`{"profile_message":{"timeout_ms":9,"busy_wait_ms":9}}`, NativeProfileMessageSettings{TimeoutMs: 9, DefaultBlockMs: DefaultNativeAgentBlockMs, BusyWaitMs: 9, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger empty object":     {`{"profile_message":{"context_trigger":{}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger null":             {`{"profile_message":{"context_trigger":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger nulls":            {`{"profile_message":{"context_trigger":{"action":null,"after":null}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger compact":          {`{"profile_message":{"context_trigger":{"action":"compact"}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextCompact}}},
		"trigger reset after":      {`{"profile_message":{"context_trigger":{"action":"reset","after":9000}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextReset, After: 9000}}},
		"trigger off after set":    {`{"profile_message":{"context_trigger":{"action":"off","after":3}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 3}}},
		"trigger after only":       {`{"profile_message":{"context_trigger":{"after":3}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 3}}},
		"queue cap set":            {`{"profile_message":{"max_queued_per_profile":9}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 9, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"queue cap zero":           {`{"profile_message":{"max_queued_per_profile":0}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 0, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"queue cap null":           {`{"profile_message":{"max_queued_per_profile":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"max chain set":            {`{"profile_message":{"max_chain":4}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 4, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json", MessageCaller{})
			if err != nil || got == nil || !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeProfileMessageSettingsRejectsInvalidSettings(t *testing.T) {
	cases := map[string]struct{ native, want string }{
		"compact removed on":             {`{"profile_message":{"compact":"on"}}`, "tools.native.profile_message.compact was removed; use context_trigger"},
		"compact removed off":            {`{"profile_message":{"compact":"off"}}`, "tools.native.profile_message.compact was removed"},
		"compact removed null":           {`{"profile_message":{"compact":null}}`, "tools.native.profile_message.compact was removed"},
		"compact removed invalid":        {`{"profile_message":{"compact":"ask"}}`, "tools.native.profile_message.compact was removed"},
		"compact after removed":          {`{"profile_message":{"compact_after":7}}`, "tools.native.profile_message.compact_after was removed; use context_trigger"},
		"compact after removed null":     {`{"profile_message":{"compact_after":null}}`, "tools.native.profile_message.compact_after was removed"},
		"compact removed beside trigger": {`{"profile_message":{"compact":"on","context_trigger":{"action":"compact"}}}`, "tools.native.profile_message.compact was removed"},
		"compact after beside trigger":   {`{"profile_message":{"compact_after":5,"context_trigger":{"action":"compact"}}}`, "tools.native.profile_message.compact_after was removed"},
		"unknown key":                    {`{"profile_message":{"nope":1}}`, "tools.native.profile_message.nope is not supported"},
		"old profiles key":               {`{"profile_message":{"profiles":["a/b"]}}`, "tools.native.profile_message.profiles is not supported"},
		"typo":                           {`{"profile_message":{"timeout_m":1}}`, "did you mean timeout_ms?"},
		"not object":                     {`{"profile_message":[]}`, "tools.native.profile_message"},
		"busy wait negative":             {`{"profile_message":{"busy_wait_ms":-1}}`, "busy_wait_ms"},
		"busy wait text":                 {`{"profile_message":{"busy_wait_ms":"long"}}`, "busy_wait_ms"},
		"busy wait over timeout":         {`{"profile_message":{"timeout_ms":10,"busy_wait_ms":11}}`, "busy_wait_ms must not exceed timeout_ms"},
		"timeout zero":                   {`{"profile_message":{"timeout_ms":0}}`, "timeout_ms"},
		"timeout negative":               {`{"profile_message":{"timeout_ms":-1}}`, "timeout_ms"},
		"timeout text":                   {`{"profile_message":{"timeout_ms":"slow"}}`, "timeout_ms"},
		"timeout huge":                   {`{"profile_message":{"timeout_ms":99999999999}}`, "timeout_ms"},
		"approval removed":               {`{"profile_message":{"approval":"ask"}}`, "tools.native.profile_message.approval was removed"},
		"trigger not object":             {`{"profile_message":{"context_trigger":"reset"}}`, "context_trigger must be a JSON object"},
		"trigger array":                  {`{"profile_message":{"context_trigger":[]}}`, "context_trigger must be a JSON object"},
		"trigger unknown key":            {`{"profile_message":{"context_trigger":{"action":"reset","reset_after":1}}}`, "context_trigger.reset_after is not supported"},
		"trigger action typo":            {`{"profile_message":{"context_trigger":{"actions":"reset"}}}`, "did you mean action?"},
		"trigger action bad":             {`{"profile_message":{"context_trigger":{"action":"wipe"}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger action case":            {`{"profile_message":{"context_trigger":{"action":"Reset"}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger action bool":            {`{"profile_message":{"context_trigger":{"action":true}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger after negative":         {`{"profile_message":{"context_trigger":{"after":-1}}}`, "context_trigger.after must be a safe integer between 0"},
		"trigger after text":             {`{"profile_message":{"context_trigger":{"after":"5"}}}`, "context_trigger.after must be a safe integer between 0"},
		"trigger after huge":             {`{"profile_message":{"context_trigger":{"after":9007199254740992}}}`, "context_trigger.after must be a safe integer between 0"},
		"queue cap negative":             {`{"profile_message":{"max_queued_per_profile":-1}}`, "max_queued_per_profile"},
		"queue cap huge":                 {`{"profile_message":{"max_queued_per_profile":257}}`, "max_queued_per_profile"},
		"queue cap text":                 {`{"profile_message":{"max_queued_per_profile":"4"}}`, "max_queued_per_profile"},
		"max chain one":                  {`{"profile_message":{"max_chain":1}}`, "max_chain"},
		"max chain huge":                 {`{"profile_message":{"max_chain":9}}`, "max_chain"},
		"max chain text":                 {`{"profile_message":{"max_chain":"2"}}`, "max_chain"},
		"bytes zero":                     {`{"profile_message":{"result_max_bytes":0}}`, "result_max_bytes"},
		"bytes fraction":                 {`{"profile_message":{"result_max_bytes":1.5}}`, "result_max_bytes"},
		"bytes huge":                     {`{"profile_message":{"result_max_bytes":99999999999}}`, "result_max_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json", MessageCaller{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadSessionConfigProfileMessage(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"profile_message": map[string]any{"deny": []any{map[string]any{"to": "local:*/*/exec/fast"}}, "timeout_ms": 1000},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	got := config.NativeProfileMessage
	if got == nil || got.TimeoutMs != 1000 || got.DefaultBlockMs != DefaultNativeAgentBlockMs || got.ResultMaxBytes != 16384 || got.MaxChain != 2 || got.MaxQueued != 4 ||
		got.ContextTrigger.Action != ProfileMessageContextOff || len(got.Deny) != 1 || got.Caller.Workspace != location.Root.ID || got.Caller.Session != location.SessionID {
		t.Fatalf("native.profile_message = %+v", got)
	}

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"read_image": map[string]any{}}})
	config, err = LoadSessionConfig(location, "", "")
	if err != nil || config.NativeProfileMessage != nil {
		t.Fatalf("profile_message = %+v, err=%v; want off", config.NativeProfileMessage, err)
	}

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"profile_message": map[string]any{}}})
	config, err = LoadSessionConfig(location, "", "")
	if err != nil || config.NativeProfileMessage == nil {
		t.Fatalf("empty profile_message = %+v, err=%v; want non-nil settings", config.NativeProfileMessage, err)
	}

	// The keys of the retired v1 layout are errors that name the replacement.
	for key, want := range map[string]string{"deny_profiles": `"to": "local:*/*/<category>/<profile>"`, "sessions": "use deny"} {
		location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"profile_message": map[string]any{key: []any{"a"}}}})
		if _, err = LoadSessionConfig(location, "", ""); err == nil || !strings.Contains(err.Error(), "tools.native.profile_message."+key+" was removed") || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s error = %v", key, err)
		}
	}
}
