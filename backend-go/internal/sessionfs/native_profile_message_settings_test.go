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
		"defaults":      {`{"profile_message":{"profiles":["a"]}}`, NativeProfileMessageSettings{Profiles: []string{"a"}, TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff}},
		"nulls default": {`{"profile_message":{"profiles":["a"],"timeout_ms":null,"result_max_bytes":null}}`, NativeProfileMessageSettings{Profiles: []string{"a"}, TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff}},
		"every key":     {`{"profile_message":{"profiles":["a_b-1","exec/fast.v2"],"timeout_ms":5,"result_max_bytes":7}}`, NativeProfileMessageSettings{Profiles: []string{"a_b-1", "exec/fast.v2"}, TimeoutMs: 5, ResultMaxBytes: 7, Approval: ApprovalOff}},
		"approval ask":  {`{"profile_message":{"profiles":["a"],"approval":"ask"}}`, NativeProfileMessageSettings{Profiles: []string{"a"}, TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalAsk}},
		"approval off":  {`{"profile_message":{"profiles":["a"],"approval":"off"}}`, NativeProfileMessageSettings{Profiles: []string{"a"}, TimeoutMs: 600000, ResultMaxBytes: 16384, Approval: ApprovalOff}},
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
		"unknown key":      {`{"profile_message":{"profiles":["a"],"nope":1}}`, "tools.native.profile_message.nope is not supported"},
		"typo":             {`{"profile_message":{"profiles":["a"],"timeout_m":1}}`, "did you mean timeout_ms?"},
		"not object":       {`{"profile_message":[]}`, "tools.native.profile_message"},
		"missing profiles": {`{"profile_message":{}}`, "profiles must be a non-empty array"},
		"null profiles":    {`{"profile_message":{"profiles":null}}`, "profiles must be a non-empty array"},
		"empty list":       {`{"profile_message":{"profiles":[]}}`, "profiles must be a non-empty array"},
		"not a list":       {`{"profile_message":{"profiles":"a"}}`, "profiles must be a non-empty array"},
		"not strings":      {`{"profile_message":{"profiles":[1]}}`, "profiles must be a non-empty array"},
		"duplicate":        {`{"profile_message":{"profiles":["a","b","a"]}}`, `lists "a" more than once`},
		"empty id":         {`{"profile_message":{"profiles":[""]}}`, "is not a valid profile id"},
		"bad char":         {`{"profile_message":{"profiles":["a b"]}}`, "is not a valid profile id"},
		"leading dot":      {`{"profile_message":{"profiles":["x/.hidden"]}}`, "is not a valid profile id"},
		"traversal":        {`{"profile_message":{"profiles":["../x"]}}`, "is not a valid profile id"},
		"two slashes":      {`{"profile_message":{"profiles":["a/b/c"]}}`, "is not a valid profile id"},
		"timeout zero":     {`{"profile_message":{"profiles":["a"],"timeout_ms":0}}`, "timeout_ms"},
		"timeout negative": {`{"profile_message":{"profiles":["a"],"timeout_ms":-1}}`, "timeout_ms"},
		"timeout text":     {`{"profile_message":{"profiles":["a"],"timeout_ms":"slow"}}`, "timeout_ms"},
		"timeout huge":     {`{"profile_message":{"profiles":["a"],"timeout_ms":99999999999}}`, "timeout_ms"},
		"approval null":    {`{"profile_message":{"profiles":["a"],"approval":null}}`, `approval must be "off" or "ask"`},
		"approval on":      {`{"profile_message":{"profiles":["a"],"approval":"on"}}`, `approval must be "off" or "ask"`},
		"approval bool":    {`{"profile_message":{"profiles":["a"],"approval":true}}`, `approval must be "off" or "ask"`},
		"bytes zero":       {`{"profile_message":{"profiles":["a"],"result_max_bytes":0}}`, "result_max_bytes"},
		"bytes fraction":   {`{"profile_message":{"profiles":["a"],"result_max_bytes":1.5}}`, "result_max_bytes"},
		"bytes huge":       {`{"profile_message":{"profiles":["a"],"result_max_bytes":99999999999}}`, "result_max_bytes"},
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
		"profile_message": map[string]any{"profiles": []any{"exec/fast"}, "timeout_ms": 1000},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	want := &NativeProfileMessageSettings{Profiles: []string{"exec/fast"}, TimeoutMs: 1000, ResultMaxBytes: 16384, Approval: ApprovalOff}
	if !reflect.DeepEqual(config.NativeProfileMessage, want) {
		t.Fatalf("native.profile_message = %+v, want %+v", config.NativeProfileMessage, want)
	}

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"read": map[string]any{}}})
	config, err = LoadSessionConfig(location, "", "")
	if err != nil || config.NativeProfileMessage != nil {
		t.Fatalf("profile_message = %+v, err=%v; want off", config.NativeProfileMessage, err)
	}

	location, _ = layoutLocation(t, layouttest.Spec{Native: map[string]any{"profile_message": map[string]any{"profiles": []any{}}}})
	if _, err = LoadSessionConfig(location, "", ""); err == nil || !strings.Contains(err.Error(), "tools.native.profile_message.profiles") {
		t.Fatalf("empty profiles error = %v", err)
	}
}
