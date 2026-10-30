package native

import (
	"bytes"
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
	"strconv"
	"strings"
	"testing"
	"time"

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
		{"https://blocked.example./", "host_denied"},
		{"https://BLOCKED.Example../x", "host_denied"},
		{"https://other.example./", "host_denied"},
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
		{"198.51.100.1", false},
		{"198.51.100.255", false},
		{"198.51.101.1", true},
		{"203.0.113.1", false},
		{"203.0.114.1", true},
		{"192.88.99.1", false},
		{"192.88.100.1", true},
		{"::1", false},
		{"fd00::1", false},
		{"fe80::1", false},
		{"2001:db8::1", false},
		{"2001:2::1", false},
		{"2001:2:0:1::1", false},
		{"2001:3::1", true},
		{"2001:10::1", false},
		{"2001:1f::1", false},
		{"2001:20::1", true},
		{"2001::1", true},
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
	body, _, err := readFetchResponseTruncating(response, 1_000, false)
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

func newFetchTestTool(t *testing.T, settings sessionfs.NativeFetchSettings, store *FetchStore, responses ...*http.Response) (tools.Tool, *queuedFetchRequester) {
	t.Helper()
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	requester := &queuedFetchRequester{responses: responses}
	tool, err := newFetchTool(settings, store, resolver, requester)
	if err != nil {
		t.Fatalf("construct fetch tool: %v", err)
	}
	return tool, requester
}

func fetchStoredFileCount(t *testing.T, directory string) int {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func gzipFetchResponse(t *testing.T, text string) *http.Response {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(writer, text); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := fetchTestResponse(http.StatusOK, "text/plain", "")
	response.Header.Set("Content-Encoding", "gzip")
	response.Body = io.NopCloser(bytes.NewReader(compressed.Bytes()))
	return response
}

func TestFetchToolRejectsOversizedBodyAfterReadingWithoutStoring(t *testing.T) {
	lying := fetchTestResponse(http.StatusOK, "text/plain", "0123456789")
	lying.Header.Set("Content-Length", "2")
	for name, response := range map[string]*http.Response{
		"no content length":    fetchTestResponse(http.StatusOK, "text/plain", "0123456789"),
		"lying content length": lying,
		"gzip decoded":         gzipFetchResponse(t, strings.Repeat("a", 10_000)),
	} {
		t.Run(name, func(t *testing.T) {
			settings := fetchTestSettings(t.TempDir())
			settings.MaxResponseBytes = 5
			if name == "gzip decoded" {
				settings.MaxResponseBytes = 100
				response.Header.Set("Content-Length", "50")
			}
			tool, _ := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), response)
			result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/large"}, "large")
			for _, want := range []string{"response_too_large", "limit of " + strconv.FormatInt(settings.MaxResponseBytes, 10) + " bytes", "narrower URL", "tools.native.fetch.max_response_bytes"} {
				if !strings.Contains(result, want) {
					t.Fatalf("result lacks %q: %s", want, result)
				}
			}
			if count := fetchStoredFileCount(t, settings.StorageDir); count != 0 {
				t.Fatalf("oversized response left %d stored files", count)
			}
		})
	}
}

func TestFetchToolPrecheckMessageNamesLimitAndDeclaredLength(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 5
	response := fetchTestResponse(http.StatusOK, "text/plain", "0123456789")
	response.Header.Set("Content-Length", "10")
	tool, _ := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), response)
	result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/large"}, "precheck")
	for _, want := range []string{"response_too_large", "limit of 5 bytes", "Content-Length is 10", "narrower URL", "tools.native.fetch.max_response_bytes", "tools.native.fetch.truncate_oversized_response"} {
		if !strings.Contains(result, want) {
			t.Fatalf("precheck result lacks %q: %s", want, result)
		}
	}
}

func TestFetchToolAcceptsBodyEqualToLimit(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		settings := fetchTestSettings(t.TempDir())
		settings.MaxResponseBytes = 5
		settings.TruncateOversizedResponse = truncate
		response := fetchTestResponse(http.StatusOK, "text/plain", "01234")
		response.Header.Set("Content-Length", "5")
		tool, _ := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), response)
		result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/exact"}, "exact")
		if fetchOutputContent(t, result) != "01234" || strings.Contains(result, "response_too_large") || strings.Contains(result, "truncated: true") {
			t.Fatalf("truncate=%v body equal to limit result = %s", truncate, result)
		}
	}
}

func TestFetchToolTruncatesOversizedResponseWhenEnabled(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 5
	settings.TruncateOversizedResponse = true
	declared := fetchTestResponse(http.StatusOK, "text/plain", "0123456789")
	declared.Header.Set("Content-Length", "10")
	tool, _ := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), declared, fetchTestResponse(http.StatusOK, "text/plain", "0123456789"))
	for _, callID := range []string{"declared", "undeclared"} {
		result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/" + callID}, callID)
		if fetchOutputContent(t, result) != "01234" || !strings.Contains(result, "truncated: true") || strings.Contains(result, "next_cursor: ") {
			t.Fatalf("%s truncated result = %s", callID, result)
		}
	}
}

func TestFetchToolReportsTruncationOnEveryPage(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 8
	settings.TruncateOversizedResponse = true
	tool, requester := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), fetchTestResponse(http.StatusOK, "text/plain", "01234567890"))
	limit := json.Number("3")
	first := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/pages", "limit": limit}, "p1")
	second := executeFetchTestTool(t, tool, map[string]any{"cursor": fetchOutputCursor(t, first), "limit": limit}, "p2")
	third := executeFetchTestTool(t, tool, map[string]any{"cursor": fetchOutputCursor(t, second), "limit": limit}, "p3")
	for index, page := range []struct{ output, content string }{{first, "012"}, {second, "345"}, {third, "67"}} {
		if fetchOutputContent(t, page.output) != page.content || !strings.Contains(page.output, "truncated: true") {
			t.Fatalf("page %d = %s", index+1, page.output)
		}
	}
	if strings.Contains(third, "next_cursor: ") || len(requester.targets) != 1 {
		t.Fatalf("last page = %s, requests = %d", third, len(requester.targets))
	}
}

func TestFetchToolTruncationDropsPartialRuneAndCutHTMLTag(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 3
	settings.TruncateOversizedResponse = true
	tool, _ := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), fetchTestResponse(http.StatusOK, "text/plain", "abé"))
	result := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/rune"}, "rune")
	if got := fetchOutputContent(t, result); got != "ab" || strings.Contains(result, "\uFFFD") || !strings.Contains(result, "truncated: true") {
		t.Fatalf("multibyte cut result = %q in %s", got, result)
	}

	html := `<html><head><title>Cut page</title></head><body><h1>Heading</h1><p>Hello <a href="https://example.com/long/path">link</a></p></body></html>`
	settings = fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = int64(strings.Index(html, `href="`) + len(`href="https://exa`))
	settings.TruncateOversizedResponse = true
	tool, _ = newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), fetchTestResponse(http.StatusOK, "text/html", html))
	result = executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/html", "limit": json.Number("256")}, "html")
	content := fetchOutputContent(t, result)
	if !strings.Contains(result, "truncated: true") || !strings.Contains(content, "# Heading") || !strings.Contains(content, "Hello") || strings.Contains(content, "href") || strings.Contains(content, "\uFFFD") {
		t.Fatalf("cut-mid-tag HTML result = %s", result)
	}
}

func TestFetchToolCacheTTLControlsRepeatRequests(t *testing.T) {
	for _, test := range []struct {
		ttlMS    int64
		requests int
	}{{0, 2}, {60_000, 1}} {
		settings := fetchTestSettings(t.TempDir())
		settings.CacheTTLMS = test.ttlMS
		tool, requester := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir),
			fetchTestResponse(http.StatusOK, "text/plain", "cached body"), fetchTestResponse(http.StatusOK, "text/plain", "cached body"))
		first := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/cache"}, "one")
		second := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/cache"}, "two")
		if len(requester.targets) != test.requests || fetchOutputContent(t, second) != fetchOutputContent(t, first) {
			t.Fatalf("cache_ttl_ms=%d made %d requests, want %d; second = %s", test.ttlMS, len(requester.targets), test.requests, second)
		}
	}
}

func TestFetchToolCacheHitsOnRedirectFinalURLAndReportsRequestURL(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.CacheTTLMS = 60_000
	tool, requester := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir),
		&http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/next"}}, Body: io.NopCloser(strings.NewReader(""))},
		fetchTestResponse(http.StatusOK, "text/plain", "next page"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/start"}, "start")
	hit := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/next"}, "final")
	if len(requester.targets) != 2 || fetchOutputContent(t, hit) != "next page" || !strings.Contains(hit, "\nurl: https://example.com/next") {
		t.Fatalf("final_url cache hit = %s, requests = %d", hit, len(requester.targets))
	}
	again := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/start"}, "start-again")
	if len(requester.targets) != 2 || !strings.Contains(again, "url: https://example.com/start") || !strings.Contains(again, "final_url: https://example.com/next") {
		t.Fatalf("requested-URL cache hit = %s, requests = %d", again, len(requester.targets))
	}
}

func TestFetchToolCacheExpiresByStoreClock(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.CacheTTLMS = 1_000
	store := NewFetchStore(settings.StorageDir)
	current := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return current }
	tool, requester := newFetchTestTool(t, settings, store,
		fetchTestResponse(http.StatusOK, "text/plain", "old"), fetchTestResponse(http.StatusOK, "text/plain", "new"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/expire"}, "one")
	current = current.Add(999 * time.Millisecond)
	if hit := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/expire"}, "within"); fetchOutputContent(t, hit) != "old" || len(requester.targets) != 1 {
		t.Fatalf("hit inside ttl = %s, requests = %d", hit, len(requester.targets))
	}
	current = current.Add(time.Second)
	expired := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/expire"}, "after")
	if fetchOutputContent(t, expired) != "new" || len(requester.targets) != 2 {
		t.Fatalf("expired entry result = %s, requests = %d", expired, len(requester.targets))
	}
}

func TestFetchToolCacheHitRechecksHostPolicy(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.CacheTTLMS = 60_000
	store := NewFetchStore(settings.StorageDir)
	tool, _ := newFetchTestTool(t, settings, store, fetchTestResponse(http.StatusOK, "text/plain", "public"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/policy"}, "one")

	denied := settings
	denied.DeniedHosts = []string{"example.com"}
	deniedTool, requester := newFetchTestTool(t, denied, store)
	result := executeFetchTestTool(t, deniedTool, map[string]any{"url": "https://example.com/policy"}, "denied")
	if !strings.Contains(result, "host_denied") || strings.Contains(result, "public") || len(requester.targets) != 0 {
		t.Fatalf("cached URL on a denied host = %s", result)
	}
}

func TestFetchToolCacheHitKeepsTruncatedFlag(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.MaxResponseBytes = 5
	settings.TruncateOversizedResponse = true
	settings.CacheTTLMS = 60_000
	tool, requester := newFetchTestTool(t, settings, NewFetchStore(settings.StorageDir), fetchTestResponse(http.StatusOK, "text/plain", "0123456789"))
	first := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/cut"}, "one")
	hit := executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/cut"}, "two")
	if len(requester.targets) != 1 || fetchOutputContent(t, hit) != "01234" || !strings.Contains(first, "truncated: true") || !strings.Contains(hit, "truncated: true") {
		t.Fatalf("cache hit = %s, requests = %d", hit, len(requester.targets))
	}
}

func TestFetchToolCacheHitRechecksDeniedFinalURLHost(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.CacheTTLMS = 60_000
	store := NewFetchStore(settings.StorageDir)
	tool, _ := newFetchTestTool(t, settings, store,
		&http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://b.example/next"}}, Body: io.NopCloser(strings.NewReader(""))},
		fetchTestResponse(http.StatusOK, "text/plain", "on b"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://a.example/start"}, "one")

	denied := settings
	denied.DeniedHosts = []string{"b.example"}
	deniedTool, requester := newFetchTestTool(t, denied, store)
	result := executeFetchTestTool(t, deniedTool, map[string]any{"url": "https://a.example/start"}, "denied")
	if strings.Contains(result, "on b") || len(requester.targets) != 1 {
		t.Fatalf("want refused hit and one fall-through download, got result = %s, requests = %d", result, len(requester.targets))
	}
}

func TestFetchToolCacheHitWorksWhenStorageIsFull(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	settings.CacheTTLMS = 60_000
	store := NewFetchStore(settings.StorageDir)
	tool, _ := newFetchTestTool(t, settings, store, fetchTestResponse(http.StatusOK, "text/plain", "stored"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/full"}, "one")

	full := settings
	full.MaxStorageBytes = 1
	fullTool, requester := newFetchTestTool(t, full, store)
	hit := executeFetchTestTool(t, fullTool, map[string]any{"url": "https://example.com/full"}, "full")
	if fetchOutputContent(t, hit) != "stored" || len(requester.targets) != 0 {
		t.Fatalf("cache hit with full storage = %s", hit)
	}
}

func TestFetchToolFullStorageErrorNamesTheLimitAndTheRecovery(t *testing.T) {
	settings := fetchTestSettings(t.TempDir())
	store := NewFetchStore(settings.StorageDir)
	tool, _ := newFetchTestTool(t, settings, store, fetchTestResponse(http.StatusOK, "text/plain", "stored"))
	executeFetchTestTool(t, tool, map[string]any{"url": "https://example.com/first"}, "one")

	used, err := fetchDirectoryBytes(settings.StorageDir)
	if err != nil {
		t.Fatal(err)
	}
	full := settings
	full.MaxStorageBytes = used
	fullTool, _ := newFetchTestTool(t, full, store, fetchTestResponse(http.StatusOK, "text/plain", "new"))
	output := executeFetchTestTool(t, fullTool, map[string]any{"url": "https://example.com/second"}, "full")
	for _, want := range []string{"fetch_storage_error", "fetch storage is full", "storage_dir", "max_storage_bytes"} {
		if !strings.Contains(output, want) {
			t.Fatalf("full storage result lacks %q: %s", want, output)
		}
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

func TestFetchURLPolicyTreatsRootedHostAsTheSameHost(t *testing.T) {
	resolver := &staticFetchResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	allowed := fetchTestSettings(t.TempDir())
	allowed.AllowedHosts = []string{"allowed.example"}
	for _, value := range []string{"https://allowed.example/", "https://ALLOWED.example./x"} {
		if _, err := validateFetchURL(context.Background(), value, allowed, resolver); err != nil {
			t.Errorf("validateFetchURL(%q) = %v, want allowed", value, err)
		}
	}
	open := fetchTestSettings(t.TempDir())
	if _, err := validateFetchURL(context.Background(), "https://example.com./", open, resolver); err != nil {
		t.Errorf("rooted host without policy = %v, want allowed", err)
	}
}
