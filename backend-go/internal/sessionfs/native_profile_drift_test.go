package sessionfs

import (
	"encoding/json"
	"os"
	"testing"
)

// The profile tool's list and remote switches mirror transcripts' on purpose: the same keys in
// the code and in setting.schema.json, with the same defaults. This test keeps them from drifting.
func TestProfileSettingsMirrorTranscriptsKeysAndSchema(t *testing.T) {
	shared := []string{"max_entries", "scan_max_entries", "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_remote", "remotes"}
	for _, key := range shared {
		if !containsString(nativeProfileKeys, key) || !containsString(nativeTranscriptsKeys, key) {
			t.Fatalf("%s must be a key of both profile and transcripts", key)
		}
	}
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties map[string]struct {
							Properties map[string]json.RawMessage `json:"properties"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	native := schema.Properties.Tools.Properties.Native.Properties
	for _, key := range shared {
		for _, section := range []string{"profile", "transcripts"} {
			if _, ok := native[section].Properties[key]; !ok {
				t.Fatalf("setting.schema.json tools.native.%s lacks %s", section, key)
			}
		}
	}
	if len(native["profile"].Properties) != len(shared) {
		t.Fatalf("tools.native.profile has %d keys; want exactly the %d shared keys", len(native["profile"].Properties), len(shared))
	}
}
