package mcp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

type structuredContentCapture struct {
	mu     sync.Mutex
	active *pendingToolResult
	byID   map[string]*pendingToolResult
}

type pendingToolResult struct {
	mu      sync.RWMutex
	raw     json.RawMessage
	present bool
}

func newStructuredContentCapture() *structuredContentCapture {
	return &structuredContentCapture{byID: make(map[string]*pendingToolResult)}
}

func (capture *structuredContentCapture) begin() *pendingToolResult {
	if capture == nil {
		return nil
	}
	pending := &pendingToolResult{}
	capture.mu.Lock()
	capture.active = pending
	capture.mu.Unlock()
	return pending
}

func (capture *structuredContentCapture) finish(pending *pendingToolResult) json.RawMessage {
	if capture == nil || pending == nil {
		return nil
	}
	capture.mu.Lock()
	if capture.active == pending {
		capture.active = nil
	}
	for id, active := range capture.byID {
		if active == pending {
			delete(capture.byID, id)
		}
	}
	capture.mu.Unlock()

	pending.mu.RLock()
	defer pending.mu.RUnlock()
	if !pending.present {
		return nil
	}
	return append(json.RawMessage(nil), pending.raw...)
}

func (capture *structuredContentCapture) observeWrite(message jsonrpc.Message) {
	request, ok := message.(*jsonrpc.Request)
	if !ok || request.Method != "tools/call" {
		return
	}
	id, ok := jsonRPCIDKey(request.ID)
	if !ok || capture == nil {
		return
	}
	capture.mu.Lock()
	if capture.active != nil {
		capture.byID[id] = capture.active
	}
	capture.mu.Unlock()
}

func (capture *structuredContentCapture) observeRead(message jsonrpc.Message) {
	response, ok := message.(*jsonrpc.Response)
	if !ok || capture == nil {
		return
	}
	id, ok := jsonRPCIDKey(response.ID)
	if !ok {
		return
	}
	capture.mu.Lock()
	pending := capture.byID[id]
	delete(capture.byID, id)
	capture.mu.Unlock()
	if pending == nil {
		return
	}

	raw, present := rawStructuredContent(response.Result)
	if response.Error != nil {
		raw, present = nil, false
	}
	pending.mu.Lock()
	pending.raw = append(pending.raw[:0], raw...)
	pending.present = present
	pending.mu.Unlock()
}

func rawStructuredContent(result json.RawMessage) (json.RawMessage, bool) {
	var fields struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if len(result) == 0 || json.Unmarshal(result, &fields) != nil || fields.StructuredContent == nil {
		return nil, false
	}
	return append(json.RawMessage(nil), fields.StructuredContent...), true
}

func jsonRPCIDKey(id jsonrpc.ID) (string, bool) {
	if !id.IsValid() {
		return "", false
	}
	encoded, err := json.Marshal(id.Raw())
	return string(encoded), err == nil
}

type structuredContentTransport struct {
	inner   mcpSDK.Transport
	capture *structuredContentCapture
}

func (transport *structuredContentTransport) Connect(ctx context.Context) (mcpSDK.Connection, error) {
	connection, err := transport.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &structuredContentConnection{Connection: connection, capture: transport.capture}, nil
}

func (transport *structuredContentTransport) SupportsProtocolVersion(version string) bool {
	supporter, ok := transport.inner.(mcpSDK.ProtocolVersionSupporter)
	return !ok || supporter.SupportsProtocolVersion(version)
}

type structuredContentConnection struct {
	mcpSDK.Connection
	capture *structuredContentCapture
}

func (connection *structuredContentConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	connection.capture.observeWrite(message)
	return connection.Connection.Write(ctx, message)
}

func (connection *structuredContentConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := connection.Connection.Read(ctx)
	if err == nil {
		connection.capture.observeRead(message)
	}
	return message, err
}
