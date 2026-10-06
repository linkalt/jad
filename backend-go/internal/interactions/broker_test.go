package interactions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/tools"
)

func TestBrokerReplyPublishesAndSettlesQuestion(t *testing.T) {
	changes := make(chan *tools.PendingInteraction, 4)
	broker := NewBroker(func(pending *tools.PendingInteraction) { changes <- pending })
	result := make(chan tools.InteractionAnswer, 1)
	go func() {
		answer, err := broker.Request(context.Background(), tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-1"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"goal":"ship"}`), TimeoutMS: 60_000, ExtendMS: 1_000, MaxTimeoutMS: 61_000,
			Limits: tools.InteractionLimits{MaxDraftBytes: 1024, MaxDraftDepth: 8},
		})
		if err != nil {
			result <- tools.InteractionAnswer{Reason: ptr(err.Error())}
			return
		}
		result <- answer
	}()

	pending := awaitPending(t, changes)
	if pending.Tool != "request_draft" || pending.ToolCallID != "call-1" || string(pending.Draft) != `{"goal":"ship"}` {
		t.Fatalf("pending interaction = %+v", pending)
	}
	if limits := broker.Limits(); limits == nil || limits.MaxDraftBytes != 1024 || limits.MaxDraftDepth != 8 {
		t.Fatalf("pending limits = %+v", limits)
	}
	draft := json.RawMessage(`{"goal":"ship it"}`)
	if status := broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionUpdate, Draft: draft}); status != tools.InteractionStatusAnswered {
		t.Fatalf("reply status = %q", status)
	}
	answer := awaitAnswer(t, result)
	if answer.Decision != tools.InteractionUpdate || answer.Outcome != tools.InteractionAnswered || answer.Source != tools.InteractionSourceUser || string(answer.Draft) != string(draft) {
		t.Fatalf("interaction answer = %+v", answer)
	}
	if cleared := <-changes; cleared != nil {
		t.Fatalf("pending state after reply = %+v, want nil", cleared)
	}
	if broker.Pending() != nil {
		t.Fatal("broker retained answered interaction")
	}
}

func TestBrokerExtendCapsDeadlineAndExpiresLateReply(t *testing.T) {
	changes := make(chan *tools.PendingInteraction, 4)
	broker := NewBroker(func(pending *tools.PendingInteraction) { changes <- pending })
	base := time.Now().UTC().Truncate(time.Millisecond)
	clock := base
	broker.now = func() time.Time { return clock }
	result := make(chan tools.InteractionAnswer, 1)
	go func() {
		answer, _ := broker.Request(context.Background(), tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-2"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"goal":"ship"}`), TimeoutMS: 60_000, ExtendMS: 10_000, MaxTimeoutMS: 70_000,
		})
		result <- answer
	}()
	pending := awaitPending(t, changes)
	if status := broker.Extend(pending.ID); status != tools.InteractionExtendExtended {
		t.Fatalf("first extend status = %q", status)
	}
	extended := <-changes
	if extended == nil || extended.ExpiresAt == pending.ExpiresAt {
		t.Fatalf("extended pending = %+v, previous expiry %q", extended, pending.ExpiresAt)
	}
	if status := broker.Extend(pending.ID); status != tools.InteractionExtendAtMax {
		t.Fatalf("second extend status = %q, want at_max", status)
	}
	clock = base.Add(70_001 * time.Millisecond)
	if status := broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionAccept}); status != tools.InteractionStatusExpired {
		t.Fatalf("late reply status = %q, want expired", status)
	}
	answer := awaitAnswer(t, result)
	if answer.Decision != tools.InteractionDecline || answer.Outcome != tools.InteractionTimedOut || answer.Source != tools.InteractionSourceTimeout || answer.Reason == nil || *answer.Reason != "No answer within 70 seconds; the draft was not confirmed." {
		t.Fatalf("timeout answer = %+v", answer)
	}
	if cleared := <-changes; cleared != nil {
		t.Fatalf("pending state after timeout = %+v, want nil", cleared)
	}
}

func TestBrokerCancellationSettlesWithoutPublishingUnansweredQuestion(t *testing.T) {
	changes := make(chan *tools.PendingInteraction, 4)
	broker := NewBroker(func(pending *tools.PendingInteraction) { changes <- pending })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan tools.InteractionAnswer, 1)
	go func() {
		answer, _ := broker.Request(ctx, tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-3"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"goal":"ship"}`), TimeoutMS: 60_000, MaxTimeoutMS: 60_000,
		})
		result <- answer
	}()
	pending := awaitPending(t, changes)
	cancel()
	answer := awaitAnswer(t, result)
	if answer.Decision != tools.InteractionDecline || answer.Outcome != tools.InteractionCancelled || answer.Reason == nil || *answer.Reason != "The run was stopped before the draft was answered." {
		t.Fatalf("cancel answer = %+v", answer)
	}
	if cleared := <-changes; cleared != nil {
		t.Fatalf("pending state after cancel = %+v, want nil", cleared)
	}
	if broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionAccept}) != tools.InteractionStatusUnknown {
		t.Fatal("cancelled interaction remained answerable")
	}
}

func awaitPending(t *testing.T, events <-chan *tools.PendingInteraction) *tools.PendingInteraction {
	t.Helper()
	select {
	case pending := <-events:
		if pending == nil {
			t.Fatal("received nil pending interaction")
		}
		return pending
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending interaction")
		return nil
	}
}

func awaitAnswer(t *testing.T, answers <-chan tools.InteractionAnswer) tools.InteractionAnswer {
	t.Helper()
	select {
	case answer := <-answers:
		return answer
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interaction answer")
		return tools.InteractionAnswer{}
	}
}

func ptr(value string) *string { return &value }

func TestBrokerReportsTheKindOfTheWaitingQuestion(t *testing.T) {
	changes := make(chan *tools.PendingInteraction, 4)
	broker := NewBroker(func(pending *tools.PendingInteraction) { changes <- pending })
	if broker.Kind() != tools.InteractionKindDraft {
		t.Fatalf("kind with nothing waiting = %q, want the draft kind", broker.Kind())
	}
	var nilBroker *Broker
	if nilBroker.Kind() != tools.InteractionKindDraft {
		t.Fatal("a nil broker must report the draft kind")
	}
	for _, kind := range []tools.InteractionKind{tools.InteractionKindApproval, tools.InteractionKindDraft} {
		result := make(chan tools.InteractionAnswer, 1)
		go func() {
			answer, _ := broker.Request(context.Background(), tools.InteractionIdentity{Tool: "bash", ToolCallID: "call-1"}, tools.InteractionQuestion{
				Kind: kind, Draft: json.RawMessage(`{"command":"ls"}`), TimeoutMS: 60_000, ExtendMS: 1_000, MaxTimeoutMS: 61_000,
			})
			result <- answer
		}()
		pending := awaitPending(t, changes)
		if pending.Kind != kind || broker.Kind() != kind {
			t.Fatalf("kind %q: pending %q, broker %q", kind, pending.Kind, broker.Kind())
		}
		if status := broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionAccept}); status != tools.InteractionStatusAnswered {
			t.Fatalf("reply status = %q", status)
		}
		awaitAnswer(t, result)
		<-changes
		if broker.Kind() != tools.InteractionKindDraft {
			t.Fatalf("kind after the answer = %q, want the draft kind", broker.Kind())
		}
	}
}

func TestPendingInteractionOmitsTheKindForADraft(t *testing.T) {
	draft, _ := json.Marshal(tools.PendingInteraction{ID: "a"})
	if strings.Contains(string(draft), `"kind"`) {
		t.Fatalf("a draft question carries a kind, so older clients see a new field: %s", draft)
	}
	approval, _ := json.Marshal(tools.PendingInteraction{ID: "a", Kind: tools.InteractionKindApproval})
	if !strings.Contains(string(approval), `"kind":"approval"`) {
		t.Fatalf("an approval question does not say so: %s", approval)
	}
}
