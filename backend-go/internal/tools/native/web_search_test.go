package native

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleWebSearchManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "web_search.json")

type recordedSearch struct {
	method string
	query  string
	header http.Header
	body   string
}

func searchServer(t *testing.T, status int, answer string) (*httptest.Server, func() []recordedSearch) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedSearch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, recordedSearch{method: r.Method, query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body)})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedSearch {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedSearch(nil), seen...)
	}
}

func webSearchSettings(provider, baseURL string) sessionfs.NativeWebSearchSettings {
	return sessionfs.NativeWebSearchSettings{
		Provider: provider, BaseURL: baseURL, APIKeyEnv: "SEARCH_KEY", DefaultResults: 2, MaxResults: 3,
		TimeoutMS: 5000, MaxResponseBytes: 1 << 20, MaxSnippetChars: 40, MaxQueryChars: 100, UserAgent: "test-agent",
		AllowedDomains: []string{}, BlockedDomains: []string{},
	}
}

func callWebSearch(t *testing.T, settings sessionfs.NativeWebSearchSettings, environment map[string]string, arguments map[string]any) string {
	t.Helper()
	tool, err := newWebSearchTool(settings, func(name string) string { return environment[name] }, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

const braveAnswer = `{"web":{"results":[
 {"title":"Go <strong>context</strong>","url":"https://go.dev/blog/context","description":"Package context defines the Context type, which carries deadlines &amp; cancellation signals across API boundaries."},
 {"title":"Spam","url":"https://spam.example/x","description":"buy"},
 {"title":"Repeat","url":"https://go.dev/blog/context","description":"same url"},
 {"title":"Docs","url":"https://pkg.go.dev/context","description":"API docs"},
 {"title":"Third","url":"https://www.third.example/a","description":"three"}
]}}`

func TestWebSearchBraveSendsTheKeyAndShowsFilteredResults(t *testing.T) {
	server, requests := searchServer(t, http.StatusOK, braveAnswer)
	settings := webSearchSettings(sessionfs.WebSearchProviderBrave, server.URL+"/res/v1/web/search")
	settings.BlockedDomains = []string{"spam.example"}
	output := callWebSearch(t, settings, map[string]string{"SEARCH_KEY": "brave-key"}, map[string]any{"query": "  go   context ", "max_results": json.Number("9")})
	sent := requests()
	if len(sent) != 1 || sent[0].method != http.MethodGet || sent[0].header.Get("X-Subscription-Token") != "brave-key" || sent[0].header.Get("User-Agent") != "test-agent" {
		t.Fatalf("requests = %+v", sent)
	}
	if !strings.Contains(sent[0].query, "q=go+context") || !strings.Contains(sent[0].query, "count=3") {
		t.Fatalf("query string = %s, want the collapsed query and max_results capped to 3", sent[0].query)
	}
	for _, want := range []string{`"summary":"3 results for \"go context\""`, "provider: brave", "1. Go context\n   https://go.dev/blog/context\n   Package context defines the Context type…", "2. Docs\n   https://pkg.go.dev/context", "3. Third"} {
		if !strings.Contains(output, want) {
			t.Errorf("output misses %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "spam.example") || strings.Count(output, "https://go.dev/blog/context") != 1 {
		t.Errorf("blocked or repeated results shown:\n%s", output)
	}
}

func TestWebSearchTavilyPostsTheDomainsAndReadsContent(t *testing.T) {
	server, requests := searchServer(t, http.StatusOK, `{"results":[{"title":"A","url":"https://docs.example.com/a","content":"alpha"},{"title":"B","url":"https://other.example/b","content":"beta"}]}`)
	settings := webSearchSettings(sessionfs.WebSearchProviderTavily, server.URL)
	settings.AllowedDomains = []string{"example.com"}
	output := callWebSearch(t, settings, map[string]string{"SEARCH_KEY": "tv-key"}, map[string]any{"query": "alpha", "allowed_domains": []any{"docs.example.com"}, "blocked_domains": []any{"WWW.Bad.Example"}})
	sent := requests()
	if len(sent) != 1 || sent[0].method != http.MethodPost || sent[0].header.Get("Authorization") != "Bearer tv-key" {
		t.Fatalf("requests = %+v", sent)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(sent[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if body["query"] != "alpha" || body["max_results"] != float64(2) || !reflect.DeepEqual(body["include_domains"], []any{"docs.example.com"}) || !reflect.DeepEqual(body["exclude_domains"], []any{"bad.example"}) {
		t.Fatalf("tavily body = %v", body)
	}
	if !strings.Contains(output, "https://docs.example.com/a\n   alpha") || strings.Contains(output, "other.example") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestWebSearchSearXNGNeedsNoKeyAndAddsSiteFilters(t *testing.T) {
	server, requests := searchServer(t, http.StatusOK, `{"results":[{"title":"Hit","url":"https://go.dev/x","content":"c"}]}`)
	settings := webSearchSettings(sessionfs.WebSearchProviderSearXNG, server.URL+"/search")
	settings.APIKeyEnv = ""
	output := callWebSearch(t, settings, nil, map[string]any{"query": "x", "allowed_domains": []any{"go.dev"}})
	sent := requests()
	if len(sent) != 1 || !strings.Contains(sent[0].query, "format=json") || !strings.Contains(sent[0].query, "site%3Ago.dev") || sent[0].header.Get("Authorization") != "" {
		t.Fatalf("requests = %+v", sent)
	}
	if !strings.Contains(output, "1. Hit") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestWebSearchReportsErrorsAsToolResults(t *testing.T) {
	failing, _ := searchServer(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)
	garbled, _ := searchServer(t, http.StatusOK, `not json`)
	big, _ := searchServer(t, http.StatusOK, `{"results":[{"title":"`+strings.Repeat("x", 2000)+`","url":"https://a.example"}]}`)
	cases := []struct {
		name      string
		settings  sessionfs.NativeWebSearchSettings
		env       map[string]string
		arguments map[string]any
		want      []string
	}{
		{"missing key", webSearchSettings(sessionfs.WebSearchProviderBrave, failing.URL), nil, map[string]any{"query": "q"}, []string{"api_key_missing", "SEARCH_KEY"}},
		{"blank query", webSearchSettings(sessionfs.WebSearchProviderBrave, failing.URL), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "   "}, []string{"invalid_input", "blank"}},
		{"long query", webSearchSettings(sessionfs.WebSearchProviderBrave, failing.URL), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": strings.Repeat("a", 101)}, []string{"invalid_input", "max_query_chars"}},
		{"bad domain", webSearchSettings(sessionfs.WebSearchProviderBrave, failing.URL), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "q", "blocked_domains": []any{"not a domain"}}, []string{"invalid_input", "blocked_domains"}},
		{"http error", webSearchSettings(sessionfs.WebSearchProviderBrave, failing.URL), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "q"}, []string{"search_failed", "HTTP 429", "rate limited"}},
		{"not json", webSearchSettings(sessionfs.WebSearchProviderTavily, garbled.URL), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "q"}, []string{"search_failed", "could not be read"}},
		{"too large", func() sessionfs.NativeWebSearchSettings {
			s := webSearchSettings(sessionfs.WebSearchProviderTavily, big.URL)
			s.MaxResponseBytes = 1024
			return s
		}(), map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "q"}, []string{"response_too_large", "max_response_bytes"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := callWebSearch(t, test.settings, test.env, test.arguments)
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Errorf("output misses %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestWebSearchCallCannotWidenTheAllowedDomains(t *testing.T) {
	settings := webSearchSettings(sessionfs.WebSearchProviderBrave, "https://unused.example")
	settings.AllowedDomains = []string{"go.dev"}
	output := callWebSearch(t, settings, map[string]string{"SEARCH_KEY": "k"}, map[string]any{"query": "q", "allowed_domains": []any{"evil.example"}})
	if !strings.Contains(output, "invalid_input") || !strings.Contains(output, "go.dev") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestWebSearchSampleManifestMatchesTheCode(t *testing.T) {
	root := t.TempDir()
	settings := webSearchSettings(sessionfs.WebSearchProviderBrave, "https://api.example/search")
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "setting.json"), ToolDefinitionPaths: []string{sampleWebSearchManifest},
		NativeWebSearch: &settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "web_search" {
		t.Fatalf("definitions = %+v", definitions)
	}
	tool, err := NewWebSearchTool(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || !tool.LeafBudget || definitions[0].Description != tool.Definition.Description {
		t.Fatalf("tool flags or description differ: read-only %t leaf %t %q", tool.ReadOnly, tool.LeafBudget, definitions[0].Description)
	}
}
