package native

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	htmltomarkdown "github.com/xberg-io/html-to-markdown/packages/go/v3"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type staticFetchResolver struct {
	addresses []net.IPAddr
	err       error
	hosts     []string
}

func (r *staticFetchResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.hosts = append(r.hosts, host)
	return r.addresses, r.err
}

type queuedFetchRequester struct {
	responses []*http.Response
	targets   []validatedFetchURL
}

func (r *queuedFetchRequester) Do(_ context.Context, target validatedFetchURL) (*http.Response, error) {
	r.targets = append(r.targets, target)
	if len(r.responses) == 0 {
		return nil, io.EOF
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response, nil
}

func fetchTestSettings(directory string) sessionfs.NativeFetchSettings {
	return sessionfs.NativeFetchSettings{
		StorageDir: directory, DefaultLimit: 12, MaxLimit: 256, MaxResponseBytes: 100_000,
		MaxDocumentChars: 10_000, MaxStorageBytes: 1_000_000, TimeoutMS: 1_000, MaxRedirects: 3,
		AllowedHosts: []string{}, DeniedHosts: []string{},
	}
}

func fetchTestResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func executeFetchTestTool(t *testing.T, tool tools.Tool, arguments map[string]any, callID string) string {
	t.Helper()
	result, err := tool.Execute(context.Background(), normalizeFetchArguments(arguments), tools.ExecutionContext{CallID: callID})
	if err != nil {
		t.Fatalf("execute fetch tool: %v", err)
	}
	return result
}

func fetchOutputContent(t *testing.T, output string) string {
	t.Helper()
	start := strings.Index(output, "\n--- CONTENT ")
	if start < 0 {
		t.Fatalf("fetch output has no content block: %s", output)
	}
	start = strings.Index(output[start+1:], "\n") + start + 2
	end := strings.Index(output[start:], "\n--- END CONTENT token=")
	if end < 0 {
		t.Fatalf("fetch output has no content terminator: %s", output)
	}
	return output[start : start+end]
}

func fetchOutputCursor(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "next_cursor: ") {
			return strings.TrimPrefix(line, "next_cursor: ")
		}
	}
	t.Fatalf("fetch output has no next cursor: %s", output)
	return ""
}

func TestFetchToolConvertsHTMLAndPagesPersistedMarkdown(t *testing.T) {
	directory := t.TempDir()
	settings := fetchTestSettings(directory)
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	requester := &queuedFetchRequester{responses: []*http.Response{
		fetchTestResponse(http.StatusOK, "text/html; charset=utf-8", "<html><head><title>Example page</title></head><body><h1>Heading</h1><p>This is a longer body of text for pagination.</p></body></html>"),
	}}
	tool, err := newFetchTool(settings, NewFetchStore(directory), resolver, requester)
	if err != nil {
		t.Fatalf("construct fetch tool: %v", err)
	}
	first := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/article", "limit": json.Number("12")}, "call-1")
	if !strings.Contains(first, "title: Example page") || !strings.Contains(first, "content_type: text/html") || !strings.Contains(first, "summary\":\"Document fetched; more content is available\"") {
		t.Fatalf("first fetch page metadata = %s", first)
	}
	if !strings.Contains(fetchOutputContent(t, first), "# Heading") {
		t.Fatalf("HTML was not converted to Markdown: %s", fetchOutputContent(t, first))
	}
	cursor := fetchOutputCursor(t, first)
	secondStore := NewFetchStore(directory)
	secondPage, err := secondStore.Page(cursor, 256)
	if err != nil {
		t.Fatalf("read persisted cursor with a new store: %v", err)
	}
	allContent := fetchOutputContent(t, first) + secondPage.Content
	if !strings.Contains(allContent, "This is a longer body of text for pagination.") || secondPage.NextCursor != "" {
		t.Fatalf("paginated Markdown = %q, second page = %+v", allContent, secondPage)
	}
	if len(requester.targets) != 1 {
		t.Fatalf("cursor page repeated network requests: %d", len(requester.targets))
	}
}

func TestFetchToolPassesThroughTextAndRejectsBinaryContent(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	requester := &queuedFetchRequester{responses: []*http.Response{
		fetchTestResponse(http.StatusOK, "text/plain; charset=utf-8", "<h1>Literal text</h1>"),
		fetchTestResponse(http.StatusOK, "application/octet-stream", "binary"),
	}}
	tool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, requester)
	if err != nil {
		t.Fatalf("construct fetch tool: %v", err)
	}
	plain := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/plain", "limit": json.Number("256")}, "plain")
	if got := fetchOutputContent(t, plain); got != "<h1>Literal text</h1>" {
		t.Fatalf("plain text content = %q", got)
	}
	binary := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/binary"}, "binary")
	if !strings.Contains(binary, "unsupported_content_type") {
		t.Fatalf("binary MIME type was accepted: %s", binary)
	}
}

func TestFetchToolRedirectsAndRejectsPrivateDNS(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxRedirects = 1
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	requester := &queuedFetchRequester{responses: []*http.Response{
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/next"}}, Body: io.NopCloser(strings.NewReader(""))},
		fetchTestResponse(http.StatusOK, "text/plain", "next page"),
	}}
	tool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, requester)
	if err != nil {
		t.Fatalf("construct fetch tool: %v", err)
	}
	result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/start"}, "redirect")
	if !strings.Contains(result, "final_url: https://example.com/next") || fetchOutputContent(t, result) != "next page" {
		t.Fatalf("redirect result = %s", result)
	}

	private := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}}
	untouched := &queuedFetchRequester{}
	blockedTool, err := newFetchTool(settings, NewFetchStore(t.TempDir()), private, untouched)
	if err != nil {
		t.Fatalf("construct private-address fetch tool: %v", err)
	}
	blocked := executeFetchTestTool(t, blockedTool, map[string]any{"url": "https://private.example/"}, "blocked")
	if !strings.Contains(blocked, "unsafe_url") || len(untouched.targets) != 0 {
		t.Fatalf("private DNS result was not blocked before request: %s", blocked)
	}
}

func TestFetchURLPolicyAndPublicAddressRanges(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.AllowedHosts = []string{"allowed.example"}
	settings.DeniedHosts = []string{"blocked.example"}
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	for _, test := range []struct {
		url  string
		code string
	}{
		{"file:///etc/passwd", "invalid_url"},
		{"https://user:secret@allowed.example/", "invalid_url"},
		{"https://blocked.example/", "host_denied"},
		{"https://other.example/", "host_denied"},
	} {
		_, err := validateFetchURL(context.Background(), test.url, settings, resolver)
		var requestError *fetchRequestError
		if !errors.As(err, &requestError) || requestError.Code != test.code {
			t.Errorf("validateFetchURL(%q) error = %v, want code %s", test.url, err, test.code)
		}
	}

	for _, test := range []struct {
		address string
		public  bool
	}{
		{"93.184.216.34", true},
		{"10.0.0.1", false},
		{"100.64.0.1", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"172.16.0.1", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"::1", false},
		{"fd00::1", false},
		{"fe80::1", false},
		{"2001:db8::1", false},
		{"2606:4700:4700::1111", true},
		{"fec0::1", false},
		{"64:ff9b::7f00:1", false},
		{"64:ff9b::a00:1", false},
		{"64:ff9b::a9fe:a9fe", false},
		{"64:ff9b::808:808", true},
		{"64:ff9b:1::808:808", false},
		{"2002:7f00:1::", false},
		{"2002:a00:1::", false},
		{"2002:808:808::", true},
	} {
		address := net.ParseIP(test.address)
		if got := isPublicFetchAddress(net.IPAddr{IP: address}); got != test.public {
			t.Errorf("isPublicFetchAddress(%s) = %v, want %v", test.address, got, test.public)
		}
	}
}

func TestPinnedFetchRequesterConnectsToValidatedIPAndDecodesGzip(t *testing.T) {
	var host string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host = request.Host
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Set("Content-Encoding", "gzip")
		compressor := gzip.NewWriter(writer)
		_, _ = io.WriteString(compressor, "pinned response")
		_ = compressor.Close()
	}))
	defer server.Close()
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	targetURL, err := url.Parse("http://unresolvable.invalid:" + port + "/resource")
	if err != nil {
		t.Fatal(err)
	}
	response, err := (pinnedFetchRequester{}).Do(context.Background(), validatedFetchURL{
		URL: targetURL, Address: net.IPAddr{IP: net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("pinned request: %v", err)
	}
	body, err := readFetchResponse(response, 1_000)
	if err != nil {
		t.Fatalf("read pinned response: %v", err)
	}
	if host != "unresolvable.invalid:"+port {
		t.Fatalf("request Host header = %q", host)
	}
	if string(body) != "pinned response" {
		t.Fatalf("decoded response = %q", body)
	}
}

func TestFetchStorePagesUnicodeByUTF16CursorUnits(t *testing.T) {
	store := NewFetchStore(t.TempDir())
	settings := fetchTestSettings(store.directory)
	page, err := store.CreatePage(FetchDocument{URL: "https://example.com", FinalURL: "https://example.com/", ContentType: "text/plain", Status: http.StatusOK, Markdown: "A🙂B"}, 1, settings)
	if err != nil {
		t.Fatalf("create unicode page: %v", err)
	}
	if page.Content != "A" || page.NextCursor == "" {
		t.Fatalf("first Unicode page = %+v", page)
	}
	second, err := store.Page(page.NextCursor, 1)
	if err != nil {
		t.Fatalf("read second Unicode page: %v", err)
	}
	if second.Content != "🙂" || !strings.HasSuffix(second.NextCursor, ".3") {
		t.Fatalf("second UTF-16 page = %+v", second)
	}
	third, err := store.Page(second.NextCursor, 1)
	if err != nil {
		t.Fatalf("read third Unicode page: %v", err)
	}
	if third.Content != "B" || third.NextCursor != "" {
		t.Fatalf("third UTF-16 page = %+v", third)
	}
}

func TestNativeFetchNodeOutputGolden(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.DefaultLimit = 12_000
	settings.MaxLimit = 50_000
	settings.MaxDocumentChars = 100_000
	settings.MaxStorageBytes = 1_000_000
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	html := "<html><head><title>Example page</title></head><body><h1>Heading</h1><p>This is a longer body of text for pagination.</p></body></html>"
	requester := &queuedFetchRequester{responses: []*http.Response{
		fetchTestResponse(http.StatusOK, "text/html; charset=utf-8", html),
	}}
	tool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, requester)
	if err != nil {
		t.Fatalf("construct fetch tool: %v", err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatalf("construct fetch catalog: %v", err)
	}
	call := json.RawMessage(`{"type":"function_call","call_id":"call-golden","name":"fetch","arguments":"{\"url\":\"https://93.184.216.34/article\"}"}`)
	got, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatalf("execute golden fetch call: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "native-fetch-output.golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	normalized := regexp.MustCompile(`token=r_[A-Za-z0-9_-]+`).ReplaceAllString(string(got), "token=TOKEN")
	if strings.TrimSpace(normalized) != strings.TrimSpace(string(golden)) {
		t.Fatalf("Go fetch output differs from Node golden\nGo:   %s\nNode: %s", normalized, golden)
	}
}

type waitingFetchRequester struct{}

func (waitingFetchRequester) Do(ctx context.Context, _ validatedFetchURL) (*http.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestFetchToolHonorsTimeoutAndResponseLimits(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.TimeoutMS = 5
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	timeoutTool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, waitingFetchRequester{})
	if err != nil {
		t.Fatalf("construct timeout fetch tool: %v", err)
	}
	timeoutResult := executeFetchTestTool(t, timeoutTool, map[string]any{"url": "https://example.com/slow"}, "timeout")
	if !strings.Contains(timeoutResult, "timeout") {
		t.Fatalf("slow request did not time out: %s", timeoutResult)
	}

	settings = fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 5
	tooLarge := fetchTestResponse(http.StatusOK, "text/plain", "0123456789")
	tooLarge.Header.Set("Content-Length", "10")
	requester := &queuedFetchRequester{responses: []*http.Response{tooLarge}}
	largeTool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, requester)
	if err != nil {
		t.Fatalf("construct size-limited fetch tool: %v", err)
	}
	largeResult := executeFetchTestTool(t, largeTool, map[string]any{"url": "https://example.com/large"}, "large")
	if !strings.Contains(largeResult, "response_too_large") {
		t.Fatalf("oversized response was accepted: %s", largeResult)
	}

	unsupported := fetchTestResponse(http.StatusOK, "text/plain", "body")
	unsupported.Header.Set("Content-Encoding", "zstd")
	requester = &queuedFetchRequester{responses: []*http.Response{unsupported}}
	encodingTool, err := newFetchTool(settings, NewFetchStore(t.TempDir()), resolver, requester)
	if err != nil {
		t.Fatalf("construct encoding-limited fetch tool: %v", err)
	}
	encodingResult := executeFetchTestTool(t, encodingTool, map[string]any{"url": "https://example.com/encoded"}, "encoding")
	if !strings.Contains(encodingResult, "unsupported_content_encoding") {
		t.Fatalf("unsupported content encoding was accepted: %s", encodingResult)
	}
}

func TestFetchToolFallsBackFromCursorAndEnforcesRedirectLimit(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	requester := &queuedFetchRequester{responses: []*http.Response{
		fetchTestResponse(http.StatusOK, "text/plain", "fallback content"),
	}}
	tool, err := newFetchTool(settings, NewFetchStore(settings.StorageDir), resolver, requester)
	if err != nil {
		t.Fatalf("construct cursor fallback tool: %v", err)
	}
	result := executeFetchTestTool(t, tool, map[string]any{"cursor": "not-a-cursor", "url": "https://example.com/fallback", "limit": json.Number("256")}, "fallback")
	if fetchOutputContent(t, result) != "fallback content" || len(requester.targets) != 1 {
		t.Fatalf("cursor fallback result = %s, requests = %d", result, len(requester.targets))
	}

	settings.MaxRedirects = 1
	requester = &queuedFetchRequester{responses: []*http.Response{
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/one"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/two"}}, Body: io.NopCloser(strings.NewReader(""))},
	}}
	redirectTool, err := newFetchTool(settings, NewFetchStore(t.TempDir()), resolver, requester)
	if err != nil {
		t.Fatalf("construct redirect-limited tool: %v", err)
	}
	redirectResult := executeFetchTestTool(t, redirectTool, map[string]any{"url": "https://example.com/start"}, "redirect-limit")
	if !strings.Contains(redirectResult, "too_many_redirects") || len(requester.targets) != 2 {
		t.Fatalf("redirect limit result = %s, requests = %d", redirectResult, len(requester.targets))
	}
}

func BenchmarkNativeFetchHTMLConversion(b *testing.B) {
	html := "<html><head><title>Benchmark page</title></head><body><h1>Heading</h1><p>Repeated body text for native.fetch conversion benchmarking.</p></body></html>"
	options := &htmltomarkdown.ConversionOptions{
		ExtractMetadata:          htmltomarkdown.Ptr(false),
		SkipImages:               true,
		ExtractImages:            false,
		IncludeDocumentStructure: false,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, err := htmltomarkdown.Convert(html, options)
		if err != nil {
			b.Fatal(err)
		}
		if result == nil || result.Content == nil {
			b.Fatal("converter returned no content")
		}
	}
}
