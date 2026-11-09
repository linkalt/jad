package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// interactionFixture builds a subagent tool over the real request_draft tool. The launcher runs
// each child's request_draft through the child's own catalog with the execution context it was
// given, and sends what the call returned (or the error text) on results.
type interactionFixture struct {
	catalog *tools.Catalog
	results chan string
}

func newInteractionFixture(t *testing.T, afterCall <-chan struct{}) interactionFixture {
	t.Helper()
	draft, err := NewRequestDraftTool(requestDraftTestSettings())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 4)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if afterCall != nil {
			<-afterCall
		}
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "d1", "name": "request_draft", "arguments": `{"draft":{"goal":"ship"}}`})
		raw, err := request.Tools.ExecuteFunctionCall(ctx, call, request.Execution)
		if err != nil {
			results <- "error: " + err.Error()
			return SubagentResult{Text: "done"}, nil
		}
		results <- string(raw)
		return SubagentResult{Text: "done"}, nil
	}}
	parent := append(withExecutors(t, readOnlyTool("read")), draft)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("asker")), parent, nil, launcher, nil, manager, newTestAsyncHost(t))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return interactionFixture{catalog: catalog, results: results}
}

func (f interactionFixture) call(t *testing.T, arguments string, request tools.InteractionHandler) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := f.catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, RequestInteraction: request})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (f interactionFixture) result(t *testing.T) string {
	t.Helper()
	select {
	case got := <-f.results:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the child never asked")
		return ""
	}
}

func accepting(asked *atomic.Int64) tools.InteractionHandler {
	return func(_ context.Context, identity tools.InteractionIdentity, _ tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		asked.Add(1)
		if identity.Tool == "" {
			return tools.InteractionAnswer{}, nil
		}
		return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionAnswered, Source: tools.InteractionSourceUser}, nil
	}
}

// A child's request_draft reaches the question channel of the call that started it: the user is asked and
// the user's answer is the tool's result, not an automatic acceptance.
func TestChildRequestDraftReachesTheParentsChannel(t *testing.T) {
	f := newInteractionFixture(t, nil)
	var asked atomic.Int64
	f.call(t, `{"task":"x"}`, accepting(&asked))
	got := f.result(t)
	if asked.Load() != 1 || !strings.Contains(got, `decision\": \"decline`) {
		t.Fatalf("asked %d times, child saw %s: want the parent's channel asked once and its decline returned", asked.Load(), got)
	}
}

// With no channel (a headless parent) the child's request_draft does what the parent's would: the tool's own
// headless fallback.
func TestChildRequestDraftWithNoChannelGetsTheHeadlessFallback(t *testing.T) {
	f := newInteractionFixture(t, nil)
	f.call(t, `{"task":"x"}`, nil)
	if got := f.result(t); !strings.Contains(got, `decision\": \"accept`) || strings.Contains(got, "error") {
		t.Fatalf("a child of a headless parent saw %s, want the headless fallback", got)
	}
}

// A child that outlived its call has no one to ask: the question fails the tool call (errChildChannelClosed),
// with a channel or without, and is never accepted on its own.
func TestBackgroundChildRequestDraftFailsClosedOnceItsCallReturned(t *testing.T) {
	for name, handler := range map[string]func() tools.InteractionHandler{
		"parent with a channel": func() tools.InteractionHandler { return accepting(new(atomic.Int64)) },
		"headless parent":       func() tools.InteractionHandler { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			f := newInteractionFixture(t, release)
			if out := f.call(t, `{"task":"x","block_ms":0}`, handler()); !strings.Contains(out, "still running") {
				t.Fatalf("the child was not handed off:\n%s", out)
			}
			close(release)
			got := f.result(t)
			if !strings.Contains(got, "tool_execution_failed") || strings.Contains(got, "decision") {
				t.Fatalf("a question of a child that outlived its call got %s, want a failed call that is not an acceptance", got)
			}
		})
	}
}

func TestChildExecutionContextKeepsTheCallersChannelAndChangeListener(t *testing.T) {
	var heard []string
	parent := tools.ExecutionContext{
		SessionKey: "s\x00p",
		RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{}, nil
		},
		OnFileChange: func(path, action string) { heard = append(heard, path+":"+action) },
	}
	child := childExecutionContext(parent, sessionfs.NativeSubagentAgent{Name: "a"})
	if child.RequestInteraction == nil || child.SessionKey != parent.SessionKey {
		t.Fatalf("the child lost the channel or the session key: %+v", child)
	}
	// The manager's own list and the caller's listener both hear a change.
	manager := NewSubagentManager()
	defer manager.Close()
	run, err := manager.start(subagentStart{
		config: "c", launcher: &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
			request.Execution.OnFileChange("a.txt", "created")
			return SubagentResult{Text: "ok"}, nil
		}},
		owner: "o", session: "o", changes: true, changedMax: 10, limit: 2, keep: 10, resultMax: 1024,
		request: SubagentRequest{Agent: sessionfs.NativeSubagentAgent{Name: "a"}, Execution: child},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-run.done
	if len(heard) != 1 || heard[0] != "a.txt:created" {
		t.Fatalf("the caller's change listener heard %v", heard)
	}
}
