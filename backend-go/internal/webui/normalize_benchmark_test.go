package webui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkStreamingRunMultiStepDeltas streams text deltas into a run that already holds
// several text and reasoning parts (an answer that follows tool rounds), so the cost of
// a snapshot against the text already in the message shows.
func BenchmarkStreamingRunMultiStepDeltas(b *testing.B) {
	const resetEvery = 1000
	delta := json.RawMessage(`{"type":"response.output_text.delta","delta":"word "}`)
	for _, shape := range []struct{ steps, bytesPerStep int }{{1, 100 << 10}, {20, 5 << 10}, {50, 4 << 10}} {
		b.Run(fmt.Sprintf("%dsteps-%dKB", shape.steps, shape.steps*shape.bytesPerStep>>10), func(b *testing.B) {
			build := func() *StreamingRun {
				run := NewStreamingRun("turn", "now")
				chunk := strings.Repeat("x", shape.bytesPerStep/2)
				for step := 0; step < shape.steps; step++ {
					reasoning, _ := json.Marshal(map[string]string{"type": "response.reasoning_text.delta", "delta": chunk})
					text, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": chunk})
					run.Update(reasoning)
					run.Update(text)
					if step < shape.steps-1 {
						run.AddToolResult(json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call-%d","output":"r"}`, step)))
					}
				}
				return run
			}
			run := build()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if iteration > 0 && iteration%resetEvery == 0 {
					b.StopTimer()
					run = build()
					b.StartTimer()
				}
				if _, changed := run.Update(delta); !changed {
					b.Fatal("delta did not change the message")
				}
			}
		})
	}
}

// historyFixture is a transcript of turns: a user message, a response with one tool
// call, its tool output of outputBytes (text, or one inline PNG when image is true), and
// a final answer.
func historyFixture(turns, outputBytes int, image bool) []TranscriptRecord {
	records := make([]TranscriptRecord, 0, turns*4)
	offset := int64(0)
	add := func(raw string) {
		records = append(records, TranscriptRecord{Offset: offset, Prefix: "o", Raw: json.RawMessage(raw)})
		offset += int64(len(raw)) + 1
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, outputBytes*3/4)...)
	for turn := 0; turn < turns; turn++ {
		add(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}],"created_at":1700000000}`, turn))
		add(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d-a","created_at":1700000001,"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"function_call","call_id":"call-%d","name":"read","arguments":"{\"path\":\"a\"}"}]}}`, turn, turn))
		var output string
		if image {
			url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			output = fmt.Sprintf(`[{"type":"input_text","text":"{\"status\":\"ok\"}"},{"type":"input_image","image_url":%q}]`, url)
		} else {
			encoded, _ := json.Marshal(`{"status":"ok"}` + "\n" + strings.Repeat("line of file text\n", outputBytes/18))
			output = string(encoded)
		}
		add(fmt.Sprintf(`{"type":"function_call_output","call_id":"call-%d","output":%s}`, turn, output))
		add(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d-b","created_at":1700000002,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}}`, turn, turn))
	}
	return records
}

// BenchmarkNormalizeTranscript projects a page of turns whose tool outputs are text of
// 1 KB or 200 KB, or a list with one inline image of 200 KB.
func BenchmarkNormalizeTranscript(b *testing.B) {
	for _, shape := range []struct {
		name        string
		outputBytes int
		image       bool
	}{{"text-1KB", 1 << 10, false}, {"text-200KB", 200 << 10, false}, {"image-200KB", 200 << 10, true}} {
		b.Run(shape.name, func(b *testing.B) {
			records := historyFixture(50, shape.outputBytes, shape.image)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := NormalizeTranscript(records); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindInlineImage serves the last image of a turn list in which every tool
// output carries one inline image of 2 MB.
func BenchmarkFindInlineImage(b *testing.B) {
	for _, turns := range []int{1, 10} {
		b.Run(fmt.Sprintf("%dimages-2MB", turns), func(b *testing.B) {
			records := historyFixture(turns, 2<<20, true)
			last := turns - 1
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, _, ok := FindInlineImage(records, fmt.Sprintf("resp-%d-a", last), fmt.Sprintf("call-%d", last), 0); !ok {
					b.Fatal("image not found")
				}
			}
		})
	}
}
