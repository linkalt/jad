package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// assistantText is a provider answer that ends the round with text.
func assistantText(text string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
}

// T-007 regression, end to end: a profile_message target's tool_smith script that goes on as a
// handle after the target's run ended (the handle is owned by the target's session, so the run's
// end leaves it) calls profile_message through the script bridge. The call carries the ended run's
// tree node; it is admitted below that node and answered instead of failing closed
// (profile_unavailable), and the node leaves the tree once the script ended.
func TestALateProfileMessageFromATargetsBackgroundScriptJoinsItsTree(t *testing.T) {
	interpreter := appToolSmithInterpreter(t)
	f := toolSmithFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{},"async":{}},"mcp":{}}`)
	f.overwrite(t, "tools/coding/review.json", `{"native":{"profile_message":{},"async":{},"tool_smith":{"interpreter":`+interpreter+`,"tools":["profile_message"]}},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/review"}`)
	gate, out := filepath.Join(f.cwd, "gate"), filepath.Join(f.cwd, "late.json")
	code := `const fs = await import("node:fs");
while (!fs.existsSync(input.gate)) { await new Promise((r) => setTimeout(r, 20)); }
const r = await tools.profile_message({ profile: "dev/coder", message: "late hello", block_ms: 30000 });
fs.writeFileSync(input.out, JSON.stringify(r));`
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		output := strings.Contains(last, "function_call_output")
		switch {
		case strings.Contains(last, "late hello"):
			return assistantText("answer late")
		case output:
			return assistantText("done")
		case model == "model-small":
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
		default:
			return layoutCall("call-smith", "tool_smith", map[string]any{"code": code, "input": map[string]any{"gate": gate, "out": out}, "block_ms": 0})
		}
	}
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	awaitTargetSessionFree(t, f)
	r := f.service.resolver
	// Once the run released the transcript its node is no longer counted: it is either ended
	// (still a parent of the script) or gone.
	pollFor(t, "the reviewer's message run to end while its script goes", func() bool {
		for _, node := range r.runTree.Nodes() {
			if node.State != native.RunNodeEnded {
				return false
			}
		}
		return true
	})
	nodes := r.runTree.Nodes()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var result string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(out); err == nil && len(data) > 0 {
			result = string(data)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the script's late profile_message never returned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(result, "answer late") || strings.Contains(result, "profile_unavailable") {
		t.Fatalf("the late profile_message of the ended run's script = %s", result)
	}
	if len(nodes) != 1 || nodes[0].Kind != native.RunNodeProfileMessage || nodes[0].State != native.RunNodeEnded {
		t.Fatalf("while its script goes the ended message run must stay an ended parent: %+v", nodes)
	}
	awaitEmptyRunTree(t, r)
}
