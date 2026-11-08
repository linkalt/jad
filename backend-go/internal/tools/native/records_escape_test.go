package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordsEscapeCases are the characters the writer must escape exactly once. U+2028 is
// written raw, matching the JavaScript-compatible output checked in records_json_test.go.
var recordsEscapeCases = []struct {
	name  string
	input string
}{
	{"quote", `a"b`},
	{"backslash", `back\slash`},
	{"newline", "line1\nline2"},
	{"tab", "t\tx"},
	{"carriage return", "r\rx"},
	{"backspace and form feed", "\b\f"},
	{"escape U+001B", "\x1b"},
	{"line separator U+2028", "\u2028"},
}

func TestRecordsStoreRoundTripsEscapedStrings(t *testing.T) {
	for _, tc := range recordsEscapeCases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "records")
			store := NewRecordsStore(root)
			store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
			settings := recordsTestSettings(root)
			value := recordsValueFromGo(map[string]any{"text": tc.input})
			created, err := store.CreateValue("note", value, recordsGoMap(value), settings)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Get("note", created.Ref, settings)
			if err != nil {
				t.Fatalf("Get after Create: %v", err)
			}
			if got := loaded.Data["text"]; got != tc.input {
				t.Fatalf("round-tripped text = %q, want %q", got, tc.input)
			}
		})
	}
}

func TestRecordsStoreUpdatesDoNotGrowEscapedStrings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	store.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	settings := recordsTestSettings(root)
	input := "q\" b\\ n\n t\t"
	value := recordsValueFromGo(map[string]any{"text": input})
	created, err := store.CreateValue("note", value, recordsGoMap(value), settings)
	if err != nil {
		t.Fatal(err)
	}
	ref, etag := created.Ref, created.ETag
	var size int
	for round := 0; round < 3; round++ {
		current, err := store.Get("note", ref, settings)
		if err != nil {
			t.Fatalf("round %d Get: %v", round, err)
		}
		updated := recordsValueFromGo(map[string]any{"text": current.Data["text"]})
		item, err := store.UpdateValue("note", ref, etag, updated, recordsGoMap(updated), settings)
		if err != nil {
			t.Fatalf("round %d UpdateValue: %v", round, err)
		}
		etag = item.ETag
		if round == 0 {
			size = len(item.rawText)
		} else if len(item.rawText) != size {
			t.Fatalf("round %d record is %d bytes, want %d", round, len(item.rawText), size)
		}
	}
	loaded, err := store.Get("note", ref, settings)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Data["text"]; got != input {
		t.Fatalf("text after updates = %q, want %q", got, input)
	}
}

// legacyRecord writes text as the pre-fix writer did: each escape's backslash is
// doubled. The test writes the file directly so the fixture cannot drift with the fix.
func legacyRecord(t *testing.T, path, text string) {
	t.Helper()
	legacy := strings.NewReplacer(`\`, `\\`).Replace(text)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsStoreReadsAndRepairsLegacyEscapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	settings := recordsTestSettings(root)
	name := "note-20260101T000000.000Z-0000000000000001-11111111-1111-4111-8111-111111111111.json"
	ref := "note/" + name
	path := filepath.Join(root, "note", name)
	legacyRecord(t, path, "{\n  \"text\": \"a\\\"b\"\n}\n")
	if _, err := store.Get("note", ref, settings); err != nil {
		t.Fatalf("Get of legacy record: %v", err)
	}
	repaired, err := store.RepairLegacyRecord("note", ref, settings)
	if err != nil || !repaired {
		t.Fatalf("RepairLegacyRecord = %v, %v; want true, nil", repaired, err)
	}
	loaded, err := store.Get("note", ref, settings)
	if err != nil {
		t.Fatalf("Get after repair: %v", err)
	}
	if got := loaded.Data["text"]; got != `a"b` {
		t.Fatalf("repaired text = %q, want %q", got, `a"b`)
	}
	if _, err := os.Lstat(path + ".legacy-backup"); !os.IsNotExist(err) {
		t.Fatalf("backup left behind: %v", err)
	}
	again, err := store.RepairLegacyRecord("note", ref, settings)
	if err != nil || again {
		t.Fatalf("second repair = %v, %v; want false, nil", again, err)
	}
}

func TestRecordsStoreRepairLeavesUnprovenRecordsAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store := NewRecordsStore(root)
	settings := recordsTestSettings(root)
	name := "note-20260101T000000.000Z-0000000000000002-22222222-2222-4222-8222-222222222222.json"
	ref := "note/" + name
	path := filepath.Join(root, "note", name)
	garbage := []byte("{\"text\": not json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RepairLegacyRecord("note", ref, settings); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("repair of unprovable record error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(garbage) {
		t.Fatalf("unprovable record changed to %q, error %v", after, err)
	}
	if _, err := os.Lstat(path + ".legacy-backup"); !os.IsNotExist(err) {
		t.Fatalf("backup written for unprovable record: %v", err)
	}
}
