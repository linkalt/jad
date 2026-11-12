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
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/imageresize"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func runsCall(id string, targets ...string) json.RawMessage {
	items := make([]string, len(targets))
	for index, target := range targets {
		items[index] = `{"run":"` + target + `"}`
	}
	return loadToolCall(id, `{"runs":[`+strings.Join(items, ",")+`]}`)
}

// A batch is judged target by target: one target that changes the workspace takes the checkpoint,
// and a batch of reads, plan-guarded tools or runs the runner refuses does not.
func TestRoundChangesWorkspaceJudgesEveryTargetOfALoadToolBatch(t *testing.T) {
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
			"reads only":                 {runsCall("a", "peek", "peek"), false},
			"a write after reads":        {runsCall("b", "peek", "peek", "mcp:docs:write"), includeMCP},
			"a write first":              {runsCall("c", "mcp:docs:write", "peek"), includeMCP},
			"a leaf and a read":          {runsCall("d", "leaf", "peek"), false},
			"a native tool and a read":   {runsCall("e", "exec", "peek"), false},
			"a pattern and a write":      {runsCall("f", "*", "mcp:docs:write"), includeMCP},
			"only runs that run nothing": {runsCall("g", "exec", "nothing", "*"), false},
		} {
			if got := roundChangesWorkspace(catalog, []json.RawMessage{tc.call}, run); got != tc.want {
				t.Errorf("include_mcp_tools=%v, %s: roundChangesWorkspace = %v, want %v", includeMCP, name, got, tc.want)
			}
		}
	}
	// The leaf of this fixture is not marked read-only, so it counts as a change like the write.
	if !catalog.ChangesState(runsCall("a", "peek", "mcp:docs:write")) || !catalog.ChangesState(runsCall("b", "peek", "leaf")) || catalog.ChangesState(runsCall("c", "peek", "peek")) {
		t.Error("ChangesState must follow the targets of a batch")
	}
	if catalog.IsLeafCall(runsCall("c", "leaf", "leaf")) {
		t.Error("a batch is not a leaf: the runner takes a token for each leaf item")
	}
}

// executeToolRound hands a call what a batch needs to schedule its items: the round's leaf budget, its
// scheduling rule, and the call's share of max_parallel_tool_calls.
func TestExecuteToolRoundGivesACallItsShareOfTheParallelismBound(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]tools.ExecutionContext{}
	schema := json.RawMessage(`{"type":"object"}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	record := func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
		mu.Lock()
		seen[execution.CallID] = execution
		mu.Unlock()
		return "ok", nil
	}
	catalog, err := tools.NewCatalog([]tools.Tool{
		{Definition: tools.Definition{ID: "native.reader", Source: "native", Name: "reader", InputSchema: schema}, Validator: validator, ReadOnly: true, Execute: record},
		{Definition: tools.Definition{ID: "native.writer", Source: "native", Name: "writer", InputSchema: schema}, Validator: validator, Execute: record},
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := tools.NewLeafBudget(2)
	callOf := func(id, name string) json.RawMessage {
		return json.RawMessage(`{"type":"function_call","call_id":"` + id + `","name":"` + name + `","arguments":"{}"}`)
	}
	for name, tc := range map[string]struct {
		calls      []json.RawMessage
		limit      int
		scheduling string
		want       int
		ordered    bool
	}{
		"one call has the whole bound":            {[]json.RawMessage{callOf("a", "reader")}, 8, "ordered", 8, true},
		"two reads share it":                      {[]json.RawMessage{callOf("a", "reader"), callOf("b", "reader")}, 8, "ordered", 4, true},
		"five reads share it":                     {[]json.RawMessage{callOf("a", "reader"), callOf("b", "reader"), callOf("c", "reader"), callOf("d", "reader"), callOf("e", "reader")}, 8, "ordered", 1, true},
		"a writer runs the response one by one":   {[]json.RawMessage{callOf("a", "reader"), callOf("b", "writer"), callOf("c", "reader")}, 8, "ordered", 8, true},
		"other scheduling runs them together":     {[]json.RawMessage{callOf("a", "reader"), callOf("b", "writer")}, 8, "", 4, false},
		"a limit of one keeps strict order":       {[]json.RawMessage{callOf("a", "reader")}, 1, "ordered", 1, true},
		"no limit set means one at a time":        {[]json.RawMessage{callOf("a", "reader")}, 0, "ordered", 1, true},
		"more calls than the bound cannot go low": {[]json.RawMessage{callOf("a", "reader"), callOf("b", "reader"), callOf("c", "reader")}, 2, "ordered", 1, true},
	} {
		mu.Lock()
		clear(seen)
		mu.Unlock()
		if _, err := executeToolRound(context.Background(), catalog, tc.calls, tools.ExecutionContext{}, tc.limit, tc.scheduling, budget, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, call := range tc.calls {
			var fields struct {
				CallID string `json:"call_id"`
			}
			_ = json.Unmarshal(call, &fields)
			mu.Lock()
			got, ok := seen[fields.CallID]
			mu.Unlock()
			if !ok {
				t.Fatalf("%s: call %s never ran", name, fields.CallID)
			}
			if got.NestedParallel != tc.want || got.NestedOrdered != tc.ordered || got.LeafBudget != budget {
				t.Errorf("%s: call %s got parallel %d, ordered %v, budget set %v; want %d, %v, true", name, fields.CallID, got.NestedParallel, got.NestedOrdered, got.LeafBudget == budget, tc.want, tc.ordered)
			}
		}
	}
}

// A batch of leaf items takes a token per item: with a budget of one the items run one after another, with
// a larger budget together, and the batch itself holds no token so it cannot starve its own items.
func TestLoadToolBatchOfLeavesRespectsTheLeafBudget(t *testing.T) {
	// The fixture's leaf is not read-only, so under ordered scheduling a batch of them runs one at a
	// time whatever the budget; under another scheduling the budget alone bounds them.
	for _, tc := range []struct {
		budget     int
		scheduling string
		together   int
	}{{1, "", 1}, {3, "", 3}, {3, "ordered", 1}} {
		budget := tools.NewLeafBudget(tc.budget)
		release := make(chan struct{})
		executed := make(chan string, 8)
		catalog := loadToolCatalog(t, release, executed)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			round, err := executeToolRound(context.Background(), catalog, []json.RawMessage{runsCall("batch", "leaf", "leaf", "leaf")}, tools.ExecutionContext{}, 8, tc.scheduling, budget, nil)
			if err != nil {
				t.Error(err)
			}
			if len(round.Completed) != 1 || strings.Count(string(round.Completed[0]), "done") != 3 {
				t.Errorf("budget %d/%s: round = %s, want one output with three results", tc.budget, tc.scheduling, round.Completed)
			}
		}()
		for index := 0; index < tc.together; index++ {
			select {
			case <-executed:
			case <-time.After(5 * time.Second):
				t.Fatalf("budget %d/%s: only %d leaf items started", tc.budget, tc.scheduling, index)
			}
		}
		if tc.together < 3 {
			select {
			case <-executed:
				t.Fatalf("budget %d/%s: a leaf item started beyond the budget", tc.budget, tc.scheduling)
			case <-time.After(100 * time.Millisecond):
			}
		}
		close(release)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatalf("budget %d/%s: the batch did not finish after the leaves were released", tc.budget, tc.scheduling)
		}
		if got := len(executed); got != 3-tc.together {
			t.Errorf("budget %d/%s: %d further items ran after the release, want %d", tc.budget, tc.scheduling, got, 3-tc.together)
		}
	}
}

// The image resizer finds the source of a batch through the catalog: a batch whose targets share a
// source is resized as that source, and a batch that mixes sources is left alone, so a setting
// that applies to MCP results only never changes the image of another source.
func TestImageResizeSeesTheCommonSourceOfALoadToolBatch(t *testing.T) {
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
		runsCall("same", "mcp:docs:write", "mcp:docs:write"), output("same"),
		runsCall("mixed", "mcp:docs:write", "peek"), output("mixed"),
		runsCall("none", "exec", "nothing"), output("none"),
	}
	resizer := imageresize.New(imageresize.Options{MaxEdgePx: 100, MaxBytes: 1 << 20, MaxPixels: 4_000_000, JPEGQuality: 85, CacheEntries: 8}, []string{"mcp"})
	sent, stats := resizer.Apply(history, catalog.SourceOf)
	if stats.Resized != 1 || bytes.Equal(sent[1], history[1]) || !bytes.Equal(sent[3], history[3]) || !bytes.Equal(sent[5], history[5]) {
		t.Fatalf("resized %d images; only the batch of MCP targets may change", stats.Resized)
	}
}
