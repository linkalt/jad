package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func profileUsageTranscript(inputTokens int) string {
	var body strings.Builder
	for turn := 1; turn <= 3; turn++ {
		fmt.Fprintf(&body, `{"type":"message","role":"user","content":[{"type":"input_text","text":"q%d"}]}`+"\n", turn)
		fmt.Fprintf(&body, `{"type":"response.completed","session_turn":%d,"session_step":1,"response":{"id":"r%d-%d","status":"completed","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}}`+"\n", turn, inputTokens, turn, inputTokens, inputTokens+1)
	}
	return body.String()
}

// The usage cache lives in each profile's own staging folder, so compacting one
// profile must keep its own totals (resetting only its current usage) and must
// leave every other profile's cache untouched.
func TestHTTPCompactOfOneProfileKeepsTheUsageOfEveryProfile(t *testing.T) {
	for _, compacted := range []string{"review/reviewer", "dev/coder"} {
		t.Run(compacted, func(t *testing.T) {
			f := newLayoutFixture(t)
			const session = "/api/sessions/local:demo"
			state := filepath.Join(f.cwd, ".jad", "state", "demo")
			stateOf := map[string]string{"dev/coder": filepath.Join(state, "dev", "coder"), "review/reviewer": filepath.Join(state, "review", "reviewer")}
			for profile, tokens := range map[string]int{"dev/coder": 4, "review/reviewer": 2} {
				if err := os.MkdirAll(stateOf[profile], 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stateOf[profile], "messages.jsonl"), []byte(profileUsageTranscript(tokens)), 0o600); err != nil {
					t.Fatal(err)
				}
				if status, body := f.do(t, http.MethodGet, session+"?profile="+url.QueryEscape(profile), ""); status != http.StatusOK {
					t.Fatalf("read %s: %d %s", profile, status, body)
				}
			}
			if status, body := f.do(t, http.MethodPost, session+"/compact?profile="+url.QueryEscape(compacted), ""); status != http.StatusOK {
				t.Fatalf("compact %s: %d %s", compacted, status, body)
			}
			for profile, tokens := range map[string]int64{"dev/coder": 4, "review/reviewer": 2} {
				raw, err := os.ReadFile(filepath.Join(stateOf[profile], "staging", "usage-cache.json"))
				if err != nil {
					t.Fatalf("usage cache of %s: %v", profile, err)
				}
				var cache usageCacheFile
				if err := json.Unmarshal(raw, &cache); err != nil {
					t.Fatal(err)
				}
				summary := cache.Summary
				if summary.CompletedResponses != 3 || summary.InputTokens == nil || *summary.InputTokens != 3*tokens {
					t.Fatalf("%s totals = %+v, want 3 responses and %d input tokens", profile, summary, 3*tokens)
				}
				current := summary.Current
				if current == nil || current.InputTokens == nil {
					t.Fatalf("%s current = %+v", profile, current)
				}
				want := tokens
				if profile == compacted {
					want = 0
				}
				if *current.InputTokens != want {
					t.Fatalf("%s current input tokens = %d, want %d (compacted: %s)", profile, *current.InputTokens, want, compacted)
				}
			}
		})
	}
}
