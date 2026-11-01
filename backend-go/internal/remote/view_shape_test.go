package remote

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The serving side's run JSON and the client's View name the same fields: a field the server
// writes must decode into the client, and the resume fields are present under their JSON names.
func TestViewNamesMatchTheServersRunJSON(t *testing.T) {
	server := []byte(`{"id":"rr_0123456789abcdef","state":"done","kind":"subagent","elapsed_ms":1,"text":"ok","resumed_from":"sub_00000001","config_note":"the agent definition changed"}`)
	var view View
	if err := json.Unmarshal(server, &view); err != nil {
		t.Fatal(err)
	}
	if view.ResumedFrom != "sub_00000001" || view.ConfigNote != "the agent definition changed" {
		t.Fatalf("the client did not read the resume fields: %+v", view)
	}
	typ := reflect.TypeOf(View{})
	for _, name := range []string{"resumed_from", "config_note"} {
		found := false
		for i := range typ.NumField() {
			if tag := typ.Field(i).Tag.Get("json"); tag == name+",omitempty" {
				found = true
			}
		}
		if !found {
			t.Fatalf("View has no omitempty json field %q", name)
		}
	}
}
