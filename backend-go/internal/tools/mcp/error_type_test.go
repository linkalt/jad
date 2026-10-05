package mcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The failure name is the only detail the model sees, and the TypeScript client
// reports its JavaScript error class: McpError for protocol-level failures and
// Error for the rest. Go type names such as wrapError or PathError must never
// reach the model.
func TestSafeErrorTypeNamesFailuresLikeTheTypeScriptClient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"json-rpc error response", fmt.Errorf("calling %q: %w", "tools/call", &jsonrpc.Error{Code: -32602, Message: "bad"}), "McpError"},
		{"closed connection", fmt.Errorf("calling tools/call: %w", mcpSDK.ErrConnectionClosed), "McpError"},
		{"request timeout", fmt.Errorf("calling tools/call: %w", context.DeadlineExceeded), "McpError"},
		{"missing executable", &fs.PathError{Op: "fork/exec", Path: "/nonexistent", Err: fs.ErrNotExist}, "Error"},
		{"anonymous error", errors.New("boom"), "Error"},
		{"nil", nil, "Error"},
	}
	for _, test := range cases {
		if got := safeErrorType(test.err); got != test.want {
			t.Errorf("%s: safeErrorType = %q, want %q", test.name, got, test.want)
		}
	}
}
