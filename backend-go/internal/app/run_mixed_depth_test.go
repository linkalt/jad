package app

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// R6 mixed depth (design §3.5): max_depth counts every local run between the user's run and a
// start, of either kind, so a run that profile_message starts is one level below its caller and
// its own sub-agents are counted from there. Before R6 a profile hop counted from 0 again.

// mixedDepthRun runs dev/coder -> explore -> review/reviewer -> finder (task "look around") under
// subagent settings subagent and returns the transcripts, the finder requests and the manifests.
func mixedDepthRun(t *testing.T, f *layoutFixture, script func(string, string, func() string) func(string, string) string) (string, int32, []mixedRunManifest) {
	t.Helper()
	var finderRequests atomic.Int32
	f.script = script("finder", "look around", func() string {
		finderRequests.Add(1)
		return ""
	})
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	awaitTargetSessionFree(t, f)
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	return runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m), finderRequests.Load(), mixedRunManifests(t, f)
}

// At the default max_depth 2 the coder's child runs at depth 1 and the reviewer's run it messages
// at depth 2, so the reviewer may not start a sub-agent: its call is refused before any run.
func TestMixedDepthIsCountedAcrossAProfileHop(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", ""))
	transcripts, finder, runs := mixedDepthRun(t, f, mixedChainScript)
	if finder != 0 || !strings.Contains(transcripts, "subagent_depth_exceeded") {
		t.Fatalf("the reviewer's child sent %d requests; want it refused subagent_depth_exceeded:\n%s", finder, transcripts)
	}
	if len(runs) != 1 || runs[0].Task != "ask the reviewer" || runs[0].State != "done" {
		t.Fatalf("runs = %+v, want only the coder's child, done", runs)
	}
}

// One level more admits the reviewer's child, which is then at depth 3.
func TestMixedDepthAdmitsTheTargetsChildBelowMaxDepth(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", `"max_depth":3,`))
	transcripts, finder, runs := mixedDepthRun(t, f, mixedChainScript)
	if finder != 1 || strings.Contains(transcripts, "subagent_depth_exceeded") {
		t.Fatalf("the reviewer's child sent %d requests (want 1):\n%s", finder, transcripts)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want the coder's and the reviewer's child", runs)
	}
}

// The depth crosses a session hop as it crosses a profile hop.
func TestMixedDepthIsCountedAcrossSessions(t *testing.T) {
	f := crossSessionFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset(`"sessions":["*"]`, ""))
	transcripts, finder, runs := mixedDepthRun(t, f, crossSessionScript)
	if finder != 0 || !strings.Contains(transcripts, "subagent_depth_exceeded") || !strings.Contains(transcripts, "=== other/review/reviewer") {
		t.Fatalf("the other session's child sent %d requests; want it refused subagent_depth_exceeded:\n%s", finder, transcripts)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want only the coder's child", runs)
	}
}

// A profile run the user starts is at depth 0, as before: at max_depth 1 it still starts a child.
func TestMixedDepthOfAProfileRunTheUserStartsIsZero(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", `"max_depth":1,`))
	var finderRequests atomic.Int32
	f.script = mixedChainScript("finder", "look around", func() string {
		finderRequests.Add(1)
		return ""
	})
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"review it"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	if n := finderRequests.Load(); n != 1 || strings.Contains(body, "subagent_depth_exceeded") {
		t.Fatalf("the reviewer's child sent %d requests (want 1):\n%s", n, body)
	}
}

// The slot of a sub-agent is not in the profile_message wait-for graph. With max_concurrent 1,
// dev/coder's child holds the only slot while it waits for review/reviewer. The reviewer's run
// was started from inside that child, so its own sub-agent is a nested start: it fails fast
// with subagent_busy instead of queueing for the slot its caller holds until timeout_ms.
func TestMixedChainSubagentOfATargetFailsFastWhenItsCallerHoldsTheSlot(t *testing.T) {
	const timeout = 20 * time.Second
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset(`"timeout_ms":20000`, `"max_concurrent":1,"max_depth":3,"storage_dir":".shared-runs",`))
	var reviewerChildRequests atomic.Int32
	f.script = mixedChainScript("explore", "look around", func() string {
		reviewerChildRequests.Add(1)
		return layoutCall("call-read", "skill", map[string]any{"name": "notes"})
	})
	started := time.Now()
	body := runMixedChain(t, f)
	if elapsed := time.Since(started); elapsed >= timeout/2 {
		t.Fatalf("the run took %v: the reviewer's child waited for the slot", elapsed)
	}
	if !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	awaitTargetSessionFree(t, f)
	if n := reviewerChildRequests.Load(); n != 0 {
		t.Fatalf("the reviewer's child sent %d requests; it must never get the slot", n)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	transcripts := runTranscripts(t, f.cwd, m)
	if !strings.Contains(transcripts, "subagent_busy") || strings.Contains(transcripts, "profile_timeout") {
		t.Fatalf("want the reviewer told subagent_busy and no profile_timeout:\n%s", transcripts)
	}
	runs := mixedRunManifests(t, f)
	if len(runs) != 1 || runs[0].Task != "ask the reviewer" || runs[0].State != "done" {
		t.Fatalf("runs = %+v, want only the coder's child, done", runs)
	}
}
