package app

import (
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// R5 pre-change characterisation of mixed chains across sessions (profile_message sessions): a
// chain node carries its session, and stop fans out per session and profile, so a hop into
// another session behaves as a hop into another profile of the same session.

// crossSessionScript is mixedChainScript with the coder's child messaging review/reviewer of the
// session "other", and that profile's child messaging dev/coder of "demo" or running reviewerChild.
func crossSessionScript(reviewerAgent, reviewerTask string, reviewerChild func() string) func(model, body string) string {
	return func(model, body string) string {
		switch {
		case strings.Contains(body, "function_call_output"):
			return ""
		case model == "model-small" && strings.Contains(body, "ask the reviewer"):
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "other/review/reviewer", "message": "please review"})
		case model == "model-small":
			return layoutCall("call-sub", "subagent", map[string]any{"agent": "explore", "task": "ask the reviewer"})
		case strings.Contains(body, reviewerTask):
			return reviewerChild()
		default:
			return layoutCall("call-rsub", "subagent", map[string]any{"agent": reviewerAgent, "task": reviewerTask})
		}
	}
}

func crossSessionFixture(t *testing.T) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	// max_depth 3 leaves room for the other session's child, which runs at depth 3 (R6).
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset(`"sessions":["*"]`, `"max_depth":3,`))
	addOtherSession(t, f, "other", "review")
	return f
}

// demo dev/coder -> sub-agent -> other review/reviewer -> sub-agent -> demo dev/coder is refused
// profile_cycle: the chain the reviewer's run recorded names demo's dev/coder with its session.
func TestRunBaselineMixedChainCycleAcrossSessions(t *testing.T) {
	f := crossSessionFixture(t)
	f.captureBodies = true
	f.script = crossSessionScript("explore", "ask the coder", func() string {
		return layoutCall("call-back", "profile_message", map[string]any{"profile": "demo/dev/coder", "message": "back to you"})
	})
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	transcripts := runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
	if strings.Count(transcripts, "=== ") != 4 || !strings.Contains(transcripts, "profile_cycle") || !strings.Contains(transcripts, "=== other/review/reviewer") {
		t.Fatalf("want the coder, the other session's reviewer and two child transcripts with the refused call back:\n%s", transcripts)
	}
	for _, run := range mixedRunManifests(t, f) {
		if run.State != "done" {
			t.Fatalf("run %+v did not end done", run)
		}
	}
	compareRunBaseline(t, "mixed_chain_session_transcripts.txt", transcripts)
}

// A stop of demo's dev/coder reaches the other session's waiting child through the waiting
// calls, as within one session, and (since R5c) the other session's handle child through its
// lineage: both were started under demo's dev/coder, whose session key their lineage carries,
// though other's review/reviewer owns them. The reasons depend on which path lands first: the
// lineage fan-out (user_stop), the cancel down the waiting calls (none) or the stop of the
// reviewer's run through its message (caller_cancelled).
func TestMixedChainAcrossSessionsUserStopReachesTheOtherSessionsChildren(t *testing.T) {
	for _, tc := range []struct {
		agent       string
		wantReasons []string
	}{
		{"explore", []string{"", "user_stop"}},
		{"finder", []string{"user_stop", "caller_cancelled"}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			f := crossSessionFixture(t)
			reached := make(chan struct{}, 1)
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			f.script = crossSessionScript(tc.agent, "look around", func() string {
				reached <- struct{}{}
				<-released
				return ""
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
			}()
			select {
			case <-reached:
			case <-time.After(20 * time.Second):
				t.Fatal("the other session's child never sent its request")
			}
			if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("dev/coder"), `{}`); status != http.StatusOK {
				t.Fatalf("stop: %d %s", status, body)
			}
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the coder's run did not end after the stop")
			}
			runs := awaitMixedRuns(t, f, func(runs []mixedRunManifest) bool {
				return len(runs) == 2 && runs[0].State == "stopped" && runs[1].State == "stopped"
			})
			if len(runs) != 2 || runs[0].State != "stopped" || !strings.HasSuffix(runs[0].Owner, "dev/coder") ||
				runs[1].Agent != tc.agent || runs[1].State != "stopped" || !slices.Contains(tc.wantReasons, runs[1].Reason) || !strings.Contains(runs[1].Owner, "other") {
				t.Fatalf("runs = %+v, want the coder's child and the other session's %s child stopped (reason one of %q)", runs, tc.agent, tc.wantReasons)
			}
		})
	}
}
