package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// loadToolCatalog holds the real load_tool and these lazy tools, gated by one set: exec (native), a
// MCP write, a plan-guarded read and the leaf tool of the budget tests.
func loadToolCatalog(t *testing.T, release <-chan struct{}, executed chan<- string) *tools.Catalog {
	t.Helper()
	schema := json.RawMessage(`{"type":"object"}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ran", nil }
	lazy := []tools.Tool{
		{Definition: tools.Definition{ID: "native.exec", Source: "native", Name: "exec", Description: "Run.", InputSchema: schema}, Validator: validator, ReadOnly: true, Execute: execute},
		{Definition: tools.Definition{ID: "mcp:docs:write", Source: "mcp", Server: "docs", Name: "mcp_docs_write_0123456789", Description: "Write.", InputSchema: schema}, Validator: validator, Execute: execute},
		{Definition: tools.Definition{ID: "native.peek", Source: "native", Name: "peek", Description: "Peek.", InputSchema: schema}, Validator: validator, ReadOnly: true, PlanGuarded: true, Execute: execute},
		leafTool(t, release, executed),
	}
	set, err := tools.OpenLazySet(t.TempDir(), []string{"exec", "mcp:docs:write", "peek", "leaf"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Bind(lazy)
	loadTool, err := native.NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	registered := []tools.Tool{loadTool}
	for _, tool := range lazy {
		gated, ok := set.GateFor(tool)
		if !ok {
			t.Fatalf("the set did not gate %s", tool.Definition.Name)
		}
		registered = append(registered, gated)
	}
	catalog, err := tools.NewCatalog(registered)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func loadToolCall(id, arguments string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": id, "name": "load_tool", "arguments": arguments})
	return encoded
}

// A load_tool run is judged as the tool it runs: a run of exec takes the checkpoint by the name
// exec, a run of an MCP tool by its source under checkpoint.include_mcp_tools, and a list, a describe, a
// pattern or a plan-guarded target does not.
func TestRoundChangesWorkspaceJudgesALoadToolRunByItsTarget(t *testing.T) {
	catalog := loadToolCatalog(t, nil, nil)
	for _, includeMCP := range []bool{true, false} {
		run := checkpoint.NewRun(checkpoint.NewService(checkpoint.Options{Settings: sessionfs.CheckpointSettings{Enabled: true, SnapshotOn: "first_mutation", IncludeMCPTools: includeMCP}}), "turn", 1)
		if run == nil {
			t.Fatal("no checkpoint run")
		}
		for name, tc := range map[string]struct {
			call json.RawMessage
			want bool
		}{
			"run exec":           {loadToolCall("c1", `{"run":"exec","arguments":{}}`), true},
			"run mcp write":      {loadToolCall("c2", `{"run":"mcp:docs:write"}`), includeMCP},
			"run mcp by name":    {loadToolCall("c3", `{"run":"mcp_docs_write_0123456789"}`), includeMCP},
			"list":               {loadToolCall("c4", `{}`), false},
			"describe":           {loadToolCall("c5", `{"names":["exec"]}`), false},
			"run a pattern":      {loadToolCall("c6", `{"run":"*"}`), false},
			"run a server glob":  {loadToolCall("c7", `{"run":"mcp:docs:*"}`), false},
			"run a plain target": {loadToolCall("c8", `{"run":"peek"}`), false},
		} {
			if got := roundChangesWorkspace(catalog, []json.RawMessage{tc.call}, run); got != tc.want {
				t.Errorf("include_mcp_tools=%v, %s: roundChangesWorkspace = %v, want %v", includeMCP, name, got, tc.want)
			}
		}
		// A direct call of the lazy tool is judged by its own name as before.
		if got := roundChangesWorkspace(catalog, []json.RawMessage{call("exec", `{}`)}, run); !got {
			t.Errorf("include_mcp_tools=%v: a direct exec call must still take the checkpoint", includeMCP)
		}
	}
}

// A run of a tool a plan-mode catalog guards is not a change: the guard refuses it first.
func TestRoundChangesWorkspaceSkipsAPlanGuardedLoadToolRun(t *testing.T) {
	catalog := loadToolCatalog(t, nil, nil)
	if !catalog.IsPlanGuardedCall(loadToolCall("c1", `{"run":"peek"}`)) || catalog.IsPlanGuardedCall(loadToolCall("c2", `{"run":"exec"}`)) {
		t.Fatal("IsPlanGuardedCall must follow the target of a load_tool run")
	}
}

// A load_tool run of a leaf tool takes one token of the leaf budget for the whole call and runs the
// leaf inline: the runner itself is no leaf, so no nested wait on the budget can form, and a batch of
// such runs finishes with a budget of one.
func TestLoadToolRunOfALeafTakesOneTokenAndRunsInline(t *testing.T) {
	budget := tools.NewLeafBudget(1)
	release := make(chan struct{})
	close(release)
	executed := make(chan string, 4)
	catalog := loadToolCatalog(t, release, executed)
	runCall := func(id string) json.RawMessage { return loadToolCall(id, `{"run":"leaf"}`) }
	if !catalog.IsLeafCall(runCall("a")) || catalog.IsLeafCall(loadToolCall("l", `{}`)) {
		t.Fatal("IsLeafCall must follow the target of a run, and a list is no leaf")
	}
	runWithin(t, "load_tool leaf batch", func() {
		round, err := executeToolRound(context.Background(), catalog, []json.RawMessage{runCall("a"), runCall("b")}, tools.ExecutionContext{}, 8, "", budget, nil)
		if err != nil {
			t.Error(err)
		}
		if len(round.Completed) != 2 {
			t.Errorf("completed = %d, want 2", len(round.Completed))
		}
		for _, output := range round.Completed {
			if !strings.Contains(string(output), "done") {
				t.Errorf("output = %s, want the leaf's own result", output)
			}
		}
	})
	if len(executed) != 2 {
		t.Fatalf("executed %d leaf calls, want 2", len(executed))
	}
}

// An image a load_tool run of an MCP tool returns is resized as an MCP result: the resizer
// finds the source of the call through the run's catalog, and a run of a native tool or a list is
// not an MCP call.
func TestImageResizeSeesTheSourceOfALoadToolRunTarget(t *testing.T) {
	catalog := loadToolCatalog(t, nil, nil)
	random := rand.New(rand.NewSource(1))
	picture := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	for index := range picture.Pix {
		picture.Pix[index] = uint8(random.Intn(256))
	}
	for index := 3; index < len(picture.Pix); index += 4 {
		picture.Pix[index] = 255
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	output := func(callID string) []byte {
		item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": callID, "output": []any{map[string]any{"type": "input_image", "image_url": url, "detail": "auto"}}})
		return item
	}
	history := [][]byte{
		loadToolCall("m", `{"run":"mcp:docs:write"}`), output("m"),
		loadToolCall("n", `{"run":"exec"}`), output("n"),
		loadToolCall("l", `{}`), output("l"),
	}
	resizer := imageresize.New(imageresize.Options{MaxEdgePx: 100, MaxBytes: 1 << 20, MaxPixels: 4_000_000, JPEGQuality: 85, CacheEntries: 8}, []string{"mcp"})
	sent, stats := resizer.Apply(history, catalog.SourceOf)
	if stats.Resized != 1 || bytes.Equal(sent[1], history[1]) || !bytes.Equal(sent[3], history[3]) || !bytes.Equal(sent[5], history[5]) {
		t.Fatalf("resized %d images; the run of the MCP tool must be the only one changed", stats.Resized)
	}
}
