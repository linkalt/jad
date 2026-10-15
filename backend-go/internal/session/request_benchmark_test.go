package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkBuildRequestWithTranscriptHistory(b *testing.B) {
	history := make([][]byte, 100)
	for index := range history {
		history[index] = []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"item-%d"}]}`, index))
	}
	settings := ModelSettings{
		Model:      "gpt-test",
		Request:    json.RawMessage(`{"temperature":0.2,"metadata":{"source":"benchmark"}}`),
		PromptRole: "developer",
		PromptText: "benchmark prompt",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := BuildRequest(settings, history); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildRequestWithLargeHistory(b *testing.B) {
	text := strings.Repeat("x", 1500)
	history := make([][]byte, 2000)
	for index := range history {
		history[index] = []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"%d %s"}]}`, index, text))
	}
	settings := ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "benchmark prompt"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := BuildRequest(settings, history); err != nil {
			b.Fatal(err)
		}
	}
}

func TestBuildRequestRejectsNonObjectHistoryItems(t *testing.T) {
	settings := ModelSettings{Model: "gpt-test"}
	for name, item := range map[string]string{
		"array":    `[1]`,
		"string":   `"x"`,
		"number":   `1`,
		"null":     `null`,
		"invalid":  `{"a":`,
		"trailing": `{} {}`,
		"empty":    ``,
	} {
		_, err := BuildRequest(settings, [][]byte{[]byte(`{"type":"message"}`), []byte(item)})
		if err == nil || err.Error() != "history item 1 must be a JSON object" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := BuildRequest(settings, [][]byte{[]byte(" \n{\"type\":\"message\"}\n")}); err != nil {
		t.Errorf("padded object: %v", err)
	}
}
