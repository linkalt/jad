package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// usageProvider answers every request with one assistant message that reports 50 tokens.
func usageProvider(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"resp-b","status":"completed","model":"gpt-test","usage":{"total_tokens":50},"output":[` + item + `]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)
	return provider, &requests
}

func TestSessionBudgetRefusesARunOnceItIsSpent(t *testing.T) {
	provider, requests := usageProvider(t)
	_, api, messagesPath := startPreCompactService(t, map[string]any{"budget": map[string]any{"max_session_tokens": 50}}, provider, &lockedBuffer{})

	first := postRunBody(t, api.URL+"/api/sessions/local:one/run", `{"input":"first"}`)
	body, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || strings.Contains(string(body), "event: error") {
		t.Fatalf("first run status=%d body=%s", first.StatusCode, body)
	}
	// The ledger is the session's, in its state folder above the profile's transcript.
	ledger, err := os.ReadFile(filepath.Join(sessionStateDir(messagesPath), "budget.json"))
	if err != nil {
		t.Fatalf("the budget ledger was not written in the session's state folder: %v", err)
	}
	var stored struct {
		Session struct {
			Tokens int64 `json:"tokens"`
		} `json:"session"`
	}
	if err := json.Unmarshal(ledger, &stored); err != nil || stored.Session.Tokens != 50 {
		t.Fatalf("ledger = %s, %v", ledger, err)
	}

	before, _ := os.ReadFile(messagesPath)
	second := postRunBody(t, api.URL+"/api/sessions/local:one/run", `{"input":"second"}`)
	body, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if !strings.Contains(string(body), "budget.max_session_tokens limit of 50 tokens") {
		t.Fatalf("second run status=%d body=%s, want the budget refusal", second.StatusCode, body)
	}
	after, _ := os.ReadFile(messagesPath)
	if string(after) != string(before) || requests.Load() != 1 {
		t.Fatalf("the refused run stored its input or called the provider (requests %d)", requests.Load())
	}
}

func TestSessionBudgetIsOffByDefault(t *testing.T) {
	provider, _ := usageProvider(t)
	_, api, messagesPath := startPreCompactService(t, map[string]any{}, provider, &lockedBuffer{})
	response := postRunBody(t, api.URL+"/api/sessions/local:one/run", `{"input":"hi"}`)
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if _, err := os.Stat(filepath.Join(sessionStateDir(messagesPath), "budget.json")); !os.IsNotExist(err) {
		t.Fatalf("a budget ledger was written with no budget set: %v", err)
	}
}

func TestSessionBudgetCostCapNeedsPricing(t *testing.T) {
	provider, requests := usageProvider(t)
	logs := &lockedBuffer{}
	_, api, _ := startPreCompactService(t, map[string]any{"budget": map[string]any{"max_daily_cost_usd": 1}}, provider, logs)
	response := postRunBody(t, api.URL+"/api/sessions/local:one/run", `{"input":"hi"}`)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "event: error") || !strings.Contains(logs.String(), "budget caps a cost in US dollars") || requests.Load() != 0 {
		t.Fatalf("status=%d body=%s requests=%d logs=%s", response.StatusCode, body, requests.Load(), logs.String())
	}
}

// sessionStateDir is <state>/<session id> for a transcript at <state>/<session id>/<kind>/<profile>/messages.jsonl.
func sessionStateDir(messagesPath string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(messagesPath)))
}
