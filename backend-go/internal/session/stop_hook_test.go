package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

func TestStopHookMakesTheRunGoOnUpToItsLimit(t *testing.T) {
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		tokenRound(0, 0, seventyTokens),
		tokenRound(1, 0, seventyTokens),
		tokenRound(2, 0, seventyTokens),
	}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		StopMaxContinues: 2,
		StopMessageRole:  "developer",
		OnStop: func(_ context.Context, _ string, answer string, continues int) (string, error) {
			asked = append(asked, answer)
			return "keep going " + itoa(continues), nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// Two continues, then the limit lets the run end without asking again.
	if len(streamer.requests) != 3 || len(asked) != 2 || asked[0] != "all done" {
		t.Fatalf("requests = %d, asked = %q", len(streamer.requests), asked)
	}
	if !strings.Contains(string(streamer.requests[2]), "keep going 1") || !strings.Contains(string(streamer.requests[1]), `"role":"developer"`) {
		t.Fatalf("the continue message is not in the next request: %s", streamer.requests[2])
	}
	all := ""
	_ = journal.ReadLines(func(line []byte) error { all += string(line); return nil })
	if strings.Count(all, "keep going") != 2 {
		t.Fatalf("transcript = %s", all)
	}
	// A stop hook's message continues the run's turn, as a steering message does: its
	// record is marked so compaction keeps it with the run, and the mark never reaches the
	// provider.
	var marked int
	_ = journal.ReadLines(func(line []byte) error {
		if strings.Contains(string(line), "keep going") && strings.Contains(string(line), `"`+transcript.SessionInputSteerField+`":true`) {
			marked++
		}
		return nil
	})
	if marked != 2 {
		t.Fatalf("stop hook records with the steer mark = %d, want 2: %s", marked, all)
	}
	for index, request := range streamer.requests {
		if strings.Contains(string(request), transcript.SessionInputSteerField) {
			t.Fatalf("request %d carries the steer mark: %s", index, request)
		}
	}
}

func TestStopHookWithNoMessageLetsTheRunEnd(t *testing.T) {
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{tokenRound(0, 0, seventyTokens)}}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		StopMaxContinues: 3,
		OnStop:           func(context.Context, string, string, int) (string, error) { calls++; return "", nil },
	})
	if err != nil || calls != 1 || len(streamer.requests) != 1 {
		t.Fatalf("err = %v, calls = %d, requests = %d", err, calls, len(streamer.requests))
	}
}
