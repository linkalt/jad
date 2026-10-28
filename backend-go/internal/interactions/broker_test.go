package interactions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func TestBrokerReplyPublishesAndSettlesQuestion(t *testing.T) {
	changes := make(chan *tools.PendingInteraction, 4)
	broker := NewBroker(func(pending *tools.PendingInteraction, _ *tools.InteractionEnd) { changes <- pending })
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
	broker := NewBroker(func(pending *tools.PendingInteraction, _ *tools.InteractionEnd) { changes <- pending })
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
	broker := NewBroker(func(pending *tools.PendingInteraction, _ *tools.InteractionEnd) { changes <- pending })
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

// ended records what the broker told the run about each clearing notification.
type endedLog struct {
	pending chan *tools.PendingInteraction
	ended   chan *tools.InteractionEnd
}

func newEndedLog() (*Broker, *endedLog) {
	log := &endedLog{pending: make(chan *tools.PendingInteraction, 8), ended: make(chan *tools.InteractionEnd, 8)}
	broker := NewBroker(func(pending *tools.PendingInteraction, ended *tools.InteractionEnd) {
		log.pending <- pending
		log.ended <- ended
	})
	return broker, log
}

func askDraft(broker *Broker, ctx context.Context) <-chan tools.InteractionAnswer {
	result := make(chan tools.InteractionAnswer, 1)
	go func() {
		answer, _ := broker.Request(ctx, tools.InteractionIdentity{Tool: "bash", ToolCallID: "call-1"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"command":"ls"}`), TimeoutMS: 60_000, MaxTimeoutMS: 60_000,
		})
		result <- answer
	}()
	return result
}

func TestBrokerTellsTheRunWhyAQuestionEnded(t *testing.T) {
	t.Run("answered carries the decision", func(t *testing.T) {
		for _, decision := range []tools.InteractionDecision{tools.InteractionAccept, tools.InteractionDecline} {
			broker, log := newEndedLog()
			result := askDraft(broker, context.Background())
			pending := awaitPending(t, log.pending)
			if opened := <-log.ended; opened != nil {
				t.Fatalf("a question being opened carries an end: %+v", opened)
			}
			reason := "no"
			broker.Reply(pending.ID, tools.InteractionReply{Decision: decision, Reason: &reason})
			awaitAnswer(t, result)
			<-log.pending
			ended := <-log.ended
			want := tools.InteractionEnd{InteractionID: pending.ID, Tool: "bash", Outcome: tools.InteractionAnswered, Decision: decision, Seq: 2}
			if ended == nil || *ended != want {
				t.Fatalf("answered %q: ended = %+v, want %+v", decision, ended, want)
			}
		}
	})
	t.Run("a stop carries no decision", func(t *testing.T) {
		broker, log := newEndedLog()
		ctx, cancel := context.WithCancel(context.Background())
		result := askDraft(broker, ctx)
		pending := awaitPending(t, log.pending)
		<-log.ended
		cancel()
		awaitAnswer(t, result)
		<-log.pending
		ended := <-log.ended
		want := tools.InteractionEnd{InteractionID: pending.ID, Tool: "bash", Outcome: tools.InteractionCancelled, Seq: 2}
		if ended == nil || *ended != want {
			t.Fatalf("stopped: ended = %+v, want %+v", ended, want)
		}
	})
	t.Run("a run that ends cancels the question", func(t *testing.T) {
		broker, log := newEndedLog()
		result := askDraft(broker, context.Background())
		awaitPending(t, log.pending)
		<-log.ended
		broker.Cancel("run over")
		awaitAnswer(t, result)
		<-log.pending
		if ended := <-log.ended; ended == nil || ended.Outcome != tools.InteractionCancelled || ended.Decision != "" {
			t.Fatalf("cancelled by the run: ended = %+v", ended)
		}
	})
	t.Run("a late reply reports the timeout, not an answer", func(t *testing.T) {
		broker, log := newEndedLog()
		clock := time.Now().UTC()
		broker.now = func() time.Time { return clock }
		result := askDraft(broker, context.Background())
		pending := awaitPending(t, log.pending)
		<-log.ended
		clock = clock.Add(61 * time.Second)
		if status := broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionAccept}); status != tools.InteractionStatusExpired {
			t.Fatalf("late reply status = %q", status)
		}
		awaitAnswer(t, result)
		<-log.pending
		if ended := <-log.ended; ended == nil || ended.Outcome != tools.InteractionTimedOut || ended.Decision != "" {
			t.Fatalf("late reply: ended = %+v", ended)
		}
	})
	t.Run("a draft question says draft", func(t *testing.T) {
		broker, log := newEndedLog()
		result := make(chan tools.InteractionAnswer, 1)
		go func() {
			answer, _ := broker.Request(context.Background(), tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-2"}, tools.InteractionQuestion{
				Draft: json.RawMessage(`{"goal":"ship"}`), TimeoutMS: 60_000, MaxTimeoutMS: 60_000,
			})
			result <- answer
		}()
		pending := awaitPending(t, log.pending)
		<-log.ended
		broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionUpdate, Draft: json.RawMessage(`{"goal":"x"}`)})
		awaitAnswer(t, result)
		<-log.pending
		if ended := <-log.ended; ended == nil || ended.Decision != tools.InteractionUpdate {
			t.Fatalf("draft: ended = %+v", ended)
		}
	})
}

func TestInteractionEndOmitsADecisionWhenNobodyDecided(t *testing.T) {
	raw, _ := json.Marshal(tools.InteractionEnd{InteractionID: "a", Tool: "bash", Outcome: tools.InteractionTimedOut})
	if strings.Contains(string(raw), "decision") {
		t.Fatalf("a timeout carries a decision: %s", raw)
	}
}
