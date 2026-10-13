package httpdebug

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
