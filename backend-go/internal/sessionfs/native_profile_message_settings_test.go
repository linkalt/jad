package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestNormalizeNativeProfileMessageSettings(t *testing.T) {
	absent, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{}`), "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("absent = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeNativeProfileMessageSettings(map[string]json.RawMessage{}, "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   NativeProfileMessageSettings
	}{
		"defaults":                     {`{"profile_message":{}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"nulls default":                {`{"profile_message":{"deny_profiles":null,"timeout_ms":null,"busy_wait_ms":null,"result_max_bytes":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"every key":                    {`{"profile_message":{"deny_profiles":["a_b-1/x","exec/fast.v2"],"timeout_ms":50,"busy_wait_ms":5,"result_max_bytes":7}}`, NativeProfileMessageSettings{DenyProfiles: []string{"a_b-1/x", "exec/fast.v2"}, TimeoutMs: 50, DefaultBlockMs: DefaultNativeAgentBlockMs, BusyWaitMs: 5, ResultMaxBytes: 7, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"busy wait equals timeout":     {`{"profile_message":{"timeout_ms":9,"busy_wait_ms":9}}`, NativeProfileMessageSettings{TimeoutMs: 9, DefaultBlockMs: DefaultNativeAgentBlockMs, BusyWaitMs: 9, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"empty deny":                   {`{"profile_message":{"deny_profiles":[]}}`, NativeProfileMessageSettings{DenyProfiles: []string{}, TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"compact on":                   {`{"profile_message":{"compact":"on"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextCompact}}},
		"compact after set":            {`{"profile_message":{"compact":"on","compact_after":5000}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextCompact, After: 5000}}},
		"compact off":                  {`{"profile_message":{"compact":"off"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"legacy compact off after set": {`{"profile_message":{"compact":"off","compact_after":7}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 7}}},
		"legacy compact after only":    {`{"profile_message":{"compact_after":7}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 7}}},
		"trigger empty object":         {`{"profile_message":{"context_trigger":{}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger null":                 {`{"profile_message":{"context_trigger":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger nulls":                {`{"profile_message":{"context_trigger":{"action":null,"after":null}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"trigger compact":              {`{"profile_message":{"context_trigger":{"action":"compact"}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextCompact}}},
		"trigger reset after":          {`{"profile_message":{"context_trigger":{"action":"reset","after":9000}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextReset, After: 9000}}},
		"trigger off after set":        {`{"profile_message":{"context_trigger":{"action":"off","after":3}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 3}}},
		"trigger after only":           {`{"profile_message":{"context_trigger":{"after":3}}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff, After: 3}}},
		"queue cap set":                {`{"profile_message":{"max_queued_per_profile":9}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 9, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"queue cap zero":               {`{"profile_message":{"max_queued_per_profile":0}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 0, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"queue cap null":               {`{"profile_message":{"max_queued_per_profile":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"sessions set":                 {`{"profile_message":{"sessions":["work-*","shared"]}}`, NativeProfileMessageSettings{Sessions: []string{"work-*", "shared"}, TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
		"max chain set":                {`{"profile_message":{"max_chain":4}}`, NativeProfileMessageSettings{TimeoutMs: 600000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 4, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json")
			if err != nil || got == nil || !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeProfileMessageSettingsRejectsInvalidSettings(t *testing.T) {
	cases := map[string]struct{ native, want string }{
		"unknown key":                       {`{"profile_message":{"nope":1}}`, "tools.native.profile_message.nope is not supported"},
		"old profiles key":                  {`{"profile_message":{"profiles":["a/b"]}}`, "tools.native.profile_message.profiles is not supported"},
		"typo":                              {`{"profile_message":{"timeout_m":1}}`, "did you mean timeout_ms?"},
		"not object":                        {`{"profile_message":[]}`, "tools.native.profile_message"},
		"not a list":                        {`{"profile_message":{"deny_profiles":"a"}}`, "deny_profiles must be an array of profile ids"},
		"not strings":                       {`{"profile_message":{"deny_profiles":[1]}}`, "deny_profiles must be an array of profile ids"},
		"duplicate":                         {`{"profile_message":{"deny_profiles":["x/a","x/b","x/a"]}}`, `lists "x/a" more than once`},
		"bare id":                           {`{"profile_message":{"deny_profiles":["coder"]}}`, "is not a valid profile id"},
		"empty id":                          {`{"profile_message":{"deny_profiles":[""]}}`, "is not a valid profile id"},
		"bad char":                          {`{"profile_message":{"deny_profiles":["a b"]}}`, "is not a valid profile id"},
		"leading dot":                       {`{"profile_message":{"deny_profiles":["x/.hidden"]}}`, "is not a valid profile id"},
		"traversal":                         {`{"profile_message":{"deny_profiles":["../x"]}}`, "is not a valid profile id"},
		"two slashes":                       {`{"profile_message":{"deny_profiles":["a/b/c"]}}`, "is not a valid profile id"},
		"busy wait negative":                {`{"profile_message":{"busy_wait_ms":-1}}`, "busy_wait_ms"},
		"busy wait text":                    {`{"profile_message":{"busy_wait_ms":"long"}}`, "busy_wait_ms"},
		"busy wait over timeout":            {`{"profile_message":{"timeout_ms":10,"busy_wait_ms":11}}`, "busy_wait_ms must not exceed timeout_ms"},
		"timeout zero":                      {`{"profile_message":{"timeout_ms":0}}`, "timeout_ms"},
		"timeout negative":                  {`{"profile_message":{"timeout_ms":-1}}`, "timeout_ms"},
		"timeout text":                      {`{"profile_message":{"timeout_ms":"slow"}}`, "timeout_ms"},
		"timeout huge":                      {`{"profile_message":{"timeout_ms":99999999999}}`, "timeout_ms"},
		"approval removed":                  {`{"profile_message":{"approval":"ask"}}`, "tools.native.profile_message.approval was removed"},
		"sessions not a list":               {`{"profile_message":{"sessions":"a"}}`, "sessions must be an array of session id globs"},
		"sessions class":                    {`{"profile_message":{"sessions":["a[b]"]}}`, "is not a session id glob"},
		"sessions slash":                    {`{"profile_message":{"sessions":["a/b"]}}`, "is not a session id glob"},
		"sessions root":                     {`{"profile_message":{"sessions":["local:a"]}}`, "is not a session id glob"},
		"sessions dup":                      {`{"profile_message":{"sessions":["a","a"]}}`, `lists "a" more than once`},
		"compact null":                      {`{"profile_message":{"compact":null}}`, `compact must be "off" or "on"`},
		"compact ask":                       {`{"profile_message":{"compact":"ask"}}`, `compact must be "off" or "on"`},
		"compact bool":                      {`{"profile_message":{"compact":true}}`, `compact must be "off" or "on"`},
		"compact case":                      {`{"profile_message":{"compact":"ON"}}`, `compact must be "off" or "on"`},
		"compact after negative":            {`{"profile_message":{"compact_after":-1}}`, "compact_after must be a safe integer between 0"},
		"compact after text":                {`{"profile_message":{"compact_after":"5"}}`, "compact_after must be a safe integer between 0"},
		"compact after huge":                {`{"profile_message":{"compact_after":9007199254740992}}`, "compact_after must be a safe integer between 0"},
		"trigger with legacy compact":       {`{"profile_message":{"compact":"on","context_trigger":{"action":"reset"}}}`, "context_trigger cannot be combined with the legacy compact or compact_after"},
		"trigger with legacy compact off":   {`{"profile_message":{"compact":"off","context_trigger":{}}}`, "context_trigger cannot be combined with the legacy compact or compact_after"},
		"trigger with legacy compact after": {`{"profile_message":{"compact_after":5,"context_trigger":{"action":"compact"}}}`, "context_trigger cannot be combined with the legacy compact or compact_after"},
		"trigger not object":                {`{"profile_message":{"context_trigger":"reset"}}`, "context_trigger must be a JSON object"},
		"trigger array":                     {`{"profile_message":{"context_trigger":[]}}`, "context_trigger must be a JSON object"},
		"trigger unknown key":               {`{"profile_message":{"context_trigger":{"action":"reset","reset_after":1}}}`, "context_trigger.reset_after is not supported"},
		"trigger action typo":               {`{"profile_message":{"context_trigger":{"actions":"reset"}}}`, "did you mean action?"},
		"trigger action bad":                {`{"profile_message":{"context_trigger":{"action":"wipe"}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger action case":               {`{"profile_message":{"context_trigger":{"action":"Reset"}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger action bool":               {`{"profile_message":{"context_trigger":{"action":true}}}`, `context_trigger.action must be "off", "compact" or "reset"`},
		"trigger after negative":            {`{"profile_message":{"context_trigger":{"after":-1}}}`, "context_trigger.after must be a safe integer between 0"},
		"trigger after text":                {`{"profile_message":{"context_trigger":{"after":"5"}}}`, "context_trigger.after must be a safe integer between 0"},
		"trigger after huge":                {`{"profile_message":{"context_trigger":{"after":9007199254740992}}}`, "context_trigger.after must be a safe integer between 0"},
		"queue cap negative":                {`{"profile_message":{"max_queued_per_profile":-1}}`, "max_queued_per_profile"},
		"queue cap huge":                    {`{"profile_message":{"max_queued_per_profile":257}}`, "max_queued_per_profile"},
		"queue cap text":                    {`{"profile_message":{"max_queued_per_profile":"4"}}`, "max_queued_per_profile"},
		"max chain one":                     {`{"profile_message":{"max_chain":1}}`, "max_chain"},
		"max chain huge":                    {`{"profile_message":{"max_chain":9}}`, "max_chain"},
		"max chain text":                    {`{"profile_message":{"max_chain":"2"}}`, "max_chain"},
		"bytes zero":                        {`{"profile_message":{"result_max_bytes":0}}`, "result_max_bytes"},
		"bytes fraction":                    {`{"profile_message":{"result_max_bytes":1.5}}`, "result_max_bytes"},
		"bytes huge":                        {`{"profile_message":{"result_max_bytes":99999999999}}`, "result_max_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadSessionConfigProfileMessage(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{Native: map[string]any{
		"profile_message": map[string]any{"deny_profiles": []any{"exec/fast"}, "timeout_ms": 1000},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	want := &NativeProfileMessageSettings{DenyProfiles: []string{"exec/fast"}, TimeoutMs: 1000, DefaultBlockMs: DefaultNativeAgentBlockMs, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, ContextTrigger: ProfileMessageContextTrigger{Action: ProfileMessageContextOff}}
	if !reflect.DeepEqual(config.NativeProfileMessage, want) {
		t.Fatalf("native.profile_message = %+v, want %+v", config.NativeProfileMessage, want)
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

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"profile_message": map[string]any{"deny_profiles": []any{"a", "a"}}}})
	if _, err = LoadSessionConfig(location, "", ""); err == nil || !strings.Contains(err.Error(), "tools.native.profile_message.deny_profiles") {
		t.Fatalf("duplicate deny_profiles error = %v", err)
	}
}

// compact_after is config only: setting.schema.json must describe it as the loader reads it.
func TestProfileMessageCompactAfterSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties struct {
							ProfileMessage struct {
								Properties map[string]struct {
									Type    json.RawMessage `json:"type"`
									Default json.RawMessage `json:"default"`
									Minimum *int64          `json:"minimum"`
									Maximum *int64          `json:"maximum"`
								} `json:"properties"`
							} `json:"profile_message"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	field, ok := schema.Properties.Tools.Properties.Native.Properties.ProfileMessage.Properties["compact_after"]
	if !ok {
		t.Fatal("setting.schema.json has no tools.native.profile_message.compact_after")
	}
	if string(field.Type) != `"integer"` || string(field.Default) != "0" || field.Minimum == nil || *field.Minimum != 0 || field.Maximum == nil || *field.Maximum != maxSafeInteger {
		t.Fatalf("schema compact_after = %+v, want integer, default 0, minimum 0, maximum %d", field, maxSafeInteger)
	}
}
