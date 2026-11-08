package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/webui"
)

// TestRenderedSubagentResultsSayWhetherTheyOmitTheChildsLastText pins the contract between
// renderRun and the browser's subagent transcript (webui.SubagentCallOf): only a result
// whose content is the child's last text, uncut, lets the browser leave that text out of
// the transcript as a copy. A finished, stopped or timed-out result with its text does; a
// cut one, a handed-off one (run id only), a failed or empty one (no text) and a lost one
// (its text can come from a step the snapshot leaves out) do not.
func TestRenderedSubagentResultsSayWhetherTheyOmitTheChildsLastText(t *testing.T) {
	text := SubagentResult{Text: "the last text", Rounds: 2, Tokens: 9}
	cases := []struct {
		name string
		run  subagentRunView
		want bool
	}{
		{"finished", subagentRunView{state: subagentRunDone, result: text}, false},
		{"finished with changes", subagentRunView{state: subagentRunDone, result: text, tracked: true, changes: true, files: []subagentFileChange{{Path: "a.go", Action: "created"}}}, false},
		{"finished and cut", subagentRunView{state: subagentRunDone, result: text, cut: true}, true},
		{"finished without text", subagentRunView{state: subagentRunDone}, true},
		{"stopped", subagentRunView{state: subagentRunStopped, result: text}, false},
		{"stopped and cut", subagentRunView{state: subagentRunStopped, result: text, cut: true}, true},
		{"stopped without text", subagentRunView{state: subagentRunStopped}, false},
		{"timed out", subagentRunView{state: subagentRunTimedOut, result: text}, false},
		{"timed out and cut", subagentRunView{state: subagentRunTimedOut, result: text, cut: true}, true},
		{"lost", subagentRunView{state: subagentRunLost, result: text}, true},
		{"failed", subagentRunView{state: subagentRunFailed, result: text, reason: "provider error"}, true},
		{"queued", subagentRunView{state: subagentRunQueued, id: "sub_0a1b2c3d"}, true},
		{"running", subagentRunView{state: subagentRunRunning, id: "sub_0a1b2c3d", result: text}, true},
	}
	arguments := `{"agent":"explore","task":"look"}`
	for _, tc := range cases {
		tc.run.agent, tc.run.dir = "explore", "/store/c-0a1b2c3d"
		output, err := (&subagentTool{}).renderRun("c", tc.run)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		call, ok := webui.SubagentCallOf(
			webui.Part{Type: "tool_call", CallID: "c", Name: "subagent", Arguments: &arguments},
			webui.Part{Type: "tool_result", CallID: "c", Output: output},
		)
		if !ok || call.OmitsLastText != tc.want || call.Agent != "explore" || !strings.HasSuffix(call.Transcript, "messages.jsonl") {
			t.Errorf("%s: call = %+v, %v; want omits last text %v\n%s", tc.name, call, ok, tc.want, output)
		}
	}
}
