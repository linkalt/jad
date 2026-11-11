package app

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// baselineTurns is a transcript of count user turns, each answered by one response that used 10
// input and 1 output tokens, numbered from first.
func baselineTurns(first, count int) string {
	var body strings.Builder
	for turn := first; turn < first+count; turn++ {
		fmt.Fprintf(&body, `{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`+"\n", turn)
		fmt.Fprintf(&body, `{"type":"response.completed","session_turn":%d,"session_step":1,"response":{"id":"resp-%d","status":"completed","usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}}`+"\n", turn, turn, turn)
	}
	return body.String()
}

// baselineCompaction keeps only the newest turn, so earlier turns leave the transcript.
var baselineCompaction = transcript.CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace"}

type baselineSession struct {
	shared *sharedSession
	config sessionfs.SessionConfig
}

// newBaselineSession starts a session whose transcript holds turns user turns.
func newBaselineSession(t *testing.T, turns int) baselineSession {
	t.Helper()
	f := newUsageTimestampFixture(t)
	if err := os.WriteFile(f.config.MessagesPath, []byte(baselineTurns(1, turns)), 0o600); err != nil {
		t.Fatal(err)
	}
	shared, release, err := f.resolver.sharedStore(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return baselineSession{shared: shared, config: f.config}
}

func (b baselineSession) scan(t *testing.T) transcriptIndex {
	t.Helper()
	index, err := scanTranscriptIndex(b.shared, b.config)
	if err != nil {
		t.Fatalf("scanTranscriptIndex: %v", err)
	}
	return index
}

func (b baselineSession) appendRaw(t *testing.T, text string) {
	t.Helper()
	file, err := os.OpenFile(b.config.MessagesPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func (b baselineSession) transcriptText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(b.config.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wantTotals fails unless the usage counts responses completed responses of 10 input tokens.
func wantTotals(t *testing.T, label string, usage webui.UsageSummary, responses int64) {
	t.Helper()
	if usage.CompletedResponses != responses || usage.InputTokens == nil || *usage.InputTokens != 10*responses ||
		usage.OutputTokens == nil || *usage.OutputTokens != responses {
		t.Fatalf("%s: usage = %+v (input %v), want %d responses of 10 input tokens", label, usage, usage.InputTokens, responses)
	}
}
