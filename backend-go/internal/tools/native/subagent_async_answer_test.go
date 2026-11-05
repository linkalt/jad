package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// TestSubagentBackgroundAnswerIsDeliveredWholeByWaitAndStatus: a run that ends after its call
// returned a run id delivers its answer through async wait and status as a run that ended
// within the call does, up to the async response budget. It used to be cut to
// summary_max_bytes (512) by wait and to status_summary_max_bytes (4096) by status.
func TestSubagentBackgroundAnswerIsDeliveredWholeByWaitAndStatus(t *testing.T) {
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: filepath.Join(dir, "sub"), Keep: 10})
	defer manager.Close()
	// About 4.9 KB: over both limits it used to be cut to, under the response budget.
	answer := strings.Repeat("A finding of the run, with its file and line.\n", 106) + "END-OF-ANSWER"
	release := make(chan struct{})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		select {
		case <-release:
			return SubagentResult{Text: answer}, nil
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.ResultMaxBytes = 32 * 1024
	sub, err := newSubagentToolAsync(settings, withExecutors(t, readOnlyTool("read")), nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	execution := tools.ExecutionContext{CallID: "c", WorkspaceRoot: dir, SessionKey: "s1"}
	out, err := sub.Execute(context.Background(), map[string]any{"task": "look", "block_ms": json.Number("0")}, execution)
	if err != nil {
		t.Fatal(err)
	}
	id := runIDOf(t, out)
	close(release)
	waitForState(t, host, "s1", id, asyncmgr.StateDone)

	answerOf := func(output string) string {
		t.Helper()
		for _, key := range []string{"signals: ", "entry: "} {
			start := strings.Index(output, "\n"+key)
			if start < 0 {
				continue
			}
			line := output[start+1+len(key):]
			line = line[:strings.IndexByte(line, '\n')]
			var entry asyncmgr.Entry
			if strings.HasPrefix(line, "[") {
				var entries []asyncmgr.Entry
				if err := json.Unmarshal([]byte(line), &entries); err != nil || len(entries) != 1 {
					t.Fatalf("signals %q: %v", line, err)
				}
				entry = entries[0]
			} else if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("entry %q: %v", line, err)
			}
			return entry.Summary
		}
		t.Fatalf("no entry in:\n%s", output)
		return ""
	}
	if got := answerOf(callAsync(t, async, "s1", map[string]any{"action": "status", "id": id})); got != answer {
		t.Errorf("status delivered %d of the answer's %d bytes", len(got), len(answer))
	}
	if got := answerOf(callAsync(t, async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")})); got != answer {
		t.Errorf("wait delivered %d of the answer's %d bytes", len(got), len(answer))
	}
}
