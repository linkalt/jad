package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/transcript"
)

// BenchmarkRunRouteStreamingDeltas is one whole run through the run route with the real
// session runner and a local provider that streams N text deltas: provider decode, runner
// validation, normalization, coalescing, SSE frames and the commit. The response is not
// read by a browser; the recorder only collects it.
const deltaEvent = `{"type":"response.output_text.delta","sequence_number":42,"item_id":"msg_0123456789abcdef","output_index":0,"content_index":0,"delta":"word ","logprobs":[]}`

func BenchmarkRunRouteStreamingDeltas(b *testing.B) {
	for _, deltas := range []int{200, 2000} {
		b.Run(fmt.Sprintf("%d-deltas", deltas), func(b *testing.B) {
			var body strings.Builder
			for index := 0; index < deltas; index++ {
				body.WriteString("data: " + deltaEvent + "\n\n")
			}
			text := strings.Repeat("word ", deltas)
			fmt.Fprintf(&body, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n\n", text)
			body.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-test\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9},\"output\":[]}}\n\ndata: [DONE]\n\n")
			payload := body.String()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(payload))
			}))
			defer provider.Close()

			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				dir := b.TempDir()
				journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
				if err != nil {
					b.Fatal(err)
				}
				store, err := transcript.NewTurnStore(journal, filepath.Join(dir, "staging"), transcript.TurnStoreOptions{MaxItemBytes: 1 << 20, ResponseMetadata: transcript.ResponseMetadataFull})
				if err != nil {
					b.Fatal(err)
				}
				client := openaiapi.NewClient(openaiapi.Config{APIKey: "local-test-key", BaseURL: provider.URL + "/v1"})
				runner, err := session.NewRunner(store, client, session.ModelSettings{Model: "gpt-test", Request: json.RawMessage(`{}`)})
				if err != nil {
					b.Fatal(err)
				}
				resolver := &fakeResolver{resolved: ResolvedSession{Reference: "session-1", Runner: runner}}
				handler, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 64 << 20, RunStateRetention: time.Millisecond, ToolStreamInterval: 120 * time.Millisecond, SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 16 * 1024}, resolver)
				if err != nil {
					b.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run", strings.NewReader(`{"input":"hello"}`))
				request.Header.Set("Authorization", "Bearer "+testAPIToken)
				response := httptest.NewRecorder()
				b.StartTimer()
				handler.ServeHTTP(response, request)
				b.StopTimer()
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: completed") {
					b.Fatalf("run failed: %d %.200s", response.Code, response.Body.String())
				}
				handler.Close()
			}
		})
	}
}
