package sessionfs

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// context_trigger is written once in setting.schema.json: its nested keys, enum, default,
// bounds and refusal of unknown keys have to agree with the loader, and its text has to say
// that reset deletes history.
func TestProfileMessageContextTriggerSchemaMatchesTheLoader(t *testing.T) {
	setting := readSchemaJSON(t, "setting.schema.json")
	native := setting["properties"].(map[string]any)["tools"].(map[string]any)["properties"].(map[string]any)["native"].(map[string]any)["properties"].(map[string]any)
	trigger := native["profile_message"].(map[string]any)["properties"].(map[string]any)["context_trigger"].(map[string]any)
	if trigger["additionalProperties"] != false {
		t.Error("context_trigger must refuse unknown keys in the schema, as the loader does")
	}
	properties := trigger["properties"].(map[string]any)
	var keys []string
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := append([]string(nil), profileMessageContextTriggerKeys...); !reflect.DeepEqual(keys, want) {
		t.Errorf("schema keys %v, loader accepts %v", keys, want)
	}
	action := properties["action"].(map[string]any)
	var enum []string
	for _, value := range action["enum"].([]any) {
		if text, ok := value.(string); ok {
			enum = append(enum, text)
		}
	}
	if want := []string{ProfileMessageContextOff, ProfileMessageContextCompact, ProfileMessageContextReset}; !reflect.DeepEqual(enum, want) {
		t.Errorf("action enum %v, loader accepts %v", enum, want)
	}
	if action["default"] != ProfileMessageContextOff {
		t.Errorf("action default = %v, want %q", action["default"], ProfileMessageContextOff)
	}
	after := properties["after"].(map[string]any)
	if after["default"] != float64(0) || after["minimum"] != float64(0) || after["maximum"] != float64(maxSafeInteger) {
		t.Errorf("after schema = %v, loader: default 0, 0..%d", after, maxSafeInteger)
	}
	text, _ := json.Marshal(trigger["description"])
	for _, phrase := range []string{"DELETES the target's whole history", "derived from it", "destructive", "remote"} {
		if !strings.Contains(string(text), phrase) {
			t.Errorf("context_trigger description does not say %q", phrase)
		}
	}
}
