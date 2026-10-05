package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAssistantFinalItemsPinsResultsAndErrors fixes what assistantFinalItems
// returns for each kind of journal record, including the error text, so that a
// change to how it parses records cannot alter either.
func TestAssistantFinalItemsPinsResultsAndErrors(t *testing.T) {
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}`
	answer := func(text string) string {
		return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
	}
	completed := func(status, output string) string {
		return `{"type":"response.completed","response":{"id":"r","status":"` + status + `","output":` + output + `}}`
	}
	cases := []struct {
		name    string
		records []string
		want    []string
		wantErr string
	}{
		{name: "empty", records: nil, want: nil},
		{name: "completed answer", records: []string{user, completed("completed", "["+answer("a")+"]")}, want: []string{answer("a")}},
		{name: "incomplete answer is not final", records: []string{user, completed("incomplete", "["+answer("a")+"]")}, want: nil},
		{name: "later answer of the same turn wins", records: []string{user, completed("completed", "["+answer("a")+"]"), completed("completed", "["+answer("b")+"]")}, want: []string{answer("b")}},
		{name: "function call supersedes the answer", records: []string{user, completed("completed", "["+answer("a")+"]"), completed("completed", `[{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]`)}, want: nil},
		{name: "one final per turn", records: []string{user, completed("completed", "["+answer("a")+"]"), user, completed("completed", "["+answer("b")+"]")}, want: []string{answer("a"), answer("b")}},
		{name: "whitespace is removed and key order is kept", records: []string{user, `{"type":"response.completed","response":{"id":"r","status":"completed","output":[ {"role":"assistant","type":"message","content":[{"text":"a","type":"output_text"}]} ]}}`}, want: []string{`{"role":"assistant","type":"message","content":[{"text":"a","type":"output_text"}]}`}},
		{name: "empty output", records: []string{user, completed("completed", "[]")}, want: nil},
		{name: "record without response", records: []string{user, `{"type":"response.completed"}`}, want: nil},
		{name: "record without output", records: []string{user, `{"type":"response.completed","response":{"id":"r","status":"completed"}}`}, want: nil},
		{name: "non response record", records: []string{user, `{"type":"function_call_output","call_id":"c","output":"ok"}`}, want: nil},
		{name: "response is not an object", records: []string{`{"type":"response.completed","response":[]}`}, wantErr: "expected a JSON object"},
		{name: "response is null", records: []string{`{"type":"response.completed","response":null}`}, wantErr: "expected a JSON object"},
		{name: "output is a string", records: []string{`{"type":"response.completed","response":{"status":"completed","output":"x"}}`}, wantErr: "expected a JSON array"},
		{name: "output is null", records: []string{`{"type":"response.completed","response":{"status":"completed","output":null}}`}, wantErr: "expected a JSON array"},
		{name: "output is an object", records: []string{`{"type":"response.completed","response":{"status":"completed","output":{}}}`}, wantErr: "expected a JSON array"},
		{name: "output item is a number", records: []string{`{"type":"response.completed","response":{"status":"completed","output":[1]}}`}, wantErr: "expected a JSON object"},
		{name: "output item is a string", records: []string{`{"type":"response.completed","response":{"status":"completed","output":["x"]}}`}, wantErr: "expected a JSON object"},
		{name: "output item is null", records: []string{`{"type":"response.completed","response":{"status":"completed","output":[null]}}`}, wantErr: "expected a JSON object"},
		{name: "record is not an object", records: nil, wantErr: "expected a JSON object"},
		{name: "assistant item with invalid utf8 is replaced", records: []string{user, "{\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"a\xffb\"}]}}"}, want: []string{"{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"a\ufffdb\"}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
				MaxItemBytes:        1 << 20,
				CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range tc.records {
				if err := journal.Append([]byte(record)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "record is not an object" {
				// Append refuses non-objects, so write the line directly.
				if err := os.WriteFile(journal.path, []byte("[1]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			previous, err := store.readCompactionCache()
			if err != nil {
				t.Fatal(err)
			}
			finals, err := store.assistantFinalItems(previous)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("assistantFinalItems() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("assistantFinalItems() error = %v", err)
			}
			got := make([]string, 0, len(finals))
			for item := range finals {
				got = append(got, item)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("assistantFinalItems() = %q, want %q", got, want)
			}
		})
	}
}

// TestSecondCompactionKeepsAssistantFinalFromThePreviousCache compacts twice.
// The first compaction moves the early turns, with their final answers, into
// the compaction cache; the second one must still recognize those answers as
// final, which only the cache can tell it.
func TestSecondCompactionKeepsAssistantFinalFromThePreviousCache(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	addTurn := func(name string) {
		t.Helper()
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question ` + name + `"}]}`)); err != nil {
			t.Fatal(err)
		}
		thinking := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working ` + name + `"}]}`
		final := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final ` + name + `"}]}`
		if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"a_` + name + `","status":"completed","output":[` + thinking + `,` + final + `]}}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"one", "two", "three", "four"} {
		addTurn(name)
	}
	options := CompactionOptions{TurnType: "assistant_final", KeepTurns: 3, BackupMode: "replace"}
	if result, err := store.CompactHistory(options); err != nil || !result.Compacted {
		t.Fatalf("first compaction = %+v, %v", result, err)
	}
	addTurn("five")
	options.KeepTurns = 3
	if result, err := store.CompactHistory(options); err != nil || !result.Compacted {
		t.Fatalf("second compaction = %+v, %v", result, err)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	var joined []string
	for _, item := range history {
		joined = append(joined, string(item))
	}
	all := strings.Join(joined, "\n")
	for _, name := range []string{"three", "four", "five"} {
		if !strings.Contains(all, "final "+name) {
			t.Errorf("history lost the final answer of turn %s:\n%s", name, all)
		}
	}
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		if strings.Contains(all, "working "+name) {
			t.Errorf("history kept the non-final answer of turn %s:\n%s", name, all)
		}
	}
	if strings.Contains(all, "question one") || strings.Contains(all, "question two") {
		t.Errorf("history kept a turn that should have been compacted away:\n%s", all)
	}
}
