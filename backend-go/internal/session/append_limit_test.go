package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
)

// seedBackup writes a transcript backup of n old user turns at path.
func seedBackup(t *testing.T, path string, n int) {
	t.Helper()
	var builder strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&builder, `{"type":"message","role":"user","content":[{"type":"input_text","text":"seed-%d"}]}`+"\n", i)
		fmt.Fprintf(&builder, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"seed-answer-%d"}]}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func backupUserTexts(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"role":"user"`) {
			continue
		}
		start := strings.Index(line, `"text":"`) + len(`"text":"`)
		texts = append(texts, line[start:start+strings.Index(line[start:], `"`)])
	}
	return texts
}

// AppendLimit reaches the backup of an automatic compaction.
func TestAutomaticCompactionRollsTheAppendBackupByAppendLimit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		want  []string
	}{
		{0, []string{"seed-1", "seed-2", "seed-3", "older context", "new request"}},
		{2, []string{"older context", "new request"}},
		{1, []string{"new request"}},
		{3, []string{"seed-3", "older context", "new request"}},
	} {
		t.Run(fmt.Sprintf("limit %d", tc.limit), func(t *testing.T) {
			store, journal, root := newAutomaticCompactionStore(t, "")
			backup := filepath.Join(root, "messages.backup.jsonl")
			seedBackup(t, backup, 3)
			appendOlderCompactionTurn(t, store, journal)
			event := responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":100},"output":[]}}`)}
			runner, err := NewRunner(store, &fakeStreamer{events: []responses.Event{event}}, ModelSettings{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
				AutomaticCompaction: &AutomaticCompactionSettings{
					Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: tc.limit,
				},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if got := backupUserTexts(t, backup); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("backup user turns = %v, want %v", got, tc.want)
			}
		})
	}
}

// AppendLimit reaches the backup of the compaction a context overflow starts, which the
// session's compaction settings drive.
func TestOverflowCompactionRollsTheAppendBackupByAppendLimit(t *testing.T) {
	store, journal, root := newAutomaticCompactionStore(t, "")
	backup := filepath.Join(root, "messages.backup.jsonl")
	seedBackup(t, backup, 3)
	appendOlderCompactionTurn(t, store, journal)
	streamer := &scriptedStreamer{errs: []error{errOverflow}, rounds: [][]responses.Event{nil, finalAnswerRound("recovered", 10)}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []overflowCall
	settings := overflowCompaction(true, 1, &calls)
	settings.AppendLimit = 2
	if _, err := runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{AutomaticCompaction: settings}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(calls) != 1 || !calls[0].compacted {
		t.Fatalf("overflow calls = %+v, want one compaction", calls)
	}
	if got := backupUserTexts(t, backup); strings.Join(got, ",") != "older context,new request" {
		t.Fatalf("backup user turns = %v, want the newest two (the run's own request is one)", got)
	}
}

// A corrupt backup makes the automatic compaction fail the run with the file and byte,
// and leaves the transcript's history as it was.
func TestAutomaticCompactionStopsOnACorruptBackupWithAppendLimit(t *testing.T) {
	store, journal, root := newAutomaticCompactionStore(t, "")
	backup := filepath.Join(root, "messages.backup.jsonl")
	if err := os.WriteFile(backup, []byte("{oops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendOlderCompactionTurn(t, store, journal)
	event := responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":100},"output":[]}}`)}
	runner, err := NewRunner(store, &fakeStreamer{events: []responses.Event{event}}, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		AutomaticCompaction: &AutomaticCompactionSettings{Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: 2},
	})
	if err == nil || !strings.Contains(err.Error(), "messages.backup.jsonl: record at byte 0") {
		t.Fatalf("Run() error = %v, want the backup error naming the file and byte", err)
	}
	if got, _ := os.ReadFile(backup); string(got) != "{oops\n" {
		t.Fatalf("the backup was changed: %q", got)
	}
	history, err := store.ReadInputItems()
	if err != nil || len(history) < 3 || !strings.Contains(string(history[0]), "older context") {
		t.Fatalf("history after the failed compaction = %q, %v", history, err)
	}
}
