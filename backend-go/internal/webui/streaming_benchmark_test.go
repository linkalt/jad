package webui

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkStreamingRunTextDeltas(b *testing.B) {
	for _, total := range []int{2_000, 20_000, 200_000} {
		b.Run(fmt.Sprintf("%dB", total), func(b *testing.B) {
			const token = "word "
			deltas := total / len(token)
			event := []byte(`{"type":"response.output_text.delta","delta":"` + token + `"}`)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				run := NewStreamingRun("turn", "now")
				for d := 0; d < deltas; d++ {
					if _, changed := run.Update(event); !changed {
						b.Fatal("delta did not change the message")
					}
				}
			}
		})
	}
}

func TestStreamingRunTextDeltasKeepEveryPartCumulative(t *testing.T) {
	run := NewStreamingRun("turn", "now")
	steps := []struct{ kind, event, text string }{
		{"text", `{"type":"response.output_text.delta","delta":"Hel"}`, "Hel"},
		{"text", `{"type":"response.output_text.delta","delta":"lo"}`, "lo"},
		{"reasoning", `{"type":"response.reasoning_text.delta","delta":"Think"}`, "Think"},
		{"reasoning", `{"type":"response.reasoning_summary_text.delta","delta":"ing"}`, "ing"},
		{"text", `{"type":"response.output_text.delta","delta":" there"}`, " there"},
	}
	var message Message
	var changed bool
	for _, step := range steps {
		message, changed = run.Update([]byte(step.event))
		if !changed {
			t.Fatalf("event %s did not change the message", step.event)
		}
		for _, part := range message.Parts {
			if part.Type == step.kind && part.Text == "" {
				t.Fatalf("empty %s part in %+v", step.kind, message.Parts)
			}
		}
	}
	if len(message.Parts) != 3 || message.Parts[0].Text != "Hello" || message.Parts[1].Text != "Thinking" || message.Parts[2].Text != " there" {
		t.Fatalf("parts = %+v", message.Parts)
	}
	if message.Text != "Hello there" || message.ReasoningContent != "Thinking" {
		t.Fatalf("summaries = %q / %q", message.Text, message.ReasoningContent)
	}

	run.Update([]byte(`{"type":"response.completed","response":{"id":"r","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Final"}]}]}}`))
	message, _ = run.Update([]byte(`{"type":"response.output_text.delta","delta":"!"}`))
	last := message.Parts[len(message.Parts)-1]
	if last.Type != "text" || !strings.HasSuffix(last.Text, "!") {
		t.Fatalf("delta after a completed response lost its text: %+v", message.Parts)
	}
	for _, part := range message.Parts {
		if part.Type == "text" && part.Text == "!" {
			t.Fatalf("delta after completion started a stale part: %+v", message.Parts)
		}
	}
}
