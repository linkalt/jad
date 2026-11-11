package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// countingAsker answers every question by accepting it, and counts the questions it is shown.
func countingAsker(count *atomic.Int32) tools.InteractionHandler {
	return func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		count.Add(1)
		return tools.InteractionAnswer{Decision: tools.InteractionAccept, Outcome: tools.InteractionAnswered}, nil
	}
}

func legacyCallAsking(t *testing.T, tool tools.Tool, arguments map[string]any, asker tools.InteractionHandler) string {
	t.Helper()
	output, err := tool.ExecuteOutput(context.Background(), arguments, tools.ExecutionContext{CallID: "c-" + tool.Definition.ID, ToolName: tool.Definition.ID, RequestInteraction: asker})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// An older-protocol server's elicitation request sent on its own while an unrelated run's call
// is in progress names no request of the client: it is refused, and neither run's user is shown
// the question (before the fix it was shown to the user of the call in progress).
func TestLegacyElicitationSentOnItsOwnDuringAnUnrelatedCallIsRefused(t *testing.T) {
	manager, servers, directory := legacyServer(t, 1)
	var armerAsked, holderAsked atomic.Int32
	armer := legacyListing(t, manager, servers, directory, nil)
	holder := legacyListing(t, manager, servers, directory, nil)
	trigger, out := filepath.Join(directory, "trigger"), filepath.Join(directory, "out")
	wantText(t, "arm", legacyCallAsking(t, armer["arm"], map[string]any{"trigger": trigger, "out": out, "kind": "elicit"}, countingAsker(&armerAsked)), "armed")
	wantText(t, "hold", legacyCallAsking(t, holder["hold"], map[string]any{"trigger": trigger, "out": out}, countingAsker(&holderAsked)), "held")
	wantText(t, "elicitation sent during the other run's call", legacyAwaitFile(t, out), "refused: ", errElicitationUnattributed.Error())
	if armerAsked.Load() != 0 || holderAsked.Load() != 0 {
		t.Fatalf("a user was shown an unattributed question: armer %d, holder %d", armerAsked.Load(), holderAsked.Load())
	}
}

// It is refused during the run's own call and resource read too, after the call, and during the
// user's own resource read: nothing in it says which request it belongs to.
func TestLegacyElicitationIsRefusedEvenDuringTheRunsOwnCallAndResourceRead(t *testing.T) {
	manager, servers, directory := legacyServer(t, 1)
	var asked atomic.Int32
	run := legacyListing(t, manager, servers, directory, nil)
	wantText(t, "a run's call", legacyCallAsking(t, run["sample"], map[string]any{"kind": "elicit"}, countingAsker(&asked)), "refused: ", errElicitationUnattributed.Error())
	wantText(t, "a run's resource read", legacyCallAsking(t, run[readResourceName], map[string]any{"uri": "doc://elicit"}, countingAsker(&asked)), "refused: ", errElicitationUnattributed.Error())
	read, err := manager.ReadUserResource(context.Background(), servers, directory, UserResourceRead{Server: "legacy", URI: "doc://elicit"})
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, "the user's resource read", read.Text, "refused: ", errElicitationUnattributed.Error())
	trigger, out := filepath.Join(directory, "trigger"), filepath.Join(directory, "out")
	wantText(t, "arm", legacyCallAsking(t, run["arm"], map[string]any{"trigger": trigger, "out": out, "kind": "elicit"}, countingAsker(&asked)), "armed")
	legacyWriteFile(trigger, "go")
	wantText(t, "elicitation after the call ended", legacyAwaitFile(t, out), "refused: ", errElicitationUnattributed.Error())
	if asked.Load() != 0 {
		t.Fatalf("the user was shown %d unattributed questions", asked.Load())
	}
}

// A 2026-07-28 server asks in the result of the request it serves, so its question is attributed:
// a run's resource read asks that run's user, and the user's own read (no run, no one to ask)
// cancels.
func TestElicitationInAResourceReadAsksTheRunsUserOnly(t *testing.T) {
	manager, _, directory := probeServer(t, map[string]any{"elicitation": map[string]any{"enabled": true, "timeout_ms": 5000}})
	servers := []sessionfs.MCPServerReference{{ID: "probe", ConfigPath: filepath.Join(directory, "probe.json"), Enabled: true, Resources: true}}
	loaded, err := manager.LoadTools(context.Background(), servers, directory)
	if err != nil {
		t.Fatal(err)
	}
	var read tools.Tool
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:probe:"+readResourceName {
			read = tool
		}
	}
	if read.Definition.ID == "" {
		t.Fatal("no resource read tool")
	}
	var asked atomic.Int32
	var identity tools.InteractionIdentity
	asker := func(ctx context.Context, who tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		identity = who
		answer, err := countingAsker(&asked)(ctx, who, question)
		answer.Draft = json.RawMessage(`{"server":"probe","message":"Which color?","content":{"color":"blue"}}`)
		return answer, err
	}
	output, err := read.ExecuteOutput(context.Background(), map[string]any{"uri": "doc://elicit"}, tools.ExecutionContext{CallID: "read-1", ToolName: "mcp__probe__read", RequestInteraction: asker})
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, "a run's resource read", string(output), `elicited accept {\"color\":\"blue\"}`)
	if asked.Load() != 1 || identity.ToolCallID != "read-1" {
		t.Fatalf("asked %d times as %+v, want once for read-1", asked.Load(), identity)
	}
	user, err := manager.ReadUserResource(context.Background(), servers, directory, UserResourceRead{Server: "probe", URI: "doc://elicit"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user.Text, "elicited cancel") || asked.Load() != 1 {
		t.Fatalf("the user's read = %s (asked %d), want a cancel nobody saw", user.Text, asked.Load())
	}
}

// Only a request whose context carries the client's own request mark is shown, and to the user of
// the call it marks; an attributed request no run made cancels.
func TestElicitAsksOnlyForAnAttributedCall(t *testing.T) {
	features := clientFeatures{Elicitation: true, ElicitTimeoutMS: 1000, ElicitDraftBytes: 4096, ElicitDraftDepth: 8}
	request := &mcpSDK.ElicitRequest{Params: &mcpSDK.ElicitParams{Message: "Which color?", RequestedSchema: map[string]any{"type": "object"}}}
	var asked atomic.Int32
	call := &activeCall{request: countingAsker(&asked), identity: tools.InteractionIdentity{ToolCallID: "c"}}
	if result, err := elicit(context.Background(), "s", features, request); !errors.Is(err, errElicitationUnattributed) || result != nil {
		t.Fatalf("unattributed: %+v %v, want %v", result, err, errElicitationUnattributed)
	}
	if result, err := elicit(withInlineRequest(context.Background(), nil), "s", features, request); err != nil || result.Action != "cancel" {
		t.Fatalf("no run's call: %+v %v, want a cancel", result, err)
	}
	if asked.Load() != 0 {
		t.Fatalf("asked %d times before an attributed call", asked.Load())
	}
	if result, err := elicit(withInlineRequest(context.Background(), call), "s", features, request); err != nil || result.Action != "accept" || asked.Load() != 1 {
		t.Fatalf("attributed call: %+v %v (asked %d), want an accept asked once", result, err, asked.Load())
	}
}

// Concurrent calls on one connection each ask their own user once: the question in a call's
// result goes to the call that got it.
func TestConcurrentCallsAskTheirOwnUsers(t *testing.T) {
	_, probe, _ := probeServer(t, map[string]any{"elicitation": map[string]any{"enabled": true, "timeout_ms": 5000}})
	const calls = 8
	var asked [calls]atomic.Int32
	var wrong atomic.Int32
	var group sync.WaitGroup
	outputs := make([]string, calls)
	for i := range calls {
		group.Add(1)
		go func() {
			defer group.Done()
			id := fmt.Sprintf("call-%d", i)
			asker := func(_ context.Context, who tools.InteractionIdentity, _ tools.InteractionQuestion) (tools.InteractionAnswer, error) {
				asked[i].Add(1)
				if who.ToolCallID != id {
					wrong.Add(1)
				}
				return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionAnswered}, nil
			}
			output, err := probe.ExecuteOutput(context.Background(), map[string]any{"ask": []any{"elicit"}}, tools.ExecutionContext{CallID: id, RequestInteraction: asker})
			if err != nil {
				t.Error(err)
			}
			outputs[i] = string(output)
		}()
	}
	group.Wait()
	for i := range calls {
		if asked[i].Load() != 1 || !strings.Contains(outputs[i], "elicited decline") {
			t.Fatalf("call %d asked %d times: %s", i, asked[i].Load(), outputs[i])
		}
	}
	if wrong.Load() != 0 {
		t.Fatalf("%d questions went to another call's user", wrong.Load())
	}
}
