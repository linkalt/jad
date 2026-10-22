package native

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// These benchmarks record the cost of the handle stores as they are, before the async
// manager changes them: startup scan, listing many finished handles, and the time from
// start to answer.

func seedSubagentManifests(tb testing.TB, root string, count int) {
	tb.Helper()
	store := newSubagentStore(root, nil)
	started := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < count; index++ {
		err := store.write(subagentManifest{
			Version: subagentManifestVersion, ID: fmt.Sprintf("sub_%08x", index), Owner: "session\x00profile",
			Agent: "explore", Task: "task", State: subagentRunDone, Rounds: 2, Tokens: 100,
			StartedAt: started.Add(time.Duration(index) * time.Second), EndedAt: started.Add(time.Duration(index)*time.Second + time.Second),
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
}

func BenchmarkSubagentRecoverManifests(b *testing.B) {
	for _, count := range []int{1000, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root := b.TempDir()
			seedSubagentManifests(b, root, count)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: count})
				_ = manager.Close()
			}
		})
	}
}

func BenchmarkSubagentListManyFinished(b *testing.B) {
	root := b.TempDir()
	seedSubagentManifests(b, root, 1000)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 1000})
	defer manager.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(manager.list("session\x00profile")) != 1000 {
			b.Fatal("unexpected list length")
		}
	}
}

func BenchmarkProcessRecoverManifests(b *testing.B) {
	for _, count := range []int{1000, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			directory := filepath.Join(b.TempDir(), "processes")
			seedCompletedProcesses(b, directory, count)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				manager := NewProcessManager(directory)
				if err := manager.Recover(); err != nil {
					b.Fatal(err)
				}
				_ = manager.Close()
			}
		})
	}
}

func BenchmarkProcessListManyFinished(b *testing.B) {
	directory := filepath.Join(b.TempDir(), "processes")
	seedCompletedProcesses(b, directory, 1000)
	manager := NewProcessManager(directory)
	if err := manager.Recover(); err != nil {
		b.Fatal(err)
	}
	defer manager.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(manager.List("")) != 1000 {
			b.Fatal("unexpected list length")
		}
	}
}

func BenchmarkProcessStartToExit(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("managed process groups require a POSIX platform")
	}
	directory := b.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()
	request := processTestRequest(directory, "true", 1024)
	request.Policy.MaxCompleted = 1 << 20
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		started, err := manager.Start(request)
		if err != nil {
			b.Fatal(err)
		}
		if !manager.Wait(started.ID, 5000).Waited {
			b.Fatal("process did not finish")
		}
	}
}

func BenchmarkSubagentInlineRun(b *testing.B) {
	manager := NewSubagentManager()
	defer manager.Close()
	catalog := benchSubagentCatalog(b, manager, &fakeLauncher{})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchSubagentCall(b, catalog, `{"task":"x"}`)
	}
}

// BenchmarkSubagentDetachedWake measures hand-off plus the async wait that collects the
// answer once the child is done: the wake-up path of the async manager.
func BenchmarkSubagentDetachedWake(b *testing.B) {
	manager := NewSubagentManager()
	defer manager.Close()
	host, err := NewAsyncHost(filepath.Join(b.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer host.Close()
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "done", Rounds: 1}, nil
	}}
	parent := []tools.Tool{{
		Definition: tools.Definition{ID: "native:read", Source: "native", Name: "read", Description: "read"},
		ReadOnly:   true, Validator: mustBenchSchema(b),
		Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil },
	}}
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, nil, host)
	if err != nil {
		b.Fatal(err)
	}
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		b.Fatal(err)
	}
	exec := tools.ExecutionContext{CallID: "bench", WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: "s1"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		arguments := tool.NormalizeArguments(map[string]any{"task": "x", "block_ms": int64(0)})
		out, err := tool.Execute(context.Background(), arguments, exec)
		if err != nil || subagentRunIDPattern.FindString(out) == "" {
			b.Fatalf("no run id in %s (err %v)", out, err)
		}
		waited, err := asyncTool.Execute(context.Background(), map[string]any{"action": "wait", "wait_ms": int64(5000)}, exec)
		if err != nil || !strings.Contains(waited, "done") {
			b.Fatalf("wait = %s (err %v)", waited, err)
		}
	}
}

func mustBenchSchema(b *testing.B) *tools.SchemaValidator {
	b.Helper()
	validator, err := tools.CompileSchema(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		b.Fatal(err)
	}
	return validator
}

func benchSubagentCatalog(b *testing.B, manager *SubagentManager, launcher SubagentLauncher) *tools.Catalog {
	b.Helper()
	validator, err := tools.CompileSchema(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		b.Fatal(err)
	}
	parent := []tools.Tool{{
		Definition: tools.Definition{ID: "native:read", Source: "native", Name: "read", Description: "read"},
		ReadOnly:   true, Validator: validator,
		Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil },
	}}
	tool, err := NewSubagentToolWithManager(subagentSettings(agentNamed("explore")), parent, launcher, nil, manager)
	if err != nil {
		b.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		b.Fatal(err)
	}
	return catalog
}

func benchSubagentCall(b *testing.B, catalog *tools.Catalog, arguments string) string {
	b.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess"})
	if err != nil {
		b.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		b.Fatal(err)
	}
	return item.Output
}
