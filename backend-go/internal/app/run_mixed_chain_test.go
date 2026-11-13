package app

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// R5 pre-change characterisation of chains that mix both kinds of run: a sub-agent of dev/coder
// messages review/reviewer, whose run starts a sub-agent of its own. These pin today's cycle,
// slot and stop behaviour before the registry and admission are merged (design §3.5, §3.7).

// mixedChainToolset gives both profiles profile_message, skill, async and two sub-agents:
// explore may send profile_message, which makes it a writer, and with background_writers off a
// writer runs without a handle, so its call waits for it; finder only reads and runs as a handle.
// profileMessage and subagent are extra keys of those two sections.
func mixedChainToolset(profileMessage, subagent string) string {
	return `{"native":{"profile_message":{` + profileMessage + `},"skill":{},"async":{},"subagent":{` + subagent +
		`"agents":{"explore":{"description":"Finds facts.","tools":["skill","profile_message"]},"finder":{"description":"Reads notes.","tools":["skill"]}}}},"mcp":{}}`
}

// mixedChainScript answers dev/coder (model-small) with a sub-agent call whose child messages
// review/reviewer, and review/reviewer (model-large) with a call of reviewerAgent whose task is
// reviewerTask; reviewerChild answers that child's first request.
func mixedChainScript(reviewerAgent, reviewerTask string, reviewerChild func() string) func(model, body string) string {
	return func(model, body string) string {
		switch {
		case strings.Contains(body, "function_call_output"):
			return ""
		case model == "model-small" && strings.Contains(body, "ask the reviewer"):
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
		case model == "model-small":
			return layoutCall("call-sub", "subagent", map[string]any{"agent": "explore", "task": "ask the reviewer"})
		case strings.Contains(body, reviewerTask):
			return reviewerChild()
		default:
			return layoutCall("call-rsub", "subagent", map[string]any{"agent": reviewerAgent, "task": reviewerTask})
		}
	}
}

type mixedRunManifest struct {
	Owner, Agent, Task, State, Reason string
}

// mixedRunManifests reads every sub-agent run manifest under the fixture's folders, sorted by task.
func mixedRunManifests(t *testing.T, f *layoutFixture) []mixedRunManifest {
	t.Helper()
	var runs []mixedRunManifest
	for _, root := range []string{f.cwd} { // f.root is inside it
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Base(filepath.Dir(path)) != ".runs" || filepath.Ext(path) != ".json" {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var run mixedRunManifest
			if err := json.Unmarshal(data, &run); err != nil {
				return err
			}
			runs = append(runs, run)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Task < runs[j].Task })
	return runs
}

func runMixedChain(t *testing.T, f *layoutFixture) string {
	t.Helper()
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
	if status != http.StatusOK {
		t.Fatalf("run: %d %s", status, body)
	}
	return body
}

// A -> sub-agent -> B -> sub-agent -> A is refused as a cycle at the second profile_message:
// the child of review/reviewer is checked against the chain review/reviewer's run recorded
// (dev/coder, review/reviewer), and the sub-agent levels in between add no node. Since R6 the
// depth is counted along the chain (design §3.5): the reviewer's child runs at depth 3, so the
// test sets max_depth 3 (before R6 it ran at depth 1 and max_depth 1 was enough; the goldens are
// the same, as no tool definition depends on max_depth above 2). With less the reviewer's call is
// refused instead (TestMixedDepthIsCountedAcrossAProfileHop).
func TestRunBaselineMixedChainCycleThroughSubagents(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", `"max_depth":3,`))
	f.captureBodies = true
	f.script = mixedChainScript("explore", "ask the coder", func() string {
		return layoutCall("call-back", "profile_message", map[string]any{"profile": "dev/coder", "message": "back to you"})
	})
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	transcripts := runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
	if strings.Count(transcripts, "=== ") != 4 || strings.Count(transcripts, "profile_cycle") == 0 {
		t.Fatalf("want the coder, reviewer and two child transcripts with the refused call back:\n%s", transcripts)
	}
	for _, run := range mixedRunManifests(t, f) {
		if run.State != "done" {
			t.Fatalf("run %+v did not end done", run)
		}
	}
	compareRunBaseline(t, "mixed_chain_cycle_transcripts.txt", transcripts)
	compareRunBaseline(t, "mixed_chain_cycle_tools.txt", requestTools(t, f.capturedBodies(), m))
}

// stopMixedChain runs dev/coder -> explore -> review/reviewer -> reviewerAgent and stops dev/coder
// once the reviewer's child has sent its request (which is then held until the test ends). It
// returns once the coder's run has ended and the reviewer's transcript is free.
func stopMixedChain(t *testing.T, reviewerAgent string) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	// max_depth 3 leaves room for the reviewer's child, which runs at depth 3 (R6).
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", `"max_depth":3,`))
	reached := make(chan struct{}, 1)
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	f.script = mixedChainScript(reviewerAgent, "look around", func() string {
		reached <- struct{}{}
		<-released
		return ""
	})
	done := make(chan string, 1)
	go func() {
		_, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
		done <- body
	}()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the reviewer's child never sent its request")
	}
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("dev/coder"), `{}`); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the coder's run did not end after the stop")
	}
	awaitTargetSessionFree(t, f)
	return f
}

// awaitMixedRuns polls the run manifests until want accepts them or 10 s pass.
func awaitMixedRuns(t *testing.T, f *layoutFixture, want func([]mixedRunManifest) bool) []mixedRunManifest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs := mixedRunManifests(t, f)
		if want(runs) || time.Now().After(deadline) {
			return runs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A user stop of dev/coder reaches through the persistent hop as far as calls wait: the
// coder's child is stopped, its waiting profile_message call stops review/reviewer's
// run (caller_cancelled, profileMessageTool.execute), and that run's waiting writer child is
// cancelled with it. Since R5c the stop's lineage fan-out (Resolver.cancelDescendants) reaches
// that child directly too, as work started under dev/coder: its reason is user_stop when the
// fan-out lands first, or empty when the cancel down the waiting calls does (it carries none).
// The coder's child's reason is user_stop or empty for the same kind of race (handleRunStop stops
// the run before it fans out), so neither is pinned beyond those two.
func TestMixedChainUserStopReachesTheTargetsWaitingChild(t *testing.T) {
	f := stopMixedChain(t, "explore")
	runs := awaitMixedRuns(t, f, func(runs []mixedRunManifest) bool {
		return len(runs) == 2 && runs[0].State == "stopped" && runs[1].State == "stopped"
	})
	if len(runs) != 2 || runs[0].State != "stopped" || !strings.HasSuffix(runs[0].Owner, "dev/coder") ||
		runs[1].State != "stopped" || (runs[1].Reason != "" && runs[1].Reason != "user_stop") || !strings.HasSuffix(runs[1].Owner, "review/reviewer") {
		t.Fatalf("runs = %+v, want both children stopped, the reviewer's by user_stop or without a reason", runs)
	}
}

// The reviewer's child that runs as a handle outlives its call and is owned by review/reviewer,
// so the owner fan-out of dev/coder's stop does not name it (the pre-R5c gap). Its lineage does:
// it was started under dev/coder's session key, so the stop's lineage fan-out reaches it
// (user_stop), or the stop of the reviewer's run through its message does (caller_cancelled),
// whichever lands first.
func TestMixedChainUserStopReachesTheTargetsHandleChild(t *testing.T) {
	f := stopMixedChain(t, "finder")
	runs := awaitMixedRuns(t, f, func(runs []mixedRunManifest) bool {
		return len(runs) == 2 && runs[0].State == "stopped" && runs[1].State == "stopped"
	})
	if len(runs) != 2 || runs[0].State != "stopped" || runs[1].Agent != "finder" || runs[1].State != "stopped" ||
		(runs[1].Reason != "user_stop" && runs[1].Reason != "caller_cancelled") {
		t.Fatalf("runs = %+v, want the coder's child and the reviewer's handle child stopped", runs)
	}
}
