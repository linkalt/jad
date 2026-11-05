package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// summaryProvider answers every request with one assistant message of text and records
// the request bodies.
func summaryProvider(t *testing.T, text string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"resp-s","status":"completed","model":"gpt-test","usage":{"input_tokens":5},"output":[` + item + `]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)
	return provider, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func compactionCacheText(t *testing.T, messagesPath string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(filepath.Dir(messagesPath), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(filepath.Base(path), "compaction") && strings.HasSuffix(path, ".json") {
			data, _ := os.ReadFile(path)
			found += string(data)
		}
		return nil
	})
	return found
}

func TestManualCompactionStoresTheModelsSummaryWhenEnabled(t *testing.T) {
	provider, requests := summaryProvider(t, "MANUAL-SUMMARY")
	logs := &lockedBuffer{}
	_, api, messagesPath := startPreCompactService(t, map[string]any{
		"compaction": map[string]any{
			"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}},
			"summary":   map[string]any{"enabled": true, "prompt": "WRITE THE SUMMARY"},
		},
	}, provider, logs)

	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	body := readMutationResponse(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	sent := requests()
	if len(sent) != 1 || !strings.Contains(sent[0], "WRITE THE SUMMARY") || !strings.Contains(sent[0], "old context") || strings.Contains(sent[0], "latest context") {
		t.Fatalf("summary requests = %q", sent)
	}
	cache := compactionCacheText(t, messagesPath)
	if !strings.Contains(cache, "MANUAL-SUMMARY") || strings.Contains(cache, "old context") {
		t.Fatalf("compaction cache = %s, want the summary and not the dropped turn", cache)
	}
	transcript, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), "MANUAL-SUMMARY") {
		t.Fatalf("the summary reached messages.jsonl: %s", transcript)
	}
}

func TestManualCompactionSendsNoSummaryRequestByDefault(t *testing.T) {
	provider, requests := summaryProvider(t, "UNUSED")
	logs := &lockedBuffer{}
	_, api, messagesPath := startPreCompactService(t, map[string]any{
		"compaction": map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
	}, provider, logs)
	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	body := readMutationResponse(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	if sent := requests(); len(sent) != 0 {
		t.Fatalf("provider requests = %d, want none with the summary off", len(sent))
	}
	if strings.Contains(compactionCacheText(t, messagesPath), "Summary of the earlier conversation") {
		t.Fatal("a summary was stored with the summary off")
	}
}
