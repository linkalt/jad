package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/transcript"
)

func pauseQuantile(before, after *metrics.Float64Histogram, q float64) float64 {
	var total uint64
	delta := make([]uint64, len(after.Counts))
	for index := range after.Counts {
		delta[index] = after.Counts[index]
		if before != nil {
			delta[index] -= before.Counts[index]
		}
		total += delta[index]
	}
	if total == 0 {
		return 0
	}
	target, seen := uint64(float64(total)*q), uint64(0)
	for index, count := range delta {
		seen += count
		if seen > target {
			return after.Buckets[index+1]
		}
	}
	return after.Buckets[len(after.Buckets)-1]
}

// BenchmarkLongRunHeap drives b.N sequential runs through the run route against one
// handler, one runner and one growing transcript, with a local provider that streams 200
// text deltas. Every b.N/4 runs it records the heap after two forced collections, so the
// last metrics show whether the live heap grows with the number of runs.
func BenchmarkLongRunHeap(b *testing.B) {
	const deltas = 200
	var body strings.Builder
	for index := 0; index < deltas; index++ {
		body.WriteString("data: " + deltaEvent + "\n\n")
	}
	fmt.Fprintf(&body, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n\n", strings.Repeat("word ", deltas))
	body.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-test\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9},\"output\":[]}}\n\ndata: [DONE]\n\n")
	payload := body.String()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(payload))
	}))
	defer provider.Close()

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
	handler, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 64 << 20, RunStateRetention: 20 * time.Millisecond, ToolStreamInterval: 120 * time.Millisecond, SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 16 * 1024}, resolver)
	if err != nil {
		b.Fatal(err)
	}
	defer handler.Close()

	samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/gc/pauses:seconds"}, {Name: "/gc/heap/goal:bytes"}}
	metrics.Read(samples)
	cyclesBefore := samples[0].Value.Uint64()
	var pausesBefore *metrics.Float64Histogram
	if histogram := samples[1].Value.Float64Histogram(); histogram != nil {
		copied := *histogram
		copied.Counts = append([]uint64(nil), histogram.Counts...)
		pausesBefore = &copied
	}
	goroutinesBefore := runtime.NumGoroutine()
	var mem runtime.MemStats
	var heaps []float64
	every := b.N / 4
	if every < 1 {
		every = 1
	}
	var peakGoal uint64
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run", strings.NewReader(`{"input":"hello"}`))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "event: completed") {
			b.Fatalf("run failed: %d %.200s", response.Code, response.Body.String())
		}
		if (iteration+1)%every == 0 {
			b.StopTimer()
			time.Sleep(60 * time.Millisecond)
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&mem)
			heaps = append(heaps, float64(mem.HeapAlloc))
			metrics.Read(samples[2:])
			if goal := samples[2].Value.Uint64(); goal > peakGoal {
				peakGoal = goal
			}
			b.StartTimer()
		}
	}
	b.StopTimer()
	metrics.Read(samples)
	cycles := samples[0].Value.Uint64() - cyclesBefore
	pauses := samples[1].Value.Float64Histogram()
	if len(heaps) >= 2 {
		b.ReportMetric(heaps[0]/(1<<20), "heap-MiB-first")
		b.ReportMetric(heaps[len(heaps)-1]/(1<<20), "heap-MiB-last")
		runsBetween := float64(every * (len(heaps) - 1))
		b.ReportMetric((heaps[len(heaps)-1]-heaps[0])/runsBetween, "heap-B/run-growth")
	}
	b.ReportMetric(float64(cycles)/float64(b.N), "gc-cycles/run")
	b.ReportMetric(pauseQuantile(pausesBefore, pauses, 0.99)*1e6, "gc-pause-p99-us")
	b.ReportMetric(float64(peakGoal)/(1<<20), "heap-goal-MiB")
	b.ReportMetric(float64(runtime.NumGoroutine()-goroutinesBefore), "goroutine-delta")
}
