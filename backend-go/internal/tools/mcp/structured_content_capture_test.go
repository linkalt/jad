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
