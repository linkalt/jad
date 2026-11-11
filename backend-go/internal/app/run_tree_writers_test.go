package app

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// T-007 Q3: max_background_writers is the session's (one owner's) cap, and the checkpoint gate
// counts the owner's writers in every sub-agent manager (Resolver.BackgroundWriters). An owner's
// writers can be in two managers: its storage_dir changed while a writer it started still runs, so
// the next start goes through the manager of the new folder.
func TestBackgroundWriterCapIsTheOwnersAcrossManagers(t *testing.T) {
	f := newLayoutFixture(t)
	writers := func(dir string) string {
		return mixedChainToolset("", `"background_writers":true,"max_background_writers":1,"storage_dir":"`+dir+`",`)
	}
	f.overwrite(t, "tools/coding/none.json", writers("subs-a"))
	held, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	var second string
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		output := strings.Contains(last, "function_call_output")
		switch {
		case !output && strings.Contains(last, "hold A"):
			held <- struct{}{}
			<-release
			return ""
		case output && strings.Contains(last, "call-b"):
			mu.Lock()
			second = last
			mu.Unlock()
			return ""
		case output || model != "model-small":
			return ""
		case strings.Contains(last, "first"):
			return layoutCall("call-a", "subagent", map[string]any{"agent": "explore", "task": "hold A", "block_ms": 0})
		case strings.Contains(last, "second"):
			return layoutCall("call-b", "subagent", map[string]any{"agent": "explore", "task": "quick B", "block_ms": 0})
		default:
			return ""
		}
	}
	run := func(input string) {
		t.Helper()
		status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":`+quoteJSON(input)+`}`)
		if status != http.StatusOK || !strings.Contains(body, "event: completed") {
			t.Fatalf("run %q: %d %s", input, status, body)
		}
	}
	r := f.service.resolver
	reference := sessionReference(t, f)
	run("first")
	awaitSignal(t, held, "the first writer to hold")
	if got := r.BackgroundWriters(reference, "dev/coder"); got != 1 {
		t.Fatalf("background writers after the first start = %d, want 1", got)
	}
	f.overwrite(t, "tools/coding/none.json", writers("subs-b"))
	run("second")
	if managers := len(r.subagentManagerList()); managers != 2 {
		t.Fatalf("the owner's starts went through %d managers, want 2 (one per storage_dir)", managers)
	}
	mu.Lock()
	got := second
	mu.Unlock()
	if !strings.Contains(got, "Not handed off") || strings.Contains(got, "state: running") {
		t.Fatalf("the second writer must run in its call, the owner's one place being taken in the other manager:\n%s", got)
	}
	if got := r.BackgroundWriters(reference, "dev/coder"); got != 1 {
		t.Fatalf("background writers = %d, want the cap of 1", got)
	}
}
