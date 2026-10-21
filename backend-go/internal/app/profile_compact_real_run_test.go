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

func readProfileUsageCache(t *testing.T, path string) (usageCacheFile, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return usageCacheFile{}, false
	}
	var cache usageCacheFile
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	return cache, true
}

// Profiles whose transcripts come from real runs keep their totals when another
// profile (or itself) is compacted through the UI's compact route.
func TestHTTPCompactAfterRealRunsKeepsTotals(t *testing.T) {
	for _, compacted := range []string{"review/reviewer", "dev/coder"} {
		for _, readBefore := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readBefore=%v", compacted, readBefore), func(t *testing.T) {
				f := newLayoutFixture(t)
				f.usage = `"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6},`
				const session = "/api/sessions/local:demo"
				state := filepath.Join(f.cwd, ".jad", "state", "demo")
				cachePath := map[string]string{
					"dev/coder":       filepath.Join(state, "dev", "coder", "staging", "usage-cache.json"),
					"review/reviewer": filepath.Join(state, "review", "reviewer", "staging", "usage-cache.json"),
				}
				for _, profile := range []string{"dev/coder", "review/reviewer"} {
					for turn := 1; turn <= 3; turn++ {
						body := fmt.Sprintf(`{"input":"turn %d"}`, turn)
						if status, out := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape(profile), body); status != 200 {
							t.Fatalf("run %s: %d %s", profile, status, out)
						}
					}
				}
				if readBefore {
					for profile := range cachePath {
						f.do(t, http.MethodGet, session+"?profile="+url.QueryEscape(profile), "")
					}
				}
				before := map[string]usageCacheFile{}
				for profile, path := range cachePath {
					cache, ok := readProfileUsageCache(t, path)
					t.Logf("before %s: cache=%v totals=%+v", profile, ok, cache.Summary.TokenUsage)
					before[profile] = cache
				}
				status, out := f.do(t, http.MethodPost, session+"/compact?profile="+url.QueryEscape(compacted), "")
				t.Logf("compact %s: %d %s", compacted, status, out)
				if status != 200 {
					t.Fatal("compact failed")
				}
				for profile, path := range cachePath {
					status, body := f.do(t, http.MethodGet, session+"?profile="+url.QueryEscape(profile), "")
					if status != 200 {
						t.Fatalf("read %s: %d", profile, status)
					}
					cache, ok := readProfileUsageCache(t, path)
					t.Logf("after %s: cache=%v completed=%d input=%v api-usage=%s", profile, ok, cache.Summary.CompletedResponses, cache.Summary.InputTokens, extractUsage(body))
					if cache.Summary.CompletedResponses != 3 || cache.Summary.InputTokens == nil || *cache.Summary.InputTokens != 15 {
						t.Errorf("%s: totals lost: ok=%v summary=%+v", profile, ok, cache.Summary)
					}
					if !strings.Contains(extractUsage(body), `"completed_responses":3`) {
						t.Errorf("%s: API usage lost totals: %s", profile, extractUsage(body))
					}
				}
			})
		}
	}
}

func extractUsage(body string) string {
	index := strings.Index(body, `"usage"`)
	if index < 0 {
		return "<none>"
	}
	end := index + 400
	if end > len(body) {
		end = len(body)
	}
	return body[index:end]
}
