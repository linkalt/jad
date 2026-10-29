package mcp

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestStructuredContentCaptureCorrelatesToolCallResponseByRequestID(t *testing.T) {
	capture := newStructuredContentCapture()
	pending := capture.begin()
	capture.observeWrite(&jsonrpc.Request{ID: mustTestJSONRPCID(t, "list-1"), Method: "tools/list"})
	capture.observeRead(&jsonrpc.Response{
		ID:     mustTestJSONRPCID(t, "list-1"),
		Result: json.RawMessage(`{"tools":[]}`),
	})
	if got := capture.finish(pending); got != nil {
		t.Fatalf("captured structured content from tools/list = %s, want none", got)
	}

	pending = capture.begin()
	capture.observeWrite(&jsonrpc.Request{ID: mustTestJSONRPCID(t, float64(42)), Method: "tools/call"})
	capture.observeRead(&jsonrpc.Response{
		ID:     mustTestJSONRPCID(t, float64(41)),
		Result: json.RawMessage(`{"structuredContent":{"wrong":true}}`),
	})
	if got := capture.finish(pending); got != nil {
		t.Fatalf("captured structured content from unrelated response = %s, want none", got)
	}

	pending = capture.begin()
	capture.observeWrite(&jsonrpc.Request{ID: mustTestJSONRPCID(t, float64(43)), Method: "tools/call"})
	capture.observeRead(&jsonrpc.Response{
		ID:     mustTestJSONRPCID(t, float64(43)),
		Result: json.RawMessage(`{"content":[],"structuredContent":{"second":2,"first":1}}`),
	})
	if got, want := string(capture.finish(pending)), `{"second":2,"first":1}`; got != want {
		t.Fatalf("captured structured content = %s, want %s", got, want)
	}
}

func mustTestJSONRPCID(t *testing.T, value any) jsonrpc.ID {
	t.Helper()
	id, err := jsonrpc.MakeID(value)
	if err != nil {
		t.Fatalf("MakeID(%v): %v", value, err)
	}
	return id
}

// The SDK cannot decode a structured content number outside float64's range, so the field
// is dropped once its exact bytes are captured; every other member of the result stays.
func TestWithoutStructuredContentDropsOnlyTheCapturedField(t *testing.T) {
	response := &jsonrpc.Response{
		ID:     mustTestJSONRPCID(t, "call-1"),
		Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"structuredContent":{"m":1e400}}`),
	}
	stripped, ok := withoutStructuredContent(response).(*jsonrpc.Response)
	if !ok {
		t.Fatalf("withoutStructuredContent() did not return a response: %#v", stripped)
	}
	if want := `{"content":[{"type":"text","text":"ok"}]}`; string(stripped.Result) != want {
		t.Fatalf("stripped result = %s, want %s", stripped.Result, want)
	}

	untouched := &jsonrpc.Response{ID: mustTestJSONRPCID(t, "call-2"), Result: json.RawMessage(`{"content":[],"tools":[]}`)}
	if got := withoutStructuredContent(untouched).(*jsonrpc.Response).Result; string(got) != `{"content":[],"tools":[]}` {
		t.Fatalf("result without structured content = %s, want it unchanged", got)
	}
}
