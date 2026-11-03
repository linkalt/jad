package native

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/tools"
)

// referenceTurns is the turn table every line decoded, as indexing did before the prefilter.
func referenceTurns(t *testing.T, path string, maxLine int64) ([]transcriptTurn, int64) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	var turns []transcriptTurn
	var count, end int64
	err = scanTranscript(context.Background(), file, 0, info.Size(), maxLine, func(line transcriptLine) (bool, error) {
		end = line.off + line.length
		var head transcriptHead
		if !line.tooLong {
			_ = json.Unmarshal(line.data, &head)
		}
		if len(turns) == 0 || head.startsUserTurn() {
			count++
			turns = append(turns, transcriptTurn{off: line.off, number: count})
		}
		if head.SessionTurn != nil && *head.SessionTurn > 0 {
			count = *head.SessionTurn
			turns[len(turns)-1].number = count
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return turns, end
}

func cachedTurns(t *testing.T, cache *transcriptIndexCache, path string, maxLine int64, capacity int) ([]transcriptTurn, int64) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	identity, err := transcriptIdentity(file, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	turns, end, err := cache.turns(context.Background(), file, path, identity, info.Size(), maxLine, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return turns, end
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func requireTurns(t *testing.T, what string, cache *transcriptIndexCache, path string, capacity int) {
	t.Helper()
	want, wantEnd := referenceTurns(t, path, 2048)
	got, end := cachedTurns(t, cache, path, 2048, capacity)
	if !slices.Equal(got, want) || end != wantEnd {
		t.Fatalf("%s: turns %v end %d, want %v end %d", what, got, end, want, wantEnd)
	}
}

// TestTranscriptIndexMatchesADecodeOfEveryLine: the prefilter skips no line that starts or
// numbers a turn, over transcripts with batches, compaction numbering, over-long and unreadable
// lines.
func TestTranscriptIndexMatchesADecodeOfEveryLine(t *testing.T) {
	for seed := uint64(1); seed <= 8; seed++ {
		path := equivalenceTranscript(t, seed, 40)
		want, wantEnd := referenceTurns(t, path, 2048)
		got, end := cachedTurns(t, &transcriptIndexCache{entries: map[string]*transcriptIndexEntry{}}, path, 2048, 0)
		if !slices.Equal(got, want) || end != wantEnd || len(want) != 40 {
			t.Fatalf("seed %d: %d turns end %d, want %d turns end %d", seed, len(got), end, len(want), wantEnd)
		}
	}
}

// TestTranscriptIndexCacheFollowsTheFile: a kept table is extended by what was appended, a turn
// renumbered by an appended session_turn included; a file cut and grown again past the old end
// (a front-compaction keeps the first line) or rewritten (a reset) is indexed from the start.
// Every table equals a decode of the whole file.
func TestTranscriptIndexCacheFollowsTheFile(t *testing.T) {
	cache := &transcriptIndexCache{entries: map[string]*transcriptIndexEntry{}}
	path := equivalenceTranscript(t, 3, 10)
	requireTurns(t, "first read", cache, path, 4)

	appendLines(t, path, userLine("a new question"))
	requireTurns(t, "after a user message", cache, path, 4)
	appendLines(t, path, answerLine(90, 1, textItem("its answer, numbered by session_turn")))
	requireTurns(t, "after its numbered answer", cache, path, 4)
	appendLines(t, path, userLine("a question with no answer yet"))
	requireTurns(t, "after an unanswered question", cache, path, 4)
	appendLines(t, path, userLine("another one"))
	requireTurns(t, "after a second unanswered question", cache, path, 4)
	appendLines(t, path, `{"type":"function_call_output","call_id":"x","output":"partial`)
	requireTurns(t, "with an incomplete last line", cache, path, 4)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := data[:lastLineEnd(data[:len(data)/2])]
	grown := append(append([]byte(nil), cut...), []byte(userLine("after the cut")+"\n"+answerLine(7, 1, textItem("x"))+"\n")...)
	for len(grown) < len(data)+100 {
		grown = append(grown, []byte(userLine("filler question past the old end")+"\n")...)
	}
	if err := os.WriteFile(path, grown, 0o644); err != nil {
		t.Fatal(err)
	}
	requireTurns(t, "cut and grown past the old end", cache, path, 4)

	if err := os.WriteFile(path, []byte(userLine("a reset transcript")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireTurns(t, "after a reset", cache, path, 4)
}

func lastLineEnd(data []byte) int {
	for index := len(data) - 1; index >= 0; index-- {
		if data[index] == '\n' {
			return index + 1
		}
	}
	return 0
}

// TestTranscriptIndexCacheKeepsAtMostItsCapacity: the least recently used table goes first, and a
// capacity of 0 keeps nothing.
func TestTranscriptIndexCacheKeepsAtMostItsCapacity(t *testing.T) {
	cache := &transcriptIndexCache{entries: map[string]*transcriptIndexEntry{}}
	paths := []string{equivalenceTranscript(t, 1, 3), equivalenceTranscript(t, 2, 3), equivalenceTranscript(t, 3, 3)}
	cachedTurns(t, cache, paths[0], 2048, 2)
	cachedTurns(t, cache, paths[1], 2048, 2)
	cachedTurns(t, cache, paths[0], 2048, 2)
	cachedTurns(t, cache, paths[2], 2048, 2)
	if len(cache.entries) != 2 || cache.entries[transcriptIndexKey(paths[1], 2048)] != nil || cache.entries[transcriptIndexKey(paths[0], 2048)] == nil {
		t.Fatalf("kept %d tables; want paths 0 and 2", len(cache.entries))
	}
	empty := &transcriptIndexCache{entries: map[string]*transcriptIndexEntry{}}
	cachedTurns(t, empty, paths[0], 2048, 0)
	if len(empty.entries) != 0 {
		t.Fatal("capacity 0 kept a table")
	}
}

// TestTranscriptIndexCacheUnderConcurrentReadsAndAppends: calls that share the kept table while
// a run appends turns see consistent tables (run with -race), and the last read lists the newest
// turn the file holds.
func TestTranscriptIndexCacheUnderConcurrentReadsAndAppends(t *testing.T) {
	path := equivalenceTranscript(t, 5, 5)
	settings := transcriptTestSettings()
	settings.IndexCacheEntries = 8
	done := make(chan struct{})
	go func() {
		defer close(done)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		for turn := 100; turn < 160; turn++ {
			if _, err := file.WriteString(userLine("appended question") + "\n" + answerLine(turn, 1, textItem("appended answer")) + "\n"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 30 {
				for _, arguments := range []map[string]any{{}, {"turns": "-1"}, {"step": "100.1"}} {
					tool, err := NewTranscriptTool(settings, TranscriptFiles{Messages: path}, nil, nil)
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c"}); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	readers.Wait()
	<-done
	if out := runTranscript(t, settings, path, map[string]any{"turns": "-1"}); !strings.Contains(out, "159.1 assistant: appended answer") {
		t.Fatalf("the last read does not list the newest turn:\n%s", out)
	}
	requireTurns(t, "after the appends", transcriptIndexes, path, 8)
}
