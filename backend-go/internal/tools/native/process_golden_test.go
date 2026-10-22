package native

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type processGoldenManager struct {
	snapshot ProcessSnapshot
}

func (manager *processGoldenManager) Recover() error { return nil }

func (manager *processGoldenManager) Start(ProcessStartRequest) (ProcessSnapshot, error) {
	return manager.snapshot, nil
}

func (manager *processGoldenManager) SnapshotWithLogs(id string, _, _ ProcessStreamLimits, _ int, _ bool) (*ProcessSnapshot, bool) {
	if id != manager.snapshot.ID {
		return nil, false
	}
	snapshot := manager.snapshot
	return &snapshot, true
}

func (manager *processGoldenManager) List(string) []ProcessSnapshot {
	return []ProcessSnapshot{manager.snapshot}
}

func (manager *processGoldenManager) Wait(string, int64) ProcessWaitResult {
	return ProcessWaitResult{Snapshot: &manager.snapshot, Found: true, Waited: true}
}

func (manager *processGoldenManager) Stop(string, string) (*ProcessSnapshot, bool) {
	return &manager.snapshot, true
}

func (manager *processGoldenManager) Close() error { return nil }

func TestNativeProcessOutputMatchesNodeMessagesJSONLGolden(t *testing.T) {
	manager := &processGoldenManager{snapshot: ProcessSnapshot{
		ProcessManifest: ProcessManifest{
			SchemaVersion: 1,
			ID:            "proc_fixed",
			State:         ProcessExited,
			CWD:           "workspace",
			StartedAt:     "2026-10-03T08:00:00.000Z",
			EndedAt:       "2026-10-03T08:00:01.000Z",
			PID:           123,
			ExitCode:      json.RawMessage("0"),
			Signal:        json.RawMessage("null"),
		},
		Output: bashRenderedOutput{
			stdout: bashStreamOutput{producedBytes: 3, shownBytes: 3},
			stderr: bashStreamOutput{},
		},
		StdoutStoredBytes: 3,
	}}
	tool, err := NewProcessToolWithAsync(sessionfs.NativeProcessSettings{
		Stdout:       sessionfs.NativeBashStreamBudget{HeadBytes: 32, TailBytes: 32},
		Stderr:       sessionfs.NativeBashStreamBudget{HeadBytes: 32, TailBytes: 32},
		MaxLineChars: 200,
		StripANSI:    true,
		StorageDir:   t.TempDir(),
	}, manager, "PROCESS_TOOL_TEST_SECRET", nil)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"command": "true", "block_ms": json.Number("0")}, tools.ExecutionContext{
		CallID:        "call-process-status",
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: "call-process-status", Output: output})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "messages-native-process-status.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(line, want) {
		t.Fatalf("process tool messages.jsonl line differs from Node-generated golden\n got: %s\nwant: %s", line, want)
	}
}
