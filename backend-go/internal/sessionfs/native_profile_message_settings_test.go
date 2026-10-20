package sessionfs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestNormalizeNativeProfileMessageSettings(t *testing.T) {
	absent, err := normalizeNativeProfileMessageSettings(decideSettingsFor(`{}`), "setting.json")
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
		"defaults":                 {`{"profile_message":{}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"nulls default":            {`{"profile_message":{"deny_profiles":null,"timeout_ms":null,"busy_wait_ms":null,"result_max_bytes":null}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"every key":                {`{"profile_message":{"deny_profiles":["a_b-1/x","exec/fast.v2"],"timeout_ms":50,"busy_wait_ms":5,"result_max_bytes":7}}`, NativeProfileMessageSettings{DenyProfiles: []string{"a_b-1/x", "exec/fast.v2"}, TimeoutMs: 50, BusyWaitMs: 5, ResultMaxBytes: 7, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"busy wait equals timeout": {`{"profile_message":{"timeout_ms":9,"busy_wait_ms":9}}`, NativeProfileMessageSettings{TimeoutMs: 9, BusyWaitMs: 9, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"empty deny":               {`{"profile_message":{"deny_profiles":[]}}`, NativeProfileMessageSettings{DenyProfiles: []string{}, TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"approval ask":             {`{"profile_message":{"approval":"ask"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalAsk, Compact: ProfileMessageCompactOff}},
		"compact on":               {`{"profile_message":{"compact":"on"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOn}},
		"compact off":              {`{"profile_message":{"compact":"off"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
		"approval off":             {`{"profile_message":{"approval":"off"}}`, NativeProfileMessageSettings{TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeProfileMessageSettings(decideSettingsFor(tc.native), "setting.json")
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
		"approval null":          {`{"profile_message":{"approval":null}}`, `approval must be "off" or "ask"`},
		"approval on":            {`{"profile_message":{"approval":"on"}}`, `approval must be "off" or "ask"`},
		"approval bool":          {`{"profile_message":{"approval":true}}`, `approval must be "off" or "ask"`},
		"compact null":           {`{"profile_message":{"compact":null}}`, `compact must be "off" or "on"`},
		"compact ask":            {`{"profile_message":{"compact":"ask"}}`, `compact must be "off" or "on"`},
		"compact bool":           {`{"profile_message":{"compact":true}}`, `compact must be "off" or "on"`},
		"compact case":           {`{"profile_message":{"compact":"ON"}}`, `compact must be "off" or "on"`},
		"bytes zero":             {`{"profile_message":{"result_max_bytes":0}}`, "result_max_bytes"},
		"bytes fraction":         {`{"profile_message":{"result_max_bytes":1.5}}`, "result_max_bytes"},
		"bytes huge":             {`{"profile_message":{"result_max_bytes":99999999999}}`, "result_max_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeProfileMessageSettings(decideSettingsFor(tc.native), "setting.json")
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
	want := &NativeProfileMessageSettings{DenyProfiles: []string{"exec/fast"}, TimeoutMs: 1000, ResultMaxBytes: 16384, Approval: ApprovalOff, Compact: ProfileMessageCompactOff}
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
