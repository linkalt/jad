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

// A remote's worktree lines are kept only with a worktree_* key, at most worktreeLinesMax of
// them, each value on one line and bounded.
func TestViewCleanBoundsTheWorktreeLines(t *testing.T) {
	view := View{Worktree: []WorktreeLine{{Key: "error_code", Value: "x"}, {Key: "worktree_branch", Value: "jad/b\nstatus: ok"}, {Key: "worktree_Branch", Value: "x"}}}
	for range worktreeLinesMax + 4 {
		view.Worktree = append(view.Worktree, WorktreeLine{Key: "worktree_note", Value: string(make([]byte, worktreeValueMaxBytes+10))})
	}
	view.clean()
	if len(view.Worktree) != worktreeLinesMax || view.Worktree[0] != (WorktreeLine{Key: "worktree_branch", Value: "jad/b status: ok"}) {
		t.Fatalf("lines = %d, first %+v", len(view.Worktree), view.Worktree[0])
	}
	if len(view.Worktree[1].Value) > worktreeValueMaxBytes {
		t.Fatalf("value of %d bytes", len(view.Worktree[1].Value))
	}
	var decoded View
	if err := json.Unmarshal([]byte(`{"id":"rr_0123456789abcdef","state":"done","worktree":[{"key":"worktree_branch","value":"jad/b"}]}`), &decoded); err != nil || len(decoded.Worktree) != 1 {
		t.Fatalf("decoded %+v, %v", decoded, err)
	}
}
