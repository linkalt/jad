package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestNormalizeNativeProfileMessageSettings(t *testing.T) {
	absent, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{}`), "setting.json", "")
	if err != nil || absent != nil {
		t.Fatalf("absent = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeNativeProfileMessageSettings(map[string]json.RawMessage{}, "setting.json", "")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   NativeProfileMessageSettings
	}{
		"defaults":                 {`{"profile_message":{}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"nulls default":            {`{"profile_message":{"deny_profiles":null,"timeout_ms":null,"busy_wait_ms":null,"result_max_bytes":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"every key":                {`{"profile_message":{"deny_profiles":["a_b-1/x","exec/fast.v2"],"timeout_ms":50,"busy_wait_ms":5,"result_max_bytes":7}}`, NativeProfileMessageSettings{DenyProfiles: []string{"a_b-1/x", "exec/fast.v2"}, TimeoutMs: 50, BusyWaitMs: 5, ResultMaxBytes: 7, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"busy wait equals timeout": {`{"profile_message":{"timeout_ms":9,"busy_wait_ms":9}}`, NativeProfileMessageSettings{TimeoutMs: 9, BusyWaitMs: 9, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"empty deny":               {`{"profile_message":{"deny_profiles":[]}}`, NativeProfileMessageSettings{DenyProfiles: []string{}, TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"compact on":               {`{"profile_message":{"compact":"on"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOn}},
		"compact off":              {`{"profile_message":{"compact":"off"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"queue cap set":            {`{"profile_message":{"max_queued_per_profile":9}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 9, Compact: ProfileMessageCompactOff}},
		"queue cap zero":           {`{"profile_message":{"max_queued_per_profile":0}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 0, Compact: ProfileMessageCompactOff}},
		"queue cap null":           {`{"profile_message":{"max_queued_per_profile":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"sessions set":             {`{"profile_message":{"sessions":["work-*","shared"]}}`, NativeProfileMessageSettings{Sessions: []string{"work-*", "shared"}, TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
		"max chain set":            {`{"profile_message":{"max_chain":4}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, MaxChain: 4, MaxQueued: 4, Compact: ProfileMessageCompactOff}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json", "")
			if err != nil || got == nil || !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeProfileMessageSettingsRejectsInvalidSettings(t *testing.T) {
	cases := map[string]struct{ native, want string }{
		"unknown key":            {`{"profile_message":{"nope":1}}`, "tools.native.profile_message.nope is not supported"},
		"old profiles key":       {`{"profile_message":{"profiles":["a/b"]}}`, "tools.native.profile_message.profiles is not supported"},
		"typo":                   {`{"profile_message":{"timeout_m":1}}`, "did you mean timeout_ms?"},
		"not object":             {`{"profile_message":[]}`, "tools.native.profile_message"},
		"not a list":             {`{"profile_message":{"deny_profiles":"a"}}`, "deny_profiles must be an array of profile ids"},
		"not strings":            {`{"profile_message":{"deny_profiles":[1]}}`, "deny_profiles must be an array of profile ids"},
		"duplicate":              {`{"profile_message":{"deny_profiles":["x/a","x/b","x/a"]}}`, `lists "x/a" more than once`},
		"bare id":                {`{"profile_message":{"deny_profiles":["coder"]}}`, "is not a valid profile id"},
		"empty id":               {`{"profile_message":{"deny_profiles":[""]}}`, "is not a valid profile id"},
		"bad char":               {`{"profile_message":{"deny_profiles":["a b"]}}`, "is not a valid profile id"},
		"leading dot":            {`{"profile_message":{"deny_profiles":["x/.hidden"]}}`, "is not a valid profile id"},
		"traversal":              {`{"profile_message":{"deny_profiles":["../x"]}}`, "is not a valid profile id"},
		"two slashes":            {`{"profile_message":{"deny_profiles":["a/b/c"]}}`, "is not a valid profile id"},
		"busy wait negative":     {`{"profile_message":{"busy_wait_ms":-1}}`, "busy_wait_ms"},
		"busy wait text":         {`{"profile_message":{"busy_wait_ms":"long"}}`, "busy_wait_ms"},
		"busy wait over timeout": {`{"profile_message":{"timeout_ms":10,"busy_wait_ms":11}}`, "busy_wait_ms must not exceed timeout_ms"},
		"timeout zero":           {`{"profile_message":{"timeout_ms":0}}`, "timeout_ms"},
		"timeout negative":       {`{"profile_message":{"timeout_ms":-1}}`, "timeout_ms"},
		"timeout text":           {`{"profile_message":{"timeout_ms":"slow"}}`, "timeout_ms"},
		"timeout huge":           {`{"profile_message":{"timeout_ms":99999999999}}`, "timeout_ms"},
		"approval removed":       {`{"profile_message":{"approval":"ask"}}`, "tools.native.profile_message.approval was removed"},
		"sessions not a list":    {`{"profile_message":{"sessions":"a"}}`, "sessions must be an array of session id globs"},
		"sessions class":         {`{"profile_message":{"sessions":["a[b]"]}}`, "is not a session id glob"},
		"sessions slash":         {`{"profile_message":{"sessions":["a/b"]}}`, "is not a session id glob"},
		"sessions root":          {`{"profile_message":{"sessions":["local:a"]}}`, "is not a session id glob"},
		"sessions dup":           {`{"profile_message":{"sessions":["a","a"]}}`, `lists "a" more than once`},
		"compact null":           {`{"profile_message":{"compact":null}}`, `compact must be "off" or "on"`},
		"compact ask":            {`{"profile_message":{"compact":"ask"}}`, `compact must be "off" or "on"`},
		"compact bool":           {`{"profile_message":{"compact":true}}`, `compact must be "off" or "on"`},
		"compact case":           {`{"profile_message":{"compact":"ON"}}`, `compact must be "off" or "on"`},
		"queue cap negative":     {`{"profile_message":{"max_queued_per_profile":-1}}`, "max_queued_per_profile"},
		"queue cap huge":         {`{"profile_message":{"max_queued_per_profile":257}}`, "max_queued_per_profile"},
		"queue cap text":         {`{"profile_message":{"max_queued_per_profile":"4"}}`, "max_queued_per_profile"},
		"max chain one":          {`{"profile_message":{"max_chain":1}}`, "max_chain"},
		"max chain huge":         {`{"profile_message":{"max_chain":9}}`, "max_chain"},
		"max chain text":         {`{"profile_message":{"max_chain":"2"}}`, "max_chain"},
		"bytes zero":             {`{"profile_message":{"result_max_bytes":0}}`, "result_max_bytes"},
		"bytes fraction":         {`{"profile_message":{"result_max_bytes":1.5}}`, "result_max_bytes"},
		"bytes huge":             {`{"profile_message":{"result_max_bytes":99999999999}}`, "result_max_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(tc.native), "setting.json", "")
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
	want := &NativeProfileMessageSettings{DenyProfiles: []string{"exec/fast"}, TimeoutMs: 1000, ResultMaxBytes: 16384, MaxChain: 2, MaxQueued: 4, Compact: ProfileMessageCompactOff}
	if !reflect.DeepEqual(config.NativeProfileMessage, want) {
		t.Fatalf("native.profile_message = %+v, want %+v", config.NativeProfileMessage, want)
	}

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"read": map[string]any{}}})
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

func TestNormalizeNativeProfileMessageSettingsReadsSystemPromptPath(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "system.md"), []byte("System text."), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"system_prompt_path":"system.md"}}`), "setting.json", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got.SystemPromptPath != filepath.Join(cwd, "system.md") || got.SystemPrompt != "System text." {
		t.Errorf("SystemPromptPath = %q, SystemPrompt = %q", got.SystemPromptPath, got.SystemPrompt)
	}
}

func TestNormalizeNativeProfileMessageSettingsMissingSystemPromptNamesTheKey(t *testing.T) {
	cwd := t.TempDir()
	missing := filepath.Join(cwd, "nope.md")
	_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"system_prompt_path":"nope.md"}}`), "setting.json", cwd)
	if err == nil || !strings.Contains(err.Error(), "tools.native.profile_message.system_prompt_path[0] could not be read: "+missing) {
		t.Fatalf("error = %v, want the key and %s", err, missing)
	}
}
