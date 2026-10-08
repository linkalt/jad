package anthropicadapter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink the handler goroutine writes and the test reads.
type syncBuffer struct {
	mutex sync.Mutex
	buf   bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buf.String()
}

// adapterWithLog builds an adapter whose log goes to the returned buffer.
func adapterWithLog(t *testing.T, config string) (*httptest.Server, *syncBuffer) {
	t.Helper()
	sink := &syncBuffer{}
	handler := NewHandler(mustConfig(t, config), nil)
	handler.SetLogger(slog.New(slog.NewTextHandler(sink, nil)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, sink
}

const rateLimitBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`

func rateLimitHeaders() http.Header {
	return http.Header{
		"Request-Id":                         {"req_01ABC"},
		"Retry-After":                        {"30"},
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Set-Cookie":                         {"session=SECRET-COOKIE"},
		"Server":                             {"cloudflare"},
	}
}

func TestHTTPErrorIsLoggedWithHeadersAndBody(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 429, rateLimitBody, rateLimitHeaders())
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`)
	status, header, _ := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if status != 429 || header.Get("Retry-After") != "30" {
		t.Fatalf("the caller must still see the status and Retry-After: %d %q", status, header.Get("Retry-After"))
	}
	line := sink.String()
	for _, want := range []string{
		"upstream request failed", "kind=http_status", "status=429", "model=claude-x", "stream=false",
		"response_headers.request-id=req_01ABC", "response_headers.retry-after=30",
		"response_headers.anthropic-ratelimit-unified-status=rejected", "response_headers.server=cloudflare",
		"rate_limit_error",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "SECRET-COOKIE") || strings.Contains(line, "set-cookie") {
		t.Errorf("a header outside the allow-list was logged:\n%s", line)
	}
}

func TestTheCredentialNeverReachesTheLog(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"bad"}}`, nil)
	path := writeCredential(t, validCredential("sk-ant-oat01-TOPSECRET", time.Hour))
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`","credential_file":"`+path+`","extra_headers":{"X-Custom":"VALUE-SECRET"}}}`)
	for _, body := range []string{`{"model":"claude-x","input":"hi"}`, `{"model":"claude-x","input":"hi","stream":true}`} {
		post(t, adapter.URL+"/v1/responses", body, nil)
	}
	line := sink.String()
	if line == "" {
		t.Fatal("nothing was logged")
	}
	for _, secret := range []string{"TOPSECRET", "REFRESH-SECRET", "VALUE-SECRET", "Bearer"} {
		if strings.Contains(line, secret) {
			t.Fatalf("%q reached the log:\n%s", secret, line)
		}
	}
	// The names are logged, so a missing header (for example no anthropic-beta) is visible.
	for _, name := range []string{"authorization", "anthropic-version", "content-type", "x-custom"} {
		if !strings.Contains(line, name) {
			t.Errorf("sent_header_names lacks %q:\n%s", name, line)
		}
	}
	if strings.Contains(line, "anthropic-beta") {
		t.Errorf("anthropic-beta was not configured, so it must not be listed:\n%s", line)
	}
}

func TestStreamingHTTPErrorIsLogged(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 429, rateLimitBody, rateLimitHeaders())
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`)
	status, _, _ := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, map[string]string{"Authorization": "Bearer t"})
	if status != 429 || !strings.Contains(sink.String(), "stream=true") || !strings.Contains(sink.String(), "response_headers.request-id=req_01ABC") {
		t.Fatalf("status %d, log:\n%s", status, sink.String())
	}
}

func TestErrorEventInAStreamIsLogged(t *testing.T) {
	events := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_STREAM")
		_, _ = w.Write([]byte(events))
	}))
	t.Cleanup(upstream.Close)
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`)
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, map[string]string{"Authorization": "Bearer t"})
	line := sink.String()
	for _, want := range []string{"kind=stream_error_event", "error_type=overloaded_error", "Overloaded", "response_headers.request-id=req_STREAM"} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q:\n%s", want, line)
		}
	}
}

func TestNonStreamBodyForAStreamRequestIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>blocked by gateway</html>"))
	}))
	t.Cleanup(upstream.Close)
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`)
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, map[string]string{"Authorization": "Bearer t"})
	if !strings.Contains(sink.String(), "kind=not_event_stream") || !strings.Contains(sink.String(), "blocked by gateway") {
		t.Fatalf("log:\n%s", sink.String())
	}
}

func TestSuccessIsNotLogged(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, messageBody, nil)
	adapter, sink := adapterWithLog(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`)
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if sink.String() != "" {
		t.Fatalf("a successful call wrote to the log:\n%s", sink.String())
	}
}

func TestLogSettings(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 429, rateLimitBody, rateLimitHeaders())
	post1 := func(config string) string {
		adapter, sink := adapterWithLog(t, config)
		post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
		return sink.String()
	}
	if got := post1(`{"upstream":{"base_url":"` + upstream.URL + `"},"log":{"upstream_errors":false}}`); got != "" {
		t.Errorf("upstream_errors=false still logged:\n%s", got)
	}
	got := post1(`{"upstream":{"base_url":"` + upstream.URL + `"},"log":{"body_bytes":0,"headers":["server"]}}`)
	if strings.Contains(got, "rate_limit_error") || strings.Contains(got, "request-id") || !strings.Contains(got, "response_headers.server=cloudflare") {
		t.Errorf("body_bytes=0 and a one-name header list were not honoured:\n%s", got)
	}
	got = post1(`{"upstream":{"base_url":"` + upstream.URL + `"},"log":{"headers":["Anthropic-RateLimit-*"]}}`)
	if !strings.Contains(got, "anthropic-ratelimit-unified-status=rejected") || strings.Contains(got, "retry-after") {
		t.Errorf("a prefix entry (any case) was not honoured:\n%s", got)
	}
}

func TestLogConfigIsValidated(t *testing.T) {
	for text, want := range map[string]string{
		`{"log":{"body_bytes":-1}}`:        "body_bytes",
		`{"log":{"body_bytes":99999999}}`:  "body_bytes",
		`{"log":{"headers":["bad name"]}}`: "log.headers",
		`{"log":{"headers":[""]}}`:         "log.headers",
		`{"log":{"bogus":1}}`:              "unknown field",
	} {
		if _, err := LoadConfig(writeConfig(t, text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", text, err, want)
		}
	}
}

func TestClipKeepsRunesWholeAndSaysHowMuchWasCut(t *testing.T) {
	// 13 bytes (é and ö take two). A cut at byte 2 is inside é, so it backs up to byte 1.
	if got := clip("héllo wörld", 2); got != "h…(12 more bytes)" {
		t.Errorf("cut inside a rune: %q", got)
	}
	if got := clip("a\n  b\tc", 100); got != "a b c" {
		t.Errorf("whitespace not folded to one line: %q", got)
	}
	if got := clip("abc", 0); got != "" {
		t.Errorf("limit 0: %q", got)
	}
	if got := clip(strings.Repeat("x", 10), 10); got != strings.Repeat("x", 10) {
		t.Errorf("exact fit was cut: %q", got)
	}
}
