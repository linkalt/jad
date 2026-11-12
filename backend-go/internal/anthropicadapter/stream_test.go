package anthropicadapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sse builds an Anthropic event stream from "name|json" pairs.
func sse(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		name, data, _ := strings.Cut(event, "|")
		out.WriteString("event: " + name + "\ndata: " + data + "\n\n")
	}
	return out.String()
}

// scriptedUpstream serves one stream body per request, in order; the last body
// repeats. It returns the server and a request counter.
func scriptedUpstream(t *testing.T, bodies ...string) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(atomic.AddInt32(&count, 1)) - 1
		if index >= len(bodies) {
			index = len(bodies) - 1
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, bodies[index])
	}))
	t.Cleanup(server.Close)
	return server, &count
}

type streamedEvent struct {
	Name string
	Body map[string]any
}

func (e streamedEvent) get(path ...string) any {
	var current any = e.Body
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

// postStream sends a streaming request and parses the Responses SSE reply.
func postStream(t *testing.T, adapterURL, body string) (int, []streamedEvent, bool) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, adapterURL+"/v1/responses", strings.NewReader(body))
	request.Header.Set("x-api-key", "k")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var events []streamedEvent
	sawDone := false
	if response.StatusCode != 200 {
		return response.StatusCode, nil, false
	}
	err = readSSE(response.Body, DefaultMaxEventBytes, func(raw sseEvent) bool {
		if raw.Data == "[DONE]" {
			sawDone = true
			return true
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw.Data), &parsed); err != nil {
			t.Fatalf("event %q is not JSON: %v", raw.Data, err)
		}
		events = append(events, streamedEvent{Name: raw.Name, Body: parsed})
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return 200, events, sawDone
}

func types(events []streamedEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Body["type"].(string)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const startEvent = `message_start|{"type":"message_start","message":{"id":"msg_s1","model":"claude-x","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":5}}}`

func TestStreamTextSequence(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`ping|{"type":"ping"}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`,
		`message_stop|{"type":"message_stop"}`))
	adapter := adapterFor(t, upstream.URL, "")
	status, events, done := postStream(t, adapter.URL, `{"model":"claude-x","input":"hi","stream":true}`)
	if status != 200 || !done {
		t.Fatalf("status %d done=%v", status, done)
	}
	want := []string{
		"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	}
	if got := types(events); !equalStrings(got, want) {
		t.Fatalf("event order\n got  %v\n want %v", got, want)
	}
	for index, event := range events {
		if int(event.Body["sequence_number"].(float64)) != index {
			t.Fatalf("event %d has sequence_number %v", index, event.Body["sequence_number"])
		}
		if event.Name != event.Body["type"] {
			t.Fatalf("SSE event name %q differs from type %q", event.Name, event.Body["type"])
		}
	}
	if events[4].get("delta") != "Hel" || events[6].get("text") != "Hello" {
		t.Fatalf("text deltas wrong: %v %v", events[4].Body, events[6].Body)
	}
	last := events[len(events)-1]
	if last.get("response", "status") != "completed" || last.get("response", "id") != "resp_s1" ||
		last.get("response", "usage", "output_tokens") != float64(15) ||
		last.get("response", "usage", "input_tokens") != float64(30) ||
		last.get("response", "usage", "input_tokens_details", "cached_tokens") != float64(5) {
		t.Fatalf("final response wrong: %v", last.Body)
	}
	output := last.get("response", "output").([]any)
	if len(output) != 1 || output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("final output must carry the item: %v", output)
	}
}

func TestStreamToolCallWithThinkingAndText(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`content_block_start|{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"RED"}}`,
		`content_block_stop|{"type":"content_block_stop","index":1}`,
		`content_block_start|{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"checking"}}`,
		`content_block_stop|{"type":"content_block_stop","index":2}`,
		`content_block_start|{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_9","name":"read","input":{}}}`,
		`content_block_delta|{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
		`content_block_delta|{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"th\":\"a\"}"}}`,
		`content_block_stop|{"type":"content_block_stop","index":3}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`,
		`message_stop|{"type":"message_stop"}`))
	adapter := adapterFor(t, upstream.URL, "")
	_, events, done := postStream(t, adapter.URL, `{"model":"claude-x","input":"hi","stream":true}`)
	if !done {
		t.Fatal("no [DONE]")
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_text.delta", "response.reasoning_text.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done", "response.completed",
	}
	if got := types(events); !equalStrings(got, want) {
		t.Fatalf("event order\n got  %v\n want %v", got, want)
	}
	reasoning := events[5].get("item").(map[string]any)
	encrypted := reasoning["encrypted_content"].(string)
	if blocks := decodeThinkingBlocks(encrypted); len(blocks) != 2 || blocks[0].Signature != "SIG" || *blocks[0].Thinking != "step one" || blocks[1].Data != "RED" {
		t.Fatalf("both thinking blocks must be replayable from one reasoning item: %s", encrypted)
	}
	if events[2].get("output_index") != float64(0) || events[6].get("output_index") != float64(1) || events[12].get("output_index") != float64(2) {
		t.Fatal("output_index must increase per item")
	}
	call := events[16].get("item").(map[string]any)
	if call["call_id"] != "toolu_9" || call["arguments"] != `{"path":"a"}` || call["status"] != "completed" {
		t.Fatalf("function_call wrong: %v", call)
	}
	// The terminal event repeats every item, which the TypeScript backend rebuilds from.
	final := events[len(events)-1].get("response", "output").([]any)
	if len(final) != 3 || final[0].(map[string]any)["type"] != "reasoning" || final[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("final output wrong: %v", final)
	}
}

func TestStreamedItemsMatchNonStreamingShape(t *testing.T) {
	// The same message through both paths must give the same output items.
	message := `{"id":"msg_s1","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn",
	 "content":[{"type":"thinking","thinking":"why","signature":"S"},{"type":"text","text":"ok"},
	 {"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],"usage":{"input_tokens":1,"output_tokens":2}}`
	plainUpstream, _ := fakeAnthropic(t, 200, message, nil)
	_, _, raw := post(t, adapterFor(t, plainUpstream.URL, "").URL+"/v1/responses", `{"model":"claude-x","input":"x"}`, map[string]string{"x-api-key": "k"})
	var plain responseObject
	if err := json.Unmarshal([]byte(raw), &plain); err != nil {
		t.Fatal(err)
	}
	streamUpstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"why"}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"S"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`content_block_start|{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`,
		`content_block_stop|{"type":"content_block_stop","index":1}`,
		`content_block_start|{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`content_block_delta|{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`content_block_stop|{"type":"content_block_stop","index":2}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`message_stop|{"type":"message_stop"}`))
	_, events, _ := postStream(t, adapterFor(t, streamUpstream.URL, "").URL, `{"model":"claude-x","input":"x","stream":true}`)
	streamed := events[len(events)-1].get("response", "output")
	if asJSON(t, streamed) != asJSON(t, plain.Output) {
		t.Fatalf("streamed items differ from non-streaming\n stream %s\n plain  %s", asJSON(t, streamed), asJSON(t, plain.Output))
	}
}

func TestStreamStopReasons(t *testing.T) {
	cases := map[string]struct{ event, reason string }{
		"end_turn":                      {"response.completed", ""},
		"max_tokens":                    {"response.incomplete", "max_output_tokens"},
		"refusal":                       {"response.incomplete", "content_filter"},
		"model_context_window_exceeded": {"response.incomplete", "model_context_window_exceeded"},
	}
	for stop, want := range cases {
		upstream, _ := scriptedUpstream(t, sse(startEvent,
			`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`,
			`content_block_stop|{"type":"content_block_stop","index":0}`,
			`message_delta|{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":3}}`,
			`message_stop|{"type":"message_stop"}`))
		_, events, done := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
		last := events[len(events)-1]
		if !done || last.Body["type"] != want.event {
			t.Errorf("%s: terminal %v done=%v, want %s", stop, last.Body["type"], done, want.event)
			continue
		}
		if reason := last.get("response", "incomplete_details", "reason"); want.reason != "" && reason != want.reason {
			t.Errorf("%s: reason %v, want %s", stop, reason, want.reason)
		}
	}
}

func TestStreamErrorEventAfterOutputFails(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`,
		`error|{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	_, events, done := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if !done || last.Body["type"] != "response.failed" ||
		last.get("response", "error", "code") != "overloaded_error" || last.get("response", "error", "message") != "Overloaded" {
		t.Fatalf("an error after output must end in response.failed: %v", last.Body)
	}
	for _, kind := range types(events) {
		if kind == "response.output_item.done" || kind == "response.completed" {
			t.Fatalf("a partial item must not be closed or completed: %v", types(events))
		}
	}
}

func TestStreamEndingWithoutMessageStopFails(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	_, events, done := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if !done || last.Body["type"] != "response.failed" || last.get("response", "error", "code") != "upstream_stream_ended" {
		t.Fatalf("a truncated stream must fail: %v", last.Body)
	}
}

const overloaded = `error|{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`

func okStream() string {
	return sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fine"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`message_stop|{"type":"message_stop"}`)
}

func TestStreamRetryBeforeAnyOutput(t *testing.T) {
	retry := `,"upstream":{"stream_retry":{"max_retries":2,"delay_ms":0}}`
	upstream, count := scriptedUpstream(t, sse(overloaded), sse(overloaded), okStream())
	adapter := adapterFor(t, upstream.URL, "")
	_ = adapter
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","stream_retry":{"max_retries":2,"delay_ms":0}}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	_ = retry
	_, events, done := postStream(t, server.URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if !done || last.Body["type"] != "response.completed" || atomic.LoadInt32(count) != 3 {
		t.Fatalf("expected success on the third attempt, got %v after %d requests", last.Body["type"], atomic.LoadInt32(count))
	}
	if events[0].Body["type"] != "response.created" || events[0].Body["sequence_number"] != float64(0) {
		t.Fatalf("a retried stream must start cleanly: %v", types(events))
	}
}

func TestStreamRetryGivesUpAndRespectsTypeList(t *testing.T) {
	config := func(url, extra string) *httptest.Server {
		cfg := mustConfig(t, `{"upstream":{"base_url":"`+url+`","stream_retry":{"max_retries":1,"delay_ms":0`+extra+`}}}`)
		server := httptest.NewServer(NewHandler(cfg, nil))
		t.Cleanup(server.Close)
		return server
	}
	upstream, count := scriptedUpstream(t, sse(overloaded))
	_, events, _ := postStream(t, config(upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if last.Body["type"] != "response.failed" || atomic.LoadInt32(count) != 2 {
		t.Fatalf("one retry then failure expected: %v after %d requests", last.Body["type"], atomic.LoadInt32(count))
	}
	other, otherCount := scriptedUpstream(t, sse(`error|{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	_, events, _ = postStream(t, config(other.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	if events[len(events)-1].Body["type"] != "response.failed" || atomic.LoadInt32(otherCount) != 1 {
		t.Fatalf("an error type off the list must not be retried (%d requests)", atomic.LoadInt32(otherCount))
	}
	custom, customCount := scriptedUpstream(t, sse(`error|{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`), okStream())
	_, events, _ = postStream(t, config(custom.URL, `,"error_types":["rate_limit_error"]`).URL, `{"model":"m","input":"x","stream":true}`)
	if events[len(events)-1].Body["type"] != "response.completed" || atomic.LoadInt32(customCount) != 2 {
		t.Fatalf("a configured error type must be retried (%d requests)", atomic.LoadInt32(customCount))
	}
}

func TestStreamRetryOffByDefaultAndNeverAfterOutput(t *testing.T) {
	upstream, count := scriptedUpstream(t, sse(overloaded), okStream())
	_, events, _ := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	if events[len(events)-1].Body["type"] != "response.failed" || atomic.LoadInt32(count) != 1 {
		t.Fatalf("retry must be off by default (%d requests)", atomic.LoadInt32(count))
	}
	afterOutput := sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`, overloaded)
	again, againCount := scriptedUpstream(t, afterOutput, okStream())
	cfg := mustConfig(t, `{"upstream":{"base_url":"`+again.URL+`","stream_retry":{"max_retries":3,"delay_ms":0}}}`)
	server := httptest.NewServer(NewHandler(cfg, nil))
	t.Cleanup(server.Close)
	_, events, _ = postStream(t, server.URL, `{"model":"m","input":"x","stream":true}`)
	if events[len(events)-1].Body["type"] != "response.failed" || atomic.LoadInt32(againCount) != 1 {
		t.Fatalf("an error after output must not be retried (%d requests)", atomic.LoadInt32(againCount))
	}
}

func TestStreamHTTPErrorStatusIsReturnedBeforeTheStream(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, http.Header{"Retry-After": {"3"}})
	adapter := adapterFor(t, upstream.URL, "")
	request, _ := http.NewRequest(http.MethodPost, adapter.URL+"/v1/responses", strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	request.Header.Set("x-api-key", "k")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 || response.Header.Get("Retry-After") != "3" || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("an HTTP error must keep its status for the caller's retry: %d %v", response.StatusCode, response.Header)
	}
}

func TestStreamUnknownEventsAndMultilineData(t *testing.T) {
	body := "event: message_start\ndata: " + strings.TrimPrefix(startEvent, "message_start|") + "\n\n" +
		": a comment\n\n" +
		"event: future_event\ndata: {\"type\":\"future_event\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\n" +
		"data: \"content_block\":{\"type\":\"server_tool_use\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lost\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream, _ := scriptedUpstream(t, body)
	_, events, done := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	got := types(events)
	if !done || got[len(got)-1] != "response.completed" {
		t.Fatalf("unknown events and block types must be skipped, got %v", got)
	}
	for _, kind := range got {
		if strings.Contains(kind, "output_text") {
			t.Fatalf("an ignored block must not produce output: %v", got)
		}
	}
}

func TestStreamInvalidEventJSONFails(t *testing.T) {
	upstream, _ := scriptedUpstream(t, "event: message_start\ndata: {not json\n\n")
	_, events, _ := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if last.Body["type"] != "response.failed" || last.get("response", "error", "code") != "upstream_invalid_event" {
		t.Fatalf("invalid event JSON: %v", last.Body)
	}
}

func TestStreamNonSSEUpstreamBodyIsAnError(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, `{"type":"error","error":{"type":"api_error","message":"oops"}}`, nil)
	adapter := adapterFor(t, upstream.URL, "")
	request, _ := http.NewRequest(http.MethodPost, adapter.URL+"/v1/responses", strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	request.Header.Set("x-api-key", "k")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 500 {
		t.Fatalf("a JSON body where a stream was expected must be an error, got %d", response.StatusCode)
	}
}

func TestReadSSEHandlesCRLFAndFinalEventWithoutBlankLine(t *testing.T) {
	var got []sseEvent
	err := readSSE(strings.NewReader("event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2"), DefaultMaxEventBytes, func(e sseEvent) bool {
		got = append(got, e)
		return true
	})
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[0].Data != "1" || got[1].Data != "2" {
		t.Fatalf("got %v err %v", got, err)
	}
}

// Two thinking blocks form one reasoning item. The text streamed as deltas must
// equal the text the non-streaming path puts in the item.
func TestStreamedReasoningTextJoinsBlocksLikeNonStreaming(t *testing.T) {
	plainUpstream, _ := fakeAnthropic(t, 200, `{"id":"msg_s1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn",
	 "content":[{"type":"thinking","thinking":"one","signature":"A"},{"type":"thinking","thinking":"two","signature":"B"},{"type":"text","text":"ok"}],
	 "usage":{"input_tokens":1,"output_tokens":1}}`, nil)
	_, _, raw := post(t, adapterFor(t, plainUpstream.URL, "").URL+"/v1/responses", `{"model":"m","input":"x"}`, map[string]string{"x-api-key": "k"})
	var plain responseObject
	if err := json.Unmarshal([]byte(raw), &plain); err != nil {
		t.Fatal(err)
	}
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one"}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"A"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`content_block_start|{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"two"}}`,
		`content_block_delta|{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"B"}}`,
		`content_block_stop|{"type":"content_block_stop","index":1}`,
		`content_block_start|{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ok"}}`,
		`content_block_stop|{"type":"content_block_stop","index":2}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`message_stop|{"type":"message_stop"}`))
	_, events, _ := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	var deltas string
	var doneText any
	for _, event := range events {
		switch event.Body["type"] {
		case "response.reasoning_text.delta":
			deltas += event.Body["delta"].(string)
		case "response.reasoning_text.done":
			doneText = event.Body["text"]
		}
	}
	item := plain.Output[0]["content"].([]any)[0].(map[string]any)["text"]
	if deltas != item || doneText != item || deltas != "one\n\ntwo" {
		t.Fatalf("deltas %q, done %v, non-streaming %v", deltas, doneText, item)
	}
}

// failingWriter accepts the headers and fails every body write, as a client
// that disconnected would.
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// countingReader reports how many bytes the translator consumed.
type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += n
	return n, err
}

// Once the client is gone, the adapter must stop reading the upstream stream
// instead of consuming it to the end.
func TestStreamStopsReadingWhenClientIsGone(t *testing.T) {
	var body strings.Builder
	body.WriteString(sse(startEvent, `content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	delta := `content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`
	for i := 0; i < 5000; i++ {
		body.WriteString(sse(delta))
	}
	total := body.Len()
	reader := &countingReader{reader: strings.NewReader(body.String())}
	sink := newSink(&failingWriter{header: http.Header{}})
	translator := newStreamTranslator(sink, DefaultConfigPointer(), "m", func() time.Time { return time.Unix(0, 0) })
	translator.run(reader)
	if sink.err == nil {
		t.Fatal("the write error was not recorded")
	}
	if reader.read >= total {
		t.Fatalf("the whole upstream stream (%d bytes) was read after the client left", total)
	}
}

// DefaultConfigPointer returns a default configuration ready for use.
func DefaultConfigPointer() *Config {
	config := DefaultConfig()
	if err := config.finish(); err != nil {
		panic(err)
	}
	return &config
}

func TestStreamCacheCreationSplit(t *testing.T) {
	start := `message_start|{"type":"message_start","message":{"id":"msg_s1","model":"claude-x","usage":{"input_tokens":3,"output_tokens":1,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":6}}}}`
	upstream, _ := scriptedUpstream(t, sse(start,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`message_stop|{"type":"message_stop"}`))
	adapter := adapterFor(t, upstream.URL, "")
	_, events, _ := postStream(t, adapter.URL, `{"model":"claude-x","input":"hi","stream":true}`)
	last := events[len(events)-1]
	for key, want := range map[string]float64{"cache_creation_tokens": 10, "cache_creation_5m_tokens": 4, "cache_creation_1h_tokens": 6} {
		if got := last.get("response", "usage", "input_tokens_details", key); got != want {
			t.Fatalf("%s = %v, want %v (%v)", key, got, want, last.Body)
		}
	}
	// a message_delta that carries the split replaces the start values
	later := `message_delta|{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":9}}}`
	upstream2, _ := scriptedUpstream(t, sse(start, later, `message_stop|{"type":"message_stop"}`))
	adapter2 := adapterFor(t, upstream2.URL, "")
	_, events2, _ := postStream(t, adapter2.URL, `{"model":"claude-x","input":"hi","stream":true}`)
	if got := events2[len(events2)-1].get("response", "usage", "input_tokens_details", "cache_creation_1h_tokens"); got != float64(9) {
		t.Fatalf("delta split ignored: %v", got)
	}
}

func TestStreamErrorEventCarriesRequestID(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`error|{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_STREAMERR"}`))
	_, events, _ := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"m","input":"x","stream":true}`)
	last := events[len(events)-1]
	if message, _ := last.get("response", "error", "message").(string); !strings.Contains(message, "req_STREAMERR") {
		t.Fatalf("request id missing from %v", last.Body)
	}
}

func scriptedUpstreamWithHeader(t *testing.T, extra http.Header, body string) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		for key, values := range extra {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &count
}

// trailerUpstream answers each request with a complete stream followed by a
// trailer after message_stop. With stall set it then holds the response open.
func trailerUpstream(t *testing.T, stall <-chan struct{}) (*httptest.Server, *int32) {
	t.Helper()
	var connections int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, okStream())
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, ": trailer\n\n")
		if stall != nil {
			w.(http.Flusher).Flush()
			<-stall
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&connections, 1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, &connections
}

func TestStreamDrainsTheTailSoTheUpstreamConnectionIsReused(t *testing.T) {
	upstream, connections := trailerUpstream(t, nil)
	adapter := adapterFor(t, upstream.URL, "")
	for round := 0; round < 3; round++ {
		status, _, done := postStream(t, adapter.URL, `{"model":"m","input":"x","stream":true}`)
		if status != 200 || !done {
			t.Fatalf("round %d: status %d done %v", round, status, done)
		}
	}
	if got := atomic.LoadInt32(connections); got != 1 {
		t.Errorf("upstream connections = %d, want 1", got)
	}
}

func TestStreamDrainIsBoundedWhenTheUpstreamStallsInTheTail(t *testing.T) {
	stall := make(chan struct{})
	upstream, _ := trailerUpstream(t, stall)
	t.Cleanup(func() { close(stall) })
	adapter := adapterFor(t, upstream.URL, "")
	start := time.Now()
	status, _, done := postStream(t, adapter.URL, `{"model":"m","input":"x","stream":true}`)
	if status != 200 || !done {
		t.Fatalf("status %d done %v", status, done)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("handler held for %v by a stalled tail", elapsed)
	}
}

func TestReadSSERefusesAnEventOverTheCap(t *testing.T) {
	long := "data: " + strings.Repeat("x", 5000) + "\n\n"
	err := readSSE(strings.NewReader(long), 1000, func(sseEvent) bool { t.Fatal("handled an oversize event"); return false })
	if err == nil || !strings.Contains(err.Error(), "max_event_bytes") {
		t.Fatalf("error = %v, want one naming upstream.max_event_bytes", err)
	}
	many := strings.Repeat("data: xxxxxxxxxx\n", 100) + "\n"
	if err := readSSE(strings.NewReader(many), 1000, func(sseEvent) bool { return true }); err == nil {
		t.Fatal("an event made of many small lines over the cap was accepted")
	}
}

func TestReadSSEAcceptsEventsJustUnderTheCapAndResetsPerEvent(t *testing.T) {
	event := "data: " + strings.Repeat("x", 90) + "\n\n"
	var got int
	err := readSSE(strings.NewReader(strings.Repeat(event, 50)), int64(len(event))+1, func(e sseEvent) bool {
		got++
		return len(e.Data) == 90
	})
	if err != nil || got != 50 {
		t.Fatalf("events = %d, err = %v; want 50 events and no error", got, err)
	}
}

func TestStreamEndsWithAClearErrorOnAnOversizeEvent(t *testing.T) {
	big := `content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 4000) + `"}}`
	upstream, _ := scriptedUpstream(t, sse(startEvent, big))
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","max_event_bytes":2000}}`)
	adapter := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(adapter.Close)
	_, events, done := postStream(t, adapter.URL, `{"model":"claude-x","input":"hi","stream":true}`)
	last := events[len(events)-1]
	if !done || last.Body["type"] != "response.failed" || last.get("response", "error", "code") != "upstream_read_failed" ||
		!strings.Contains(fmt.Sprint(last.get("response", "error", "message")), "max_event_bytes") {
		t.Fatalf("an oversize event must end in response.failed naming max_event_bytes: %v", last.Body)
	}
}

// Every item closes in the order it opened and with the next output_index, so the
// done events and the completed output agree on both order and index: text around
// two tool calls gives 0..3 in every list.
func TestStreamedOutputIndexesMatchOutputPositionsAcrossToolCalls(t *testing.T) {
	upstream, _ := scriptedUpstream(t, sse(startEvent,
		`content_block_start|{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
		`content_block_stop|{"type":"content_block_stop","index":0}`,
		`content_block_start|{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`content_block_delta|{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`content_block_stop|{"type":"content_block_stop","index":1}`,
		`content_block_start|{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`content_block_delta|{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"b"}}`,
		`content_block_stop|{"type":"content_block_stop","index":2}`,
		`content_block_start|{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_2","name":"g","input":{}}}`,
		`content_block_delta|{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`content_block_stop|{"type":"content_block_stop","index":3}`,
		`message_delta|{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		`message_stop|{"type":"message_stop"}`))
	_, events, _ := postStream(t, adapterFor(t, upstream.URL, "").URL, `{"model":"claude-x","input":"x","stream":true}`)
	var doneIndexes, doneTypes []string
	for _, one := range events {
		if one.get("type") == "response.output_item.done" {
			doneIndexes = append(doneIndexes, asJSON(t, one.get("output_index")))
			doneTypes = append(doneTypes, asJSON(t, one.get("item", "type")))
		}
	}
	if got := strings.Join(doneIndexes, ","); got != "0,1,2,3" {
		t.Fatalf("done output_index order = %s", got)
	}
	output, _ := events[len(events)-1].get("response", "output").([]any)
	if len(output) != 4 {
		t.Fatalf("completed output has %d items", len(output))
	}
	for position, raw := range output {
		if got, want := asJSON(t, raw.(map[string]any)["type"]), doneTypes[position]; got != want {
			t.Fatalf("completed output %d is %s, the done event at that index is %s", position, got, want)
		}
	}
}
