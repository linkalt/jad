package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

func TestResolverSharesOneSubagentManagerPerStorageDirAndClosesIt(t *testing.T) {
	workspace := t.TempDir()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)

	configFor := func(dir string) sessionfs.SessionConfig {
		return sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: filepath.Join(workspace, dir)}}
	}
	first := resolver.sharedSubagentManager(configFor("kids"))
	if first == nil || first != resolver.sharedSubagentManager(configFor("kids/")) {
		t.Fatal("the resolver did not reuse the manager for the same storage directory")
	}
	if other := resolver.sharedSubagentManager(configFor("other")); other == nil || other == first {
		t.Fatal("a different storage directory shared the manager")
	}
	if resolver.sharedSubagentManager(sessionfs.SessionConfig{}) != nil {
		t.Fatal("a session without the subagent tool got a manager")
	}

	// A child started on the published manager is cancelled when the resolver closes.
	var launches atomic.Int64
	var cancelled atomic.Int64
	launcher := blockingLauncher(&launches, &cancelled)
	tool, err := native.NewSubagentToolWithManager(subagentLifecycleSettings(filepath.Join(workspace, "kids")), subagentLifecycleParent(t), launcher, nil, first)
	if err != nil {
		t.Fatal(err)
	}
	// Without an async host a run has no handle, so its call waits for the run to end: the
	// call runs in the background and Close is what ends the child.
	go subagentOutput(t, tool)
	waitFor(t, "the child to start", func() bool { return launches.Load() == 1 })

	resolver.Close()
	waitFor(t, "the child to be cancelled by Close", func() bool { return cancelled.Load() == 1 })

	// A start through the closed manager is refused: the launcher is not called again.
	if out := subagentOutput(t, tool); !strings.Contains(out, `"status":"error"`) {
		t.Fatalf("a start through the closed manager was not refused:\n%s", out)
	}
	if launches.Load() != 1 {
		t.Fatalf("the launcher ran %d times, want 1: a start after Close reached it", launches.Load())
	}
	if resolver.sharedSubagentManager(configFor("late")) != nil {
		t.Fatal("a closed resolver created a manager")
	}
}

// blockingLauncher runs a child until its context ends, counting the starts and the
// children that were cancelled.
func blockingLauncher(launches, cancelled *atomic.Int64) native.SubagentLauncher {
	return subagentLauncherFunc(func(ctx context.Context, _ native.SubagentRequest) (native.SubagentResult, error) {
		launches.Add(1)
		<-ctx.Done()
		cancelled.Add(1)
		return native.SubagentResult{}, ctx.Err()
	})
}

type subagentLauncherFunc func(context.Context, native.SubagentRequest) (native.SubagentResult, error)

func (f subagentLauncherFunc) Run(ctx context.Context, request native.SubagentRequest) (native.SubagentResult, error) {
	return f(ctx, request)
}

// subagentLifecycleSettings is one inline read-only agent that persists nothing, stored under
// storage.
func subagentLifecycleSettings(storage string) sessionfs.NativeSubagentSettings {
	return sessionfs.NativeSubagentSettings{
		ResultMaxBytes: 1024, MaxConcurrent: 2, KeepFinished: 10, MaxQueued: 4, StorageDir: storage,
		// The timeout is far past the test, so only Close can cancel the child.
		Agents: []sessionfs.NativeSubagentAgent{{Name: "explore", MaxToolRounds: 5, TimeoutMS: 60_000}},
	}
}

// subagentLifecycleParent is the one read-only tool the child may use.
func subagentLifecycleParent(t *testing.T) []tools.Tool {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	read := tools.Tool{Definition: tools.Definition{ID: "native.read", Source: "native", Name: "read", Description: "read"}, ReadOnly: true, Validator: validator}
	read.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil }
	return []tools.Tool{read}
}

// subagentOutput starts one run through tool with a task and returns the tool result text.
func subagentOutput(t *testing.T, tool tools.Tool) string {
	t.Helper()
	output, _ := tool.Execute(context.Background(), map[string]any{"task": "x", "block_ms": json.Number("0")}, tools.ExecutionContext{CallID: "call", SessionKey: "session"})
	return output
}

// waitFor polls condition for up to three seconds and fails the test when it never holds.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// seedFinishedRuns writes three finished runs, oldest first, each with a transcript
// folder directly under storage, as an earlier server would have left them.
func seedFinishedRuns(t *testing.T, storage string) []string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(storage, ".runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	var folders []string
	base := time.Now().Add(-time.Hour)
	for index, id := range []string{"sub_00000001", "sub_00000002", "sub_00000003"} {
		folder := filepath.Join(storage, "call-"+id)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "messages.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, folder)
		ended := base.Add(time.Duration(index) * time.Minute)
		manifest, _ := json.Marshal(map[string]any{
			"version": 1, "id": id, "agent": "a", "task": "t", "state": "done",
			"started_at": ended.Add(-time.Second), "ended_at": ended, "dir": folder,
		})
		if err := os.WriteFile(filepath.Join(storage, ".runs", id+".json"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return folders
}

func TestResolverPassesDeleteTranscriptsToTheManager(t *testing.T) {
	for _, deleteTranscripts := range []bool{false, true} {
		workspace := t.TempDir()
		resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
		if err != nil {
			t.Fatal(err)
		}
		storage := filepath.Join(workspace, "kids")
		folders := seedFinishedRuns(t, storage)
		manager := resolver.sharedSubagentManager(sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{
			StorageDir: storage, PersistRuns: true, KeepFinished: 1, DeleteTranscripts: deleteTranscripts,
		}})
		if manager == nil {
			t.Fatal("no manager")
		}
		for index, folder := range folders {
			_, err := os.Stat(folder)
			kept := err == nil
			wantKept := !deleteTranscripts || index == 2
			if kept != wantKept {
				t.Errorf("delete_transcripts=%v: folder %d kept = %v, want %v", deleteTranscripts, index, kept, wantKept)
			}
		}
		resolver.Close()
	}
}
