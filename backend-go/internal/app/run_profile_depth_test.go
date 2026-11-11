package app

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// R6 follow-up (design §3.5, U4): a profile_message counts against the caller config's max_depth
// like a sub-agent start. The run a message starts is one level below its caller, so a message
// whose run would pass max_depth is refused profile_depth_exceeded before anything is queued or
// started. max_chain stays a cap of its own: a message must pass both, and a cycle or a too long
// chain is reported before the depth.

// profileHops scripts a chain of profile messages: the run whose request carries a key's text
// (and no tool output yet) calls profile_message with the value's target and message. Every
// other request answers with no call.
type profileHop struct{ target, message string }

func profileHopScript(hops map[string]profileHop) func(model, body string) string {
	return func(model, body string) string {
		if strings.Contains(body, "function_call_output") {
			return ""
		}
		for marker, hop := range hops {
			if strings.Contains(body, marker) {
				return layoutCall("call-"+marker, "profile_message", map[string]any{"profile": hop.target, "message": hop.message})
			}
		}
		return ""
	}
}

// profileDepthFixture is the demo session with review/helper and review/fourth added, whose
// toolset is toolset, and whose profiles all carry tools (a profile tools block, or "").
func profileDepthFixture(t *testing.T, toolset, tools string) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", toolset)
	extra := ""
	if tools != "" {
		extra = `,"tools":` + tools
	}
	f.overwrite(t, "profiles/dev/coder.json", `{"models":["openai/small","openai/large"],"model_default":"openai/small","toolset":"coding/none"`+extra+`}`)
	for _, name := range []string{"reviewer", "helper", "fourth"} {
		f.overwrite(t, "profiles/review/"+name+".json", `{"models":["openai/large"],"toolset":"coding/none"`+extra+`}`)
	}
	return f
}

// fourHops is dev/coder (depth 0) -> review/reviewer (1) -> review/helper (2) -> review/fourth (3),
// and fourth answers.
var fourHops = map[string]profileHop{
	"start the chain": {"review/reviewer", "HOP-1"},
	"HOP-1":           {"review/helper", "HOP-2"},
	"HOP-2":           {"review/fourth", "HOP-3"},
}

// runProfileChain runs dev/coder on "start the chain" and returns the masked transcripts.
func runProfileChain(t *testing.T, f *layoutFixture) string {
	t.Helper()
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	return runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
}

// countBodies counts the provider requests that contain text.
func countBodies(f *layoutFixture, text string) int {
	n := 0
	for _, body := range f.capturedBodies() {
		if strings.Contains(body, text) {
			n++
		}
	}
	return n
}

// With no tools.native.subagent section the default max_depth 2 applies: the helper's run is at
// depth 2, so its message to fourth is refused, though max_chain 8 would allow the chain. The
// refusal comes before the start: fourth's history, which a context_trigger reset would delete,
// is left byte for byte, and fourth gets no request.
func TestProfileMessageIsRefusedPastTheDefaultMaxDepthWithoutASubagentSection(t *testing.T) {
	f := profileDepthFixture(t, `{"native":{"profile_message":{"max_chain":8,"context_trigger":{"action":"reset"}}},"mcp":{}}`, "")
	f.captureBodies = true
	f.script = profileHopScript(fourHops)
	history := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "fourth", "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(history), 0o755); err != nil {
		t.Fatal(err)
	}
	old := strings.ReplaceAll(profileUsageTranscript(2), "q", "fourth-old-q")
	if err := os.WriteFile(history, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	transcripts := runProfileChain(t, f)
	if !strings.Contains(transcripts, "profile_depth_exceeded") || strings.Contains(transcripts, "profile_chain_too_long") {
		t.Fatalf("want the helper's message refused profile_depth_exceeded:\n%s", transcripts)
	}
	if n := countBodies(f, "HOP-3"); n != 1 {
		t.Fatalf("%d requests carry HOP-3, want only the helper's own call (fourth must get none)", n)
	}
	if data, err := os.ReadFile(history); err != nil || string(data) != old {
		t.Fatalf("fourth's history changed (%v):\n%s", err, data)
	}
	if n := countBodies(f, "HOP-2"); n != 3 {
		t.Fatalf("%d requests carry HOP-2, want the reviewer's follow-up and the helper's two requests", n)
	}
}

// The caller's configured max_depth applies even when its subagent tool is not offered: the
// section sets max_depth 3 with no agent and the profiles deny the subagent tool, so fourth (depth
// 3) is reached, and nothing offers the subagent tool.
func TestProfileMessageDepthUsesTheConfiguredMaxDepthWithTheSubagentToolDenied(t *testing.T) {
	f := profileDepthFixture(t, `{"native":{"profile_message":{"max_chain":8},"async":{},"subagent":{"max_depth":3,"agents":{}}},"mcp":{}}`, `{"deny":["subagent"]}`)
	f.captureBodies = true
	var fourth atomic.Int32
	script := profileHopScript(fourHops)
	f.script = func(model, body string) string {
		if strings.Contains(body, "HOP-3") && !strings.Contains(body, "function_call_output") && !strings.Contains(body, "HOP-2") {
			fourth.Add(1)
		}
		return script(model, body)
	}
	transcripts := runProfileChain(t, f)
	if fourth.Load() != 1 || strings.Contains(transcripts, "profile_depth_exceeded") {
		t.Fatalf("fourth got %d requests, want 1 and no refusal:\n%s", fourth.Load(), transcripts)
	}
	if n := countBodies(f, `"name":"subagent"`); n != 0 {
		t.Fatalf("%d requests offer the subagent tool, want none (denied)", n)
	}
}

// A cycle and a too long chain are reported before the depth when both apply.
func TestProfileMessageCycleAndChainRefusalsComeBeforeTheDepth(t *testing.T) {
	for _, tc := range []struct {
		name, maxChain, want string
		hops                 map[string]profileHop
	}{
		// The helper (depth 2) messages dev/coder, which is in its chain: a cycle and too deep.
		{"cycle", "8", "profile_cycle", map[string]profileHop{
			"start the chain": {"review/reviewer", "HOP-1"},
			"HOP-1":           {"review/helper", "HOP-2"},
			"HOP-2":           {"dev/coder", "HOP-3"},
		}},
		// max_chain 3: the helper's message to fourth makes a chain of 4 and a run at depth 3.
		{"chain", "3", "profile_chain_too_long", fourHops},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := profileDepthFixture(t, `{"native":{"profile_message":{"max_chain":`+tc.maxChain+`}},"mcp":{}}`, "")
			f.script = profileHopScript(tc.hops)
			transcripts := runProfileChain(t, f)
			if !strings.Contains(transcripts, tc.want) || strings.Contains(transcripts, "profile_depth_exceeded") {
				t.Fatalf("want %s and no profile_depth_exceeded:\n%s", tc.want, transcripts)
			}
		})
	}
}

// Mixed: dev/coder's sub-agent (depth 1) messages the reviewer (depth 2), whose message to the
// helper would run at depth 3 and is refused at the default max_depth 2; with max_depth 3 the
// helper answers.
func TestProfileMessageDepthCountsTheSubagentLevelsBeforeIt(t *testing.T) {
	for _, tc := range []struct {
		name, subagent string
		refused        bool
	}{
		{"default", "", true},
		{"max_depth 3", `"max_depth":3,`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := profileDepthFixture(t, mixedChainToolset(`"max_chain":8`, tc.subagent), "")
			f.captureBodies = true
			f.script = func(model, body string) string {
				switch {
				case strings.Contains(body, "function_call_output"):
					return ""
				case strings.Contains(body, "please review"):
					return layoutCall("call-helper", "profile_message", map[string]any{"profile": "review/helper", "message": "HELPER-HOP"})
				case strings.Contains(body, "HELPER-HOP"):
					return ""
				case model == "model-small" && strings.Contains(body, "ask the reviewer"):
					return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
				case model == "model-small":
					return layoutCall("call-sub", "subagent", map[string]any{"agent": "explore", "task": "ask the reviewer"})
				}
				return ""
			}
			transcripts := runProfileChain(t, f)
			if refused := strings.Contains(transcripts, "profile_depth_exceeded"); refused != tc.refused {
				t.Fatalf("refused = %v, want %v:\n%s", refused, tc.refused, transcripts)
			}
			// The reviewer's follow-up carries its call; a helper that ran adds its one request.
			want := 2
			if tc.refused {
				want = 1
			}
			if n := countBodies(f, "HELPER-HOP"); n != want {
				t.Fatalf("%d requests carry HELPER-HOP, want %d:\n%s", n, want, transcripts)
			}
		})
	}
}

// Across sessions the depth counts on: demo's dev/coder (0) -> other's reviewer (1) -> other's
// helper (2), whose message to other's fourth is refused at the default max_depth.
func TestProfileMessageDepthIsCountedAcrossSessions(t *testing.T) {
	f := profileDepthFixture(t, `{"native":{"profile_message":{"max_chain":8,"sessions":["*"]}},"mcp":{}}`, "")
	addOtherSession(t, f, "other", "review")
	f.captureBodies = true
	f.script = profileHopScript(map[string]profileHop{
		"start the chain": {"other/review/reviewer", "HOP-1"},
		"HOP-1":           {"review/helper", "HOP-2"},
		"HOP-2":           {"review/fourth", "HOP-3"},
	})
	transcripts := runProfileChain(t, f)
	if !strings.Contains(transcripts, "=== other/review/helper") || !strings.Contains(transcripts, "profile_depth_exceeded") {
		t.Fatalf("want other's helper refused profile_depth_exceeded:\n%s", transcripts)
	}
	if n := countBodies(f, "HOP-3"); n != 1 {
		t.Fatalf("%d requests carry HOP-3, want only the helper's own call", n)
	}
}
