package anthropicadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type upstreamCall struct {
	Header http.Header
	Body   string
	Path   string
}

// fakeAnthropic answers every POST with status and body, recording the call.
func fakeAnthropic(t *testing.T, status int, body string, header http.Header) (*httptest.Server, *[]upstreamCall) {
	t.Helper()
	calls := &[]upstreamCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*calls = append(*calls, upstreamCall{Header: r.Header.Clone(), Body: string(raw), Path: r.URL.Path})
		for key, values := range header {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func adapterFor(t *testing.T, upstreamURL, extra string) *httptest.Server {
	t.Helper()
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstreamURL+`","beta":["b-1","b-2"]}`+extra+`}`)
	handler := NewHandler(config, nil)
	handler.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, url, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header, string(raw)
}

const messageBody = `{"id":"msg_01abc","type":"message","role":"assistant","model":"claude-x",
 "content":[
  {"type":"thinking","thinking":"hmm","signature":"sig1"},
  {"type":"redacted_thinking","data":"opaque"},
  {"type":"text","text":"let me check"},
  {"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a"}}],
 "stop_reason":"tool_use",
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}`

// auth_header chooses the upstream header; the caller may send either form.
func TestAuthHeaderChoice(t *testing.T) {
	cases := []struct {
		name, authHeader string // authHeader "" leaves the key out: the default applies
		caller           map[string]string
		wantAuth         string
		wantXAPIKey      string
	}{
		{"bearer in, default out", "", map[string]string{"Authorization": "Bearer tok-1"}, "Bearer tok-1", ""},
		{"x-api-key in, default out", "", map[string]string{"x-api-key": "tok-2"}, "Bearer tok-2", ""},
		{"bearer in, x-api-key out", "x-api-key", map[string]string{"Authorization": "Bearer tok-3"}, "", "tok-3"},
		{"x-api-key in, x-api-key out", "x-api-key", map[string]string{"x-api-key": "tok-4"}, "", "tok-4"},
	}
	for _, c := range cases {
		upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
		setting := ""
		if c.authHeader != "" {
			setting = `,"auth_header":"` + c.authHeader + `"`
		}
		server := httptest.NewServer(NewHandler(mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`"`+setting+`}}`), nil))
		t.Cleanup(server.Close)
		if status, _, raw := post(t, server.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, c.caller); status != 200 {
			t.Fatalf("%s: status %d body %s", c.name, status, raw)
		}
		call := (*calls)[0]
		if call.Header.Get("Authorization") != c.wantAuth || call.Header.Get("x-api-key") != c.wantXAPIKey {
			t.Errorf("%s: Authorization=%q x-api-key=%q", c.name, call.Header.Get("Authorization"), call.Header.Get("x-api-key"))
		}
	}
}

func TestNonStreamingEndToEnd(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses",
		`{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer sk-ant-test"})
	if status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
	call := (*calls)[0]
	if call.Path != "/v1/messages" || call.Header.Get("Authorization") != "Bearer sk-ant-test" ||
		call.Header.Get("anthropic-version") != DefaultAnthropicVersion || call.Header.Get("anthropic-beta") != "b-1,b-2" {
		t.Fatalf("upstream call wrong: %+v", call)
	}
	if call.Header.Get("x-api-key") != "" {
		t.Fatal("the default auth_header is bearer: x-api-key must not be sent")
	}
	var response responseObject
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "resp_01abc" || response.Status != "completed" || response.CreatedAt != 1_700_000_000 || response.Object != "response" {
		t.Fatalf("envelope wrong: %+v", response)
	}
	if len(response.Output) != 3 {
		t.Fatalf("expected reasoning, message and function_call, got %s", raw)
	}
	reasoning := response.Output[0]
	if reasoning["type"] != "reasoning" || !strings.Contains(reasoning["encrypted_content"].(string), `"signature":"sig1"`) ||
		!strings.Contains(reasoning["encrypted_content"].(string), `"data":"opaque"`) {
		t.Fatalf("reasoning item wrong: %v", reasoning)
	}
	call3 := response.Output[2]
	if call3["type"] != "function_call" || call3["call_id"] != "toolu_1" || call3["arguments"] != `{"path":"a"}` {
		t.Fatalf("function_call wrong: %v", call3)
	}
	usage := response.Usage
	if usage.InputTokens != 16 || usage.OutputTokens != 5 || usage.TotalTokens != 21 || usage.InputTokensDetails["cached_tokens"] != 4 {
		t.Fatalf("usage wrong: %+v", usage)
	}
}

// What the adapter returned must be accepted when the caller sends it back.
func TestReasoningReplaysUnchanged(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	headers := map[string]string{"Authorization": "Bearer k"}
	_, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, headers)
	var first responseObject
	if err := json.Unmarshal([]byte(raw), &first); err != nil {
		t.Fatal(err)
	}
	history, _ := json.Marshal(first.Output)
	next := `{"model":"claude-x","input":[{"role":"user","content":"hi"},` + strings.Trim(string(history), "[]") +
		`,{"type":"function_call_output","call_id":"toolu_1","output":"done"}]}`
	if status, _, body := post(t, adapter.URL+"/v1/responses", next, headers); status != 200 {
		t.Fatalf("replay status %d body %s", status, body)
	}
	var sent anthropicRequest
	if err := json.Unmarshal([]byte((*calls)[1].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 3 || sent.Messages[1].Role != "assistant" {
		t.Fatalf("replayed history wrong: %s", (*calls)[1].Body)
	}
	assistant := sent.Messages[1].Content
	if assistant[0].Type != "thinking" || *assistant[0].Thinking != "hmm" || assistant[0].Signature != "sig1" ||
		assistant[1].Type != "redacted_thinking" || assistant[1].Data != "opaque" ||
		assistant[2].Type != "text" || assistant[3].Type != "tool_use" {
		t.Fatalf("assistant blocks not replayed in order with signatures: %s", (*calls)[1].Body)
	}
	if sent.Messages[2].Content[0].Type != "tool_result" {
		t.Fatalf("tool_result must lead the user message: %s", (*calls)[1].Body)
	}
}

// A reasoning item we did not produce, or one without a signature, must not
// reach Anthropic: it would be rejected as an invalid thinking block.
func TestUnverifiableReasoningIsDropped(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	unsigned := `[{"type":"thinking","thinking":"x"},{"type":"redacted_thinking"},{"type":"text","text":"sneaky"}]`
	quoted, _ := json.Marshal(unsigned)
	body := `{"model":"m","input":[{"role":"user","content":"hi"},
	  {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":` + string(quoted) + `},
	  {"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"gAAAAAB-openai-opaque"},
	  {"type":"reasoning","id":"rs_3","summary":[]},
	  {"role":"assistant","content":[{"type":"output_text","text":"hello"}]},
	  {"role":"user","content":"more"}]}`
	if status, _, raw := post(t, adapter.URL+"/v1/responses", body, map[string]string{"x-api-key": "k"}); status != 200 {
		t.Fatalf("status %d %s", status, raw)
	}
	sent := (*calls)[0].Body
	if strings.Contains(sent, `"thinking"`) || strings.Contains(sent, "sneaky") || strings.Contains(sent, "redacted") {
		t.Fatalf("unverifiable reasoning was forwarded: %s", sent)
	}
	var request anthropicRequest
	_ = json.Unmarshal([]byte(sent), &request)
	if len(request.Messages) != 3 {
		t.Fatalf("dropping reasoning must keep the turns intact: %s", sent)
	}
}

func TestStopReasons(t *testing.T) {
	cases := map[string]struct{ status, reason string }{
		"end_turn":                      {"completed", ""},
		"tool_use":                      {"completed", ""},
		"stop_sequence":                 {"completed", ""},
		"max_tokens":                    {"incomplete", "max_output_tokens"},
		"refusal":                       {"incomplete", "content_filter"},
		"pause_turn":                    {"incomplete", "pause_turn"},
		"model_context_window_exceeded": {"incomplete", "model_context_window_exceeded"},
	}
	for stop, want := range cases {
		status, details := statusFor(stop)
		reason := ""
		if details != nil {
			reason = details.Reason
		}
		if status != want.status || reason != want.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", stop, status, reason, want.status, want.reason)
		}
	}
}

func TestOmittedThinkingKeepsSignature(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn",
	 "content":[{"type":"thinking","thinking":"","signature":"S"},{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	upstream, _ := fakeAnthropic(t, 200, body, nil)
	adapter := adapterFor(t, upstream.URL, "")
	_, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x"}`, map[string]string{"x-api-key": "k"})
	var response responseObject
	_ = json.Unmarshal([]byte(raw), &response)
	item := response.Output[0]
	if _, hasContent := item["content"]; hasContent {
		t.Fatalf("an empty thinking block must not produce reasoning text: %v", item)
	}
	if decoded := decodeThinkingBlocks(item["encrypted_content"].(string)); len(decoded) != 1 || decoded[0].Signature != "S" || *decoded[0].Thinking != "" {
		t.Fatalf("signature lost: %v", item)
	}
}

func TestMissingKeyAndUnsupportedRoutes(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	if status, _, _ := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x"}`, nil); status != 401 {
		t.Fatalf("missing key: %d", status)
	}
	if len(*calls) != 0 {
		t.Fatal("a request without a key must not reach the upstream")
	}
	response, err := http.Get(adapter.URL + "/v1/responses/resp_1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 501 {
		t.Fatalf("stored response lookup: %d", response.StatusCode)
	}
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x","previous_response_id":"r"}`, map[string]string{"x-api-key": "k"})
	if status != 501 || !strings.Contains(raw, "unsupported_operation") {
		t.Fatalf("previous_response_id: %d %s", status, raw)
	}
}

func TestUpstreamErrorsKeepStatusAndRetryAfter(t *testing.T) {
	body := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	for _, status := range []int{429, 500, 529, 401} {
		upstream, _ := fakeAnthropic(t, status, body, http.Header{"Retry-After": {"7"}})
		adapter := adapterFor(t, upstream.URL, "")
		got, header, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x"}`, map[string]string{"x-api-key": "k"})
		if got != status || header.Get("Retry-After") != "7" {
			t.Errorf("status %d: got %d retry-after %q", status, got, header.Get("Retry-After"))
		}
		var envelope errorEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil || envelope.Error.Message != "Overloaded" || envelope.Error.Type != "overloaded_error" {
			t.Errorf("status %d: envelope %s (%v)", status, raw, err)
		}
	}
}

func TestSDKDoesNotRetryNonStreamingFailures(t *testing.T) {
	upstream, calls := fakeAnthropic(t, http.StatusInternalServerError,
		`{"type":"error","error":{"type":"api_error","message":"temporary"}}`, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, _ := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"})
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d", status)
	}
	if len(*calls) != 1 {
		t.Fatalf("the SDK made %d upstream attempts; want 1", len(*calls))
	}
}

func TestConfiguredTimeoutAllowsLargeNonStreamingRequests(t *testing.T) {
	upstream, calls := fakeAnthropic(t, http.StatusOK, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses",
		`{"model":"claude-x","input":"hi","max_output_tokens":32000}`, map[string]string{"x-api-key": "k"})
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, raw)
	}
	if len(*calls) != 1 {
		t.Fatalf("got %d upstream calls, want 1", len(*calls))
	}
	var sent anthropicRequest
	if err := json.Unmarshal([]byte((*calls)[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MaxTokens != 32000 {
		t.Fatalf("max_tokens = %d, want 32000", sent.MaxTokens)
	}
}

func TestUnknownResponseContentBlocksAreIgnored(t *testing.T) {
	body := `{"id":"msg_unknown","type":"message","role":"assistant","model":"claude-x","content":[{"type":"future_block","value":"ignored"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	upstream, _ := fakeAnthropic(t, http.StatusOK, body, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"x-api-key": "k"})
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, raw)
	}
	var response responseObject
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || !strings.Contains(raw, `"text":"ok"`) {
		t.Fatalf("unknown block changed the response: %s", raw)
	}
}

func TestErrorBodyWithStatus200IsAnError(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x"}`, map[string]string{"x-api-key": "k"})
	if status < 500 || !strings.Contains(raw, "overloaded_error") {
		t.Fatalf("an error body behind HTTP 200 must not become a response: %d %s", status, raw)
	}
}

func TestIgnoredFieldsHeaderAndBadUpstream(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, `not json`, nil)
	adapter := adapterFor(t, upstream.URL, "")
	status, header, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x","top_logprobs":2}`, map[string]string{"x-api-key": "k"})
	if status != 502 || !strings.Contains(raw, "upstream_invalid_body") {
		t.Fatalf("invalid upstream body: %d %s", status, raw)
	}
	_ = header
	good, _ := fakeAnthropic(t, 200, messageBody, nil)
	adapter = adapterFor(t, good.URL, "")
	_, header, _ = post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x","top_logprobs":2}`, map[string]string{"x-api-key": "k"})
	if header.Get("X-Adapter-Ignored-Fields") != "top_logprobs" {
		t.Fatalf("ignored header = %q", header.Get("X-Adapter-Ignored-Fields"))
	}
}

func TestRequestSizeLimitAndUnreachableUpstream(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, `,"max_request_bytes":20`)
	if status, _, _ := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"this is longer than twenty bytes"}`, map[string]string{"x-api-key": "k"}); status != 413 {
		t.Fatalf("oversized body: %d", status)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	adapter = adapterFor(t, url, "")
	if status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"m","input":"x"}`, map[string]string{"x-api-key": "k"}); status != 502 || !strings.Contains(raw, "upstream_unreachable") {
		t.Fatalf("unreachable upstream: %d %s", status, raw)
	}
}

func TestRequestIDIsForwardedOnErrorsAndSuccess(t *testing.T) {
	headers := map[string]string{"x-api-key": "k"}
	errUpstream, _ := fakeAnthropic(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_BODY"}`,
		http.Header{"Request-Id": {"req_HEADER"}})
	status, header, raw := post(t, adapterFor(t, errUpstream.URL, "").URL+"/v1/responses", `{"model":"m","input":"x"}`, headers)
	if status != 529 || header.Get("request-id") != "req_BODY" || !strings.Contains(raw, "Overloaded (request_id: req_BODY)") {
		t.Fatalf("error: %d %q %s", status, header.Get("request-id"), raw)
	}
	headerOnly, _ := fakeAnthropic(t, 500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, http.Header{"Request-Id": {"req_HEADER"}})
	status, header, raw = post(t, adapterFor(t, headerOnly.URL, "").URL+"/v1/responses", `{"model":"m","input":"x"}`, headers)
	if status != 500 || header.Get("request-id") != "req_HEADER" || strings.Contains(raw, "request_id") {
		t.Fatalf("header only: %d %q %s", status, header.Get("request-id"), raw)
	}
	ok, _ := fakeAnthropic(t, 200, messageBody, http.Header{"Request-Id": {"req_OK"}})
	status, header, _ = post(t, adapterFor(t, ok.URL, "").URL+"/v1/responses", `{"model":"m","input":"x"}`, headers)
	if status != 200 || header.Get("request-id") != "req_OK" {
		t.Fatalf("success: %d %q", status, header.Get("request-id"))
	}
	stream, _ := scriptedUpstreamWithHeader(t, http.Header{"Request-Id": {"req_STREAM"}}, okStream())
	request, _ := http.NewRequest(http.MethodPost, adapterFor(t, stream.URL, "").URL+"/v1/responses", strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	request.Header.Set("x-api-key", "k")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get("request-id") != "req_STREAM" {
		t.Fatalf("stream request-id = %q", response.Header.Get("request-id"))
	}
}
