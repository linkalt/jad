package httpdebug

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransportLogsRedactedHeadersAndBoundedBodies(t *testing.T) {
	const requestBody = "request-body"
	const responseBody = "response-body"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read provider request body: %v", err)
		}
		if string(body) != requestBody {
			t.Errorf("provider received request body %q, want %q", body, requestBody)
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-API-Key") != "key-secret" {
			t.Errorf("provider request credentials changed: %#v", request.Header)
		}
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(writer, responseBody)
	}))
	defer server.Close()

	directory := t.TempDir()
	transport := NewTransport(directory, Settings{
		Enabled:                true,
		IncludeRequestHeaders:  true,
		IncludeRequestBody:     true,
		IncludeResponseHeaders: true,
		IncludeResponseBody:    true,
		MaxBodyBytes:           4,
	}, server.Client().Transport).(*Transport)
	client := &http.Client{Transport: transport}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-API-Key", "key-secret")
	request.Header.Set("Cookie", "session-secret")
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read provider response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close provider response: %v", err)
	}
	if response.StatusCode != http.StatusCreated || string(body) != responseBody {
		t.Fatalf("provider response = status %d, body %q", response.StatusCode, body)
	}
	transport.logger.pending.Wait()

	requestRecord := readRecord(t, directory, "request1")
	if requestRecord["method"] != "POST" || requestRecord["url"] != server.URL+"/v1/responses" || requestRecord["body_file"] != "request1.body" || requestRecord["body_bytes"] != float64(4) || requestRecord["body_truncated"] != true {
		t.Fatalf("request record = %#v", requestRecord)
	}
	requestHeaders, ok := requestRecord["headers"].(map[string]any)
	if !ok || requestHeaders["authorization"] != redactedValue || requestHeaders["x-api-key"] != redactedValue || requestHeaders["cookie"] != redactedValue {
		t.Fatalf("request headers were not redacted: %#v", requestRecord["headers"])
	}

	responseRecord := readRecord(t, directory, "response1")
	if responseRecord["status"] != float64(http.StatusCreated) || responseRecord["status_text"] != "Created" || responseRecord["body_file"] != "response1.body" || responseRecord["body_bytes"] != float64(4) || responseRecord["body_truncated"] != true {
		t.Fatalf("response record = %#v", responseRecord)
	}
	responseHeaders, ok := responseRecord["headers"].(map[string]any)
	if !ok || responseHeaders["content-type"] != "text/plain" {
		t.Fatalf("response headers = %#v", responseRecord["headers"])
	}
	if got := readRaw(t, directory, "request1.body"); got != "requ" {
		t.Fatalf("request body file = %q, want the first 4 bytes", got)
	}
	if got := readRaw(t, directory, "response1.body"); got != "resp" {
		t.Fatalf("response body file = %q, want the first 4 bytes", got)
	}
}

// Each SSE frame is saved exactly as the provider sent it: no JSON re-encoding of the
// payload, and the request keeps the bytes that went on the wire, escapes included.
func TestTransportSavesRawStreamBytesAndRequestBytes(t *testing.T) {
	const requestBody = `{"input":"<b>&","stream":true,  "z":{"b":1,  "a":2}}`
	frames := []string{
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"h\\u00e9\"}\n\n",
		": keep-alive\n\n",
		"event: response.completed\ndata:   {\"type\":\"response.completed\"}\n\n",
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = io.WriteString(writer, frame)
			writer.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	transport := NewTransport(directory, Settings{Enabled: true, IncludeRequestBody: true, IncludeResponseBody: true, MaxBodyBytes: 1 << 20}, server.Client().Transport).(*Transport)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	transport.logger.pending.Wait()

	if got := readRaw(t, directory, "response1.body"); got != strings.Join(frames, "") {
		t.Fatalf("response body file = %q, want the provider bytes %q", got, strings.Join(frames, ""))
	}
	if got := readRaw(t, directory, "request1.body"); got != requestBody {
		t.Fatalf("request body file = %q, want %q", got, requestBody)
	}
	record := readRecord(t, directory, "response1")
	if record["body_bytes"] != float64(len(strings.Join(frames, ""))) || record["body_truncated"] != false || record["body_file"] != "response1.body" {
		t.Fatalf("response record = %#v", record)
	}
}

// A stream that is stopped part-way keeps the bytes that arrived, in the file and in the
// record.
func TestTransportKeepsTheBytesOfAnInterruptedStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: first\n\n")
		writer.(http.Flusher).Flush()
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	directory := t.TempDir()
	transport := NewTransport(directory, Settings{Enabled: true, IncludeResponseBody: true, MaxBodyBytes: 1 << 20}, server.Client().Transport).(*Transport)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	n, err := response.Body.Read(buffer)
	if err != nil || string(buffer[:n]) != "data: first\n\n" {
		t.Fatalf("first read = %q, %v", buffer[:n], err)
	}
	_ = response.Body.Close()
	transport.logger.pending.Wait()

	if got := readRaw(t, directory, "response1.body"); got != "data: first\n\n" {
		t.Fatalf("response body file = %q", got)
	}
	if record := readRecord(t, directory, "response1"); record["body_bytes"] != float64(len("data: first\n\n")) || record["body_truncated"] != false {
		t.Fatalf("response record = %#v", record)
	}
}

func TestTransportLoggingFailureDoesNotChangeProviderResult(t *testing.T) {
	root := t.TempDir()
	blockedDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedDirectory, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("ok")),
			Request:       request,
			ContentLength: 2,
		}, nil
	})
	transport := NewTransport(filepath.Join(blockedDirectory, "debug"), Settings{
		Enabled: true, IncludeRequestHeaders: true, MaxBodyBytes: 64,
	}, base).(*Transport)
	request, err := http.NewRequest(http.MethodGet, "https://provider.example.test/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("logging failure changed request result: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("provider result = %q, err=%v", body, err)
	}
	_ = response.Body.Close()
	transport.logger.pending.Wait()
}

func TestNewTransportLeavesDisabledTransportUnchanged(t *testing.T) {
	base := &roundTripperStub{}
	if got := NewTransport(t.TempDir(), Settings{}, base); got != base {
		t.Fatalf("disabled transport = %T, want original %T", got, base)
	}
}

type roundTripperStub struct{}

func (*roundTripperStub) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func readRecord(t *testing.T, directory, name string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var record map[string]any
	if err := json.Unmarshal(contents, &record); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return record
}

func readRaw(t *testing.T, directory, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(contents)
}

func TestTransportReportsDebugDirectoryFailureOnTheInjectedLogger(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	base := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	transport := NewTransport(filepath.Join(blocked, "debug"), Settings{Logger: logger, Enabled: true, MaxBodyBytes: 64}, base).(*Transport)
	request, err := http.NewRequest(http.MethodGet, "https://provider.example.test/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	// Debug files are written on a background goroutine; wait for it before
	// reading the log.
	transport.logger.pending.Wait()
	logged := output.String()
	if !strings.Contains(logged, "unable to initialize HTTP debug directory") || !strings.Contains(logged, "level=WARN") {
		t.Errorf("directory failure not logged on the injected logger:\n%s", logged)
	}
}

// lockedBuffer is a bytes.Buffer that the writer goroutine and the test can
// share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRedactURLMasksCredentialsAndKeepsTheRest(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"https://api.example.com/v1/responses", "https://api.example.com/v1/responses"},
		{"https://user:pw@api.example.com/v1", "https://REDACTED@api.example.com/v1"},
		{"https://h/v1?key=abc&model=gpt&access_token=zzz", "https://h/v1?key=[REDACTED]&model=gpt&access_token=[REDACTED]"},
		{"https://h/v1?api%5Fkey=abc&flag&X-Signature=s", "https://h/v1?api%5Fkey=[REDACTED]&flag&X-Signature=[REDACTED]"},
	} {
		parsed, err := url.Parse(test.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := redactURL(parsed); got != test.want {
			t.Errorf("redactURL(%q) = %q, want %q", test.in, got, test.want)
		}
	}
	if redactURL(nil) != "" {
		t.Error("redactURL(nil) must be empty")
	}
}

func TestTransportRedactsResponseHeadersAndQueryAndUsesPrivateDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Set-Cookie", "sid=secret")
		writer.Header().Set("X-Request-Id", "req-1")
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	directory := filepath.Join(t.TempDir(), "debug", "http")
	transport := NewTransport(directory, Settings{Enabled: true, IncludeRequestHeaders: true, IncludeResponseHeaders: true, MaxBodyBytes: 8}, server.Client().Transport).(*Transport)
	response, err := (&http.Client{Transport: transport}).Get(server.URL + "/v1?key=query-secret&model=m")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	transport.logger.pending.Wait()

	record := readRecord(t, directory, "response1")
	headers := record["headers"].(map[string]any)
	if headers["set-cookie"] != "[REDACTED]" || headers["x-request-id"] != "req-1" {
		t.Errorf("response headers = %#v", headers)
	}
	for _, name := range []string{"request1", "response1"} {
		if raw := readRaw(t, directory, name); strings.Contains(raw, "query-secret") || strings.Contains(raw, "sid=secret") {
			t.Errorf("%s leaks a credential: %s", name, raw)
		}
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("debug directory mode = %v, error=%v, want 0700", info.Mode().Perm(), err)
	}
}

func TestTransportRedactsTokenHeadersAndTheURLInATransportError(t *testing.T) {
	failing := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: request.URL.String(), Err: io.ErrUnexpectedEOF}
	})
	directory := filepath.Join(t.TempDir(), "debug")
	transport := NewTransport(directory, Settings{Enabled: true, IncludeRequestHeaders: true, MaxBodyBytes: 8}, failing).(*Transport)
	request := httptest.NewRequest(http.MethodGet, "https://h/v1?key=query-secret&model=m", nil)
	request.Header.Set("X-Session-Token", "header-token")
	request.Header.Set("X-Client-Secret", "header-secret")
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("want the transport error")
	}
	transport.logger.pending.Wait()

	headers := readRecord(t, directory, "request1")["headers"].(map[string]any)
	if headers["x-session-token"] != "[REDACTED]" || headers["x-client-secret"] != "[REDACTED]" {
		t.Errorf("request headers = %#v", headers)
	}
	if raw := readRaw(t, directory, "response1"); strings.Contains(raw, "query-secret") {
		t.Errorf("transport error leaks the query credential: %s", raw)
	}
}

func TestTransportPrunesOldestFilesBeyondTheTotalCap(t *testing.T) {
	directory := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for index, name := range []string{"request1", "response1", "request2.body", "response2"} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(directory, "notes.txt")
	if err := os.WriteFile(foreign, make([]byte, 500), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := newFileLogger(directory, 250, slog.New(slog.NewTextHandler(io.Discard, nil)))
	firstID := logger.reserve()
	for name, wantKept := range map[string]bool{"request1": false, "response1": false, "request2.body": true, "response2": true, "notes.txt": true} {
		_, err := os.Stat(filepath.Join(directory, name))
		if (err == nil) != wantKept {
			t.Errorf("%s kept=%v, want %v", name, err == nil, wantKept)
		}
	}
	// ids continue after the pruned files so new records never reuse a name
	if firstID != 3 {
		t.Errorf("first id = %d, want 3", firstID)
	}
}

func TestTransportKeepsEverythingWhenTheTotalCapIsZero(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"request1", "response1"} {
		if err := os.WriteFile(filepath.Join(directory, name), make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newFileLogger(directory, 0, slog.New(slog.NewTextHandler(io.Discard, nil))).reserve()
	if entries, _ := os.ReadDir(directory); len(entries) != 2 {
		t.Fatalf("files = %d, want 2", len(entries))
	}
}
