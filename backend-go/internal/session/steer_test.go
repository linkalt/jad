package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
)

// pushingStreamer runs before(n) on request n, then answers it from rounds.
type pushingStreamer struct {
	scriptedStreamer
	before func(index int)
}

func (s *pushingStreamer) Stream(ctx context.Context, request json.RawMessage, handle responses.EventHandler) error {
	if s.before != nil {
		s.before(len(s.requests))
	}
	return s.scriptedStreamer.Stream(ctx, request, handle)
}

func TestSteerMessageSentWhileTheRunWorksGetsAnotherRound(t *testing.T) {
	store, _, _ := newAutomaticCompactionStore(t, "")
	queue := NewSteerQueue(4)
	streamer := &pushingStreamer{scriptedStreamer: scriptedStreamer{rounds: [][]responses.Event{finalAnswerRound("first", 1), finalAnswerRound("second", 1)}}}
	streamer.before = func(index int) {
		if index == 0 {
			if _, err := queue.Push(json.RawMessage(`"also check the tests"`)); err != nil {
				t.Errorf("Push() error = %v", err)
			}
		}
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var steered [][]byte
	result, err := runner.Run(context.Background(), json.RawMessage(`"fix the bug"`), RunOptions{
		Steer:   queue,
		OnSteer: func(items [][]byte) { steered = append(steered, items...) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want a second round for the steering message", len(streamer.requests))
	}
	second := streamer.requests[1]
	first, steer := strings.Index(second, "first"), strings.Index(second, "also check the tests")
	if first < 0 || steer < first {
		t.Fatalf("second request = %s, want the steering message after the first answer", second)
	}
	if strings.Contains(streamer.requests[0], "also check the tests") {
		t.Fatalf("the steering message reached the round it was sent during: %s", streamer.requests[0])
	}
	if len(steered) != 1 || !strings.Contains(string(steered[0]), `"role":"user"`) {
		t.Fatalf("OnSteer items = %q", steered)
	}
	if !strings.Contains(string(result.Event), "second") {
		t.Fatalf("result = %s, want the second round's answer", result.Event)
	}
	if _, err := queue.Push(json.RawMessage(`"too late"`)); !errors.Is(err, ErrSteerClosed) {
		t.Fatalf("Push() after the run = %v, want ErrSteerClosed", err)
	}
}

func TestSteerQueueRefusesBadInputAFullQueueAndAfterClose(t *testing.T) {
	queue := NewSteerQueue(1)
	if _, err := queue.Push(json.RawMessage(`42`)); err == nil {
		t.Fatal("a number was queued")
	}
	if pending, err := queue.Push(json.RawMessage(`"one"`)); err != nil || pending != 1 {
		t.Fatalf("Push() = %d, %v", pending, err)
	}
	if _, err := queue.Push(json.RawMessage(`"two"`)); !errors.Is(err, ErrSteerFull) {
		t.Fatalf("Push() over the limit = %v, want ErrSteerFull", err)
	}
	if left := queue.Close(); len(left) != 1 {
		t.Fatalf("Close() returned %d messages, want the waiting one", len(left))
	}
	if _, err := queue.Push(json.RawMessage(`"three"`)); !errors.Is(err, ErrSteerClosed) {
		t.Fatalf("Push() after Close = %v", err)
	}
	if _, err := NewSteerQueue(0).Push(json.RawMessage(`"x"`)); !errors.Is(err, ErrSteerFull) {
		t.Fatalf("a zero limit queued a message: %v", err)
	}
	var none *SteerQueue
	if _, err := none.Push(json.RawMessage(`"x"`)); !errors.Is(err, ErrSteerClosed) || !none.closeIfEmpty() || none.Close() != nil {
		t.Fatal("a nil queue must refuse and close")
	}
}
