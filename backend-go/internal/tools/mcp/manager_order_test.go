package mcp

import (
	"bytes"
	"encoding/json"
	"testing"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func remoteTool(name string) *mcpSDK.Tool {
	return &mcpSDK.Tool{Name: name, Description: "does " + name, InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}}}`)}
}

// A server that lists its tools in another order the next time gives the same tools array: the tools
// of a server are sorted by name, so the provider payload (and its prompt cache) does not follow the
// server's listing order.
func TestToolsFromListSortsByNameWhateverTheServerOrder(t *testing.T) {
	manager := NewManager(nil)
	server := sessionfs.MCPServerReference{ID: "docs", Enabled: true, DenyTools: []string{"banned"}}
	current := &connection{maxDescriptionBytes: 1 << 16, maxSchemaBytes: 1 << 16}
	encode := func(listing []*mcpSDK.Tool) []byte {
		t.Helper()
		loaded, err := manager.toolsFromList(server, current, listing)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := tools.NewCatalog(loaded)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := catalog.ResponseTools()
		if err != nil {
			t.Fatal(err)
		}
		var joined []byte
		for _, element := range encoded {
			joined = append(append(joined, element...), '\n')
		}
		return joined
	}
	forward := []*mcpSDK.Tool{remoteTool("alpha"), remoteTool("banned"), remoteTool("charlie"), remoteTool("bravo")}
	reversed := []*mcpSDK.Tool{remoteTool("bravo"), remoteTool("charlie"), remoteTool("banned"), remoteTool("alpha")}
	first, second := encode(forward), encode(reversed)
	if !bytes.Equal(first, second) {
		t.Fatalf("the tools array follows the server's order:\n%s\n%s", first, second)
	}
	loaded, err := manager.toolsFromList(server, current, reversed)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tool := range loaded {
		ids = append(ids, tool.Definition.ID)
	}
	if want := []string{"mcp:docs:alpha", "mcp:docs:bravo", "mcp:docs:charlie"}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("tools = %v, want %v", ids, want)
	}
	if reversed[0].Name != "bravo" {
		t.Fatal("the caller's list was reordered")
	}
}
