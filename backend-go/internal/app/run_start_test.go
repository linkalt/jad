package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// callerValuesKey marks the calling tool call's context in the adapter test below.
type callerValuesKey struct{}

// TestEphemeralRunSpecCarriesEveryRequestField pins the R2 adapter: every field of a subagent
// request reaches the RunSpec StartRun gets, unchanged, and the template's base is the
// configuration of the session that holds the tool. A field added to SubagentRequest fails
// here until the adapter maps it.
func TestEphemeralRunSpecCarriesEveryRequestField(t *testing.T) {
	var transcriptDir string
	callerValues := context.WithValue(context.Background(), callerValuesKey{}, uint64(7))
	catalog, err := tools.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	request := native.SubagentRequest{
		Agent:        sessionfs.NativeSubagentAgent{Name: "explore", ModelPath: "/models/child.json", MaxToolRounds: 3},
		Task:         "find \"it\"",
		CallID:       "call-9",
		Tools:        catalog,
		Execution:    tools.ExecutionContext{WorkspaceRoot: "/work/tree", AgentDepth: 2, ProcessOwner: "sub_1", RootRun: "sub_0"},
		OnTranscript: func(dir string) { transcriptDir = dir },
		Seed:         "/runs/old/messages.jsonl",
		CallerValues: callerValues,
		Caller:       tools.CallerContext{Workspace: "/work", Depth: 1, ModelChoice: "fast", SandboxChoice: "tight"},
	}
	// Each request field and the RunSpec field it lands in; the walk below fails on a field
	// that is not listed.
	mapped := map[string]func(t *testing.T, spec RunSpec){
		"Agent": func(t *testing.T, spec RunSpec) {
			if !reflect.DeepEqual(spec.Template.Agent, request.Agent) {
				t.Errorf("Template.Agent = %+v", spec.Template.Agent)
			}
		},
		"Task": func(t *testing.T, spec RunSpec) {
			var task string
			if err := json.Unmarshal(spec.Input, &task); err != nil || task != request.Task {
				t.Errorf("Input = %s (%v), want the task as a JSON string", spec.Input, err)
			}
		},
		"CallID": func(t *testing.T, spec RunSpec) {
			if spec.CallID != request.CallID {
				t.Errorf("CallID = %q", spec.CallID)
			}
		},
		"Tools": func(t *testing.T, spec RunSpec) {
			if spec.Tools != catalog {
				t.Error("Tools is not the request's catalog")
			}
		},
		"Execution": func(t *testing.T, spec RunSpec) {
			if !reflect.DeepEqual(spec.Execution, request.Execution) {
				t.Errorf("Execution = %+v", spec.Execution)
			}
		},
		"OnTranscript": func(t *testing.T, spec RunSpec) {
			spec.OnTranscript("/runs/new")
			if transcriptDir != "/runs/new" {
				t.Error("OnTranscript is not the request's")
			}
		},
		"Seed": func(t *testing.T, spec RunSpec) {
			if spec.Seed != request.Seed {
				t.Errorf("Seed = %q", spec.Seed)
			}
		},
		"CallerValues": func(t *testing.T, spec RunSpec) {
			if spec.CallerValues != callerValues {
				t.Error("CallerValues is not the request's")
			}
		},
		"Caller": func(t *testing.T, spec RunSpec) {
			if !reflect.DeepEqual(spec.Caller, request.Caller) {
				t.Errorf("Caller = %+v", spec.Caller)
			}
		},
	}
	parent := sessionfs.SessionConfig{MessagesPath: "/state/messages.jsonl", MaxParallelToolCalls: 4}
	spec, err := ephemeralRunSpec(parent, request)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != RunEphemeral {
		t.Errorf("Kind = %d, want RunEphemeral", spec.Kind)
	}
	if !reflect.DeepEqual(spec.Template.Base, parent) {
		t.Errorf("Template.Base is not the session's configuration: %+v", spec.Template.Base)
	}
	fields := reflect.TypeOf(request)
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		check, ok := mapped[name]
		if !ok {
			t.Errorf("SubagentRequest.%s does not reach the RunSpec: map it in ephemeralRunSpec and list it here", name)
			continue
		}
		check(t, spec)
		delete(mapped, name)
	}
	for name := range mapped {
		t.Errorf("listed field %s is no longer in SubagentRequest", name)
	}
}

// TestStartRunRefusesAKindItDoesNotStart: in R2 StartRun starts ephemeral runs only. Any other
// kind is refused before a folder is made or the caller is told of one.
func TestStartRunRefusesAKindItDoesNotStart(t *testing.T) {
	storage := t.TempDir()
	called := false
	spec := RunSpec{
		Template:     RunTemplate{Base: sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: storage}}},
		Input:        json.RawMessage(`"task"`),
		CallID:       "call-1",
		OnTranscript: func(string) { called = true },
	}
	if _, err := (&Resolver{}).StartRun(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "run kind 0 is not started by StartRun") {
		t.Fatalf("StartRun of the zero kind = %v, want a refusal", err)
	}
	entries, err := os.ReadDir(storage)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || called {
		t.Fatalf("a refused start made %d folders (OnTranscript called: %v)", len(entries), called)
	}
}

// TestStartRunReportsAModelErrorBeforeItMakesAFolder: the template's model is loaded before the
// run's folder exists, so an agent whose model does not load leaves nothing behind, and the
// error names the agent as the launcher's did.
func TestStartRunReportsAModelErrorBeforeItMakesAFolder(t *testing.T) {
	storage := t.TempDir()
	spec := RunSpec{
		Kind: RunEphemeral,
		Template: RunTemplate{
			Base:  sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: storage}},
			Agent: sessionfs.NativeSubagentAgent{Name: "explore", ModelPath: filepath.Join(storage, "missing.json")},
		},
		Input:  json.RawMessage(`"task"`),
		CallID: "call-1",
	}
	if _, err := (&Resolver{}).StartRun(context.Background(), spec); err == nil || !strings.HasPrefix(err.Error(), "load the model of agent explore: ") {
		t.Fatalf("StartRun = %v, want the agent's model error", err)
	}
	if entries, _ := os.ReadDir(storage); len(entries) != 0 {
		t.Fatalf("a run whose model did not load made %d folders", len(entries))
	}
}

// TestStartRunOfACancelledCallKeepsItsFolderAndReportsTheCancel: a run whose context is already
// over still gets its own folder under the storage directory and names it once, and ends with
// the context's error, which the manager turns into stopped or timed out.
func TestStartRunOfACancelledCallKeepsItsFolderAndReportsTheCancel(t *testing.T) {
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	location, err := sessionfs.Resolve(loaded.Roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{"FIXTURE_KEY": "key"}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	storage := t.TempDir()
	config.NativeSubagent = &sessionfs.NativeSubagentSettings{StorageDir: storage}
	catalog, err := tools.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handle, err := resolver.StartRun(ctx, RunSpec{
		Kind:         RunEphemeral,
		Template:     RunTemplate{Base: config, Agent: sessionfs.NativeSubagentAgent{Name: "explore", MaxToolRounds: 2}},
		Input:        json.RawMessage(`"task"`),
		CallID:       "call/1",
		Tools:        catalog,
		OnTranscript: func(dir string) { dirs = append(dirs, dir) },
	})
	result := handle.Result()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartRun of a cancelled call = %+v, %v; want context.Canceled", result, err)
	}
	if result.Rounds != 0 || result.Text != "" {
		t.Errorf("a cancelled run reported work: %+v", result)
	}
	if len(dirs) != 1 || filepath.Dir(dirs[0]) != storage || !strings.HasPrefix(filepath.Base(dirs[0]), "call_1-") {
		t.Fatalf("OnTranscript got %v, want one folder call_1-<hex> under %s", dirs, storage)
	}
	if info, err := os.Stat(dirs[0]); err != nil || !info.IsDir() {
		t.Errorf("the run's folder %s does not exist: %v", dirs[0], err)
	}
}
