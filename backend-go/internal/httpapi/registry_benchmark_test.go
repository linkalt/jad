package httpapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/webui"
)

func BenchmarkSSEDeltaEventEncoding(b *testing.B) {
	data := map[string]any{"id": "web-00000000-0000-4000-8000-000000000000", "index": 0, "append": "response text delta"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := makeSSEEvent("ui.delta", data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkMessage(parts, bytesPerPart int, revision int) webUIMessage {
	message := webUIMessage{ID: "web-00000000-0000-4000-8000-000000000000", Sender: "assistant", Status: "streaming"}
	text := strings.Repeat("x", bytesPerPart)
	for index := 0; index < parts; index++ {
		message.Parts = append(message.Parts, webui.Part{Type: "text", Text: text})
	}
	if parts > 0 {
		message.Parts[parts-1].Text += strings.Repeat("y", revision)
	}
	return message
}

// BenchmarkPublishMessage is one emit of a message of N parts that changed by one
// delta, with one subscriber attached.
func BenchmarkPublishMessage(b *testing.B) {
	for _, shape := range []struct{ parts, bytes int }{{1, 2048}, {1, 50 << 10}, {100, 2048}, {100, 20 << 10}} {
		b.Run(fmt.Sprintf("%dx%dB", shape.parts, shape.bytes), func(b *testing.B) {
			registry := newRunRegistry(time.Minute, 1<<30, 0)
			run, err := registry.begin("key", "turn")
			if err != nil {
				b.Fatal(err)
			}
			queue := run.subscribe()
			go func() {
				for {
					if _, ok := queue.next(context.Background()); !ok {
						return
					}
				}
			}()
			if err := run.publishMessage(benchmarkMessage(shape.parts, shape.bytes, 0)); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 1; iteration <= b.N; iteration++ {
				if err := run.publishMessage(benchmarkMessage(shape.parts, shape.bytes, iteration%64)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRunStateWhilePublishing is how long a polling route (state) waits for the
// run lock while another goroutine publishes a large message back to back.
func BenchmarkRunStateWhilePublishing(b *testing.B) {
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		b.Fatal(err)
	}
	var stop atomic.Bool
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for revision := 0; !stop.Load(); revision++ {
			_ = run.publishMessage(benchmarkMessage(100, 20<<10, revision%64))
		}
	}()
	b.ResetTimer()
	var worst time.Duration
	for iteration := 0; iteration < b.N; iteration++ {
		started := time.Now()
		_ = run.state()
		if elapsed := time.Since(started); elapsed > worst {
			worst = elapsed
		}
	}
	b.StopTimer()
	stop.Store(true)
	group.Wait()
	b.ReportMetric(float64(worst.Microseconds()), "worst-wait-us")
}
