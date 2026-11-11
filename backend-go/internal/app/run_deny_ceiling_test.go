package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
)

// R6 U3 (design §4, "inherited like the sandbox ceiling"): a caller's tools.deny narrows every
// local run it starts, at every level and across both kinds of run, and each level adds its own
// deny. A profile_message target is narrowed for that run only: the profile's settings and its
// other runs keep its own tools. Allow lists are not carried.

// bodyToolNames is the names in a provider request's tools array.
func bodyToolNames(t *testing.T, body string) []string {
	t.Helper()
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("request body: %v", err)
	}
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// toolsOfRequests is the tool names of every captured request that match picks, in order.
func toolsOfRequests(t *testing.T, f *layoutFixture, picks func(body string) bool) [][]string {
	t.Helper()
	var all [][]string
	for _, body := range f.capturedBodies() {
		if picks(body) {
			all = append(all, bodyToolNames(t, body))
		}
	}
	return all
}

// denyChainToolset is mixedChainToolset with explore denying async and finder with no tools list,
// so finder has every tool its caller's ceiling leaves.
func denyChainToolset(profileMessage string) string {
	return `{"native":{"profile_message":{` + profileMessage + `},"skill":{},"async":{},"subagent":{"max_depth":3,` +
		`"agents":{"explore":{"description":"Finds facts.","tools":{"deny":["async"]}},"finder":{"description":"Reads notes."}}}},"mcp":{}}`
}

func firstRequest(body string) bool { return !strings.Contains(body, "function_call_output") }

// dev/coder -> explore (denies async) -> review/reviewer (denies skill) -> finder: the reviewer's
// run lacks async, which explore denied, besides its own skill, and finder lacks both. The same
// profile and agent run directly by the user keep async: the narrowing was that run's only, and
// the reviewer's history goes on in one transcript. The cross-session case crosses a session too.
func TestDenyCeilingCrossesEphemeralAndPersistentHops(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture func(*testing.T) *layoutFixture
		session string
		script  func(string, string, func() string) func(string, string) string
		pm      string
	}{
		"same session":  {newLayoutFixture, "demo", mixedChainScript, ""},
		"other session": {crossSessionFixture, "other", crossSessionScript, `"sessions":["*"]`},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.fixture(t)
			f.overwrite(t, "tools/coding/none.json", denyChainToolset(tc.pm))
			f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/none","tools":{"deny":["skill"]}}`)
			f.captureBodies = true
			chain := tc.script("finder", "look around", func() string { return "" })
			f.script = func(model, body string) string {
				// The user's own reviewer run starts finder too; its history holds tool outputs, so
				// the chain's script would answer it in text.
				if strings.Contains(body, "direct review") && !strings.Contains(body, "call-direct") {
					return layoutCall("call-direct", "subagent", map[string]any{"agent": "finder", "task": "look around"})
				}
				return chain(model, body)
			}
			if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
				t.Fatalf("chain did not complete:\n%s", body)
			}
			awaitTargetSessionFree(t, f)
			large := func(body string) bool { return strings.Contains(body, `"model":"model-large"`) }
			chained := func(body string) bool {
				return large(body) && strings.Contains(body, "please review") && !strings.Contains(body, "direct review")
			}
			direct := func(body string) bool { return large(body) && strings.Contains(body, "direct review") }
			finder := func(body string) bool {
				return large(body) && strings.Contains(body, "look around") && !strings.Contains(body, "please review") && !strings.Contains(body, "direct review")
			}
			explore := func(body string) bool {
				return strings.Contains(body, "ask the reviewer") && !strings.Contains(body, "start the chain")
			}
			requireTools(t, "explore", toolsOfRequests(t, f, explore), []string{"profile_message", "skill", "subagent"}, []string{"async"})
			requireTools(t, "the reviewer's message run", toolsOfRequests(t, f, chained), []string{"profile_message", "subagent"}, []string{"async", "skill"})
			// finder runs at depth 3, the deepest level, so it has no subagent tool.
			requireTools(t, "the reviewer's finder", toolsOfRequests(t, f, finder), []string{"profile_message"}, []string{"async", "skill"})

			path := "/api/sessions/local:" + tc.session + "/run?profile=" + url.QueryEscape("review/reviewer")
			if status, body := f.do(t, http.MethodPost, path, `{"input":"direct review"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
				t.Fatalf("direct reviewer run: %d %s", status, body)
			}
			finders := toolsOfRequests(t, f, finder)
			if len(finders) != 2 {
				t.Fatalf("finder requests: %d, want one per run", len(finders))
			}
			requireTools(t, "the reviewer's own run", toolsOfRequests(t, f, direct), []string{"async", "profile_message", "subagent"}, []string{"skill"})
			requireTools(t, "the finder of the reviewer's own run", finders[1:], []string{"async", "profile_message", "subagent"}, []string{"skill"})
			history, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", tc.session, "review", "reviewer", "messages.jsonl"))
			if err != nil || !strings.Contains(string(history), "please review") || !strings.Contains(string(history), "direct review") {
				t.Fatalf("the reviewer's transcript does not hold both runs (%v):\n%s", err, history)
			}
		})
	}
}

// requireTools checks that there are requests (a run's requests all carry the same tools) and that
// each has every name of has and none of lacks.
func requireTools(t *testing.T, who string, requests [][]string, has, lacks []string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatalf("%s: no request", who)
	}
	for _, names := range requests {
		for _, name := range has {
			if !slices.Contains(names, name) {
				t.Errorf("%s: tools %v lack %s", who, names, name)
			}
		}
		for _, name := range lacks {
			if slices.Contains(names, name) {
				t.Errorf("%s: tools %v hold %s, which the chain denies", who, names, name)
			}
		}
	}
}

// A target whose every tool the caller denies runs text-only and answers; the caller gets the
// answer, and the target's own run keeps its tool.
func TestDenyCeilingLeavesATextOnlyTargetThatStillAnswers(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{},"skill":{}},"mcp":{}}`)
	f.overwrite(t, "profiles/dev/coder.json", `{"models":["openai/small"],"toolset":"coding/none","tools":{"deny":["skill"]}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/none","tools":{"allow":["skill"]}}`)
	f.captureBodies = true
	f.script = func(model, body string) string {
		if model == "model-small" && firstRequest(body) {
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
		}
		if model == "model-large" {
			return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reviewed in text"}]}`
		}
		return ""
	}
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	awaitTargetSessionFree(t, f)
	large := func(body string) bool { return strings.Contains(body, `"model":"model-large"`) }
	if got := toolsOfRequests(t, f, large); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("the narrowed target's requests carry tools %v, want one request with none", got)
	}
	answered := false
	for _, body := range f.capturedBodies() {
		answered = answered || (strings.Contains(body, `"model":"model-small"`) && strings.Contains(body, "reviewed in text"))
	}
	if !answered {
		t.Fatal("the caller did not get the text-only target's answer")
	}
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"direct"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("direct reviewer run: %d %s", status, body)
	}
	if got := toolsOfRequests(t, f, large); len(got) != 2 || !slices.Equal(got[1], []string{"skill"}) {
		t.Fatalf("the target's own run has tools %v, want skill", got)
	}
}

// A target whose runner is not one resolved from its settings cannot be narrowed: with a deny the
// start fails rather than run it whole; without one it runs as before.
func TestTargetRunnerRefusesARunnerItCannotNarrowByDeny(t *testing.T) {
	other := struct{ httpapi.RunSession }{}
	if _, err := targetRunner(nil, "dev/coder", "m", nil, nil, []string{"exec"}, nil)(other); err == nil || !strings.Contains(err.Error(), "tools.deny") {
		t.Fatalf("with a deny: err = %v, want a refusal", err)
	}
	if runner, err := targetRunner(nil, "dev/coder", "m", nil, nil, nil, nil)(other); err != nil || runner != other {
		t.Fatalf("without a deny: %v %v", runner, err)
	}
}

// persistent -> persistent: dev/coder denies async; review/reviewer, which it messages, and
// review/helper, which the reviewer messages, both run without async, and helper's own run has it.
func TestDenyCeilingCrossesTwoProfileHops(t *testing.T) {
	f := profileDepthFixture(t, `{"native":{"profile_message":{"max_chain":8},"async":{}},"mcp":{}}`, "")
	f.overwrite(t, "profiles/dev/coder.json", `{"models":["openai/small"],"toolset":"coding/none","tools":{"deny":["async"]}}`)
	f.captureBodies = true
	f.script = profileHopScript(map[string]profileHop{"start the chain": {"review/reviewer", "HOP-1"}, "HOP-1": {"review/helper", "HOP-2"}})
	runProfileChain(t, f)
	reviewer := func(body string) bool {
		return strings.Contains(body, "HOP-1") && !strings.Contains(body, "start the chain")
	}
	helper := func(body string) bool { return strings.Contains(body, "HOP-2") && !strings.Contains(body, "HOP-1") }
	requireTools(t, "review/reviewer", toolsOfRequests(t, f, reviewer), []string{"profile_message"}, []string{"async"})
	requireTools(t, "review/helper", toolsOfRequests(t, f, helper), []string{"profile_message"}, []string{"async"})
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("review/helper"), `{"input":"own run"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("helper's own run: %d %s", status, body)
	}
	own := func(body string) bool { return strings.Contains(body, "own run") }
	requireTools(t, "review/helper's own run", toolsOfRequests(t, f, own), []string{"async", "profile_message"}, nil)
}
