package openaiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkStream reads a canned SSE stream of N delta events from an httptest fake.
func BenchmarkStream(b *testing.B) {
	for _, events := range []int{1000, 10000} {
		var body strings.Builder
		for index := 0; index < events; index++ {
			fmt.Fprintf(&body, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"token %d \"}\n\n", index)
		}
		body.WriteString("data: [DONE]\n\n")
		payload := body.String()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(payload))
		}))
		client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1"})
		request := json.RawMessage(`{"model":"m","input":[]}`)
		b.Run(fmt.Sprintf("%d-events", events), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				count := 0
				if err := client.Stream(context.Background(), request, func(Event) error { count++; return nil }); err != nil {
					b.Fatal(err)
				}
				if count != events {
					b.Fatalf("got %d events, want %d", count, events)
				}
			}
		})
		server.Close()
	}
}
