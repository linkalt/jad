package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
	"jad/backend-go/internal/webui"
)

// BenchmarkStreamEventPipeline is the per-provider-event work of the run executor
// (OnEvent): delta decode, content cache, message normalization and the coalescing
// emitter, with a publish that only counts. The leading parts are finished tool results,
// so the growth of the cost with the message shape shows.
func BenchmarkStreamEventPipeline(b *testing.B) {
	delta := json.RawMessage(`{"type":"response.output_text.delta","sequence_number":42,"item_id":"msg_0123456789abcdef","output_index":0,"content_index":0,"delta":"word ","logprobs":[]}`)
	for _, leading := range []int{0, 20, 100} {
		b.Run(fmt.Sprintf("%d-leading-parts", leading), func(b *testing.B) {
			registry := newRunRegistry(time.Minute, 1<<30, 0)
			run, err := registry.begin("key", "turn")
			if err != nil {
				b.Fatal(err)
			}
			run.setContentCache(newRunContentCache(serverconfig.DefaultRunContentMemoryBytes, serverconfig.DefaultRunContentWriteBatchBytes, 25*time.Millisecond))
			streaming := webui.NewStreamingRun("turn", "now")
			for index := 0; index < leading; index++ {
				item := json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call-%d","output":"result %d"}`, index, index))
				if _, ok := streaming.AddToolResult(item); !ok {
					b.Fatal("tool result was not added")
				}
			}
			var published atomic.Int64
			emitter := newMessageStreamEmitter(120*time.Millisecond, func(webui.Message) error { published.Add(1); return nil })
			defer emitter.discard()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				var payload struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(delta, &payload) == nil {
					run.cacheContent(payload.Delta)
				}
				message, changed := streaming.Update(delta)
				if !changed {
					b.Fatal("delta did not change the message")
				}
				if err := emitter.push(message); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(published.Load()), "publishes")
		})
	}
}

// BenchmarkStreamEventPipelineLongText is the same per-event work when the streamed
// message already holds text: one text part, or several text parts of the same total
// (text that follows tool calls), so the cost against text length shows. The run is
// rebuilt every resetEvery events so the text does not grow with b.N.
func BenchmarkStreamEventPipelineLongText(b *testing.B) {
	const resetEvery = 1000
	delta := json.RawMessage(`{"type":"response.output_text.delta","sequence_number":42,"item_id":"msg_0123456789abcdef","output_index":0,"content_index":0,"delta":"word ","logprobs":[]}`)
	for _, shape := range []struct{ textParts, totalBytes int }{{1, 10 << 10}, {1, 100 << 10}, {20, 100 << 10}} {
		b.Run(fmt.Sprintf("%dtext-parts-%dKB", shape.textParts, shape.totalBytes>>10), func(b *testing.B) {
			chunk := strings.Repeat("x", shape.totalBytes/shape.textParts)
			build := func() *webui.StreamingRun {
				streaming := webui.NewStreamingRun("turn", "now")
				for index := 0; index < shape.textParts; index++ {
					prefill, err := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": chunk})
					if err != nil {
						b.Fatal(err)
					}
					if _, ok := streaming.Update(prefill); !ok {
						b.Fatal("prefill did not change the message")
					}
					if index < shape.textParts-1 {
						item := json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call-%d","output":"r"}`, index))
						if _, ok := streaming.AddToolResult(item); !ok {
							b.Fatal("tool result was not added")
						}
					}
				}
				return streaming
			}
			newEmitter := func() *messageStreamEmitter {
				return newMessageStreamEmitter(120*time.Millisecond, func(webui.Message) error { return nil })
			}
			streaming, emitter := build(), newEmitter()
			defer func() { emitter.discard() }()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if iteration > 0 && iteration%resetEvery == 0 {
					b.StopTimer()
					emitter.discard()
					streaming, emitter = build(), newEmitter()
					b.StartTimer()
				}
				message, changed := streaming.Update(delta)
				if !changed {
					b.Fatal("delta did not change the message")
				}
				if err := emitter.push(message); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
