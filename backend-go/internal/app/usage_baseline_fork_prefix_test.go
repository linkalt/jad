package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// chainedSource is a session whose history was removed from before and after the kept bytes: three
// turns compacted away (an ordinary compaction), turns 4 to 7 run, then a front compaction from
// turn 6, which removes turns 6 and 7 and keeps turns 4 and 5. Turn 8 follows. The source has run
// 8 responses; the front entry covers 7 of them (the kept prefix is turns 4 and 5), and the older
// entry, at offset 0, covers the 3 that were compacted away.
func chainedSource(t *testing.T) (f forkUsageFixture, ids []string) {
	t.Helper()
	f = newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 3)
	if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
		t.Fatalf("compact: %d %s", status, out)
	}
	f.runs("local:demo", "dev/coder", 4, 7)
	ids = f.userIDs("local:demo", "dev/coder")
	if len(ids) != 4 {
		t.Fatalf("user messages after the compaction = %v, want turns 4 to 7", ids)
	}
	if status, out := f.post("local:demo", "dev/coder", "/front-compact", `{"message_id":"`+ids[2]+`"}`); status != http.StatusOK {
		t.Fatalf("front-compact: %d %s", status, out)
	}
	f.run("local:demo", "dev/coder", "turn 8")
	wantResponses(t, "source", f.usageOf("local:demo", "dev/coder"), 8)
	entries := f.baseline("demo", "dev/coder")
	if len(entries) != 2 || entries[0].Offset <= 0 || entries[0].Summary.CompletedResponses != 7 || entries[0].PrefixExact ||
		entries[1].Offset != 0 || entries[1].Summary.CompletedResponses != 3 {
		t.Fatalf("source baseline = %+v, want the front entry (7 responses) and the compaction's (3)", entries)
	}
	return f, f.userIDs("local:demo", "dev/coder")
}

// A fork that cuts inside the kept prefix of a front compaction that followed an ordinary one has
// the ordinary one's removed history before the cut, and the front one's removed tail after it:
// the older entry is the checkpoint, and the fork's totals are exact.
func TestForkInsideAFrontCompactedPrefixUsesTheOlderCheckpoint(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f, ids := chainedSource(t)
			if len(ids) != 3 {
				t.Fatalf("source user messages = %v, want turns 4, 5 and 8", ids)
			}

			// Before turn 5: turn 4 on top of the 3 compacted turns.
			reference, profile, state := f.fork(target, "inside", `,"message_id":"`+ids[1]+`","keep_turn":false`)
			wantResponses(t, "fork before turn 5", f.usageOf(reference, profile), 4)
			entries := f.baselineAt(state)
			if len(entries) != 1 || entries[0].Offset != 0 || entries[0].Summary.CompletedResponses != 3 {
				t.Fatalf("fork baseline = %+v, want the older entry (3 responses at offset 0)", entries)
			}
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatal(err)
			}
			wantResponses(t, "fork before turn 5, no cache", f.usageOf(reference, profile), 4)
			f.run(reference, profile, "turn 5")
			wantResponses(t, "fork before turn 5, next message", f.usageOf(reference, profile), 5)
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatal(err)
			}
			wantResponses(t, "fork before turn 5, next message, no cache", f.usageOf(reference, profile), 5)

			// Through turn 5, which is the whole kept prefix: the front entry, with the removed
			// turns 6 and 7.
			reference, profile, _ = f.fork(target, "prefix", `,"message_id":"`+ids[1]+`"`)
			wantResponses(t, "fork of exactly the kept prefix", f.usageOf(reference, profile), 7)
			reference, profile, _ = f.fork(target, "whole", "")
			wantResponses(t, "whole fork", f.usageOf(reference, profile), 8)
			wantResponses(t, "source after the forks", f.usageOf("local:demo", "dev/coder"), 8)
		})
	}
}

// Entries written before the checkpoints and PrefixExact existed cannot say what precedes a cut
// inside their bytes: the fork counts what it copied and says its history is partial, in its own
// baseline file, so that losing the cache or the history block does not make it look complete.
func TestForkInsideTheCoveredBytesOfALegacyEntryIsPartialAndStaysPartial(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f, ids := chainedSource(t)
			// A legacy baseline file: the front entry alone, without the field.
			path := f.baselinePath("demo", "dev/coder")
			entries := f.baseline("demo", "dev/coder")
			legacy := entries[0]
			legacy.PrefixExact = false
			writeBaselineEntries(t, path, legacy)

			reference, profile, state := f.fork(target, "legacy", `,"message_id":"`+ids[1]+`","keep_turn":false`)
			usage := f.usageOf(reference, profile)
			if !usage.HistoryPartial || usage.CompletedResponses != 1 {
				t.Fatalf("fork inside a legacy entry = %+v, want the copied response and a partial history", usage)
			}
			forkEntries := f.baselineAt(state)
			if len(forkEntries) != 1 || forkEntries[0].Offset != 0 || !forkEntries[0].Summary.HistoryPartial {
				t.Fatalf("fork baseline = %+v, want one partial marker", forkEntries)
			}
			for _, name := range []string{"usage-cache.json", "messages.backup.jsonl"} {
				p := filepath.Join(state, "staging", name)
				if name != "usage-cache.json" {
					p = filepath.Join(state, name)
				}
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if usage := f.usageOf(reference, profile); !usage.HistoryPartial || usage.CompletedResponses != 1 {
				t.Fatalf("legacy fork without cache and history block = %+v", usage)
			}
			f.run(reference, profile, "turn 5")
			if usage := f.usageOf(reference, profile); !usage.HistoryPartial || usage.CompletedResponses != 2 {
				t.Fatalf("legacy fork after the next message = %+v, want two responses, still partial", usage)
			}
			// The fork is compacted in its turn: the flag goes into the entry it writes.
			if status, out := f.post(reference, profile, "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact the fork: %d %s", status, out)
			}
			removeFile(t, filepath.Join(state, "staging", "usage-cache.json"))
			if usage := f.usageOf(reference, profile); !usage.HistoryPartial || usage.CompletedResponses != 2 {
				t.Fatalf("legacy fork after its own compaction = %+v, want two responses, still partial", usage)
			}

			// A cut at or past the entry's offset still carries it, exactly.
			reference, profile, _ = f.fork(target, "legacywhole", "")
			if usage := f.usageOf(reference, profile); usage.HistoryPartial || usage.CompletedResponses != 8 {
				t.Fatalf("whole fork of a legacy entry = %+v, want 8 responses and a complete history", usage)
			}
		})
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func writeBaselineEntries(t *testing.T, path string, entries ...usageBaselineEntry) {
	t.Helper()
	data, err := marshalBaseline(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A source rewritten between the fork point and the copy is not the file the point was computed
// from: the fork fails (409) instead of carrying the wrong usage, leaves nothing behind, and a
// retry works.
func TestForkOfASourceReplacedBeforeTheCopyFailsAndLeavesNothing(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 3)
			if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 4")
			source := filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl")
			beforeForkCopy = func() {
				data, err := os.ReadFile(source)
				if err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(source+".new", data, 0o600); err != nil {
					t.Error(err)
					return
				}
				if err := os.Rename(source+".new", source); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { beforeForkCopy = nil })

			var state string
			if target.name == "new profile" {
				state = f.stateDir("demo", "dev/replaced")
			} else {
				state = f.stateDir("replaced", "dev/coder")
			}
			status, _, _, _ := target.make(f, "replaced", "")
			if status != http.StatusConflict {
				t.Fatalf("fork of a source replaced before the copy: %d, want 409", status)
			}
			for _, path := range []string{filepath.Join(state, "messages.jsonl"), filepath.Join(state, "staging", "usage-cache.json.baseline")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("the refused fork left %s: %v", path, err)
				}
			}
			beforeForkCopy = nil
			reference, profile, _ := f.fork(target, "replaced", "")
			wantResponses(t, "retried fork", f.usageOf(reference, profile), 4)
		})
	}
}

// Compaction of a transcript that ends in empty lines works over HTTP, and the totals stay.
func TestHTTPCompactionOfATranscriptWithTrailingEmptyLines(t *testing.T) {
	f := newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 3)
	appendToTranscript(t, filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl"), "\n\n")
	if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
		t.Fatalf("compact: %d %s", status, out)
	}
	removeFile(t, f.cachePath("demo", "dev/coder"))
	wantResponses(t, "after the compaction, no cache", f.usageOf("local:demo", "dev/coder"), 3)
	f.run("local:demo", "dev/coder", "turn 4")
	wantResponses(t, "after the next message", f.usageOf("local:demo", "dev/coder"), 4)
}

// Forks that cut inside a chained source, under readers of the source and of the forks.
func TestForksInsideAChainedSourceWithConcurrentReaders(t *testing.T) {
	f, ids := chainedSource(t)
	const forks = 6
	stop := make(chan struct{})
	var readers sync.WaitGroup
	problems := make(chan string, 64)
	report := func(format string, args ...any) {
		select {
		case problems <- fmt.Sprintf(format, args...):
		default:
		}
	}
	for reader := 0; reader < 4; reader++ {
		readers.Add(1)
		go func(reader int) {
			defer readers.Done()
			for round := 0; ; round++ {
				select {
				case <-stop:
					return
				default:
				}
				if usage, _, status := f.snapshot("local:demo", "dev/coder"); status != http.StatusOK || usage.CompletedResponses != 8 {
					report("source read: %d %+v", status, usage)
				}
				name := fmt.Sprintf("chain%d", (reader+round)%forks)
				if usage, _, status := f.snapshot("local:"+name, "dev/coder"); status == http.StatusOK && usage.CompletedResponses != 0 && usage.CompletedResponses != 4 {
					report("fork %s read: %+v", name, usage)
				}
			}
		}(reader)
	}
	for index := 0; index < forks; index++ {
		body := fmt.Sprintf(`{"session":"local:chain%d","message_id":"%s","keep_turn":false}`, index, ids[1])
		if status, out := f.post("local:demo", "dev/coder", "/fork", body); status != http.StatusCreated {
			t.Errorf("fork %d: %d %s", index, status, out)
		}
	}
	close(stop)
	readers.Wait()
	close(problems)
	for problem := range problems {
		t.Error(problem)
	}
	for index := 0; index < forks; index++ {
		wantResponses(t, fmt.Sprintf("fork chain%d", index), f.usageOf(fmt.Sprintf("local:chain%d", index), "dev/coder"), 4)
	}
}
