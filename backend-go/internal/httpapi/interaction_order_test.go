package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/interactions"
	"jad/backend-go/internal/tools"
)

// The clear for an answered question can reach the run before the announce of that
// same question; the late announce must not bring the settled question back.
func TestAnswerDeliveredBeforeItsAnnounceLeavesNothingPending(t *testing.T) {
	run, err := newRunRegistry(time.Minute, 1<<20, 0).begin("session\x00dev", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	announce := make(chan struct{})
	announced := make(chan struct{})
	broker := interactions.NewBroker(func(pending *tools.PendingInteraction, ended *tools.InteractionEnd) {
		if pending != nil {
			close(announced)
			<-announce
		}
		run.setPendingInteraction(pending, ended)
	})
	asked := make(chan struct{})
	go func() {
		_, _ = broker.Request(context.Background(), tools.InteractionIdentity{Tool: "request_draft", ToolCallID: "call-1"}, tools.InteractionQuestion{
			Draft: json.RawMessage(`{"goal":"ship"}`), TimeoutMS: 60_000, MaxTimeoutMS: 60_000,
		})
		close(asked)
	}()
	<-announced
	pending := broker.Pending()
	if pending == nil {
		t.Fatal("broker holds no question")
	}
	if status := broker.Reply(pending.ID, tools.InteractionReply{Decision: tools.InteractionDecline}); status != tools.InteractionStatusAnswered {
		t.Fatalf("reply status = %v", status)
	}
	close(announce)
	<-asked
	run.mu.Lock()
	stale := run.pendingInteraction
	run.mu.Unlock()
	if stale != nil {
		t.Fatalf("settled question was republished as pending: %+v", stale)
	}
}

func TestFinishClearsAQuestionTheBrokerNoLongerHolds(t *testing.T) {
	run, err := newRunRegistry(time.Minute, 1<<20, 0).begin("session\x00dev", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	queue := run.subscribe()
	run.setPendingInteraction(&tools.PendingInteraction{ID: "q1", Tool: "request_draft", ToolCallID: "call-1"}, nil)
	run.finish("completed", nil)

	run.mu.Lock()
	stuck := run.pendingInteraction
	run.mu.Unlock()
	if stuck != nil {
		t.Fatalf("finished run still holds %+v", stuck)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var last streamEvent
	for {
		event, ok := queue.next(ctx)
		if !ok {
			break
		}
		if event.name == "interaction" {
			last = event
		}
	}
	if last.name == "" || !strings.Contains(string(last.frame), `"pending":null`) {
		t.Fatalf("last interaction frame = %q", last.frame)
	}
}
