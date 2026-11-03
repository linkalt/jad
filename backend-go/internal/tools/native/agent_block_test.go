package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// TestAgentBlockUsesTheToolsOwnDefault: a call without block_ms waits the tool's
// default_block_ms (zero: the async default_block_ms), within max_block_ms; block_ms wins. A
// call that later calls of its response wait behind takes the async default_block_ms instead.
func TestAgentBlockUsesTheToolsOwnDefault(t *testing.T) {
	config := asyncmgr.DefaultConfig() // default_block_ms 10 s, max_block_ms 120 s
	for name, tc := range map[string]struct {
		arguments    map[string]any
		toolDefault  int64
		batchWaiting bool
		want         time.Duration
	}{
		"tool default":              {map[string]any{}, 90_000, false, 90 * time.Second},
		"zero is async default":     {map[string]any{}, 0, false, 10 * time.Second},
		"clamped to the cap":        {map[string]any{}, 600_000, false, 120 * time.Second},
		"block_ms wins":             {map[string]any{"block_ms": json.Number("250")}, 90_000, false, 250 * time.Millisecond},
		"block_ms zero is a run":    {map[string]any{"block_ms": json.Number("0")}, 90_000, false, 0},
		"held batch: async default": {map[string]any{}, 90_000, true, 10 * time.Second},
		"held batch: block_ms wins": {map[string]any{"block_ms": json.Number("90000")}, 90_000, true, 90 * time.Second},
	} {
		if got := agentBlock(config, tc.arguments, tc.toolDefault, tc.batchWaiting); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	message := &profileMessageTool{settings: sessionfs.NativeProfileMessageSettings{DefaultBlockMs: 90_000}, host: host}
	if got := message.blockFor(map[string]any{}, tools.ExecutionContext{}); got != 90*time.Second {
		t.Errorf("profile_message block = %v, want its default_block_ms", got)
	}
	if got := message.blockFor(map[string]any{}, tools.ExecutionContext{BatchWaiting: true}); got != 10*time.Second {
		t.Errorf("profile_message block with calls waiting behind it = %v, want the async default_block_ms", got)
	}
}

// TestSubagentCallWithoutBlockMsWaitsItsDefaultBlock: through the tool, a call that sends no
// block_ms returns a run id after the agent tool's default_block_ms, not the async default.
func TestSubagentCallWithoutBlockMsWaitsItsDefaultBlock(t *testing.T) {
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: filepath.Join(dir, "sub"), Keep: 10})
	defer manager.Close()
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		return SubagentResult{}, ctx.Err()
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.DefaultBlockMs = 300
	sub, err := newSubagentToolAsync(settings, withExecutors(t, readOnlyTool("read")), nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := sub.Execute(context.Background(), map[string]any{"task": "look"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: dir, SessionKey: "s1"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	id := runIDOf(t, out)
	defer host.Manager().Stop(context.Background(), "s1", id, "test_cleanup")
	if !strings.Contains(out, "still running") || elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("returned after %v (default_block_ms 300, async default 10 s):\n%s", elapsed, out)
	}
}
