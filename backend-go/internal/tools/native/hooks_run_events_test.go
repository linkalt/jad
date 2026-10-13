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

func runHook(name, script string) sessionfs.RunEventHook {
	return sessionfs.RunEventHook{Name: name, Argv: []string{"sh", "-c", script}}
}

func runEventSettings(start, end []sessionfs.RunEventHook) sessionfs.HooksSettings {
	settings := hookTestSettings()
	settings.RunStart, settings.RunEnd = start, end
	settings.MaxPayloadBytes = 4096
	return settings
}

func TestFireRunEventSendsThePayloadAndPicksTheListByEvent(t *testing.T) {
	root := t.TempDir()
	startFile, endFile := filepath.Join(root, "start.json"), filepath.Join(root, "end.json")
	settings := runEventSettings(
		[]sessionfs.RunEventHook{runHook("begin", "cat > "+startFile)},
		[]sessionfs.RunEventHook{runHook("finish", "cat > "+endFile)},
	)

	reports := FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventRunStart, Session: "local:one", Profile: "default", TurnID: "turn-1", WorkspaceRoot: root})
	if len(reports) != 1 || !strings.Contains(reports[0], "[begin] passed") {
		t.Fatalf("run_start reports = %q", reports)
	}
	if _, err := os.Stat(endFile); err == nil {
		t.Fatal("a run_start event ran the run_end command")
	}
	var start RunEvent
	data, err := os.ReadFile(startFile)
	if err != nil || json.Unmarshal(data, &start) != nil {
		t.Fatalf("run_start payload %q, %v", data, err)
	}
	if start.Event != "run_start" || start.Session != "local:one" || start.Profile != "default" || start.TurnID != "turn-1" || start.WorkspaceRoot != root || start.Outcome != "" {
		t.Fatalf("run_start payload = %+v", start)
	}

	reports = FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventRunEnd, Session: "local:one", Profile: "default", TurnID: "turn-1", WorkspaceRoot: root, Outcome: RunOutcomeError, Error: "provider said no"})
	if len(reports) != 1 || !strings.Contains(reports[0], "[finish] passed") {
		t.Fatalf("run_end reports = %q", reports)
	}
	var end RunEvent
	data, err = os.ReadFile(endFile)
	if err != nil || json.Unmarshal(data, &end) != nil {
		t.Fatalf("run_end payload %q, %v", data, err)
	}
	if end.Event != "run_end" || end.Outcome != "error" || end.Error != "provider said no" {
		t.Fatalf("run_end payload = %+v", end)
	}
}

func TestFireRunEventStartsNothingWhenDisabledOrEmpty(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	hooks := []sessionfs.RunEventHook{runHook("touch", "touch "+marker)}

	disabled := runEventSettings(hooks, hooks)
	disabled.Enabled = false
	if reports := FireRunEvent(context.Background(), disabled, "", RunEvent{Event: HookEventRunStart, WorkspaceRoot: root}); reports != nil {
		t.Fatalf("disabled hooks returned %q", reports)
	}
	onlyEnd := runEventSettings(nil, hooks)
	if reports := FireRunEvent(context.Background(), onlyEnd, "", RunEvent{Event: HookEventRunStart, WorkspaceRoot: root}); reports != nil {
		t.Fatalf("an empty run_start list returned %q", reports)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command started although hooks were disabled or the list was empty")
	}
}

func TestFireRunEventReportsFailuresAndHonoursOnFailureOnly(t *testing.T) {
	root := t.TempDir()
	quiet := runHook("quiet", "exit 0")
	quiet.OnFailureOnly = true
	settings := runEventSettings([]sessionfs.RunEventHook{
		runHook("bad", "echo broken; exit 4"),
		quiet,
		runHook("after", "echo still ran"),
	}, nil)

	reports := FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventRunStart, WorkspaceRoot: root})
	if len(reports) != 2 {
		t.Fatalf("reports = %q, want the failing and the last command only", reports)
	}
	if !strings.Contains(reports[0], "[bad] failed, exit 4") || !strings.Contains(reports[0], "broken") {
		t.Errorf("failing report = %q", reports[0])
	}
	if !strings.Contains(reports[1], "[after] passed") || !strings.Contains(reports[1], "still ran") {
		t.Errorf("last report = %q; a failing command must not stop the next one", reports[1])
	}
}

func TestFireRunEventCutsALongErrorWithinThePayloadBudget(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "end.json")
	settings := runEventSettings(nil, []sessionfs.RunEventHook{runHook("finish", "cat > "+out)})
	FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventRunEnd, WorkspaceRoot: root, Outcome: RunOutcomeError, Error: strings.Repeat("x", 100_000)})
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var end RunEvent
	if err := json.Unmarshal(data, &end); err != nil {
		t.Fatal(err)
	}
	if !end.ErrorCut || len(end.Error) != int(settings.MaxPayloadBytes/2) {
		t.Fatalf("error_cut=%v error length=%d, want a cut to %d", end.ErrorCut, len(end.Error), settings.MaxPayloadBytes/2)
	}
}
