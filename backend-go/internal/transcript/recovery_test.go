package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverRepairsMessageAndStagingTailsAndRetainsStreamingTurn(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-partial-stage")
	if err != nil {
		t.Fatal(err)
	}
	item := `{"type":"message","role":"assistant","content":[]}`
	if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
		t.Fatal(err)
	}
	if err := turn.Close(); err != nil {
		t.Fatal(err)
	}

	itemsPath := filepath.Join(root, "staging", "turn-partial-stage", itemsFilename)
	indexPath := filepath.Join(root, "staging", "turn-partial-stage", indexFilename)
	appendRaw(t, itemsPath, `{"output_index":1,"item":{"type":"message"`)
	appendRaw(t, indexPath, `{"output_index":1,"offset":99`)
	completeLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\"}}\n")
	partialMessage := []byte(`{"type":"response.completed"`)
	if err := os.WriteFile(journal.path, append(append([]byte(nil), completeLine...), partialMessage...), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reopen against the same paths to model a process restart.
	journal, store = newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	report, err := store.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if report.RepairedMessageBytes != int64(len(partialMessage)) || report.InspectedTurns != 1 || report.RetainedTurns != 1 || report.CommittedTurns != 0 || report.RemovedTurns != 0 || len(report.Errors) != 0 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	messages, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(messages) != string(completeLine) {
		t.Fatalf("messages tail was not repaired: %s", messages)
	}
	items, err := os.ReadFile(itemsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(items), "\n") || strings.Contains(string(items), `"output_index":1`) {
		t.Fatalf("partial staged item was not discarded: %s", items)
	}
	index, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(index), "\n") || strings.Contains(string(index), `"output_index":1`) {
		t.Fatalf("partial staged index was not discarded: %s", index)
	}
}

func TestRecoverCompletesRenderedFinalAfterCommitInterruption(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turnDir := filepath.Join(root, "staging", "turn-recover-commit")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	finalLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_recover\",\"status\":\"completed\",\"output\":[]}}\n")
	hash := sha256.Sum256(finalLine)
	manifest := TurnManifest{
		SchemaVersion:       1,
		TurnID:              "turn-recover-commit",
		Status:              "committing",
		ItemCount:           0,
		NextSequence:        0,
		ResponseID:          "resp_recover",
		CanonicalLineSHA256: hex.EncodeToString(hash[:]),
	}
	writeRecoveryFixture(t, turnDir, manifest, finalLine)

	report, err := store.Recover()
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if report.InspectedTurns != 1 || report.CommittedTurns != 1 || report.RemovedTurns != 1 || report.RetainedTurns != 0 || len(report.Errors) != 0 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	messages, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(messages) != string(finalLine) {
		t.Fatalf("recovered messages.jsonl = %s, want %s", messages, finalLine)
	}
	if _, err := os.Stat(turnDir); !os.IsNotExist(err) {
		t.Fatalf("recovered staging directory remains; stat error = %v", err)
	}
}

func TestRecoverComputesMissingHashForRenderedFinal(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turnDir := filepath.Join(root, "staging", "turn-missing-hash")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	finalLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_missing_hash\",\"output\":[]}}\n")
	writeRecoveryFixture(t, turnDir, TurnManifest{
		SchemaVersion: 1,
		TurnID:        "turn-missing-hash",
		Status:        "committing",
		ItemCount:     0,
		NextSequence:  0,
	}, finalLine)

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.CommittedTurns != 1 || report.RemovedTurns != 1 || len(report.Errors) != 0 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	got, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(finalLine) {
		t.Fatalf("recovered final line = %s, want %s", got, finalLine)
	}
}

func TestRecoverDoesNotDuplicateAlreadyAppendedFinal(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	finalLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_existing\",\"status\":\"completed\",\"output\":[]}}\n")
	if err := os.WriteFile(journal.path, finalLine, 0o600); err != nil {
		t.Fatal(err)
	}
	turnDir := filepath.Join(root, "staging", "turn-after-append")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(finalLine)
	manifest := TurnManifest{
		SchemaVersion:       1,
		TurnID:              "turn-after-append",
		Status:              "committing",
		ItemCount:           0,
		NextSequence:        0,
		ResponseID:          "resp_existing",
		CanonicalLineSHA256: hex.EncodeToString(hash[:]),
	}
	writeRecoveryFixture(t, turnDir, manifest, finalLine)

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.CommittedTurns != 1 || report.RemovedTurns != 1 || len(report.Errors) != 0 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	got, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(finalLine) {
		t.Fatalf("recovery duplicated the final line: %s", got)
	}
}

func TestRecoverRetainsIncompleteFinalRecord(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turnDir := filepath.Join(root, "staging", "turn-incomplete-final")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := TurnManifest{
		SchemaVersion: 1,
		TurnID:        "turn-incomplete-final",
		Status:        "committing",
		ItemCount:     0,
		NextSequence:  0,
	}
	writeRecoveryFixture(t, turnDir, manifest, []byte(`{"type":"response.completed"}`))

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.RetainedTurns != 1 || len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "final record is incomplete") {
		t.Fatalf("incomplete final record was not retained: %+v", report)
	}
	if _, err := os.Stat(turnDir); err != nil {
		t.Fatalf("incomplete turn directory was removed: %v", err)
	}
}

func TestRecoverRemovesCommittedManifestWhenLineAlreadyExists(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	finalLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_committed\",\"output\":[]}}\n")
	if err := os.WriteFile(journal.path, finalLine, 0o600); err != nil {
		t.Fatal(err)
	}
	turnDir := filepath.Join(root, "staging", "turn-committed")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(finalLine)
	writeRecoveryFixture(t, turnDir, TurnManifest{
		SchemaVersion:       1,
		TurnID:              "turn-committed",
		Status:              "committed",
		ItemCount:           0,
		NextSequence:        0,
		CanonicalLineSHA256: hex.EncodeToString(hash[:]),
	}, nil)

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedTurns != 1 || report.CommittedTurns != 0 || report.RetainedTurns != 0 || len(report.Errors) != 0 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	if _, err := os.Stat(turnDir); !os.IsNotExist(err) {
		t.Fatalf("committed staging directory remains; stat error = %v", err)
	}
}

func TestRecoverRetainsTurnWithMismatchedFinalHash(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turnDir := filepath.Join(root, "staging", "turn-bad-hash")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	finalLine := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bad\",\"output\":[]}}\n")
	manifest := TurnManifest{
		SchemaVersion:       1,
		TurnID:              "turn-bad-hash",
		Status:              "committing",
		ItemCount:           0,
		NextSequence:        0,
		CanonicalLineSHA256: strings.Repeat("0", sha256.Size*2),
	}
	writeRecoveryFixture(t, turnDir, manifest, finalLine)

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.RetainedTurns != 1 || len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "hash does not match") {
		t.Fatalf("mismatched final hash was not reported: %+v", report)
	}
	if _, err := os.Stat(turnDir); err != nil {
		t.Fatalf("failed recovery removed turn directory: %v", err)
	}
	if _, err := os.Stat(journal.path); err != nil {
		t.Fatal(err)
	}
	messages, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("mismatched final record was committed: %s", messages)
	}
}

func TestRecoverRetainsMalformedManifestAndReportsError(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turnDir := filepath.Join(root, "staging", "turn-malformed")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, manifestFilename), []byte(`{"schema_version":1,"turn_id":"turn-malformed","status":"streaming"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.InspectedTurns != 1 || report.RetainedTurns != 1 || len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "invalid turn manifest") {
		t.Fatalf("malformed manifest was not reported: %+v", report)
	}
	if _, err := os.Stat(turnDir); err != nil {
		t.Fatalf("malformed turn directory was removed: %v", err)
	}
}

func TestRecoverRefusesWhileTurnIsActive(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-active")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(); err == nil || !strings.Contains(err.Error(), "close active turns") {
		t.Fatalf("Recover() error = %v, want active-turn guard", err)
	}
	if err := turn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(); err != nil {
		t.Fatalf("Recover() after close error = %v", err)
	}
}

func writeRecoveryFixture(t *testing.T, turnDir string, manifest TurnManifest, finalLine []byte) {
	t.Helper()
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, manifestFilename), append(manifestJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, itemsFilename), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, indexFilename), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if finalLine != nil {
		if err := os.WriteFile(filepath.Join(turnDir, finalFilename), finalLine, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func appendRaw(t *testing.T, path, fragment string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(fragment); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
