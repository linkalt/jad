package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// R5c/R6 cascade through the lineage of a run. dev/coder's sub-agent explore messages
// review/reviewer (in the same session, or in the session "other"), whose run starts finder as a
// handle and ends; finder goes on after the whole chain has ended ("look around"). A stop, reset
// or delete of dev/coder, or a stop of the message or of the sub-agent between, reaches finder
// through its lineage; review/reviewer's own work ("watch the logs") and its transcript are left
// alone.

// lastInputItem is the last input item of a provider request body as JSON text: the newest
// message or tool output, whatever history comes before it.
func lastInputItem(t *testing.T, body string) string {
	t.Helper()
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil || len(request.Input) == 0 {
		return body
	}
	return string(request.Input[len(request.Input)-1])
}

var (
	lineageMessageID  = regexp.MustCompile(`pm_[0-9a-f]{8}`)
	lineageSubagentID = regexp.MustCompile(`sub_[0-9a-f]+`)
)

// lineageChain holds the signals of one fixture's script.
type lineageChain struct {
	f *layoutFixture
	// finder announces the chain's finder request, own the reviewer's own finder request; both
	// are then held until the test ends.
	finder, own chan struct{}
	released    chan struct{}
	// targetHeld announces the reviewer's run holding after it started finder (holdTarget).
	targetHeld chan struct{}
	// exploreHeld announces explore holding after the message returned (holdExplore).
	exploreHeld chan struct{}
}

type lineageOptions struct {
	// target is the profile explore messages: review/reviewer or other/review/reviewer.
	target string
	// coderMessages makes dev/coder message the target itself as a handle (no explore) and stop
	// that handle once finder runs; holdTarget keeps the target's run going meanwhile.
	coderMessages bool
	// coderStopsExplore starts explore as a handle and stops it once finder runs; holdExplore
	// keeps explore going after its message returned.
	coderStopsExplore bool
}

func newLineageChain(t *testing.T, options lineageOptions) *lineageChain {
	t.Helper()
	f := newLayoutFixture(t)
	// max_depth 3 leaves room for the target's child, which runs at depth 3 (R6).
	profileMessage, subagent := "", `"max_depth":3,`
	if strings.HasPrefix(options.target, "other/") {
		profileMessage = `"sessions":["*"]`
	}
	if options.coderStopsExplore {
		subagent += `"background_writers":true,"max_background_writers":2,`
	}
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset(profileMessage, subagent))
	if strings.HasPrefix(options.target, "other/") {
		addOtherSession(t, f, "other", "review")
	}
	c := &lineageChain{f: f, finder: make(chan struct{}, 1), own: make(chan struct{}, 1), released: make(chan struct{}),
		targetHeld: make(chan struct{}, 1), exploreHeld: make(chan struct{}, 1)}
	t.Cleanup(func() { close(c.released) })
	hold := func(signal chan struct{}) string {
		signal <- struct{}{}
		<-c.released
		return ""
	}
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		output := strings.Contains(last, "function_call_output")
		switch {
		case !output && strings.Contains(last, "look around"):
			return hold(c.finder)
		case !output && strings.Contains(last, "watch the logs"):
			return hold(c.own)
		case model == "model-small" && output && strings.Contains(last, "call-stop"):
			return ""
		case model == "model-small" && output && options.coderMessages && strings.Contains(last, "call-pm"):
			<-c.finder
			c.finder <- struct{}{}
			return layoutCall("call-stop", "async", map[string]any{"action": "stop", "id": lineageMessageID.FindString(last)})
		case model == "model-small" && output && options.coderStopsExplore && strings.Contains(last, "call-sub"):
			<-c.finder
			c.finder <- struct{}{}
			<-c.exploreHeld
			return layoutCall("call-stop", "async", map[string]any{"action": "stop", "id": lineageSubagentID.FindString(last)})
		case model == "model-small" && output && options.coderStopsExplore && strings.Contains(last, "call-pm"):
			return hold(c.exploreHeld)
		case model != "model-small" && output && options.coderMessages && strings.Contains(last, "call-rsub"):
			return hold(c.targetHeld)
		case output:
			return ""
		case model == "model-small" && options.coderMessages:
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": options.target, "message": "please review", "block_ms": 0})
		case model == "model-small" && strings.Contains(last, "ask the reviewer"):
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": options.target, "message": "please review"})
		case model == "model-small":
			arguments := map[string]any{"agent": "explore", "task": "ask the reviewer"}
			if options.coderStopsExplore {
				arguments["block_ms"] = 0
			}
			return layoutCall("call-sub", "subagent", arguments)
		case strings.Contains(last, "work alone"):
			return layoutCall("call-own", "subagent", map[string]any{"agent": "finder", "task": "watch the logs", "block_ms": 0})
		default:
			return layoutCall("call-rsub", "subagent", map[string]any{"agent": "finder", "task": "look around", "block_ms": 0})
		}
	}
	return c
}

func (c *lineageChain) run(t *testing.T, session, profile, input string) {
	t.Helper()
	status, body := c.f.do(t, http.MethodPost, "/api/sessions/local:"+session+"/run?profile="+url.QueryEscape(profile), `{"input":`+quoteJSON(input)+`}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run of %s %s: %d %s", session, profile, status, body)
	}
}

// startOwnWork has review/reviewer of session start its own finder ("watch the logs") in a user
// run, which ends while that finder goes on.
func (c *lineageChain) startOwnWork(t *testing.T, session string) {
	t.Helper()
	c.run(t, session, "review/reviewer", "work alone")
	awaitSignal(t, c.own, "the reviewer's own finder request")
}

// startChain runs dev/coder to its end and returns once the chain's finder is running.
func (c *lineageChain) startChain(t *testing.T) {
	t.Helper()
	c.run(t, "demo", "dev/coder", "start the chain")
	awaitSignal(t, c.finder, "the chain's finder request")
	c.finder <- struct{}{}
	c.expect(t, "look around", "running", "")
}

// expect waits until the sub-agent run with task is in state (and reason, when the state is
// stopped), and fails after 10 s. A running state is checked again after a pause, so a stop on its
// way has time to land.
func (c *lineageChain) expect(t *testing.T, task, state, reason string) {
	t.Helper()
	find := func(runs []mixedRunManifest) (mixedRunManifest, bool) {
		for _, run := range runs {
			if run.Task == task {
				return run, true
			}
		}
		return mixedRunManifest{}, false
	}
	runs := awaitMixedRuns(t, c.f, func(runs []mixedRunManifest) bool {
		run, ok := find(runs)
		return ok && run.State == state
	})
	if state == "running" {
		time.Sleep(300 * time.Millisecond)
		runs = mixedRunManifests(t, c.f)
	}
	run, ok := find(runs)
	if !ok || run.State != state || (state == "stopped" && reason != "*" && run.Reason != reason) {
		t.Fatalf("run %q = %+v (found %v), want %s %s; all runs %+v", task, run, ok, state, reason, runs)
	}
}

func (c *lineageChain) transcript(t *testing.T, session, profile string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.f.cwd, ".jad", "state", session, filepath.FromSlash(profile), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (c *lineageChain) post(t *testing.T, path string) {
	t.Helper()
	if status, body := c.f.do(t, http.MethodPost, path, `{}`); status != http.StatusOK {
		t.Fatalf("%s: %d %s", path, status, body)
	}
}

// A stop of dev/coder after its chain has ended reaches the finder the reviewer's run left going:
// the coder's run, its explore child and the reviewer's run are all over by then, so nothing waits
// that could carry the cancel; only the lineage reaches it. The reviewer's own finder is left alone.
func TestLineageUserStopReachesWorkTheChainLeftGoing(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer"})
	c.startOwnWork(t, "demo")
	c.startChain(t)
	c.post(t, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("dev/coder"))
	c.expect(t, "look around", "stopped", "user_stop")
	c.expect(t, "watch the logs", "running", "")
}

// A reset of dev/coder stops the work its chain left going in review/reviewer, and resets neither
// the reviewer's transcript nor its own work.
func TestLineageResetReachesWorkTheChainLeftGoingAndKeepsTheTargetsTranscript(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer"})
	c.startOwnWork(t, "demo")
	c.startChain(t)
	reviewer := c.transcript(t, "demo", "review/reviewer")
	c.post(t, "/api/sessions/local:demo/reset?profile="+url.QueryEscape("dev/coder"))
	c.expect(t, "look around", "stopped", "session_reset")
	c.expect(t, "watch the logs", "running", "")
	if got := c.transcript(t, "demo", "review/reviewer"); got != reviewer {
		t.Fatalf("the reviewer's transcript changed with the coder's reset:\n%s\nwant\n%s", got, reviewer)
	}
	if got := c.transcript(t, "demo", "dev/coder"); strings.Contains(got, "start the chain") {
		t.Fatalf("the coder's transcript was not reset:\n%s", got)
	}
}

// A delete of session demo reaches the work its chain left going in session other, and leaves
// other's own work and transcript alone.
func TestLineageSessionDeleteReachesWorkTheChainLeftInAnotherSession(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "other/review/reviewer"})
	c.startOwnWork(t, "other")
	c.startChain(t)
	reviewer := c.transcript(t, "other", "review/reviewer")
	current := c.f.catalogCall(t, http.MethodGet, "/api/catalog/sessions/demo", "", nil)
	if reply := c.f.catalogCall(t, http.MethodDelete, "/api/catalog/sessions/demo", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", reply.status, reply.body)
	}
	c.expect(t, "look around", "stopped", "session_delete")
	c.expect(t, "watch the logs", "running", "")
	if got := c.transcript(t, "other", "review/reviewer"); got != reviewer {
		t.Fatalf("the other session's transcript changed with the delete of demo")
	}
}

// A stop of the message itself (the coder's async stop of its profile_message handle) ends the
// reviewer's run and the finder that run started; no stop of dev/coder is involved.
func TestLineageStopOfAMessageReachesTheTargetRunsWork(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer", coderMessages: true})
	c.run(t, "demo", "dev/coder", "start the chain")
	awaitSignal(t, c.targetHeld, "the reviewer's run holding")
	c.expect(t, "look around", "stopped", "stopped_by_agent")
}

// A stop of explore (the coder's async stop of its handle) reaches the finder of the reviewer's
// run that explore's message started, after that run has ended; no stop of dev/coder is involved.
func TestLineageStopOfASubagentReachesTheWorkOfTheRunItMessaged(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer", coderStopsExplore: true})
	c.run(t, "demo", "dev/coder", "start the chain")
	c.expect(t, "ask the reviewer", "stopped", "stopped_by_agent")
	c.expect(t, "look around", "stopped", "stopped_by_agent")
}
