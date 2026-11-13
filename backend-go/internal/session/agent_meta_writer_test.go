package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

// runWithSteerAndStop runs "fix the bug" with a steering message during the first round and a
// stop hook message of stopRole after the second, on a store with or without the user timestamp,
// and returns the transcript lines and the provider requests.
func runWithSteerAndStop(t *testing.T, userTimestamp bool, stopRole string) ([]string, []string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(dir, "staging"), transcript.TurnStoreOptions{MaxItemBytes: 1 << 20, UserTimestamp: userTimestamp})
	if err != nil {
		t.Fatal(err)
	}
	queue := NewSteerQueue(4)
	streamer := &pushingStreamer{scriptedStreamer: scriptedStreamer{rounds: [][]responses.Event{finalAnswerRound("first", 1), finalAnswerRound("second", 1), finalAnswerRound("third", 1)}}}
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
	if _, err := runner.Run(context.Background(), json.RawMessage(`"fix the bug"`), RunOptions{
		Steer:            queue,
		StopMaxContinues: 1,
		StopMessageRole:  stopRole,
		OnStop: func(context.Context, string, string, int) (string, error) {
			return "keep going", nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), streamer.requests
}

// The run input, a steering message and a stop hook message with the user role get the stored
// time; a stop hook message with the developer role does not. The provider requests are the
// same bytes with the setting on and off.
func TestRunInputSteeringAndUserStopMessagesGetTheStoredTime(t *testing.T) {
	stamp := regexp.MustCompile(`,"` + transcript.MetaField + `":\{"` + transcript.MetaStoredAt + `":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z"\}\}$`)
	for _, role := range []string{"user", "developer"} {
		lines, requests := runWithSteerAndStop(t, true, role)
		_, offRequests := runWithSteerAndStop(t, false, role)
		want := map[string]bool{"fix the bug": true, "also check the tests": true, "keep going": role == "user"}
		found := 0
		for _, line := range lines {
			for text, stamped := range want {
				if strings.Contains(line, `"text":"`+text+`"`) && !strings.Contains(line, "response.completed") {
					found++
					if stamp.MatchString(line) != stamped {
						t.Errorf("role %s: %q stamped = %v, want %v: %s", role, text, !stamped, stamped, line)
					}
				}
			}
			if strings.Contains(line, "response.completed") && strings.Contains(line, transcript.MetaField) {
				t.Errorf("a response record has the meta: %s", line)
			}
		}
		if found != 3 {
			t.Fatalf("role %s: found %d input records:\n%s", role, found, strings.Join(lines, "\n"))
		}
		if strings.Join(requests, "\n") != strings.Join(offRequests, "\n") || strings.Contains(strings.Join(requests, ""), "__agent_") {
			t.Fatalf("role %s: requests differ between on and off:\n%s\n---\n%s", role, requests, offRequests)
		}
	}
}
