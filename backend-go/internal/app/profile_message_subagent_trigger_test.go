package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// subagentTriggerScript scripts a three-level tree on the real service. The session's profile
// (the parent, first user text "go") starts the "mid" sub-agent; mid messages the reviewer, then
// starts the "leaf" sub-agent; leaf messages the reviewer. Each role is told by the first user
// text of its request (a child's task), and each makes its next call from the number of tool
// outputs its request already carries. A role whose call comes back refused ends the tree early,
// so the test sees the refusal in the reviewer's transcript or its bodies.
func subagentTriggerScript(midMessage, leafMessage string) func(model, body string) string {
	call := func(name string, arguments map[string]any, id string) string {
		encoded, _ := json.Marshal(arguments)
		item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": id, "name": name, "arguments": string(encoded)})
		return string(item)
	}
	return func(model, body string) string {
		if model == "model-large" {
			return ""
		}
		var request struct {
			Input []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if json.Unmarshal([]byte(body), &request) != nil {
			return ""
		}
		// The current task is the last user message; its tool outputs are the ones after it.
		task, outputs := "", 0
		for _, item := range request.Input {
			if item.Type == "message" && item.Role == "user" && len(item.Content) > 0 {
				task, outputs = item.Content[0].Text, 0
			}
			if item.Type == "function_call_output" {
				outputs++
			}
		}
		switch {
		case strings.Contains(task, "MIDTASK"):
			switch outputs {
			case 0:
				return call("profile_message", map[string]any{"profile": "review/reviewer", "message": midMessage}, "mid-pm")
			case 1:
				return call("subagent", map[string]any{"agent": "leaf", "task": "LEAFTASK"}, "mid-sub")
			}
		case strings.Contains(task, "LEAFTASK"):
			if outputs == 0 {
				return call("profile_message", map[string]any{"profile": "review/reviewer", "message": leafMessage}, "leaf-pm")
			}
		default:
			if outputs == 0 {
				return call("subagent", map[string]any{"agent": "mid", "task": "MIDTASK"}, "parent-sub")
			}
		}
		return ""
	}
}

// subagentTriggerFixture is the layout session whose profile can start sub-agents that message
// the reviewer, with a context_trigger of the given action over a reviewer that holds history.
func subagentTriggerFixture(t *testing.T, trigger map[string]any) (*layoutFixture, string) {
	t.Helper()
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", string(mustJSON(t, map[string]any{
		"native": map[string]any{
			"profile_message": map[string]any{"context_trigger": trigger},
			"skill":           map[string]any{},
			"async":           map[string]any{},
			"subagent": map[string]any{"agents": map[string]any{
				"mid":  map[string]any{"tools": []string{"profile_message", "subagent"}},
				"leaf": map[string]any{"tools": []string{"profile_message"}},
			}},
		},
		"mcp": map[string]any{},
	})))
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	return f, messages
}

// reviewerBodies are the requests the reviewer's model received, in order.
func reviewerBodies(f *layoutFixture) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, body := range f.bodies {
		if strings.Contains(body, `"model":"model-large"`) {
			out = append(out, body)
		}
	}
	return out
}

// The end-to-end case: a sub-agent and its own sub-agent message the reviewer through the real
// launcher, with the reset trigger on. The tree belongs to one caller run, so the reviewer is
// reset once, at the first message (the child's), and the grandchild's message finds the
// child's. Without the caller run id on the sub-agents' contexts, each would be refused.
func TestContextTriggerResetRunsOncePerCallerRunAcrossSubagentsAndNestedSubagents(t *testing.T) {
	f, messages := subagentTriggerFixture(t, map[string]any{"action": "reset"})
	f.script = subagentTriggerScript("mid-says-hello", "leaf-says-hello")
	status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"go"}`)
	if status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	bodies := reviewerBodies(f)
	if len(bodies) != 2 {
		t.Fatalf("reviewer requests = %d, want 2 (the child's and the grandchild's):\n%v", len(bodies), bodies)
	}
	if strings.Contains(bodies[0], "old-secret") || !strings.Contains(bodies[0], "mid-says-hello") {
		t.Fatalf("the child's message did not reset the reviewer first:\n%s", bodies[0])
	}
	if strings.Contains(bodies[1], "old-secret") || !strings.Contains(bodies[1], "mid-says-hello") || !strings.Contains(bodies[1], "leaf-says-hello") {
		t.Fatalf("the grandchild's message reset again (or saw the old history):\n%s", bodies[1])
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "old-secret") || !strings.Contains(string(data), "leaf-says-hello") {
		t.Fatalf("reviewer's messages.jsonl:\n%s", data)
	}

	// The next run of the caller is a new caller run: it resets again.
	f.mu.Lock()
	f.bodies = nil
	f.mu.Unlock()
	f.script = subagentTriggerScript("second-run-mid", "second-run-leaf")
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("second run: %d %s", status, body)
	}
	bodies = reviewerBodies(f)
	if len(bodies) != 2 || strings.Contains(bodies[0], "mid-says-hello") || !strings.Contains(bodies[0], "second-run-mid") || !strings.Contains(bodies[1], "second-run-mid") || !strings.Contains(bodies[1], "second-run-leaf") {
		t.Fatalf("second run's reviewer requests:\n%v", bodies)
	}
	if len(f.service.resolver.callerRuns.runs) != 0 {
		t.Fatalf("caller runs left behind: %v", f.service.resolver.callerRuns.runs)
	}
}

// The same tree with the trigger off sends both messages to the reviewer's history untouched.
func TestContextTriggerOffLeavesTheReviewerAloneForSubagents(t *testing.T) {
	f, messages := subagentTriggerFixture(t, map[string]any{"action": "off"})
	f.script = subagentTriggerScript("mid-says-hello", "leaf-says-hello")
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	if data, _ := os.ReadFile(messages); !strings.Contains(string(data), "old-secret") || !strings.Contains(string(data), "leaf-says-hello") {
		t.Fatalf("reviewer's messages.jsonl:\n%s", data)
	}
}
