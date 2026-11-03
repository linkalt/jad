package tools

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
	"testing"
)

// ResponseTools caches each tool's encoded definition (audit P2). The cached bytes must equal
// a fresh encode, and a lazy tool must still appear once it is listed and leave once it is not.
func TestResponseToolsCacheMatchesAFreshEncodeAndFollowsListed(t *testing.T) {
	always := lazyTool(t, "native.read", "native", "read")
	lazy := lazyTool(t, "native.bash", "native", "bash")
	var listed atomic.Bool
	lazy.Listed = listed.Load
	catalog, err := NewCatalog([]Tool{always, lazy})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(tool Tool) json.RawMessage {
		encoded, err := json.Marshal(struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		}{"function", tool.Definition.Name, tool.Definition.Description, tool.Definition.InputSchema, false})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	check := func(when string, want ...Tool) {
		t.Helper()
		got, err := catalog.ResponseTools()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d tools, want %d", when, len(got), len(want))
		}
		for index, tool := range want {
			if !bytes.Equal(got[index], encode(tool)) {
				t.Fatalf("%s: tool %d = %s, want %s", when, index, got[index], encode(tool))
			}
		}
	}
	check("first round", always)
	check("second round (cached)", always)
	listed.Store(true)
	check("after load_tool", always, lazy)
	check("after load_tool (cached)", always, lazy)
	listed.Store(false)
	check("after unlisting", always)
}
