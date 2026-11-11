package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Records of a test transcript. Each is one JSONL line with a label in its text.
func userLine(label string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + label + `"}]}` + "\n"
}

func steerLine(label string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + label + `"}],"` + SessionInputSteerField + `":true}` + "\n"
}

func assistantLine(label string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + label + `"}]}` + "\n"
}

// turnLines is the lines of numbered turns first..last: the user record and the answer.
func turnLines(first, last int) string {
	var builder strings.Builder
	for n := first; n <= last; n++ {
		builder.WriteString(userLine(fmt.Sprintf("u%d", n)))
		builder.WriteString(assistantLine(fmt.Sprintf("a%d", n)))
	}
	return builder.String()
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// backupFiles returns a live transcript holding source and a backup holding existing; a
// nil existing means no backup file yet.
func backupFiles(t *testing.T, existing *string, source string) (sourcePath, backupPath string) {
	t.Helper()
	dir := t.TempDir()
	sourcePath = filepath.Join(dir, "messages.jsonl")
	backupPath = filepath.Join(dir, "messages.backup.jsonl")
	writeTestFile(t, sourcePath, source)
	if existing != nil {
		writeTestFile(t, backupPath, *existing)
	}
	return sourcePath, backupPath
}

func text(value string) *string { return &value }

func TestBackupTranscriptAppendLimit(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		limit    int
		existing *string
		source   string
		want     string
	}{
		{
			name: "limit 0 keeps every turn", mode: "append", limit: 0,
			existing: text(turnLines(1, 12)), source: turnLines(13, 14),
			want: turnLines(1, 14),
		},
		{
			name: "turns within the limit keep everything", mode: "append", limit: 5,
			existing: text(turnLines(1, 2)), source: turnLines(3, 5),
			want: turnLines(1, 5),
		},
		{
			name: "no backup yet", mode: "append", limit: 2,
			existing: nil, source: turnLines(1, 2),
			want: turnLines(1, 2),
		},
		{
			name: "no backup yet and the transcript is over the limit keeps its newest turns", mode: "append", limit: 2,
			existing: nil, source: turnLines(1, 5),
			want: turnLines(4, 5),
		},
		{
			name: "one batch with more turns than the limit keeps its newest turns", mode: "append", limit: 3,
			existing: text(turnLines(1, 2)), source: turnLines(3, 9),
			want: turnLines(7, 9),
		},
		{
			name: "the cut falls in the existing backup", mode: "append", limit: 3,
			existing: text(turnLines(1, 4)), source: turnLines(5, 5),
			want: turnLines(3, 5),
		},
		{
			name: "the cut falls at the first turn of the transcript", mode: "append", limit: 2,
			existing: text(turnLines(1, 4)), source: turnLines(5, 6),
			want: turnLines(5, 6),
		},
		{
			name: "limit 1", mode: "append", limit: 1,
			existing: text(turnLines(1, 3)), source: turnLines(4, 4),
			want: turnLines(4, 4),
		},
		{
			name: "a steering record stays with the turn before it", mode: "append", limit: 2,
			existing: text(userLine("u1") + assistantLine("a1") + steerLine("s1") + assistantLine("b1") + userLine("u2") + assistantLine("a2") + steerLine("s2") + assistantLine("b2")),
			source:   userLine("u3") + assistantLine("a3"),
			want:     userLine("u2") + assistantLine("a2") + steerLine("s2") + assistantLine("b2") + userLine("u3") + assistantLine("a3"),
		},
		{
			name: "a steering record at the boundary goes with the turn it follows and is not orphaned", mode: "append", limit: 1,
			existing: text(userLine("u1") + assistantLine("a1") + steerLine("s1")),
			source:   userLine("u2") + assistantLine("a2") + steerLine("s2"),
			want:     userLine("u2") + assistantLine("a2") + steerLine("s2"),
		},
		{
			name: "records in front of the cut turn go with it, leading steering included", mode: "append", limit: 1,
			existing: text(assistantLine("lead") + steerLine("lead-steer") + userLine("u1") + assistantLine("a1")),
			source:   userLine("u2"),
			want:     userLine("u2"),
		},
		{
			name: "leading records stay while the turns fit", mode: "append", limit: 3,
			existing: text(assistantLine("lead") + steerLine("lead-steer") + "\n" + userLine("u1") + assistantLine("a1")),
			source:   userLine("u2"),
			want:     assistantLine("lead") + steerLine("lead-steer") + "\n" + userLine("u1") + assistantLine("a1") + userLine("u2"),
		},
		{
			name: "leading records stay when the turns exactly fill the limit", mode: "append", limit: 2,
			existing: text(assistantLine("lead") + steerLine("lead-steer") + userLine("u1") + assistantLine("a1")),
			source:   userLine("u2"),
			want:     assistantLine("lead") + steerLine("lead-steer") + userLine("u1") + assistantLine("a1") + userLine("u2"),
		},
		{
			name: "a transcript without a user turn is appended as it is", mode: "append", limit: 2,
			existing: text(assistantLine("x")), source: assistantLine("y") + steerLine("z"),
			want: assistantLine("x") + assistantLine("y") + steerLine("z"),
		},
		{
			name: "no turn at all and no backup", mode: "append", limit: 2,
			existing: nil, source: assistantLine("y"),
			want: assistantLine("y"),
		},
		{
			// No separator is added: a backup whose last line has no newline is joined to the
			// first line of the transcript. That is only fine when the joined line is a record.
			name: "an unterminated blank last line is joined to the first record", mode: "append", limit: 5,
			existing: text(userLine("u1") + "  "), source: userLine("u2"),
			want: userLine("u1") + "  " + userLine("u2"),
		},
		{
			name: "limit 0 joins an unterminated last line to the transcript as before", mode: "append", limit: 0,
			existing: text(strings.TrimSuffix(userLine("u1"), "\n")), source: userLine("u2"),
			want: strings.TrimSuffix(userLine("u1"), "\n") + userLine("u2"),
		},
		{
			name: "an unterminated last line with an empty transcript is kept as it is", mode: "append", limit: 5,
			existing: text(strings.TrimSuffix(userLine("u1"), "\n")), source: "",
			want: strings.TrimSuffix(userLine("u1"), "\n"),
		},
		{
			// The line written is "  " + the first record, one record that starts at the
			// blank, so a cut at that turn keeps the blank with it and drops what is before.
			name: "an unterminated blank last line joined to a user record is the turn it starts", mode: "append", limit: 1,
			existing: text(turnLines(1, 1) + "  "), source: userLine("u2") + assistantLine("a2"),
			want: "  " + userLine("u2") + assistantLine("a2"),
		},
		{
			name: "the joined user record counts once", mode: "append", limit: 2,
			existing: text(turnLines(1, 3) + "  "), source: userLine("u4"),
			want: userLine("u3") + assistantLine("a3") + "  " + userLine("u4"),
		},
		{
			name: "the joined user record is a turn of its own after the leading blank", mode: "append", limit: 1,
			existing: text("  "), source: turnLines(1, 2),
			want: turnLines(2, 2),
		},
		{
			name: "the joined user record is not cut when the turns fit", mode: "append", limit: 2,
			existing: text("  "), source: turnLines(1, 2),
			want: "  " + turnLines(1, 2),
		},
		{
			name: "an unterminated user record joined to a blank first line is the turn it starts", mode: "append", limit: 1,
			existing: text(turnLines(1, 1) + strings.TrimSuffix(userLine("u2"), "\n")), source: "\n" + assistantLine("a2"),
			want: userLine("u2") + assistantLine("a2"),
		},
		{
			name: "blank lines and CRLF are not records", mode: "append", limit: 1,
			existing: text(strings.ReplaceAll(userLine("u1"), "\n", "\r\n") + "\n   \n" + assistantLine("a1")),
			source:   userLine("u2"),
			want:     userLine("u2"),
		},
		{
			name: "replace ignores the limit and overwrites", mode: "replace", limit: 1,
			existing: text(turnLines(1, 9)), source: turnLines(10, 13),
			want: turnLines(10, 13),
		},
		{
			name: "replace with limit 0 overwrites", mode: "replace", limit: 0,
			existing: text(turnLines(1, 9)), source: turnLines(10, 10),
			want: turnLines(10, 10),
		},
	}
	for _, tc := range cases {
		for _, durability := range []DurabilityMode{DurabilityProcess, DurabilityTurn} {
			t.Run(tc.name+"/"+string(durability), func(t *testing.T) {
				source, backup := backupFiles(t, tc.existing, tc.source)
				if err := backupTranscript(source, backup, tc.mode, tc.limit, durability); err != nil {
					t.Fatal(err)
				}
				if got := readTestFile(t, backup); got != tc.want {
					t.Fatalf("backup:\n%s\nwant:\n%s", got, tc.want)
				}
				if got := readTestFile(t, source); got != tc.source {
					t.Fatal("the transcript was changed by its backup")
				}
				assertNoBackupTemporaries(t, filepath.Dir(backup))
			})
		}
	}
}

func assertNoBackupTemporaries(t *testing.T, dir string) {
	t.Helper()
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".compact-backup-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary backup files left behind: %v", leftovers)
	}
}

// Appending one batch of turns at a time keeps the newest limit turns at every step, and
// the oldest are gone.
func TestBackupTranscriptAppendLimitRollsOverBatches(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "messages.jsonl")
	backup := filepath.Join(dir, "messages.backup.jsonl")
	const limit = 10
	total := 0
	for _, batch := range []int{2, 2, 2, 2, 2, 2} {
		writeTestFile(t, source, turnLines(total+1, total+batch))
		total += batch
		if err := backupTranscript(source, backup, "append", limit, DurabilityProcess); err != nil {
			t.Fatal(err)
		}
		first := total - limit + 1
		if first < 1 {
			first = 1
		}
		if got, want := readTestFile(t, backup), turnLines(first, total); got != want {
			t.Fatalf("after %d turns the backup is:\n%s\nwant turns %d..%d:\n%s", total, got, first, total, want)
		}
	}
	if total != 12 || strings.Contains(readTestFile(t, backup), `"u1"`) || strings.Contains(readTestFile(t, backup), `"u2"`) || !strings.Contains(readTestFile(t, backup), `"u3"`) {
		t.Fatalf("the oldest turns were not dropped:\n%s", readTestFile(t, backup))
	}
}

// Each turn of an input batch is a turn of its own for the limit, as for compaction.
func TestBackupTranscriptAppendLimitCountsEveryRecordOfAnInputBatch(t *testing.T) {
	batched := func(label string) string {
		return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + label + `"}],"` + SessionInputBatchIDField + `":"batch-1"}` + "\n"
	}
	source, backup := backupFiles(t, nil, batched("b1")+batched("b2")+batched("b3")+assistantLine("a"))
	if err := backupTranscript(source, backup, "append", 2, DurabilityProcess); err != nil {
		t.Fatal(err)
	}
	if got, want := readTestFile(t, backup), batched("b2")+batched("b3")+assistantLine("a"); got != want {
		t.Fatalf("backup:\n%s\nwant:\n%s", got, want)
	}
}

// With limit 0 the bytes are exactly the existing backup followed by the transcript, and
// nothing is parsed: a corrupt line is carried along, as before.
func TestBackupTranscriptWithoutALimitKeepsTheOldBytes(t *testing.T) {
	existing := turnLines(1, 2) + "not json\n" + `{"type":"message","role":"user"`
	source := "also not json\n" + turnLines(3, 3)
	for _, mode := range []string{"append", "replace"} {
		sourcePath, backup := backupFiles(t, &existing, source)
		if err := backupTranscript(sourcePath, backup, mode, 0, DurabilityProcess); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		want := existing + source
		if mode == "replace" {
			want = source
		}
		if got := readTestFile(t, backup); got != want {
			t.Fatalf("%s: backup = %q, want %q", mode, got, want)
		}
	}
}

// A corrupt record stops an append with a limit before anything is written: the error says
// which file and byte, and the backup and the transcript stay as they were.
func TestBackupTranscriptAppendLimitRefusesCorruptRecords(t *testing.T) {
	good := turnLines(1, 2)
	cases := []struct {
		name             string
		existing, source string
		wantInError      string
	}{
		{"corrupt line in the backup", good + "garbage\n" + turnLines(3, 3), turnLines(4, 4), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(good))},
		{"corrupt first line in the backup", "{broken\n", turnLines(4, 4), "messages.backup.jsonl: record at byte 0 "},
		{"corrupt line in the transcript", good, turnLines(3, 3) + "[1,2]\n", "messages.jsonl: record at byte " + fmt.Sprint(len(turnLines(3, 3)))},
		{"unterminated corrupt last line", good, turnLines(3, 3) + `{"role":"user"`, "messages.jsonl: record at byte"},
		{"valid object followed by junk", good + `{"role":"user"} x` + "\n", turnLines(3, 3), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(good))},
		{"unterminated last record joined to the first record of the transcript", good + strings.TrimSuffix(assistantLine("a"), "\n"), assistantLine("x"), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(good)) + " has no newline"},
		{"unterminated last record joined to the first record, in the kept part", turnLines(1, 1) + strings.TrimSuffix(assistantLine("a"), "\n"), assistantLine("x"), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(turnLines(1, 1))) + " has no newline"},
		// The joined line is checked before anything is cut, whatever the cut then drops.
		{"unterminated last record joined to a record, then cut away in the backup", turnLines(1, 1) + strings.TrimSuffix(assistantLine("a"), "\n"), turnLines(2, 2), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(turnLines(1, 1))) + " has no newline"},
		{"unterminated last record joined to a record, then cut away with the backup", turnLines(1, 1) + strings.TrimSuffix(assistantLine("a"), "\n"), turnLines(2, 3), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(turnLines(1, 1))) + " has no newline"},
		{"unterminated last user record joined to a user record", turnLines(1, 1) + strings.TrimSuffix(userLine("u2"), "\n"), userLine("u3"), "messages.backup.jsonl: record at byte " + fmt.Sprint(len(turnLines(1, 1))) + " has no newline"},
		{"a JSON value that is not an object", `"text"` + "\n", turnLines(3, 3), "messages.backup.jsonl: record at byte 0 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, backup := backupFiles(t, &tc.existing, tc.source)
			err := backupTranscript(source, backup, "append", 1, DurabilityProcess)
			if err == nil || !strings.Contains(err.Error(), tc.wantInError) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantInError)
			}
			if got := readTestFile(t, backup); got != tc.existing {
				t.Fatalf("the backup was changed: %q", got)
			}
			if got := readTestFile(t, source); got != tc.source {
				t.Fatalf("the transcript was changed: %q", got)
			}
			assertNoBackupTemporaries(t, filepath.Dir(backup))
		})
	}
	// The same files are carried over by replace and by append without a limit.
	source, backup := backupFiles(t, text("garbage\n"), turnLines(4, 4))
	if err := backupTranscript(source, backup, "replace", 1, DurabilityProcess); err != nil {
		t.Fatalf("replace: %v", err)
	}
}

func TestBackupTranscriptMissingFiles(t *testing.T) {
	// A missing transcript leaves the backup alone, with a limit or without.
	for _, limit := range []int{0, 3} {
		dir := t.TempDir()
		backup := filepath.Join(dir, "messages.backup.jsonl")
		writeTestFile(t, backup, turnLines(1, 5))
		if err := backupTranscript(filepath.Join(dir, "messages.jsonl"), backup, "append", limit, DurabilityProcess); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if got := readTestFile(t, backup); got != turnLines(1, 5) {
			t.Fatalf("limit %d: the backup changed to %q", limit, got)
		}
		assertNoBackupTemporaries(t, dir)
	}
	// A missing directory for the backup is created.
	dir := t.TempDir()
	source := filepath.Join(dir, "messages.jsonl")
	writeTestFile(t, source, turnLines(1, 3))
	backup := filepath.Join(dir, "deeper", "messages.backup.jsonl")
	if err := backupTranscript(source, backup, "append", 2, DurabilityProcess); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, backup); got != turnLines(2, 3) {
		t.Fatalf("backup = %q", got)
	}
	if err := backupTranscript("", backup, "append", 2, DurabilityProcess); err == nil {
		t.Fatal("empty source path accepted")
	}
}

// The turn rule is the one compaction uses: a record with role user that is not marked as
// steering starts a turn, and a steering record does not.
func TestBackupTurnWindowCountsUserRecordsAndSkipsSteering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	records := userLine("go") + assistantLine("a") + steerLine("also this") + assistantLine("b") + userLine("next")
	writeTestFile(t, path, records)
	window := newBackupTurnWindow(5)
	size, rest, err := scanBackupTurns(path, 0, window, nil)
	if err != nil || window.turns != 2 || size != int64(len(records)) || rest != nil {
		t.Fatalf("turns = %d, size = %d, rest = %+v, err = %v; want 2 turns", window.turns, size, rest, err)
	}
	if got := window.starts[1]; got.offset != int64(len(userLine("go")+assistantLine("a")+steerLine("also this")+assistantLine("b"))) {
		t.Fatalf("second turn starts at %+v", got)
	}
	// A steer mark that is false is a turn start.
	writeTestFile(t, path, strings.Replace(userLine("x"), `}]}`, `}],"`+SessionInputSteerField+`":false}`, 1))
	window = newBackupTurnWindow(5)
	if _, _, err := scanBackupTurns(path, 0, window, nil); err != nil || window.turns != 1 {
		t.Fatalf("steer false: turns = %d, err = %v", window.turns, err)
	}
}

// The window remembers the starts of the newest limit turns only, however long the backup.
func TestBackupTurnWindowIsBoundedByTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestFile(t, path, turnLines(1, 5000))
	window := newBackupTurnWindow(3)
	if _, _, err := scanBackupTurns(path, 0, window, nil); err != nil {
		t.Fatal(err)
	}
	if len(window.starts) != 3 || window.turns != 5000 {
		t.Fatalf("window = %d starts for %d turns", len(window.starts), window.turns)
	}
	cut, trimmed := window.cut()
	prefix := int64(len(turnLines(1, 4997)))
	if !trimmed || cut.offset != prefix {
		t.Fatalf("cut = %+v trimmed=%v, want the start of turn 4998 at byte %d", cut, trimmed, prefix)
	}
}

// compactionStore is a turn store with a transcript backup of the given mode and limit.
func compactionStore(t *testing.T) (*TurnStore, string, string) {
	t.Helper()
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	backup := filepath.Join(dir, "messages.backup.jsonl")
	journal, err := NewJournal(messages, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  backup,
		CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, messages, backup
}

func appendTurns(t *testing.T, store *TurnStore, first, last int) {
	t.Helper()
	for n := first; n <= last; n++ {
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"u` + fmt.Sprint(n) + `"}]}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactHistoryRollsTheAppendBackupByAppendLimit(t *testing.T) {
	store, _, backup := compactionStore(t)
	for round, first := range []int{1, 4, 7} {
		appendTurns(t, store, first, first+2)
		result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: 4})
		if err != nil || !result.Compacted {
			t.Fatalf("round %d: %+v, %v", round, result, err)
		}
	}
	got := readTestFile(t, backup)
	// Round 1 backs up u1..u3; round 2 sees the retained u3 and u4..u6; round 3 sees u6 and u7..u9.
	if turns := strings.Count(got, `"role":"user"`); turns != 4 {
		t.Fatalf("backup holds %d user records, want the newest 4:\n%s", turns, got)
	}
	for _, want := range []string{"u9", "u8", "u7", "u6"} {
		if !strings.Contains(got, `"`+want+`"`) {
			t.Errorf("backup misses %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"u1"`) || strings.Contains(got, `"u3"`) {
		t.Errorf("backup still holds old turns:\n%s", got)
	}
}

func TestCompactHistoryWithoutAppendLimitGrowsTheAppendBackup(t *testing.T) {
	store, _, backup := compactionStore(t)
	for _, first := range []int{1, 4, 7} {
		appendTurns(t, store, first, first+2)
		if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(readTestFile(t, backup), `"u1"`) {
		t.Fatalf("limit 0 dropped a turn:\n%s", readTestFile(t, backup))
	}
}

func TestCompactHistoryReplaceBackupIgnoresAppendLimit(t *testing.T) {
	store, _, backup := compactionStore(t)
	writeTestFile(t, backup, turnLines(1, 9))
	appendTurns(t, store, 10, 13)
	if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "replace", AppendLimit: 1}); err != nil {
		t.Fatal(err)
	}
	got := readTestFile(t, backup)
	if strings.Contains(got, `"a1"`) || strings.Count(got, `"role":"user"`) != 4 {
		t.Fatalf("replace backup = %s, want the four turns of the transcript only", got)
	}
}

// A corrupt backup stops the compaction with an append limit: the transcript, the cache
// and the backup are as they were, and the next compaction without the limit goes on.
func TestCompactHistoryStopsOnACorruptAppendBackup(t *testing.T) {
	store, messages, backup := compactionStore(t)
	corrupt := turnLines(1, 1) + "{oops\n"
	writeTestFile(t, backup, corrupt)
	appendTurns(t, store, 2, 4)
	before := readTestFile(t, messages)
	_, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: 2})
	if err == nil || !strings.Contains(err.Error(), "backup transcript before compaction") || !strings.Contains(err.Error(), "messages.backup.jsonl: record at byte") {
		t.Fatalf("CompactHistory() error = %v, want the backup error naming the file and byte", err)
	}
	if got := readTestFile(t, messages); got != before {
		t.Fatalf("the transcript changed:\n%s", got)
	}
	if got := readTestFile(t, backup); got != corrupt {
		t.Fatalf("the backup changed: %q", got)
	}
	if _, statErr := os.Stat(store.options.CompactionCachePath); !os.IsNotExist(statErr) {
		t.Fatalf("a compaction cache was written (%v)", statErr)
	}
	history, err := store.ReadInputItems()
	if err != nil || len(history) != 3 {
		t.Fatalf("history = %d items, %v; want the 3 turns", len(history), err)
	}
	if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"}); err != nil {
		t.Fatalf("without a limit the corrupt backup is carried along: %v", err)
	}
}

// Reset removes the backup whatever the limit.
func TestResetRemovesTheBackupOfAnAppendLimit(t *testing.T) {
	store, _, backup := compactionStore(t)
	appendTurns(t, store, 1, 3)
	if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("the backup survived the reset (%v)", err)
	}
}
