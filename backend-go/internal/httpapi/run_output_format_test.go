package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func TestParseRunOutputFormat(t *testing.T) {
	format, err := parseRunOutputFormat([]byte(`{"type":"json_schema","schema":{ "type" : "object" },"strict":false,"description":"d"}`))
	if err != nil || string(format) != `{"type":"json_schema","name":"output","description":"d","schema":{"type":"object"},"strict":false}` {
		t.Fatalf("format = %s, %v", format, err)
	}
	format, err = parseRunOutputFormat([]byte(`{"type":"json_schema","name":"Answer_1","schema":{}}`))
	if err != nil || string(format) != `{"type":"json_schema","name":"Answer_1","schema":{}}` {
		t.Fatalf("named format = %s, %v", format, err)
	}
	if format, err := parseRunOutputFormat([]byte(`null`)); err != nil || format != nil {
		t.Fatalf("null = %s, %v", format, err)
	}
	for name, raw := range map[string]string{
		"not an object":   `"json_schema"`,
		"other type":      `{"type":"json_object","schema":{}}`,
		"no type":         `{"schema":{}}`,
		"no schema":       `{"type":"json_schema"}`,
		"schema not obj":  `{"type":"json_schema","schema":true}`,
		"invalid schema":  `{"type":"json_schema","schema":{"type":7}}`,
		"external ref":    `{"type":"json_schema","schema":{"$ref":"http://example.com/s.json"}}`,
		"bad name":        `{"type":"json_schema","name":"has space","schema":{}}`,
		"strict not bool": `{"type":"json_schema","schema":{},"strict":"yes"}`,
		"unknown member":  `{"type":"json_schema","schema":{},"extra":1}`,
	} {
		_, err := parseRunOutputFormat([]byte(raw))
		var status *StatusError
		if !errors.As(err, &status) || status.Code != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want a 400", name, err)
		}
	}
}

func TestRunRouteHandsTheOutputFormatToTheRunner(t *testing.T) {
	completed := json.RawMessage(`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{}"}]}]}}`)
	formats := make(chan json.RawMessage, 4)
	runner := runnerFunc(func(_ context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		formats <- options.OutputFormat
		return session.RunResult{TurnID: options.TurnID, Event: completed}, nil
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-format", Profile: "dev", Runner: runner}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 16 * 1024, RunStateRetention: time.Minute})
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	post := func(body string) (int, string) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/sessions/session-format/run?profile=dev", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw)
	}

	status, body := post(`{"input":"go","output_format":{"type":"json_schema","schema":{"type":"object"}}}`)
	if status != http.StatusOK {
		t.Fatalf("run: %d %s", status, body)
	}
	if got := string(<-formats); got != `{"type":"json_schema","name":"output","schema":{"type":"object"}}` {
		t.Fatalf("runner got output format %s", got)
	}
	if status, body := post(`{"input":"go"}`); status != http.StatusOK {
		t.Fatalf("plain run: %d %s", status, body)
	}
	if got := <-formats; got != nil {
		t.Fatalf("a run without output_format got %s", got)
	}
	status, body = post(`{"input":"go","output_format":{"type":"json_schema","schema":{"type":7}}}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "output_format.schema is not a valid JSON Schema") {
		t.Fatalf("bad schema: %d %s", status, body)
	}
	select {
	case got := <-formats:
		t.Fatalf("a refused run reached the runner with %s", got)
	default:
	}
}
