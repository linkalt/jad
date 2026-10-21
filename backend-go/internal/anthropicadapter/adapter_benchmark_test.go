package anthropicadapter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func benchmarkConfig(b *testing.B, upstreamURL string) *Config {
	b.Helper()
	path := filepath.Join(b.TempDir(), "adapter.json")
	if err := os.WriteFile(path, []byte(`{"upstream":{"base_url":"`+upstreamURL+`"}}`), 0o600); err != nil {
		b.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		b.Fatal(err)
	}
	return config
}

func BenchmarkTranslateRequest(b *testing.B) {
	config := benchmarkConfig(b, "http://127.0.0.1:1")
	var input strings.Builder
	for turn := 0; turn < 20; turn++ {
		fmt.Fprintf(&input, `{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]},`, turn, turn)
	}
	body := []byte(`{"model":"claude-x","stream":true,"instructions":"be brief","input":[` + input.String() + `{"type":"message","role":"user","content":[{"type":"input_text","text":"last"}]}],"tools":[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for iteration := 0; iteration < b.N; iteration++ {
		if _, apiErr := translateRequest(config, body); apiErr != nil {
			b.Fatalf("translate: %v", apiErr)
		}
	}
}

// BenchmarkStreamThroughAdapter sends N text deltas through the real handler: upstream
// Anthropic SSE from an httptest fake, translated to Responses SSE, flushed per event.
func BenchmarkStreamThroughAdapter(b *testing.B) {
	for _, deltas := range []int{100, 2000} {
		var upstreamBody strings.Builder
		upstreamBody.WriteString(sse(startEvent, `content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		for index := 0; index < deltas; index++ {
			upstreamBody.WriteString(sse(`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tok "}}`))
		}
		upstreamBody.WriteString(sse(`content_block_stop|{"type":"content_block_stop","index":0}`,
			`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`,
			`message_stop|{"type":"message_stop"}`))
		payload := upstreamBody.String()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, payload)
		}))
		adapter := httptest.NewServer(NewHandler(benchmarkConfig(b, upstream.URL), nil))
		b.Run(fmt.Sprintf("%d-deltas", deltas), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				request, _ := http.NewRequest(http.MethodPost, adapter.URL+"/v1/responses", strings.NewReader(`{"model":"claude-x","input":"hi","stream":true}`))
				request.Header.Set("x-api-key", "k")
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					b.Fatal(err)
				}
				count := 0
				err = readSSE(response.Body, func(sseEvent) bool { count++; return true })
				response.Body.Close()
				if err != nil || count < deltas {
					b.Fatalf("events=%d err=%v", count, err)
				}
			}
		})
		adapter.Close()
		upstream.Close()
	}
}
