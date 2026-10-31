package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets a run goroutine and the test read and write one log buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturedLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	output := &syncBuffer{}
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level})), output
}

type failingCatalog struct {
	*fakeCatalogResolver
	err error
}

func (c *failingCatalog) ListSessions(context.Context, string, SessionRunProvider) (CatalogResponse, error) {
	return CatalogResponse{}, c.err
}

func authorized(method, target string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	return request
}

func TestInternalErrorLogsItsCauseButNotInTheBody(t *testing.T) {
	logger, output := capturedLogger(slog.LevelDebug)
	cause := errors.New("read /srv/data/session-9/messages.jsonl: permission denied")
	resolver := &failingCatalog{fakeCatalogResolver: &fakeCatalogResolver{fakeResolver: &fakeResolver{}}, err: cause}
	handler := testHandler(t, resolver, Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorized(http.MethodGet, "/api/sessions"))

	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != `{"error":"internal server error"}` {
		t.Fatalf("response = %d %q; the body must stay generic", response.Code, response.Body.String())
	}
	logged := output.String()
	for _, want := range []string{"level=ERROR", `msg="request failed"`, "route=/api/sessions", "status=500", "permission denied"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log is missing %q:\n%s", want, logged)
		}
	}
}

func TestClientErrorsAreNotLoggedAtErrorLevel(t *testing.T) {
	logger, output := capturedLogger(slog.LevelInfo)
	handler := testHandler(t, &fakeResolver{}, Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/sessions", nil), // 401
		authorized(http.MethodGet, "/api/nothing"),                // 404
	} {
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if logged := output.String(); logged != "" {
		t.Errorf("401 and 404 responses must not be logged at info level, got:\n%s", logged)
	}
}

func TestAccessLogAtDebugUsesTemplatesAndNeverSecrets(t *testing.T) {
	logger, output := capturedLogger(slog.LevelDebug)
	resolver := &fakeCatalogResolver{fakeResolver: &fakeResolver{}, snapshot: CatalogResponse{Body: json.RawMessage(`{"id":"x"}`), ETag: `"v1"`}}
	handler := testHandler(t, resolver, Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	const secretQuery = "profile=top-secret-profile&token=SECRETQUERYVALUE"
	request := authorized(http.MethodGet, "/api/sessions/my-session?"+secretQuery)
	request.Header.Set("Cookie", "sid=SECRETCOOKIE")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), authorized(http.MethodGet, "/api/sessions/my-session/UNTRUSTED%0Afake=line"))

	logged := output.String()
	if !strings.Contains(logged, "route=/api/sessions/{session}") || !strings.Contains(logged, "route=unmatched") {
		t.Errorf("expected route templates in the access log:\n%s", logged)
	}
	for _, secret := range []string{testAPIToken, "SECRETQUERYVALUE", "top-secret-profile", "SECRETCOOKIE", "my-session", "UNTRUSTED", "fake=line"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the access log leaked %q:\n%s", secret, logged)
		}
	}
	if strings.Count(logged, "\n") != 2 {
		t.Errorf("expected exactly one line per request, got:\n%s", logged)
	}
}

func TestRouteTemplate(t *testing.T) {
	cases := map[string]string{
		"/api/health":                                  "/api/health",
		"/api/sessions":                                "/api/sessions",
		"/api/sessions/abc":                            "/api/sessions/{session}",
		"/api/sessions/abc/run":                        "/api/sessions/{session}/run",
		"/api/sessions/abc/run/stream":                 "/api/sessions/{session}/run/stream",
		"/api/sessions/abc/run/interaction/extend":     "/api/sessions/{session}/run/interaction/extend",
		"/api/sessions/abc/messages/m1":                "/api/sessions/{session}/messages/{message}",
		"/api/sessions/abc/front-compact":              "/api/sessions/{session}/front-compact",
		"/api/library":                                 "/api/library",
		"/api/library/skill":                           "/api/library/{kind}",
		"/api/library/skill/agent-tool-design":         "/api/library/{kind}/{id}",
		"/api/sessions/abc/unknown":                    "unmatched",
		"/api/sessions/abc/run/stream/extra":           "unmatched",
		"/somewhere/else":                              "unmatched",
		"/":                                            "unmatched",
		"/api/library/a/b/c":                           "unmatched",
		"/api/sessions/abc/messages/m1/extra/segments": "unmatched",
		"/api/sessions/abc/messages/m1/images/0/0":     "/api/sessions/{session}/messages/{message}/images/{image}",
		"/api/sessions/abc/images/i1":                  "/api/sessions/{session}/images/{image}",
		"/api/sessions/abc/run/images/c1/i1":           "/api/sessions/{session}/run/images/{call}/{image}",
		"/api/sessions/abc/checkpoints":                "/api/sessions/{session}/checkpoints",
		"/api/sessions/abc/checkpoints/c1/restore":     "/api/sessions/{session}/checkpoints/{checkpoint}/restore",
		"/api/sessions/abc/run/interaction":            "/api/sessions/{session}/run/interaction",
		"/api/catalog/models":                          "/api/catalog/{kind}",
		"/api/catalog/models/openai/large":             "/api/catalog/{kind}/{id}",
		"/api/catalog/profiles/dev/solo":               "/api/catalog/{kind}/{id}",
		"/api/catalog/models/openai":                   "unmatched",
	}
	for path, want := range cases {
		if got := routeTemplate(path); got != want {
			t.Errorf("routeTemplate(%q) = %q, want %q", path, got, want)
		}
	}
}

type panicCatalog struct{ *fakeCatalogResolver }

func (panicCatalog) ListSessions(context.Context, string, SessionRunProvider) (CatalogResponse, error) {
	panic("catalog exploded")
}

func TestPanicInARouteBecomesAGeneric500AndIsLogged(t *testing.T) {
	logger, output := capturedLogger(slog.LevelInfo)
	resolver := panicCatalog{&fakeCatalogResolver{fakeResolver: &fakeResolver{}}}
	handler := testHandler(t, resolver, Config{Logger: logger, APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorized(http.MethodGet, "/api/sessions"))

	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != `{"error":"internal server error"}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	logged := output.String()
	for _, want := range []string{`msg="request panicked"`, "catalog exploded", "route=/api/sessions", "request_observer_test.go"} {
		if !strings.Contains(logged, want) {
			t.Errorf("panic log is missing %q:\n%s", want, logged)
		}
	}
}

func TestAbortHandlerPanicIsStillPropagated(t *testing.T) {
	handler := testHandler(t, &fakeResolver{}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler: net/http uses it to drop a connection quietly", recovered)
		}
	}()
	handler.observeRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), func(http.ResponseWriter) {
		panic(http.ErrAbortHandler)
	})
}

func TestObservedWriterKeepsOptionalInterfaces(t *testing.T) {
	handler := testHandler(t, &fakeResolver{}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()

	recorder := httptest.NewRecorder() // implements http.Flusher
	handler.observeRequest(recorder, httptest.NewRequest(http.MethodGet, "/", nil), func(w http.ResponseWriter) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("a flushable response writer must stay flushable: SSE depends on it")
		}
		if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
			t.Error("Unwrap is needed by http.ResponseController")
		}
		w.(http.Flusher).Flush()
	})
	if !recorder.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}

	handler.observeRequest(plainWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil), func(w http.ResponseWriter) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("a writer that cannot flush must not claim to: handlers check http.Flusher to reject SSE")
		}
	})
}

// plainWriter hides the recorder's Flush method.
type plainWriter struct{ recorder *httptest.ResponseRecorder }

func (w plainWriter) Header() http.Header         { return w.recorder.Header() }
func (w plainWriter) Write(b []byte) (int, error) { return w.recorder.Write(b) }
func (w plainWriter) WriteHeader(status int)      { w.recorder.WriteHeader(status) }
