package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type dirSyncRecorder struct {
	mu    sync.Mutex
	paths []string
	fail  string
}

func (r *dirSyncRecorder) hook(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	if r.fail != "" && path == r.fail {
		return errors.New("injected directory sync failure")
	}
	return nil
}

func (r *dirSyncRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.paths
	r.paths = nil
	return out
}

func recordedStore(t *testing.T, mode DurabilityMode) (*Journal, *TurnStore, *dirSyncRecorder, string) {
	t.Helper()
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, mode, ResponseMetadataMinimal)
	recorder := &dirSyncRecorder{}
	journal.dirSyncHook = recorder.hook
	return journal, store, recorder, root
}

func TestDirectorySyncPerStep(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityTurn, DurabilityStrict} {
		t.Run(string(mode), func(t *testing.T) {
			journal, store, recorder, _ := recordedStore(t, mode)
			turn, err := store.BeginTurn("turn-dir")
			if err != nil {
				t.Fatal(err)
			}
			turnDir, stagingDir := turn.turnDir, store.stagingDir
			begin := recorder.take()
			if mode == DurabilityStrict {
				if len(begin) != 2 || begin[0] != turnDir || begin[1] != stagingDir {
					t.Fatalf("begin syncs = %v, want turn dir then staging dir", begin)
				}
			} else if len(begin) != 0 {
				t.Fatalf("turn mode begin synced directories: %v", begin)
			}
			output := stageItems(t, turn, 8)
			items := recorder.take()
			if mode == DurabilityStrict {
				if len(items) != 1 || items[0] != turnDir {
					t.Fatalf("item syncs = %v, want exactly one turn dir sync for 8 items", items)
				}
			} else if len(items) != 0 {
				t.Fatalf("turn mode item syncs: %v", items)
			}
			if _, err := turn.Commit(completedEvent("resp_dir", output)); err != nil {
				t.Fatal(err)
			}
			commit := recorder.take()
			want := []string{turnDir}
			if mode == DurabilityTurn {
				want = append(want, stagingDir)
			}
			want = append(want, filepath.Dir(journal.path))
			if strings.Join(commit, "|") != strings.Join(want, "|") {
				t.Fatalf("commit syncs = %v, want %v", commit, want)
			}
			next, err := store.BeginTurn("turn-dir-2")
			if err != nil {
				t.Fatal(err)
			}
			stageItems(t, next, 1)
			recorder.take()
			if _, err := next.Commit(completedEvent("resp_dir2", stageItemsOutput(1))); err != nil {
				t.Fatal(err)
			}
			for _, path := range recorder.take() {
				if path == filepath.Dir(journal.path) {
					t.Fatal("journal directory synced again for an existing transcript")
				}
			}
		})
	}
}

func stageItemsOutput(count int) string {
	var parts []string
	for index := 0; index < count; index++ {
		parts = append(parts, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer `+strconv.Itoa(index)+`"}]}`)
	}
	return strings.Join(parts, ",")
}

func TestProcessModeNeverSyncsDirectories(t *testing.T) {
	_, store, recorder, _ := recordedStore(t, DurabilityProcess)
	turn, err := store.BeginTurn("turn-proc")
	if err != nil {
		t.Fatal(err)
	}
	output := stageItems(t, turn, 3)
	if _, err := turn.Commit(completedEvent("resp_proc", output)); err != nil {
		t.Fatal(err)
	}
	if got := recorder.take(); len(got) != 0 {
		t.Fatalf("process mode synced directories: %v", got)
	}
}

func TestDirectorySyncFailureFailsBeforeTheJournalAppend(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityTurn, DurabilityStrict} {
		for _, target := range []string{"turn", "staging"} {
			t.Run(string(mode)+"/"+target, func(t *testing.T) {
				journal, store, recorder, _ := recordedStore(t, mode)
				fail := store.stagingDir
				if target == "turn" {
					fail = filepath.Join(store.stagingDir, "turn-fail")
				}
				recorder.fail = fail
				turn, err := store.BeginTurn("turn-fail")
				if mode == DurabilityStrict {
					if err == nil {
						t.Fatal("strict BeginTurn ignored a directory sync failure")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				output := stageItems(t, turn, 2)
				_, err = turn.Commit(completedEvent("resp_fail", output))
				if err == nil || !strings.Contains(err.Error(), "sync") {
					t.Fatalf("Commit error = %v, want directory sync failure", err)
				}
				if info, statErr := os.Stat(journal.path); statErr == nil && info.Size() != 0 {
					t.Fatal("journal was appended after a failed directory sync")
				}
			})
		}
	}
}

func TestStrictFirstItemDirectorySyncFailureRollsBack(t *testing.T) {
	_, store, recorder, _ := recordedStore(t, DurabilityStrict)
	turn, err := store.BeginTurn("turn-item-fail")
	if err != nil {
		t.Fatal(err)
	}
	recorder.fail = turn.turnDir
	err = turn.StageOutputItem(outputItemEvent(0, `{"type":"message","role":"assistant","content":[]}`))
	if err == nil || !strings.Contains(err.Error(), "sync staging directory") {
		t.Fatalf("StageOutputItem error = %v", err)
	}
	if info, statErr := os.Stat(turn.itemsPath); statErr != nil || info.Size() != 0 {
		t.Fatalf("items.jsonl not rolled back: %v %v", info, statErr)
	}
	if !turn.stageDirty {
		t.Fatal("stageDirty not set")
	}
}

func TestJournalCreationSyncsItsDirectoryOnce(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityTurn, DurabilityStrict, DurabilityProcess} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), mode)
			if err != nil {
				t.Fatal(err)
			}
			recorder := &dirSyncRecorder{}
			journal.dirSyncHook = recorder.hook
			for range 3 {
				if err := journal.Append([]byte(`{"a":1}`)); err != nil {
					t.Fatal(err)
				}
			}
			want := 1
			if mode == DurabilityProcess {
				want = 0
			}
			if got := recorder.take(); len(got) != want {
				t.Fatalf("syncs = %v, want %d", got, want)
			}
			recorder.fail = dir
			if err := os.Remove(journal.path); err != nil {
				t.Fatal(err)
			}
			err = journal.Append([]byte(`{"a":2}`))
			if mode != DurabilityProcess && (err == nil || !strings.Contains(err.Error(), "sync transcript directory")) {
				t.Fatalf("Append error = %v, want directory sync failure", err)
			}
		})
	}
}
