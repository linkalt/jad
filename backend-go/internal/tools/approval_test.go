package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func askingTool(t *testing.T, executed *atomic.Int32) Tool {
	t.Helper()
	tool := testTool(t)
	tool.Approval = ApprovalAsk
	tool.Execute = func(context.Context, map[string]any, ExecutionContext) (string, error) {
		executed.Add(1)
		return "ran", nil
	}
	return tool
}

func executeEcho(t *testing.T, catalog *Catalog, callID string, execution ExecutionContext) string {
	t.Helper()
	call := json.RawMessage(`{"type":"function_call","call_id":"` + callID + `","name":"echo","arguments":"{\"command\":\"hello\"}"}`)
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
	if err != nil {
		t.Fatalf("ExecuteFunctionCall: %v", err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(output, &item); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return item.Output
}

func TestApprovalGateBlocksWithoutARequesterAndNeverRuns(t *testing.T) {
	var executed atomic.Int32
	catalog, err := NewCatalog([]Tool{askingTool(t, &executed)})
	if err != nil {
		t.Fatal(err)
	}
	output := executeEcho(t, catalog, "c1", ExecutionContext{})
	if executed.Load() != 0 {
		t.Fatal("a call that needs approval ran although the run had no way to ask")
	}
	for _, want := range []string{`"status":"blocked"`, "approval_unavailable", "Not run:", "tool: echo"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
}

func TestApprovalOffRunsAtOnceAndNeverAsks(t *testing.T) {
	var executed atomic.Int32
	tool := askingTool(t, &executed)
	tool.Approval = ""
	catalog, err := NewCatalog([]Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	output := executeEcho(t, catalog, "c1", ExecutionContext{RequestApproval: func(context.Context, ApprovalCall) ApprovalVerdict {
		asked = true
		return ApprovalVerdict{}
	}})
	if asked || executed.Load() != 1 || !strings.Contains(output, "ran") {
		t.Fatalf("asked=%v executed=%d output=%q", asked, executed.Load(), output)
	}
}

func TestApprovalVerdictDecidesAndCarriesTheCode(t *testing.T) {
	cases := []struct {
		name    string
		verdict ApprovalVerdict
		runs    int32
		want    string
	}{
		{"allowed", ApprovalVerdict{Allowed: true}, 1, "ran"},
		{"denied", ApprovalVerdict{Code: ApprovalDenied, Reason: "not that file"}, 0, "not that file"},
		{"timeout", ApprovalVerdict{Code: ApprovalTimeout, Reason: "too slow"}, 0, "approval_timeout"},
		{"cancelled", ApprovalVerdict{Code: ApprovalCancelled, Reason: "stopped"}, 0, "approval_cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var executed atomic.Int32
			catalog, err := NewCatalog([]Tool{askingTool(t, &executed)})
			if err != nil {
				t.Fatal(err)
			}
			var seen ApprovalCall
			output := executeEcho(t, catalog, "c9", ExecutionContext{RawArguments: nil, RequestApproval: func(_ context.Context, call ApprovalCall) ApprovalVerdict {
				seen = call
				return tc.verdict
			}})
			if executed.Load() != tc.runs || !strings.Contains(output, tc.want) {
				t.Fatalf("executed=%d output=%q want %q", executed.Load(), output, tc.want)
			}
			if seen.CallID != "c9" || seen.ModelToolName != "echo" || seen.Tool != "echo" || seen.Server != "" {
				t.Fatalf("approval call = %+v", seen)
			}
			if got, _ := seen.Args["command"].(string); got != "hello" {
				t.Fatalf("approval args = %+v", seen.Args)
			}
			if !tc.verdict.Allowed && !strings.Contains(output, `"status":"blocked"`) {
				t.Fatalf("a refused call is not a blocked result:\n%s", output)
			}
		})
	}
}

func TestApprovalMCPCallNamesServerAndRemoteTool(t *testing.T) {
	tool := Tool{Definition: Definition{ID: "mcp:fx:do:thing", Source: "mcp", Server: "fx", Name: "fx__do_thing"}}
	call := tool.approvalCall("c1", "fx__do_thing", map[string]any{"a": 1.0}, nil)
	if call.Server != "fx" || call.Tool != "do:thing" || call.ModelToolName != "fx__do_thing" {
		t.Fatalf("mcp approval call = %+v", call)
	}
}

func TestCallNeedsApprovalProcessOnlyOnStart(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want bool
	}{
		{"bash always", "bash", map[string]any{"command": "ls"}, true},
		{"write always", "write", map[string]any{}, true},
		{"explicit start", "process", map[string]any{"action": "start", "command": "sleep 1"}, true},
		{"start with spaces", "process", map[string]any{"action": " start "}, true},
		{"status", "process", map[string]any{"action": "status", "id": "p1"}, false},
		{"wait", "process", map[string]any{"action": "wait"}, false},
		{"list", "process", map[string]any{"action": "list"}, false},
		{"stop", "process", map[string]any{"action": "stop", "id": "p1"}, false},
		{"start inferred from command", "process", map[string]any{"command": "sleep 1"}, true},
		{"blank command is not a start", "process", map[string]any{"command": "  "}, false},
		{"nothing", "process", map[string]any{}, false},
		{"action wins over command", "process", map[string]any{"action": "stop", "command": "x"}, false},
	}
	for _, tc := range cases {
		if got := CallNeedsApproval(tc.tool, tc.args); got != tc.want {
			t.Errorf("%s: CallNeedsApproval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCutTextNeverSplitsACharacter(t *testing.T) {
	if got, cut := CutText("hello", 10); got != "hello" || cut {
		t.Fatalf("short text = %q, cut %v", got, cut)
	}
	if got, cut := CutText("hello", 5); got != "hello" || cut {
		t.Fatalf("exact text = %q, cut %v", got, cut)
	}
	// "é" is two bytes; a cut in the middle must step back to the lead byte.
	if got, cut := CutText("aé", 2); got != "a" || !cut {
		t.Fatalf("split character = %q, cut %v", got, cut)
	}
	if got, cut := CutText("日本語", 4); got != "日" || !cut {
		t.Fatalf("three-byte characters = %q, cut %v", got, cut)
	}
	if got, cut := CutText("é", 0); got != "" || !cut {
		t.Fatalf("zero bytes = %q, cut %v", got, cut)
	}
}

func TestApprovalPreviewShowsOnlyWhatWillHappen(t *testing.T) {
	long := strings.Repeat("x", 200)
	cases := []struct {
		name  string
		call  ApprovalCall
		title string
		draft string
	}{
		{"bash", ApprovalCall{Tool: "bash", Args: map[string]any{"command": "ls <dir> & echo", "cwd": "/w", "timeout_ms": 5000.0}},
			"Run shell command", `{"tool":"bash","command":"ls <dir> & echo","cwd":"/w","timeout_ms":5000}`},
		// The text piped to the command is part of what runs, so it is shown right after the command.
		{"bash with stdin", ApprovalCall{Tool: "bash", Args: map[string]any{"command": "python3 -", "stdin": "print(1)\n", "cwd": "/w"}},
			"Run shell command", `{"tool":"bash","command":"python3 -","stdin":"print(1)\n","cwd":"/w"}`},
		{"bash cut", ApprovalCall{Tool: "bash", Args: map[string]any{"command": long}},
			"Run shell command", `{"tool":"bash","command":"` + long[:16] + `","command_truncated_bytes":200}`},
		{"bash stdin cut", ApprovalCall{Tool: "bash", Args: map[string]any{"command": "python3 -", "stdin": long}},
			"Run shell command", `{"tool":"bash","command":"python3 -","stdin":"` + long[:16] + `","stdin_truncated_bytes":200}`},
		{"process start", ApprovalCall{Tool: "process", Args: map[string]any{"command": "sleep 9", "cwd": "/w"}},
			"Start background process", `{"tool":"process","action":"start","command":"sleep 9","cwd":"/w"}`},
		// The text written to the process is part of what runs, so it is shown right after the command.
		{"process start with stdin", ApprovalCall{Tool: "process", Args: map[string]any{"command": "node -", "stdin": "serve()\n", "cwd": "/w"}},
			"Start background process", `{"tool":"process","action":"start","command":"node -","stdin":"serve()\n","cwd":"/w"}`},
		{"write text", ApprovalCall{Tool: "write", Args: map[string]any{"path": "a.txt", "content": "hi"}},
			"Write file", `{"tool":"write","path":"a.txt","content_bytes":2,"content":"hi"}`},
		{"write base64 hides the content", ApprovalCall{Tool: "write", Args: map[string]any{"path": "a.bin", "content": "aGk=", "contentEncoding": "base64"}},
			"Write file", `{"tool":"write","path":"a.bin","contentEncoding":"base64","content_bytes":4}`},
		{"edit", ApprovalCall{Tool: "edit", Args: map[string]any{"path": "a", "oldText": "o", "newText": "n", "replaceAll": true}},
			"Edit file", `{"tool":"edit","path":"a","oldText":"o","newText":"n","replaceAll":true}`},
		{"tool_smith run", ApprovalCall{Tool: "tool_smith", Args: map[string]any{"run": map[string]any{"function": "return 1;", "input": map[string]any{"a": 1.0}}}},
			"Run script", `{"tool":"tool_smith","function":"return 1;"}`},
		{"tool_smith run string", ApprovalCall{Tool: "tool_smith", Args: map[string]any{"run": "return 2;"}},
			"Run script", `{"tool":"tool_smith","function":"return 2;"}`},
		{"tool_smith run cut", ApprovalCall{Tool: "tool_smith", Args: map[string]any{"run": map[string]any{"function": long}}},
			"Run script", `{"tool":"tool_smith","function":"` + long[:16] + `","function_truncated_bytes":200}`},
		{"mcp keeps the model's key order", ApprovalCall{Tool: "do", Server: "fx", Args: map[string]any{"b": 1.0, "a": 2.0}, RawArguments: json.RawMessage(`{ "b": 1, "a": 2 }`)},
			"Run fx tool do", `{"tool":"do","server":"fx","arguments":"{\"b\":1,\"a\":2}"}`},
		{"unknown native tool", ApprovalCall{Tool: "other", Args: map[string]any{"k": "v"}},
			"Run other", `{"tool":"other","arguments":"{\"k\":\"v\"}"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := ApprovalPreview(tc.call, 16)
			if tc.name != "bash cut" && tc.name != "bash stdin cut" && tc.name != "tool_smith run cut" {
				preview, err = ApprovalPreview(tc.call, 4096)
			}
			if err != nil {
				t.Fatal(err)
			}
			if preview.Title != tc.title || string(preview.Draft) != tc.draft {
				t.Fatalf("preview = %q %s\nwant      %q %s", preview.Title, preview.Draft, tc.title, tc.draft)
			}
			if !json.Valid(preview.Draft) {
				t.Fatalf("preview is not valid JSON: %s", preview.Draft)
			}
		})
	}
}

func approvalOptions() ApprovalOptions {
	return ApprovalOptions{TimeoutMS: 1000, ExtendMS: 2000, MaxTimeoutMS: 5000, PreviewBytes: 4096}
}

func TestRequesterTurnsAnswersIntoVerdicts(t *testing.T) {
	reason := "use a copy"
	cases := []struct {
		name   string
		answer InteractionAnswer
		code   string
		reason string
	}{
		{"accept", InteractionAnswer{Decision: InteractionAccept, Outcome: InteractionAnswered}, "", ""},
		{"decline with a reason", InteractionAnswer{Decision: InteractionDecline, Outcome: InteractionAnswered, Reason: &reason}, ApprovalDenied, "use a copy"},
		{"decline without a reason", InteractionAnswer{Decision: InteractionDecline, Outcome: InteractionAnswered}, ApprovalDenied, "The user declined this call."},
		{"timed out", InteractionAnswer{Decision: InteractionDecline, Outcome: InteractionTimedOut, Reason: &reason}, ApprovalTimeout, "did not decide in time"},
		{"cancelled", InteractionAnswer{Decision: InteractionDecline, Outcome: InteractionCancelled}, ApprovalCancelled, "run was stopped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var question InteractionQuestion
			var identity InteractionIdentity
			request := NewApprovalRequester(approvalOptions(), func(_ context.Context, id InteractionIdentity, q InteractionQuestion) (InteractionAnswer, error) {
				question, identity = q, id
				return tc.answer, nil
			})
			verdict := request(context.Background(), ApprovalCall{CallID: "c1", ModelToolName: "bash", Tool: "bash", Args: map[string]any{"command": "ls"}})
			if tc.code == "" {
				if !verdict.Allowed {
					t.Fatalf("verdict = %+v, want allowed", verdict)
				}
			} else if verdict.Allowed || verdict.Code != tc.code || !strings.Contains(verdict.Reason, tc.reason) {
				t.Fatalf("verdict = %+v, want code %s reason containing %q", verdict, tc.code, tc.reason)
			}
			if question.Kind != InteractionKindApproval || question.Title != "Run shell command" || question.TimeoutMS != 1000 || question.ExtendMS != 2000 || question.MaxTimeoutMS != 5000 {
				t.Fatalf("question = %+v", question)
			}
			if identity.Tool != "bash" || identity.ToolCallID != "c1" {
				t.Fatalf("identity = %+v", identity)
			}
		})
	}
}

func TestRequesterHeadlessPolicy(t *testing.T) {
	call := ApprovalCall{CallID: "c1", ModelToolName: "bash", Tool: "bash", Args: map[string]any{"command": "ls"}}
	if verdict := NewApprovalRequester(approvalOptions(), nil)(context.Background(), call); verdict.Allowed || verdict.Code != ApprovalUnavailable {
		t.Fatalf("headless deny = %+v", verdict)
	}
	allow := approvalOptions()
	allow.HeadlessAllow = true
	if verdict := NewApprovalRequester(allow, nil)(context.Background(), call); !verdict.Allowed {
		t.Fatalf("headless allow = %+v", verdict)
	}
}

func TestRequesterNeverAsksForANonStartProcessCall(t *testing.T) {
	asked := false
	request := NewApprovalRequester(approvalOptions(), func(context.Context, InteractionIdentity, InteractionQuestion) (InteractionAnswer, error) {
		asked = true
		return InteractionAnswer{}, nil
	})
	verdict := request(context.Background(), ApprovalCall{CallID: "c1", ModelToolName: "process", Tool: "process", Args: map[string]any{"action": "status"}})
	if !verdict.Allowed || asked {
		t.Fatalf("status call: verdict %+v, asked %v", verdict, asked)
	}
	// With nobody to ask, a non-start call is still not held back by headless deny.
	if verdict := NewApprovalRequester(approvalOptions(), nil)(context.Background(), ApprovalCall{Tool: "process", Args: map[string]any{"action": "stop"}}); !verdict.Allowed {
		t.Fatalf("headless status = %+v", verdict)
	}
}

func TestRequesterAsksOneQuestionAtATime(t *testing.T) {
	var active, peak atomic.Int32
	var mu sync.Mutex
	release := make(chan struct{})
	request := NewApprovalRequester(approvalOptions(), func(context.Context, InteractionIdentity, InteractionQuestion) (InteractionAnswer, error) {
		now := active.Add(1)
		mu.Lock()
		if now > peak.Load() {
			peak.Store(now)
		}
		mu.Unlock()
		<-release
		active.Add(-1)
		return InteractionAnswer{Decision: InteractionAccept, Outcome: InteractionAnswered}, nil
	})
	var wg sync.WaitGroup
	for index := 0; index < 3; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request(context.Background(), ApprovalCall{CallID: "c", ModelToolName: "bash", Tool: "bash", Args: map[string]any{"command": "ls"}})
		}()
	}
	// Give every goroutine the chance to reach the gate, then let the questions through one by one.
	time.Sleep(50 * time.Millisecond)
	for index := 0; index < 3; index++ {
		release <- struct{}{}
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d questions were open at once, want 1", peak.Load())
	}
}

func TestRequesterReportsAnAskFailureAsUnavailable(t *testing.T) {
	request := NewApprovalRequester(approvalOptions(), func(context.Context, InteractionIdentity, InteractionQuestion) (InteractionAnswer, error) {
		return InteractionAnswer{}, context.DeadlineExceeded
	})
	verdict := request(context.Background(), ApprovalCall{CallID: "c1", ModelToolName: "bash", Tool: "bash", Args: map[string]any{"command": "ls"}})
	if verdict.Allowed || verdict.Code != ApprovalUnavailable {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestApprovalIsNeverAskedForACallWithInvalidArguments(t *testing.T) {
	var executed atomic.Int32
	catalog, err := NewCatalog([]Tool{askingTool(t, &executed)})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	request := func(context.Context, ApprovalCall) ApprovalVerdict {
		asked = true
		return ApprovalVerdict{Allowed: true}
	}
	for name, arguments := range map[string]string{
		"missing required field": `{}`,
		"wrong type":             `{"command":5}`,
		"extra field":            `{"command":"x","other":1}`,
		"not json":               `{`,
	} {
		call, err := json.Marshal(map[string]string{"type": "function_call", "call_id": "c1", "name": "echo", "arguments": arguments})
		if err != nil {
			t.Fatal(err)
		}
		output, err := catalog.ExecuteFunctionCall(context.Background(), call, ExecutionContext{RequestApproval: request})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The refusal must come from validation, not from the gate.
		if strings.Contains(string(output), "approval_") || (!strings.Contains(string(output), "validation_failed") && !strings.Contains(string(output), "invalid_arguments")) {
			t.Fatalf("%s: output is not a validation refusal: %s", name, output)
		}
		if asked || executed.Load() != 0 {
			t.Fatalf("%s: asked=%v executed=%d; an invalid call must be refused before anyone is asked", name, asked, executed.Load())
		}
	}
}
