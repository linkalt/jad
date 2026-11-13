package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/transcript"
)

// The provider request is byte for byte the same whatever JAD-only fields the stored user
// records carry: none, a well-formed or malformed __agent_meta, or an __agent_ key no build
// knows. The comparison is not normalised.
func TestProviderRequestIsTheSameWithAndWithoutAgentFields(t *testing.T) {
	extras := []string{
		"",
		`,"` + transcript.MetaField + `":{"` + transcript.MetaStoredAt + `":"2026-11-13T07:01:02.345Z"}`,
		`,"` + transcript.MetaField + `":["malformed"]`,
		`,"__agent_x":{"a":1},"` + transcript.MetaField + `":{"unknown":"x"}`,
	}
	var want []json.RawMessage
	for _, extra := range extras {
		store, journal, stagingDir := newTestStore(t, transcript.ResponseMetadataMinimal)
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"earlier"}]}`)); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"resp_0","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`)); err != nil {
			t.Fatal(err)
		}
		if extra != "" {
			path := filepath.Join(filepath.Dir(stagingDir), "messages.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			first, rest, _ := bytes.Cut(data, []byte("\n"))
			data = append(append(append(append([]byte(nil), bytes.TrimSuffix(first, []byte("}"))...), extra...), "}\n"...), rest...)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		streamer := &requestStreamer{}
		runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "base prompt"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background(), json.RawMessage(`"next"`), RunOptions{TurnID: "turn-meta"}); err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		if len(streamer.requests) != 1 || strings.Contains(string(streamer.requests[0]), "__agent_") {
			t.Fatalf("%q: requests %s", extra, streamer.requests)
		}
		if want == nil {
			want = streamer.requests
			continue
		}
		if !bytes.Equal(streamer.requests[0], want[0]) {
			t.Fatalf("%q: request\n%s\nwant\n%s", extra, streamer.requests[0], want[0])
		}
	}
}
