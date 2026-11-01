package remoteapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The server's runView writes the resume fields under the names the client's View reads, and the
// fields are omitted for a run that did not resume.
func TestRunViewWritesTheResumeFieldsUnderTheClientsNames(t *testing.T) {
	typ := reflect.TypeOf(runView{})
	for _, name := range []string{"resumed_from", "config_note"} {
		found := false
		for i := range typ.NumField() {
			found = found || typ.Field(i).Tag.Get("json") == name+",omitempty"
		}
		if !found {
			t.Fatalf("runView has no omitempty json field %q", name)
		}
	}
	data, err := json.Marshal(runView{ID: "rr_0123456789abcdef", State: "done", Kind: "subagent"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["resumed_from"]; ok {
		t.Fatalf("a run that did not resume writes resumed_from: %s", data)
	}
}
