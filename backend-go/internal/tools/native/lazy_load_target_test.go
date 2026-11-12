package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/tools"
)

// A run executes the tool the lazy set names, and the catalog judges (state, files, plan mode, leaf,
// checkpoint) the tool its own lookup finds for the same target. These tests make the two lookups
// disagree and check that the run is refused before anything executes, for a single run and for a
// batch, and that the valid forms (provider name, mcp:<server>:<tool>) still run.

type targetFixture struct {
	catalog *tools.Catalog
	ran     map[string]*atomic.Int32
}

func (f targetFixture) executions(id string) int { return int(f.ran[id].Load()) }

func (f targetFixture) totalExecutions() (total int) {
	for _, counter := range f.ran {
		total += int(counter.Load())
	}
	return total
}

func lazyToolOf(source, id, name string) tools.Tool {
	return tools.Tool{Definition: tools.Definition{ID: id, Source: source, Server: "docs", Name: name, Description: name + ".", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}

// newTargetFixture binds lazy in a set, so the set resolves run targets to them. The catalog holds
// load_tool, the gated lazy tools when gated is true, and extra as they are. Every tool counts its
// executions by ID.
func newTargetFixture(t *testing.T, lazy, extra []tools.Tool, gated bool, entries ...string) targetFixture {
	t.Helper()
	f := targetFixture{ran: map[string]*atomic.Int32{}}
	count := func(tool tools.Tool) tools.Tool {
		counter := &atomic.Int32{}
		f.ran[tool.Definition.ID] = counter
		id := tool.Definition.ID
		tool.Validator = mustSchema(t, `{"type":"object"}`)
		tool.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			counter.Add(1)
			return "ran " + id, nil
		}
		return tool
	}
	for index := range lazy {
		lazy[index] = count(lazy[index])
	}
	for index := range extra {
		extra[index] = count(extra[index])
	}
	set := tools.NewLazySet(entries, 256, nil)
	set.Bind(lazy)
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	registered := []tools.Tool{loadTool}
	if gated {
		for _, tool := range lazy {
			gate, ok := set.Gate(tool)
			if !ok {
				t.Fatalf("the set did not gate %s", tool.Definition.Name)
			}
			registered = append(registered, gate)
		}
	}
	if f.catalog, err = tools.NewCatalog(append(registered, extra...)); err != nil {
		t.Fatal(err)
	}
	return f
}

func runLazyOne(t *testing.T, f targetFixture, target string) string {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"run": target})
	_, output := callEnvelope(t, f.catalog, "load_tool", string(arguments), tools.ExecutionContext{})
	return outputText(t, output)
}

func runLazyBatch(t *testing.T, f targetFixture, targets ...string) string {
	t.Helper()
	runs := make([]map[string]string, len(targets))
	for index, target := range targets {
		runs[index] = map[string]string{"run": target}
	}
	arguments, _ := json.Marshal(map[string]any{"runs": runs})
	_, output := callEnvelope(t, f.catalog, "load_tool", string(arguments), tools.ExecutionContext{})
	return outputText(t, output)
}

func TestLazyTargetMismatchIsRefusedForARunAndForABatch(t *testing.T) {
	for name, build := range map[string]func() (targetFixture, string){
		// The set matches the MCP tool by its ID, but the catalog's lookup tries provider names first
		// and finds a native tool of that name, which it would judge as the read-only runner.
		"the ID of a lazy tool is the name of a native tool": func() (targetFixture, string) {
			return newTargetFixture(t, []tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "mcp_docs_lookup_01")}, []tools.Tool{lazyToolOf("native", "native.shadow", "mcp:docs:lookup")}, true, "mcp:docs:lookup"), "mcp:docs:lookup"
		},
		// The set holds one extension tool named peek; the catalog's peek is another lazy-capable tool.
		"the same name is another lazy tool in the catalog": func() (targetFixture, string) {
			return newTargetFixture(t, []tools.Tool{lazyToolOf("extension", "extension.peek", "peek")}, []tools.Tool{lazyToolOf("extension", "extension.other", "peek")}, false, "peek"), "peek"
		},
		// Same ID and name, another source: the catalog's tool is an MCP tool, the set's an extension.
		"the same ID and name come from another source": func() (targetFixture, string) {
			return newTargetFixture(t, []tools.Tool{lazyToolOf("extension", "x.peek", "peek")}, []tools.Tool{lazyToolOf("mcp", "x.peek", "peek")}, false, "peek"), "peek"
		},
		// The set and the catalog agree on the ID, but the catalog's tool with it has another name.
		"the same ID has another name in the catalog": func() (targetFixture, string) {
			return newTargetFixture(t, []tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "name_a")},
				[]tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "name_b"), lazyToolOf("extension", "extension.a", "name_a")}, false, "mcp:docs:lookup"), "mcp:docs:lookup"
		},
		// The catalog's tool of that name cannot be lazy, so its judgments take the run for the runner.
		"the same name is a native tool in the catalog": func() (targetFixture, string) {
			return newTargetFixture(t, []tools.Tool{lazyToolOf("extension", "extension.peek", "peek")}, []tools.Tool{lazyToolOf("native", "native.peek", "peek")}, false, "peek"), "peek"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, target := build()
			single := runLazyOne(t, f, target)
			if !strings.Contains(single, "lazy_target_mismatch") || !strings.Contains(single, target) {
				t.Fatalf("a run = %s, want lazy_target_mismatch naming %s", single, target)
			}
			batch := runLazyBatch(t, f, target, target)
			if strings.Count(batch, "lazy_target_mismatch") != 2 || strings.Contains(batch, "ran ") {
				t.Fatalf("a batch = %s, want both items refused with lazy_target_mismatch", batch)
			}
			if got := f.totalExecutions(); got != 0 {
				t.Fatalf("%d executions after refused runs, want none", got)
			}
		})
	}
}

// A refused item does not stop the others of its batch, and the others run once.
func TestLazyTargetMismatchOfOneItemLeavesTheOthersRunning(t *testing.T) {
	f := newTargetFixture(t,
		[]tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "mcp_docs_lookup_01"), lazyToolOf("extension", "extension.peek", "peek")},
		[]tools.Tool{lazyToolOf("native", "native.shadow", "mcp:docs:lookup")}, true, "mcp:docs:lookup", "peek")
	got := runLazyBatch(t, f, "peek", "mcp:docs:lookup", "peek")
	if f.executions("extension.peek") != 2 || f.executions("mcp:docs:lookup") != 0 || f.executions("native.shadow") != 0 {
		t.Fatalf("executions peek=%d lookup=%d shadow=%d, want 2, 0, 0:\n%s", f.executions("extension.peek"), f.executions("mcp:docs:lookup"), f.executions("native.shadow"), got)
	}
	sections := strings.Split(got, "\n\n--- run ")
	if len(sections) != 3 || !strings.Contains(sections[0], "run 1 of 3: peek") || !strings.Contains(sections[1], "2 of 3: mcp:docs:lookup") || !strings.Contains(sections[1], "lazy_target_mismatch") || !strings.Contains(sections[2], "ran extension.peek") {
		t.Fatalf("batch output out of order or wrong:\n%s", got)
	}
}

// Both forms a run may take, the provider name and mcp:<server>:<tool>, run when the lookups agree.
func TestLazyTargetAgreementRunsByNameAndByMCPID(t *testing.T) {
	newFixture := func() targetFixture {
		return newTargetFixture(t,
			[]tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "mcp_docs_lookup_01"), lazyToolOf("extension", "extension.peek", "peek")},
			nil, true, "mcp:docs:lookup", "peek")
	}
	f := newFixture()
	for _, target := range []string{"mcp_docs_lookup_01", "mcp:docs:lookup", "peek"} {
		if got := runLazyOne(t, f, target); !strings.HasPrefix(got, "ran ") {
			t.Errorf("run %s = %s, want it to run", target, got)
		}
	}
	if f.executions("mcp:docs:lookup") != 2 || f.executions("extension.peek") != 1 {
		t.Fatalf("executions lookup=%d peek=%d, want 2 and 1", f.executions("mcp:docs:lookup"), f.executions("extension.peek"))
	}
	g := newFixture()
	got := runLazyBatch(t, g, "mcp_docs_lookup_01", "mcp:docs:lookup", "peek")
	if strings.Contains(got, "lazy_target_mismatch") || strings.Count(got, "ran ") != 3 || g.executions("mcp:docs:lookup") != 2 || g.executions("extension.peek") != 1 {
		t.Fatalf("a batch of both forms = %s", got)
	}
}

// LazyTarget is the tool the judgments take a target for: what rewriting a run for the judgments does.
func TestCatalogLazyTargetIsWhatTheJudgmentsResolve(t *testing.T) {
	f := newTargetFixture(t,
		[]tools.Tool{lazyToolOf("mcp", "mcp:docs:lookup", "mcp_docs_lookup_01"), lazyToolOf("extension", "extension.peek", "peek")},
		[]tools.Tool{lazyToolOf("native", "native.read", "read")}, true, "mcp:docs:lookup", "peek")
	for target, want := range map[string]string{"mcp_docs_lookup_01": "mcp:docs:lookup", "mcp:docs:lookup": "mcp:docs:lookup", "peek": "extension.peek"} {
		definition, ok := f.catalog.LazyTarget(target)
		if !ok || definition.ID != want {
			t.Errorf("LazyTarget(%s) = %s %v, want %s", target, definition.ID, ok, want)
		}
		// The judgments rewrite the run to the same tool.
		call := `{"type":"function_call","call_id":"c","name":"load_tool","arguments":"{\"run\":\"` + target + `\"}"}`
		var resolved struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(f.catalog.ResolveCall(json.RawMessage(call)), &resolved); err != nil || resolved.Name != definition.Name {
			t.Errorf("ResolveCall(%s) names %q, want %q", target, resolved.Name, definition.Name)
		}
	}
	for _, target := range []string{"read", "native.read", "load_tool", "nothing", "*", ""} {
		if definition, ok := f.catalog.LazyTarget(target); ok {
			t.Errorf("LazyTarget(%s) = %s, want none: a native tool, a runner and an unknown name are not lazy targets", target, definition.ID)
		}
	}
	var none *tools.Catalog
	if _, ok := none.LazyTarget("peek"); ok {
		t.Error("a nil catalog resolved a target")
	}
}

// A dispatcher that does not judge runs (no LazyTarget) is not checked: it has no judgments to
// disagree with, and a batch on it runs one item at a time.
type plainDispatcher struct{ tools.Dispatcher }

func TestLazyTargetIsNotCheckedWithoutACatalogDispatcher(t *testing.T) {
	f := newTargetFixture(t, []tools.Tool{lazyToolOf("extension", "extension.peek", "peek")}, nil, true, "peek")
	set := tools.NewLazySet([]string{"peek"}, 256, nil)
	peek := lazyToolOf("extension", "extension.peek", "peek")
	set.Bind([]tools.Tool{peek})
	execution := tools.ExecutionContext{CallID: "outer-1", Dispatcher: plainDispatcher{f.catalog}}
	name, refusal := resolveLazyTarget(set, "peek", execution)
	if refusal != "" || name != "peek" {
		t.Fatalf("resolveLazyTarget on a plain dispatcher = %q, %q", name, refusal)
	}
	if _, judged := execution.Dispatcher.(lazyTargetJudge); judged {
		t.Fatal("the plain dispatcher judges runs; the test does not test the unchecked path")
	}
	// The same target on the catalog itself is checked and agrees.
	execution.Dispatcher = f.catalog
	if name, refusal = resolveLazyTarget(set, "peek", execution); refusal != "" || name != "peek" {
		t.Fatalf("resolveLazyTarget on the catalog = %q, %q", name, refusal)
	}
}
