package remoteapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
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

// A run's worktree lines are written under "worktree" with the keys the client reads, each value
// on one line, and the field is left out for a run without a worktree.
func TestRunViewWritesTheWorktreeLines(t *testing.T) {
	server := &Server{opts: Options{ResultMaxBytes: 1024, Now: time.Now}}
	run := &doneRun{result: RunResult{State: "done", Worktree: []WorktreeLine{{Key: "worktree_branch", Value: "jad/b\nx"}}}}
	view := server.view(&runEntry{run: run, started: time.Now()}, 1024)
	data, _ := json.Marshal(view)
	if !strings.Contains(string(data), `"worktree":[{"key":"worktree_branch","value":"jad/b x"}]`) {
		t.Fatalf("view = %s", data)
	}
	run.result.Worktree = nil
	data, _ = json.Marshal(server.view(&runEntry{run: run, started: time.Now()}, 1024))
	if strings.Contains(string(data), "worktree") {
		t.Fatalf("view = %s", data)
	}
}

type doneRun struct{ result RunResult }

func (r *doneRun) Done() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (r *doneRun) Result() RunResult { return r.result }
func (r *doneRun) Stop(string)       {}
