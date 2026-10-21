package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func stageItems(t *testing.T, turn *ResponseTurn, count int) string {
	t.Helper()
	items := make([]string, count)
	for index := range items {
		items[index] = fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}`, index)
		if err := turn.StageOutputItem(outputItemEvent(int64(index), items[index])); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(items, ",")
}

func completedEvent(id, output string) []byte {
	return []byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[` + output + `]}}`)
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readManifestFile(t *testing.T, path string) TurnManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest TurnManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestStrictStreamingManifestKeepsBeginCountersUntilReady(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-strict-stale")
	if err != nil {
		t.Fatal(err)
	}
	stageItems(t, turn, 3)
	manifestPath := filepath.Join(root, "staging", "turn-strict-stale", manifestFilename)
	if got := readManifestFile(t, manifestPath); got.ItemCount != 0 || got.NextSequence != 0 || got.Status != "streaming" {
		t.Fatalf("strict streaming manifest = %+v, want Begin counters", got)
	}
	if turn.manifest.ItemCount != 3 || turn.manifest.NextSequence != 3 {
		t.Fatalf("in-memory manifest = %+v, want 3 items", turn.manifest)
	}

	snapshot := filepath.Join(t.TempDir(), "staging")
	copyTree(t, filepath.Join(root, "staging"), snapshot)
	if err := turn.Close(); err != nil {
		t.Fatal(err)
	}
	journal, recovered := newTestTurnStore(t, t.TempDir(), DurabilityStrict, ResponseMetadataMinimal)
	recovered.stagingDir = snapshot
	report, err := recovered.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if report.InspectedTurns != 1 || report.RetainedTurns != 1 || len(report.Errors) != 0 {
		t.Fatalf("recovery of a stale-counter streaming turn = %+v, want retained without errors", report)
	}
	if info, err := os.Stat(journal.path); err == nil && info.Size() != 0 {
		t.Fatal("recovery appended to the transcript for a streaming turn")
	}
}

func TestNonStrictModesStillPersistTheManifestPerItem(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityProcess, DurabilityTurn} {
		root := t.TempDir()
		_, store := newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
		turn, err := store.BeginTurn("turn-counters")
		if err != nil {
			t.Fatal(err)
		}
		stageItems(t, turn, 2)
		got := readManifestFile(t, filepath.Join(root, "staging", "turn-counters", manifestFilename))
		if got.ItemCount != 2 || got.NextSequence != 2 {
			t.Fatalf("%s manifest = %+v, want 2 items", mode, got)
		}
		_ = turn.Close()
	}
}

func TestStrictCrashAfterCommitStepsRecoversLikeBefore(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityProcess, DurabilityTurn, DurabilityStrict} {
		t.Run(string(mode), func(t *testing.T) {
			root := t.TempDir()
			journal, store := newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
			turn, err := store.BeginTurn("turn-crash")
			if err != nil {
				t.Fatal(err)
			}
			output := stageItems(t, turn, 3)
			// A directory in place of messages.jsonl makes the journal append fail
			// after final.jsonl and its hash are durable: a crash before the append.
			if err := os.Mkdir(journal.path, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := turn.Commit(completedEvent("resp_crash", output)); err == nil {
				t.Fatal("commit succeeded with an unwritable transcript")
			}
			turnDir := filepath.Join(root, "staging", "turn-crash")
			manifest := readManifestFile(t, filepath.Join(turnDir, manifestFilename))
			if manifest.Status != "committing" || manifest.ItemCount != 3 || manifest.NextSequence != 3 || manifest.CanonicalLineSHA256 == "" {
				t.Fatalf("manifest at the crash point = %+v, want committing with final counters and hash", manifest)
			}
			if _, err := os.Stat(filepath.Join(turnDir, finalFilename)); err != nil {
				t.Fatal(err)
			}
			_ = turn.Close()
			if err := os.Remove(journal.path); err != nil {
				t.Fatal(err)
			}

			journal, store = newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
			report, err := store.Recover()
			if err != nil {
				t.Fatal(err)
			}
			if report.CommittedTurns != 1 || report.RemovedTurns != 1 || len(report.Errors) != 0 {
				t.Fatalf("recovery report = %+v, want one committed turn", report)
			}
			data, err := os.ReadFile(journal.path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(data, []byte("\n")) != 1 || !bytes.Contains(data, []byte(`"id":"resp_crash"`)) {
				t.Fatalf("recovered transcript = %s", data)
			}
			if _, err := os.Stat(turnDir); !os.IsNotExist(err) {
				t.Fatal("recovered turn directory was not removed")
			}
		})
	}
}

func TestStageDirtyForcesStrictCommitSync(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-dirty")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, `{"type":"message","role":"assistant","content":[]}`)); err != nil {
		t.Fatal(err)
	}
	if turn.stageDirty {
		t.Fatal("stageDirty set after a successful append")
	}
	// A directory in place of items.jsonl fails any sync attempt, so the sync
	// is observable: skipped when clean, attempted when a rollback happened.
	if err := os.Remove(turn.itemsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(turn.itemsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := turn.syncStageFiles(); err != nil {
		t.Fatalf("clean strict syncStageFiles attempted a sync: %v", err)
	}

	if err := os.Remove(turn.indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(turn.indexPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(1, `{"type":"message","role":"assistant","content":[]}`)); err == nil {
		t.Fatal("staging succeeded with an unusable staging file")
	}
	if !turn.stageDirty {
		t.Fatal("stageDirty not set after a failed append")
	}
	if err := turn.syncStageFiles(); err == nil {
		t.Fatal("dirty strict syncStageFiles skipped the sync")
	}
}

func TestTurnModeSyncFailureFailsCommitBeforeTheJournalAppend(t *testing.T) {
	for _, broken := range []string{"items", "index", "both"} {
		t.Run(broken, func(t *testing.T) {
			root := t.TempDir()
			journal, store := newTestTurnStore(t, root, DurabilityTurn, ResponseMetadataMinimal)
			turn, err := store.BeginTurn("turn-sync-fail")
			if err != nil {
				t.Fatal(err)
			}
			output := stageItems(t, turn, 2)
			replace := func(path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if broken != "index" {
				replace(turn.itemsPath)
			}
			if broken != "items" {
				replace(turn.indexPath)
			}
			_, err = turn.Commit(completedEvent("resp_sync", output))
			if err == nil || !strings.Contains(err.Error(), "sync staged response items") {
				t.Fatalf("Commit error = %v, want sync failure", err)
			}
			if info, statErr := os.Stat(journal.path); statErr == nil && info.Size() != 0 {
				t.Fatal("journal was appended after a failed staged-file sync")
			}
			if got := readManifestFile(t, turn.manifestPath); got.Status != "ready" {
				t.Fatalf("manifest status = %q, want ready (no committing step)", got.Status)
			}
		})
	}
}

func TestConcurrentTurnsCommitUnderRace(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityTurn, DurabilityStrict} {
		root := t.TempDir()
		journal, store := newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				turn, err := store.BeginTurn(fmt.Sprintf("turn-%d", worker))
				if err != nil {
					errs <- err
					return
				}
				items := make([]string, 12)
				for index := range items {
					items[index] = fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"w%d i%d"}]}`, worker, index)
					if err := turn.StageOutputItem(outputItemEvent(int64(index), items[index])); err != nil {
						errs <- err
						return
					}
				}
				if _, err := turn.Commit(completedEvent(fmt.Sprintf("resp_%d", worker), strings.Join(items, ","))); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		data, err := os.ReadFile(journal.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(data, []byte("\n")); got != 8 {
			t.Fatalf("%s: transcript has %d lines, want 8", mode, got)
		}
	}
}

func TestJournalAppendAtRecoversFromEveryTornPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	first := `{"type":"a","n":1}`
	second := `{"type":"b","n":2}`
	if err := journal.Append([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(second)); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := first + "\n" + second + "\n"; string(full) != want {
		t.Fatalf("journal bytes = %q, want %q", full, want)
	}
	for cut := len(first) + 1; cut < len(full); cut++ {
		if err := os.WriteFile(path, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		removed, err := journal.RepairTail()
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if wantRemoved := int64(cut - (len(first) + 1)); removed != wantRemoved {
			t.Fatalf("cut %d: removed %d, want %d", cut, removed, wantRemoved)
		}
		if err := journal.Append([]byte(second)); err != nil {
			t.Fatalf("cut %d: append after repair: %v", cut, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != first+"\n"+second+"\n" {
			t.Fatalf("cut %d: journal = %q", cut, got)
		}
	}
}

func TestAtomicWritersLeaveNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "manifest.json")
	if err := writeJSONAtomic(target, []byte("{}\n"), false); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries after success, want only the target", len(entries))
	}

	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(blocked, []byte("{}\n"), false); err == nil {
		t.Fatal("rename onto a non-empty directory succeeded")
	}
	entries, _ = os.ReadDir(dir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file %s left after a failed rename", entry.Name())
		}
	}

	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-final-tmp")
	if err != nil {
		t.Fatal(err)
	}
	output := stageItems(t, turn, 1)
	if _, err := turn.Commit(completedEvent("resp_tmp", output)); err != nil {
		t.Fatal(err)
	}
}
