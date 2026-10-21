package httpapi

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func attachDrainers(run *activeRun, subscribers int) {
	for index := 0; index < subscribers; index++ {
		queue := run.subscribe()
		go func() {
			for {
				if _, ok := queue.next(context.Background()); !ok {
					return
				}
			}
		}()
	}
}

// BenchmarkPublishMessageFanOut is one emit of a 2 KiB, one-part message to N draining
// subscribers.
func BenchmarkPublishMessageFanOut(b *testing.B) {
	for _, subscribers := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("%d-subscribers", subscribers), func(b *testing.B) {
			registry := newRunRegistry(time.Minute, 1<<30, 0)
			run, err := registry.begin("key", "turn")
			if err != nil {
				b.Fatal(err)
			}
			attachDrainers(run, subscribers)
			if err := run.publishMessage(benchmarkMessage(1, 2048, 0)); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 1; iteration <= b.N; iteration++ {
				if err := run.publishMessage(benchmarkMessage(1, 2048, iteration%64)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPublishFanOut is one small usage event (publish) to N draining subscribers.
func BenchmarkPublishFanOut(b *testing.B) {
	for _, subscribers := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("%d-subscribers", subscribers), func(b *testing.B) {
			registry := newRunRegistry(time.Minute, 1<<30, 0)
			run, err := registry.begin("key", "turn")
			if err != nil {
				b.Fatal(err)
			}
			attachDrainers(run, subscribers)
			payload := map[string]any{"input_tokens": 100, "output_tokens": 20}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if err := run.publish("usage", payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
