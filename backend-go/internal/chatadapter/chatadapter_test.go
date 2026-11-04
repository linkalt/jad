package chatadapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustConfig(t *testing.T, raw string, baseURL string) *Config {
	t.Helper()
	config, err := ParseConfig(json.RawMessage(raw), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func translate(t *testing.T, config *Config, body string) map[string]any {
	t.Helper()
	payload, _, apiErr := translateRequest(config, []byte(body))
	if apiErr != nil {
		t.Fatalf("translateRequest: %+v", apiErr)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func compact(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConfigDefaultsAndRefusals(t *testing.T) {
	config := mustConfig(t, "", "")
	if config.BaseURL != DefaultBaseURL || config.MaxTokensField != MaxTokensFieldCompletion || !config.IncludeUsage || !reflect.DeepEqual(config.ReasoningFields, DefaultReasoningFields()) {
		t.Fatalf("defaults = %+v", config)
	}
	config = mustConfig(t, `{"reasoning_fields":["thinking"],"include_usage":false}`, "http://localhost:11434/v1/")
	if config.BaseURL != "http://localhost:11434/v1" || !reflect.DeepEqual(config.ReasoningFields, []string{"thinking"}) || config.IncludeUsage {
		t.Fatalf("config = %+v", config)
	}
	for name, raw := range map[string]string{
		"not an object":   `[]`,
		"unknown key":     `{"stream_usage":true}`,
		"tokens field":    `{"max_tokens_field":"tokens"}`,
		"system role":     `{"system_role":"user"}`,
		"echo field":      `{"echo_reasoning_field":"content"}`,
		"timeout":         `{"request_timeout_ms":0}`,
		"event bytes":     `{"max_event_bytes":0}`,
		"reserved header": `{"extra_headers":{"Authorization":"x"}}`,
		"bad header":      `{"extra_headers":{"a b":"x"}}`,
		"reserved body":   `{"extra_body":{"messages":[]}}`,
		"empty missing":   `{"missing_tool_result_text":" "}`,
	} {
		if _, err := ParseConfig(json.RawMessage(raw), ""); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The schema documents the loader's keys and defaults.
func TestSchemaMatchesTheConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "model.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			AdditionalProperties bool `json:"additionalProperties"`
			Properties           map[string]struct {
				Default any `json:"default"`
				Maximum any `json:"maximum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	node := schema.Properties["chat_completions"]
	if node.AdditionalProperties {
		t.Fatal("chat_completions allows additional properties")
	}
	defaults := DefaultConfig()
	want := map[string]any{
		"max_tokens_field": defaults.MaxTokensField, "include_usage": defaults.IncludeUsage, "system_role": defaults.SystemRole,
		"reasoning_fields": []any{"reasoning_content", "reasoning"}, "echo_reasoning_field": defaults.EchoReasoningField,
		"missing_tool_result_text": defaults.MissingToolResultText, "request_timeout_ms": float64(defaults.RequestTimeoutMS),
		"max_event_bytes": float64(defaults.MaxEventBytes), "max_request_bytes": float64(defaults.MaxRequestBytes),
		"extra_headers": map[string]any{}, "extra_body": map[string]any{},
	}
	if len(node.Properties) != len(want) {
		t.Fatalf("schema keys = %d, loader keys = %d", len(node.Properties), len(want))
	}
	for key, value := range want {
		if got := node.Properties[key].Default; !reflect.DeepEqual(got, value) {
			t.Errorf("%s: schema default %v, loader default %v", key, got, value)
		}
	}
	for key, value := range map[string]float64{"request_timeout_ms": MaxRequestTimeoutMS, "max_event_bytes": MaxMaxEventBytes, "max_request_bytes": MaxMaxRequestBytes} {
		if got := node.Properties[key].Maximum; got != value {
			t.Errorf("%s: schema maximum %v, loader maximum %v", key, got, value)
		}
	}
}

func TestTranslateRequestFields(t *testing.T) {
	config := mustConfig(t, `{"extra_body":{"chat_template_kwargs":{"enable_thinking":true}}}`, "")
	got := translate(t, config, `{"model":"m","stream":true,"instructions":"be brief","input":"hi",
		"tools":[{"type":"function","name":"read","description":"Read a file","parameters":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"function","name":"read"},"parallel_tool_calls":false,"max_output_tokens":100,"temperature":0.2,
		"reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},
		"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"k","user":"u1"}`)
	want := map[string]string{
		"model":                 `"m"`,
		"messages":              `[{"content":"be brief","role":"system"},{"content":"hi","role":"user"}]`,
		"tools":                 `[{"function":{"description":"Read a file","name":"read","parameters":{"type":"object"},"strict":true},"type":"function"}]`,
		"tool_choice":           `{"function":{"name":"read"},"type":"function"}`,
		"parallel_tool_calls":   `false`,
		"max_completion_tokens": `100`,
		"temperature":           `0.2`,
		"reasoning_effort":      `"high"`,
		"response_format":       `{"json_schema":{"name":"answer","schema":{"type":"object"},"strict":true},"type":"json_schema"}`,
		"user":                  `"u1"`,
		"stream":                `true`,
		"stream_options":        `{"include_usage":true}`,
		"chat_template_kwargs":  `{"enable_thinking":true}`,
	}
	if len(got) != len(want) {
		t.Errorf("fields = %v", got)
	}
	for key, value := range want {
		if compact(t, got[key]) != value {
			t.Errorf("%s = %s, want %s", key, compact(t, got[key]), value)
		}
	}
	legacy := translate(t, mustConfig(t, `{"max_tokens_field":"max_tokens","include_usage":false,"system_role":"developer"}`, ""), `{"model":"m","stream":true,"instructions":"x","input":"hi","max_output_tokens":5}`)
	if legacy["max_tokens"] != float64(5) || legacy["max_completion_tokens"] != nil || legacy["stream_options"] != nil || compact(t, legacy["messages"].([]any)[0]) != `{"content":"x","role":"developer"}` {
		t.Fatalf("legacy = %v", legacy)
	}
}

// A tool round in the history: calls join the assistant message before them, each call
// is answered right after, a call without a result gets the missing-result text, and an
// image a tool returned waits until every call of the round is answered.
func TestTranslateHistoryKeepsToolCallsAnswered(t *testing.T) {
	config := mustConfig(t, `{"echo_reasoning_field":"reasoning_content"}`, "")
	got := translate(t, config, `{"model":"m","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AA","detail":"low"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"thinking it over"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me check."}]},
		{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a\"}"},
		{"type":"function_call","call_id":"c2","name":"shot","arguments":""},
		{"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"took it"},{"type":"input_image","image_url":"data:image/png;base64,BB"}]},
		{"type":"function_call_output","call_id":"c1","output":"file a"},
		{"type":"function_call","call_id":"c3","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"lost","output":"old result"},
		{"type":"message","role":"developer","content":"note"}
	]}`)
	want := `[` +
		`{"content":[{"text":"look","type":"text"},{"image_url":{"detail":"low","url":"data:image/png;base64,AA"},"type":"image_url"}],"role":"user"},` +
		`{"content":"Let me check.","reasoning_content":"thinking it over","role":"assistant","tool_calls":[` +
		`{"function":{"arguments":"{\"path\":\"a\"}","name":"read"},"id":"c1","type":"function"},` +
		`{"function":{"arguments":"{}","name":"shot"},"id":"c2","type":"function"}]},` +
		`{"content":"took it","role":"tool","tool_call_id":"c2"},` +
		`{"content":"file a","role":"tool","tool_call_id":"c1"},` +
		`{"content":[{"text":"Attachments returned by tool call c2:","type":"text"},{"image_url":{"url":"data:image/png;base64,BB"},"type":"image_url"}],"role":"user"},` +
		`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"read"},"id":"c3","type":"function"}]},` +
		`{"content":"No result was recorded for this tool call.","role":"tool","tool_call_id":"c3"},` +
		`{"content":[{"text":"Result of tool call lost:\nold result","type":"text"}],"role":"user"},` +
		`{"content":"note","role":"system"}]`
	if got := compact(t, got["messages"]); got != want {
		t.Fatalf("messages =\n%s\nwant\n%s", got, want)
	}
}

func TestTranslateRequestRefusals(t *testing.T) {
	config := mustConfig(t, "", "")
	for name, body := range map[string]string{
		"not streaming":   `{"model":"m","input":"x"}`,
		"no model":        `{"stream":true,"input":"x"}`,
		"stateful":        `{"model":"m","stream":true,"input":"x","previous_response_id":"r"}`,
		"hosted tool":     `{"model":"m","stream":true,"input":"x","tools":[{"type":"web_search"}]}`,
		"unknown item":    `{"model":"m","stream":true,"input":[{"type":"image_generation_call"}]}`,
		"no input":        `{"model":"m","stream":true}`,
		"bad role":        `{"model":"m","stream":true,"input":[{"type":"message","role":"tool","content":"x"}]}`,
		"image in system": `{"model":"m","stream":true,"input":[{"type":"message","role":"system","content":[{"type":"input_image","image_url":"x"}]}]}`,
		"file url":        `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_url":"http://x"}]}]}`,
		"tool choice":     `{"model":"m","stream":true,"input":"x","tool_choice":"sometimes"}`,
		"text format":     `{"model":"m","stream":true,"input":"x","text":{"format":{"type":"grammar"}}}`,
	} {
		if _, _, apiErr := translateRequest(config, []byte(body)); apiErr == nil || apiErr.Status != 400 {
			t.Errorf("%s: %+v", name, apiErr)
		}
	}
}

// fakeUpstream answers /chat/completions with the given SSE events, and keeps the request.
type fakeUpstream struct {
	events      []string
	status      int
	contentType string
	body        string
	request     map[string]any
	header      http.Header
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(data, &f.request)
	f.header = r.Header.Clone()
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if f.status != 0 {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	if f.contentType != "" {
		w.Header().Set("Content-Type", f.contentType)
		_, _ = io.WriteString(w, f.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range f.events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
	}
}

type event struct {
	Type string
	Data map[string]any
}

func serve(t *testing.T, upstream *fakeUpstream, configRaw, body string) (int, http.Header, []event, string) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	handler := NewHandler(mustConfig(t, configRaw, server.URL+"/v1"), nil, nil)
	handler.now = func() time.Time { return time.Unix(1700000000, 0) }
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(recorder, request)
	var events []event
	for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(data), &decoded); err != nil {
				t.Fatalf("event %q: %v", data, err)
			}
			events = append(events, event{Type: decoded["type"].(string), Data: decoded})
		}
	}
	return recorder.Code, recorder.Header(), events, recorder.Body.String()
}

func names(events []event) string {
	list := make([]string, len(events))
	for index, one := range events {
		list[index] = strings.TrimPrefix(one.Type, "response.")
	}
	return strings.Join(list, ",")
}

func TestStreamTranslatesReasoningTextAndToolCalls(t *testing.T) {
	upstream := &fakeUpstream{events: []string{
		`{"id":"chatcmpl-9","model":"qwen-3","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Think"}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{"reasoning_content":"ing."}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{"content":"I will read."}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"pa"}}]}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"list","arguments":""}}]}}]}`,
		`{"id":"chatcmpl-9","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-9","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":9,"total_tokens":59,"prompt_tokens_details":{"cached_tokens":32},"completion_tokens_details":{"reasoning_tokens":4}}}`,
		`[DONE]`,
	}}
	status, header, events, raw := serve(t, upstream, "", `{"model":"qwen","stream":true,"input":"go"}`)
	if status != 200 || header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %v", status, header)
	}
	want := "created,in_progress,output_item.added,reasoning_text.delta,reasoning_text.delta,reasoning_text.done,output_item.done," +
		"output_item.added,content_part.added,output_text.delta,output_text.done,content_part.done,output_item.done," +
		"output_item.added,function_call_arguments.delta,function_call_arguments.delta,output_item.added," +
		"function_call_arguments.done,output_item.done,function_call_arguments.done,output_item.done,completed"
	if got := names(events); got != want {
		t.Fatalf("events =\n%s\nwant\n%s", got, want)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") || upstream.header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("stream end or credential: %q %v", raw[len(raw)-20:], upstream.header)
	}
	for index, one := range events {
		if one.Data["sequence_number"] != float64(index) {
			t.Fatalf("event %d sequence %v", index, one.Data["sequence_number"])
		}
	}
	completed := events[len(events)-1].Data["response"].(map[string]any)
	output := compact(t, completed["output"])
	wantOutput := `[{"content":[{"text":"Thinking.","type":"reasoning_text"}],"id":"rs_9_0","summary":[],"type":"reasoning"},` +
		`{"content":[{"annotations":[],"text":"I will read.","type":"output_text"}],"id":"msg_9_1","role":"assistant","status":"completed","type":"message"},` +
		`{"arguments":"{\"path\":\"x\"}","call_id":"call_a","id":"fc_9_2","name":"read","status":"completed","type":"function_call"},` +
		`{"arguments":"{}","call_id":"call_9_3","id":"fc_9_3","name":"list","status":"completed","type":"function_call"}]`
	if output != wantOutput {
		t.Fatalf("output =\n%s\nwant\n%s", output, wantOutput)
	}
	if usage := compact(t, completed["usage"]); usage != `{"input_tokens":50,"input_tokens_details":{"cached_tokens":32},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":59}` {
		t.Fatalf("usage = %s", usage)
	}
	if completed["id"] != "resp_9" || completed["model"] != "qwen-3" || completed["status"] != "completed" {
		t.Fatalf("response = %v", completed)
	}
	if upstream.request["model"] != "qwen" || upstream.request["stream"] != true {
		t.Fatalf("upstream request = %v", upstream.request)
	}
}

func TestStreamEndings(t *testing.T) {
	for name, test := range map[string]struct {
		events []string
		last   string
		check  func(map[string]any) bool
	}{
		"length is incomplete": {
			[]string{`{"id":"x","choices":[{"index":0,"delta":{"content":"cut"},"finish_reason":"length"}]}`, `[DONE]`},
			"response.incomplete", func(r map[string]any) bool {
				return compact(t, r["incomplete_details"]) == `{"reason":"max_output_tokens"}`
			},
		},
		"finish without [DONE] completes": {
			[]string{`{"id":"x","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`},
			"response.completed", func(r map[string]any) bool { return r["status"] == "completed" },
		},
		"cut stream fails": {
			[]string{`{"id":"x","choices":[{"index":0,"delta":{"content":"par"}}]}`},
			"response.failed", func(r map[string]any) bool {
				return compact(t, r["error"]) == `{"code":"upstream_stream_ended","message":"the upstream stream ended before a finish_reason"}`
			},
		},
		"error event fails": {
			[]string{`{"error":{"message":"overloaded","type":"server_error","code":"overloaded"}}`},
			"response.failed", func(r map[string]any) bool {
				return compact(t, r["error"]) == `{"code":"overloaded","message":"overloaded"}`
			},
		},
		"invalid json fails": {
			[]string{`{not json`},
			"response.failed", func(r map[string]any) bool { return r["error"].(map[string]any)["code"] == "upstream_invalid_event" },
		},
	} {
		_, _, events, _ := serve(t, &fakeUpstream{events: test.events}, "", `{"model":"m","stream":true,"input":"x"}`)
		last := events[len(events)-1]
		if last.Type != test.last || !test.check(last.Data["response"].(map[string]any)) {
			t.Errorf("%s: last = %s %v", name, last.Type, last.Data)
		}
		// A failed response does not close a half-built item.
		if test.last == "response.failed" && strings.Contains(names(events), "output_item.done") {
			t.Errorf("%s: a partial item was closed: %s", name, names(events))
		}
	}
}

// An HTTP error keeps its status and Retry-After, before any stream byte, so the
// Responses client's provider_retry sees what the provider said.
func TestUpstreamErrorKeepsStatusAndRetryAfter(t *testing.T) {
	status, header, _, raw := serve(t, &fakeUpstream{status: 429, body: `{"error":{"message":"slow down","type":"rate_limit","code":"rate_limit_exceeded"}}`}, "", `{"model":"m","stream":true,"input":"x"}`)
	if status != 429 || header.Get("Retry-After") != "7" || !strings.Contains(raw, `"message":"slow down"`) || !strings.Contains(raw, `"code":"rate_limit_exceeded"`) {
		t.Fatalf("answer = %d %v %s", status, header, raw)
	}
	status, _, _, raw = serve(t, &fakeUpstream{status: 503, body: "upstream down"}, "", `{"model":"m","stream":true,"input":"x"}`)
	if status != 503 || !strings.Contains(raw, "HTTP 503: upstream down") {
		t.Fatalf("plain error = %d %s", status, raw)
	}
	status, _, _, _ = serve(t, &fakeUpstream{}, "", `{"model":"m","input":"x"}`)
	if status != 400 {
		t.Fatalf("a non-streaming request = %d", status)
	}
}

func TestJSONCompletionIsTranslatedAsOneChunk(t *testing.T) {
	upstream := &fakeUpstream{contentType: "application/json", body: `{"id":"chatcmpl-j","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"c","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`}
	_, _, events, _ := serve(t, upstream, "", `{"model":"m","stream":true,"input":"x"}`)
	completed := events[len(events)-1]
	response := completed.Data["response"].(map[string]any)
	if completed.Type != "response.completed" || len(response["output"].([]any)) != 2 || compact(t, response["usage"].(map[string]any)["total_tokens"]) != "5" {
		t.Fatalf("completed = %v", completed.Data)
	}
}

func TestExtraHeadersReachTheUpstream(t *testing.T) {
	upstream := &fakeUpstream{events: []string{`{"id":"x","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`}}
	serve(t, upstream, `{"extra_headers":{"HTTP-Referer":"https://jad.local"}}`, `{"model":"m","stream":true,"input":"x"}`)
	if upstream.header.Get("HTTP-Referer") != "https://jad.local" || upstream.header.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers = %v", upstream.header)
	}
}

func TestOversizedEventFails(t *testing.T) {
	big := `{"id":"x","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 2048) + `"}}]}`
	_, _, events, _ := serve(t, &fakeUpstream{events: []string{big}}, `{"max_event_bytes":1024}`, `{"model":"m","stream":true,"input":"x"}`)
	last := events[len(events)-1]
	if last.Type != "response.failed" || !strings.Contains(compact(t, last.Data), "max_event_bytes") {
		t.Fatalf("last = %v", last.Data)
	}
}
