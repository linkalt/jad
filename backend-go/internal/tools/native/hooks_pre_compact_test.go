package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestPreCompactEventCarriesTheTriggerAndUsesItsOwnList(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload.json")
	other := filepath.Join(dir, "other")
	hooks := sessionfs.HooksSettings{
		Enabled: true, TimeoutMS: 10_000, MaxOutputBytes: 4096, MaxPayloadBytes: 4096, KillGraceMs: 100, KillProcessGroup: "always", MaxLineChars: 2000,
		PreCompact: []sessionfs.RunEventHook{{Name: "before", Argv: []string{"sh", "-c", "cat > " + payload + "; echo PRE-COMPACT-NOTE"}}},
		RunStart:   []sessionfs.RunEventHook{{Name: "start", Argv: []string{"sh", "-c", "touch " + other}}},
		RunEnd:     []sessionfs.RunEventHook{{Name: "end", Argv: []string{"sh", "-c", "touch " + other}}},
	}
	reports := FireRunEvent(context.Background(), hooks, "", RunEvent{
		Event: HookEventPreCompact, Session: "local:one", Profile: "dev/coder", TurnID: "t1", WorkspaceRoot: dir, Trigger: CompactTriggerAutomatic,
	})
	if len(reports) != 1 || !strings.Contains(reports[0], "[before] passed") || !strings.Contains(reports[0], "PRE-COMPACT-NOTE") {
		t.Fatalf("reports = %q", reports)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a run_start or run_end command ran for pre_compact")
	}
	raw, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil {
		t.Fatalf("payload = %q", raw)
	}
	for key, want := range map[string]string{"event": "pre_compact", "trigger": "automatic", "session": "local:one", "profile": "dev/coder", "turn_id": "t1", "workspace_root": dir} {
		if got[key] != want {
			t.Errorf("payload %s = %v, want %s", key, got[key], want)
		}
	}
	if _, has := got["outcome"]; has {
		t.Errorf("a pre_compact payload must not carry an outcome: %s", raw)
	}
}

func TestPreCompactEventStartsNothingWithoutItsList(t *testing.T) {
	hooks := sessionfs.HooksSettings{
		Enabled: true, TimeoutMS: 10_000, MaxOutputBytes: 4096, MaxPayloadBytes: 4096,
		RunStart: []sessionfs.RunEventHook{{Name: "start", Argv: []string{"false"}}},
	}
	if reports := FireRunEvent(context.Background(), hooks, "", RunEvent{Event: HookEventPreCompact, WorkspaceRoot: t.TempDir()}); len(reports) != 0 {
		t.Fatalf("reports = %q, want none", reports)
	}
}
