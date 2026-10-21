package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// BenchmarkStreamConnectionReuse reports new TCP connections per call over
// HTTP/1.1 when the handler reads every event versus stops at the terminal
// event the way the session runner does.
func BenchmarkStreamConnectionReuse(b *testing.B) {
	payload := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"a\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"
	stop := errors.New("terminal")
	for _, mode := range []string{"read-to-end", "stop-at-terminal"} {
		b.Run(mode, func(b *testing.B) {
			var connections atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(payload))
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", HTTPClient: &http.Client{Transport: transport}})
			request := json.RawMessage(`{"model":"m","input":[]}`)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				err := client.Stream(context.Background(), request, func(event Event) error {
					if mode == "stop-at-terminal" && event.Type == "response.completed" {
						return stop
					}
					return nil
				})
				if mode == "read-to-end" && err != nil {
					b.Fatal(err)
				}
				if mode == "stop-at-terminal" && !errors.Is(err, stop) {
					b.Fatalf("want stop error, got %v", err)
				}
			}
			b.ReportMetric(float64(connections.Load())/float64(b.N), "newconn/op")
		})
	}
}
