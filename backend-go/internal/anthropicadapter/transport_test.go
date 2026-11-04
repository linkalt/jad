package anthropicadapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream path keeps a path prefix of base_url, with or without its trailing slash.
func TestMessagesURLKeepsTheBasePath(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.anthropic.com":   "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":  "https://api.anthropic.com/v1/messages",
		"http://127.0.0.1:9/gateway":  "http://127.0.0.1:9/gateway/v1/messages",
		"http://127.0.0.1:9/gateway/": "http://127.0.0.1:9/gateway/v1/messages",
		"http://127.0.0.1:9/a/b?x=1":  "http://127.0.0.1:9/a/b/v1/messages",
	} {
		if got, err := messagesURL(base); err != nil || got != want {
			t.Errorf("messagesURL(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
	upstream, calls := fakeAnthropic(t, http.StatusOK, messageBody, nil)
	adapter := adapterFor(t, upstream.URL+"/proxy/anthropic", "")
	if status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"}); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	if got := (*calls)[0].Path; got != "/proxy/anthropic/v1/messages" {
		t.Fatalf("path = %q", got)
	}
}

// The request carries the configured headers, a JSON content type, the Accept of its mode
// and the adapter's User-Agent; nothing of an SDK.
func TestUpstreamRequestHeaders(t *testing.T) {
	upstream, calls := fakeAnthropic(t, http.StatusOK, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	if status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"}); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	streamed, _ := scriptedUpstream(t, okStream())
	var streamHeader http.Header
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamHeader = r.Header.Clone()
		proxied, err := http.Post(streamed.URL+r.URL.Path, "application/json", r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer proxied.Body.Close()
		w.Header().Set("Content-Type", proxied.Header.Get("Content-Type"))
		_, _ = io.Copy(w, proxied.Body)
	}))
	t.Cleanup(recorder.Close)
	streamAdapter := adapterFor(t, recorder.URL, "")
	if _, _, done := postStream(t, streamAdapter.URL, `{"model":"claude-x","input":"hi","stream":true}`); !done {
		t.Fatal("the stream did not complete")
	}

	for mode, header := range map[string]http.Header{"json": (*calls)[0].Header, "stream": streamHeader} {
		wantAccept := map[string]string{"json": "application/json", "stream": "text/event-stream"}[mode]
		if header.Get("Accept") != wantAccept || header.Get("Content-Type") != "application/json" ||
			header.Get("User-Agent") != defaultUserAgent || header.Get("anthropic-version") != DefaultAnthropicVersion ||
			header.Get("anthropic-beta") != "b-1,b-2" || header.Get("Authorization") != "Bearer k" {
			t.Errorf("%s: headers %v", mode, header)
		}
		for name := range header {
			if strings.HasPrefix(strings.ToLower(name), "x-stainless") {
				t.Errorf("%s: SDK header %s sent", mode, name)
			}
		}
	}
}

// An error status with a body that is not Anthropic's JSON (a gateway's page) keeps its
// status, and its text is the message.
func TestNonJSONErrorBodyKeepsStatusAndText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "gateway down\n")
	}))
	t.Cleanup(upstream.Close)
	adapter := adapterFor(t, upstream.URL, "")
	for _, body := range []string{`{"model":"claude-x","input":"hi"}`, `{"model":"claude-x","input":"hi","stream":true}`} {
		status, _, raw := post(t, adapter.URL+"/v1/responses", body, map[string]string{"x-api-key": "k"})
		if status != http.StatusBadGateway || !strings.Contains(raw, `"message":"gateway down"`) || !strings.Contains(raw, `"code":"api_error"`) {
			t.Errorf("%s: %d %s", body, status, raw)
		}
	}
}

// A successful status with an empty or unreadable body is a 502, never a translated reply.
func TestEmptyOrBrokenSuccessBodyIsAnError(t *testing.T) {
	for body, want := range map[string]string{"": "the upstream response is empty", " \n": "the upstream response is empty", `{"id":`: "not valid JSON"} {
		upstream, _ := fakeAnthropic(t, http.StatusOK, body, nil)
		adapter := adapterFor(t, upstream.URL, "")
		status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"})
		if status != http.StatusBadGateway || !strings.Contains(raw, "upstream_invalid_body") || !strings.Contains(raw, want) {
			t.Errorf("body %q: %d %s", body, status, raw)
		}
	}
}

// A body cut off after a 200 is a read failure, not an unreachable upstream.
func TestTruncatedSuccessBodyIsAReadFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg`)
	}))
	t.Cleanup(upstream.Close)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"})
	if status != http.StatusBadGateway || !strings.Contains(raw, "upstream_read_failed") {
		t.Fatalf("%d %s", status, raw)
	}
}

// The request timeout ends a request that gets no answer with a 504.
func TestRequestTimeoutIsAGatewayTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); upstream.Close() })
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","request_timeout_ms":50}}`)
	adapter := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(adapter.Close)
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"})
	if status != http.StatusGatewayTimeout || !strings.Contains(raw, "upstream_unreachable") {
		t.Fatalf("%d %s", status, raw)
	}
}
