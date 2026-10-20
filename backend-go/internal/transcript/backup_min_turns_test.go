package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUserTurns(t *testing.T, path string, turns int) {
	t.Helper()
	var builder strings.Builder
	for i := 0; i < turns; i++ {
		builder.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]}` + "\n")
		builder.WriteString(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}` + "\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func backupFixture(t *testing.T, oldTurns, newTurns int) (source, backup string) {
	t.Helper()
	dir := t.TempDir()
	source = filepath.Join(dir, "messages.jsonl")
	backup = filepath.Join(dir, "messages.backup.jsonl")
	writeUserTurns(t, source, newTurns)
	if oldTurns >= 0 {
		writeUserTurns(t, backup, oldTurns)
	}
	return source, backup
}

func TestBackupTranscriptMinTurns(t *testing.T) {
	cases := []struct {
		name               string
		mode               string
		minTurns           int
		oldTurns, newTurns int
		wantOverwritten    bool
	}{
		{"fuller old backup and thin snapshot is kept", "replace", 3, 5, 1, false},
		{"snapshot at min turns overwrites", "replace", 3, 5, 3, true},
		{"old backup below min turns is overwritten", "replace", 3, 2, 1, true},
		{"no old backup is written", "replace", 3, -1, 1, true},
		{"min turns 0 overwrites as before", "replace", 0, 5, 1, true},
		{"append mode ignores min turns", "append", 3, 5, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, backup := backupFixture(t, tc.oldTurns, tc.newTurns)
			before, _ := os.ReadFile(backup)
			if err := backupTranscript(source, backup, tc.mode, tc.minTurns, DurabilityProcess); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(backup)
			if err != nil {
				t.Fatal(err)
			}
			turns, err := countUserTurns(backup)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantOverwritten {
				if tc.mode == "append" {
					if want := tc.oldTurns + tc.newTurns; turns != want {
						t.Fatalf("append backup turns = %d, want %d", turns, want)
					}
				} else if turns != tc.newTurns {
					t.Fatalf("backup turns = %d, want %d (overwritten)", turns, tc.newTurns)
				}
			} else if string(after) != string(before) {
				t.Fatalf("backup changed, want the fuller one kept")
			}
		})
	}
}

func TestCompactHistoryKeepsFullerReplaceBackup(t *testing.T) {
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	backup := filepath.Join(dir, "messages.backup.jsonl")
	writeUserTurns(t, backup, 4)
	journal, err := NewJournal(messages, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20, MessagesBackupPath: backup, CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json")})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("old ", 50), "new"} {
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(backup)
	result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "replace", BackupMinTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted {
		t.Fatalf("compaction result = %+v", result)
	}
	after, _ := os.ReadFile(backup)
	if string(after) != string(before) {
		t.Fatalf("fuller backup was overwritten: %q", after)
	}
}
