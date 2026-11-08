package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeepToolPairsCountsAcrossAllKeptTurns pins compaction.keepbatch.tools:
// it is the number of tool call/result pairs kept in total across the kept
// turns, not per turn. The TypeScript backend does the same.
func TestKeepToolPairsCountsAcrossAllKeptTurns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pairs     int
		turnsKept int
		latest    bool
		want      []string
	}{
		{"one pair over five turns", 1, 5, false, []string{"c4"}},
		{"two pairs over five turns", 2, 5, false, []string{"c3", "c4"}},
		{"more pairs than the kept turns hold", 9, 4, false, []string{"c1", "c2", "c3", "c4"}},
		{"only the kept turns count", 3, 2, false, []string{"c3", "c4"}},
		// KeepLatestTurn (overflow compaction) keeps the newest turn whole; the pairs are
		// counted in the kept turns before it.
		{"latest turn whole, one pair before it", 1, 3, true, []string{"c3", "c4"}},
		{"latest turn whole with keep_turns 0", 0, 0, true, []string{"c4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			staging := filepath.Join(dir, "staging")
			journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityStrict)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewTurnStore(journal, staging, TurnStoreOptions{
				MaxItemBytes:        1 << 20,
				MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
				CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
				UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
				CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
				StateContextPath:    filepath.Join(staging, "statecontext.json"),
			})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				user := fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"u%d"}]}`, i)
				if err := store.AppendInputItems([]byte(user)); err != nil {
					t.Fatal(err)
				}
				call := fmt.Sprintf(`{"type":"function_call","call_id":"c%d","name":"records","arguments":"{}"}`, i)
				if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"a` + fmt.Sprint(i) + `","status":"completed","output":[` + call + `]}}`)); err != nil {
					t.Fatal(err)
				}
				output := fmt.Sprintf(`{"type":"function_call_output","call_id":"c%d","output":"%s"}`, i, strings.Repeat("x", 500))
				if err := store.AppendInputItems([]byte(output)); err != nil {
					t.Fatal(err)
				}
				final := fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a%d"}]}`, i)
				if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"b` + fmt.Sprint(i) + `","status":"completed","output":[` + final + `]}}`)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: tc.turnsKept, KeepToolPairs: tc.pairs, KeepLatestTurn: tc.latest, BackupMode: "replace"}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(staging, "compaction-cache.json"))
			if err != nil {
				t.Fatal(err)
			}
			var kept []string
			for i := 0; i < 5; i++ {
				if strings.Contains(string(raw), fmt.Sprintf(`"call_id":"c%d"`, i)) {
					kept = append(kept, fmt.Sprintf("c%d", i))
				}
			}
			if strings.Join(kept, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("kept pairs = %v, want %v", kept, tc.want)
			}
		})
	}
}
