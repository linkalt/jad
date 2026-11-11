package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleTranscriptManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "transcript.json")

func TestBuildCatalogLoadsTranscriptFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleTranscriptManifest},
		NativeTranscript:    &sessionfs.NativeTranscriptSettings{AllowChildren: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != transcriptToolID || definitions[0].Name != "transcript" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestBuildCatalogNeedsTheTranscriptManifest(t *testing.T) {
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(t.TempDir(), "setting.json"), NativeTranscript: &sessionfs.NativeTranscriptSettings{}})
	if err == nil || !strings.Contains(err.Error(), "tools.native.transcript is configured but native.transcript is missing from tool_definition_paths") {
		t.Fatalf("error = %v", err)
	}
}

func transcriptTestSettings() sessionfs.NativeTranscriptSettings {
	return sessionfs.NativeTranscriptSettings{
		ResponseMaxBytes: 64 * 1024, PreviewBytes: 200, ValueBytes: 80, MaxListEntries: 100, MaxDetailBytes: 16384, LineMaxBytes: 1 << 20,
		ScanMaxBytes: 1 << 30, SuggestMax: 20, TimeoutMs: 30000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true,
	}
}

// userLine, answerLine and resultLine are the transcript line shapes a run writes.
func userLine(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}, "__agent_session_input_batch_id": "b", "__agent_session_input_batch_index": 0})
	return string(raw)
}

func answerLine(turn, step int, output ...any) string {
	raw, _ := json.Marshal(map[string]any{"type": "response.completed", "session_turn": turn, "session_step": step, "response": map[string]any{"id": "r", "output": output}})
	return string(raw)
}

func textItem(text string) any {
	return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func callItem(id, name, arguments string) any {
	return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments}
}

func resultLine(id, output string) string {
	raw, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": id, "output": output})
	return string(raw)
}

func writeTestTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTranscript(t *testing.T, settings sessionfs.NativeTranscriptSettings, path string, arguments map[string]any) string {
	t.Helper()
	tool, err := NewTranscriptTool(settings, TranscriptFiles{Messages: path}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(t.Context(), arguments, tools.ExecutionContext{CallID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func threeTurns(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path,
		userLine("first question"),
		answerLine(1, 1, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "private musings"}}}, callItem("call_a", "read", `{"path":"a.go"}`)),
		resultLine("call_a", "package a"),
		answerLine(1, 2, textItem("first answer")),
		userLine("second question"),
		answerLine(2, 1, textItem("second answer")),
		userLine("third question"),
		answerLine(3, 1, textItem("third answer")),
	)
	return path
}

func TestTranscriptListsTheWholeTranscriptOldestStepFirstByDefault(t *testing.T) {
	out := runTranscript(t, transcriptTestSettings(), threeTurns(t), map[string]any{})
	if !strings.Contains(out, "turns 1-3; the transcript holds turns 1-3; oldest step first in the page") {
		t.Errorf("header: %s", out)
	}
	want := []string{"1.0 user", "1.2 call read", "1.3 assistant", "2.0 user", "2.1 assistant", "3.0 user", "3.1 assistant"}
	got := stepLines(out)
	if len(got) != len(want) {
		t.Fatalf("lines = %q", got)
	}
	for index := range want {
		if !strings.HasPrefix(got[index], want[index]) {
			t.Errorf("line %d = %q, want prefix %q", index, got[index], want[index])
		}
	}
}

func TestTranscriptTurnRanges(t *testing.T) {
	path := threeTurns(t)
	for _, tc := range []struct {
		arguments map[string]any
		want      []string
		not       []string
	}{
		{map[string]any{"turns": "1"}, []string{"1.0 user: first question", "1.2 call read ok 9B {\"path\":\"a.go\"}", "1.3 assistant: first answer"}, []string{"second", "private musings", "package a", "1.1"}},
		{map[string]any{"turns": "2-3"}, []string{"second answer", "third answer"}, []string{"first question", "first answer"}},
		{map[string]any{"turns": "-2"}, []string{"second question", "third question"}, []string{"first question", "first answer"}},
		{map[string]any{"turns": "2-"}, []string{"second question", "third answer"}, []string{"first question", "first answer"}},
	} {
		out := runTranscript(t, transcriptTestSettings(), path, tc.arguments)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%v: missing %q in %s", tc.arguments, want, out)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(out, not) {
				t.Errorf("%v: must not contain %q: %s", tc.arguments, not, out)
			}
		}
	}
}

func TestTranscriptRejectsBadSelectors(t *testing.T) {
	path := threeTurns(t)
	for _, tc := range []struct {
		arguments map[string]any
		code      string
	}{
		{map[string]any{"turns": "x"}, "invalid_turns"},
		{map[string]any{"turns": "5-2"}, "invalid_turns"},
		{map[string]any{"turns": "9"}, "no_such_turns"},
		{map[string]any{"turns": "1", "cursor": "x"}, "conflicting_selectors"},
		{map[string]any{"cursor": "!!"}, "invalid_cursor"},
	} {
		if out := runTranscript(t, transcriptTestSettings(), path, tc.arguments); !strings.Contains(out, tc.code) {
			t.Errorf("%v: want %s, got %s", tc.arguments, tc.code, out)
		}
	}
}

func TestTranscriptNumbersACompactedTranscriptByItsSessionTurnAndSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("kept"), answerLine(41, 1, textItem("kept answer")))
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{})
	if !strings.Contains(out, "41.0 user: kept") || !strings.Contains(out, "compacted") || !strings.Contains(out, "turns 41-41; the transcript holds turns 41-41") {
		t.Fatalf("out = %s", out)
	}
	if out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "41"}); !strings.Contains(out, "kept answer") {
		t.Fatalf("turn 41 not found: %s", out)
	}
}

func TestTranscriptCutsLongPreviewsAndValuesInTheMiddleAndHidesDataURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	image := "data:image/png;base64," + strings.Repeat("QUJD", 400)
	writeTestTranscript(t, path, userLine(strings.Repeat("h", 500)+strings.Repeat("t", 500)),
		answerLine(1, 1, callItem("c", "exec", `{"code":"`+strings.Repeat("x", 9000)+`","note":"short"}`)), resultLine("c", "see "+image))
	settings := transcriptTestSettings()
	settings.PreviewBytes, settings.ValueBytes = 100, 40
	out := runTranscript(t, settings, path, map[string]any{})
	if !strings.Contains(out, "…+") || strings.Contains(out, "QUJD") || strings.Contains(out, strings.Repeat("x", 50)) || !strings.Contains(out, `"note":"short"`) || !strings.Contains(out, `{"code":"xxx`) {
		t.Fatalf("out = %s", out)
	}
	if !strings.Contains(out, "1.0 user: hhh") || !strings.Contains(out, "ttt\n") && !strings.Contains(out, "ttt") {
		t.Fatalf("user preview keeps head and tail: %s", out)
	}
	if strings.Contains(out, "see data") || strings.Contains(out, "[data URL") {
		t.Fatalf("a result is not listed: %s", out)
	}
}

func cursorOf(out string) string {
	_, rest, ok := strings.Cut(out, "cursor: ")
	if !ok {
		return ""
	}
	cursor, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(cursor)
}

func TestTranscriptPagesWithACursorWithoutLosingOrRepeatingAnEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	lines := []string{userLine("start")}
	for step := 1; step <= 30; step++ {
		lines = append(lines, answerLine(1, step, textItem(fmt.Sprintf("entry-%02d %s", step, strings.Repeat("p", 80)))))
	}
	writeTestTranscript(t, path, lines...)
	unbounded := runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "1"})
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 400
	var seen []string
	arguments := map[string]any{"turns": "1"}
	for page := 0; page < 40; page++ {
		out := runTranscript(t, settings, path, arguments)
		for step := 30; step >= 1; step-- {
			if marker := fmt.Sprintf("entry-%02d ", step); strings.Contains(out, marker) {
				seen = append(seen, marker)
			}
		}
		cursor := cursorOf(out)
		if cursor == "" {
			break
		}
		arguments = map[string]any{"cursor": cursor}
	}
	if len(seen) != 30 {
		t.Fatalf("paged %d entries, want 30: %v", len(seen), seen)
	}
	for index, marker := range seen {
		if want := fmt.Sprintf("entry-%02d ", 30-index); marker != want || !strings.Contains(unbounded, want) {
			t.Fatalf("entry %d = %q, want %q", index, marker, want)
		}
	}
}

func TestTranscriptCursorIsStaleAfterACompactionOrReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	lines := []string{userLine("start")}
	for step := 1; step <= 10; step++ {
		lines = append(lines, answerLine(1, step, textItem(strings.Repeat("p", 200))))
	}
	writeTestTranscript(t, path, lines...)
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 500
	cursor := cursorOf(runTranscript(t, settings, path, map[string]any{"turns": "1"}))
	if cursor == "" {
		t.Fatal("expected a cursor")
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor}); strings.Contains(out, "cursor_stale") {
		t.Fatalf("an unchanged transcript must accept its cursor: %s", out)
	}
	// A compaction replaces the file with a different head.
	compacted := filepath.Join(filepath.Dir(path), "next")
	writeTestTranscript(t, compacted, userLine("a different first record"), answerLine(9, 1, textItem("short")))
	if err := os.Rename(compacted, path); err != nil {
		t.Fatal(err)
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor}); !strings.Contains(out, "cursor_stale") {
		t.Fatalf("want cursor_stale after compaction, got %s", out)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor}); !strings.Contains(out, "transcript is empty") && !strings.Contains(out, "cursor_stale") {
		t.Fatalf("reset: %s", out)
	}
}

func TestTranscriptNeverWritesAndLeavesAnIncompleteLastLineAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	content := userLine("q") + "\n" + answerLine(1, 1, textItem("done")) + "\n" + `{"type":"response.comple`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{})
	if !strings.Contains(out, "done") {
		t.Fatalf("out = %s", out)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != content {
		t.Fatalf("the reader changed the transcript: %q (err %v)", after, err)
	}
	info, _ := os.Stat(path)
	if !info.ModTime().Equal(past) {
		t.Fatalf("the reader touched the transcript: mtime %v, want %v", info.ModTime(), past)
	}
}

func TestTranscriptSkipsALineOverLineMaxBytesAndSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"), answerLine(1, 1, textItem(strings.Repeat("z", 5000))), answerLine(1, 2, textItem("after")))
	settings := transcriptTestSettings()
	settings.LineMaxBytes = 1000
	out := runTranscript(t, settings, path, map[string]any{})
	if !strings.Contains(out, "skipped: over tools.native.transcript.line_max_bytes") || !strings.Contains(out, "after") {
		t.Fatalf("out = %s", out)
	}
}

func TestTranscriptRefusesATranscriptOverScanMaxBytes(t *testing.T) {
	settings := transcriptTestSettings()
	settings.ScanMaxBytes = 10
	if out := runTranscript(t, settings, threeTurns(t), map[string]any{}); !strings.Contains(out, "transcript_too_large") {
		t.Fatalf("out = %s", out)
	}
}

func TestTranscriptReadsAHeldFileWhileACompactionReplacesIt(t *testing.T) {
	path := threeTurns(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	replaced := false
	var visited int
	err = scanTranscript(t.Context(), file, 0, info.Size(), 1<<20, func(line transcriptLine) (bool, error) {
		if !replaced {
			replaced = true
			next := path + ".next"
			writeTestTranscript(t, next, userLine("new"))
			if err := os.Rename(next, path); err != nil {
				return false, err
			}
		}
		visited++
		return true, nil
	})
	if err != nil || visited != 8 {
		t.Fatalf("visited %d lines of the old file, err %v; want 8", visited, err)
	}
}

func TestTranscriptReadsWhileARunAppendsWithoutRaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"))
	appender, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer appender.Close()
	var group sync.WaitGroup
	stop := make(chan struct{})
	group.Add(1)
	go func() {
		defer group.Done()
		for step := 1; step <= 2000; step++ {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
			line := answerLine(1, step, textItem("appended")) + "\n"
			half := len(line) / 2
			_, _ = appender.WriteString(line[:half])
			_, _ = appender.WriteString(line[half:])
		}
	}()
	for range 50 {
		out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "1"})
		if strings.Contains(out, "unreadable") || strings.Contains(out, "error_code") {
			t.Fatalf("a read beside a writer failed: %s", out)
		}
	}
	close(stop)
	group.Wait()
}

func TestTranscriptScanStopsOnCancelAndLeaksNoGoroutine(t *testing.T) {
	path := threeTurns(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	before := runtime.NumGoroutine()
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := scanTranscript(ctx, file, 0, info.Size(), 1<<20, func(transcriptLine) (bool, error) { return true, nil }); err == nil {
			t.Fatal("a cancelled scan must report the context error")
		}
		tool, _ := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{Messages: path}, nil, nil)
		if out, _ := tool.Execute(ctx, map[string]any{}, tools.ExecutionContext{CallID: "c"}); !strings.Contains(out, "transcript_cancelled") {
			t.Fatalf("out = %s", out)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestTranscriptRefusesAChildWhenAllowChildrenIsOff(t *testing.T) {
	settings := transcriptTestSettings()
	settings.AllowChildren = false
	tool, err := NewTranscriptTool(settings, TranscriptFiles{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(t.Context(), map[string]any{}, tools.ExecutionContext{CallID: "c1", AgentDepth: 1})
	if err != nil || !strings.Contains(out, "transcript_children_denied") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestTranscriptIsAReadOnlyToolSoAnAgentCanListIt(t *testing.T) {
	tool, err := NewTranscriptTool(sessionfs.NativeTranscriptSettings{}, TranscriptFiles{}, nil, nil)
	if err != nil || !tool.ReadOnly {
		t.Fatalf("tool = %+v, err = %v", tool, err)
	}
}

func filterTranscript(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path,
		userLine("please find the Needle"),
		answerLine(1, 1, callItem("c1", "read", `{"path":"a.go"}`)),
		resultLine("c1", "package a // has a needle inside"),
		answerLine(1, 2, callItem("c2", "exec", `{"code":"ls"}`)),
		resultLine("c2", "file1 file2"),
		answerLine(1, 3, textItem("nothing else")),
	)
	return path
}

func TestTranscriptFiltersByRole(t *testing.T) {
	path := filterTranscript(t)
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"role": "tool"})
	if !strings.Contains(out, "call read") || !strings.Contains(out, "call exec") || strings.Contains(out, "user:") || strings.Contains(out, "assistant:") {
		t.Fatalf("role tool: %s", out)
	}
	out = runTranscript(t, transcriptTestSettings(), path, map[string]any{"role": "user"})
	if !strings.Contains(out, "user: please") || strings.Contains(out, "call read") {
		t.Fatalf("role user: %s", out)
	}
}

func TestTranscriptFiltersByToolNameForCallsAndTheirResults(t *testing.T) {
	path := filterTranscript(t)
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"tool": "ex*"})
	if !strings.Contains(out, "call exec") || strings.Contains(out, "call read") || strings.Contains(out, "user:") {
		t.Fatalf("tool ex*: %s", out)
	}
}

func TestTranscriptSearchesCaseInsensitivelyWithContext(t *testing.T) {
	path := filterTranscript(t)
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"search": "NEEDLE"})
	if !strings.Contains(out, "user: please find the Needle") || !strings.Contains(out, "call read") || strings.Contains(out, "has a needle inside") || strings.Contains(out, "call exec") || strings.Contains(out, "nothing else") {
		t.Fatalf("search: %s", out)
	}
	out = runTranscript(t, transcriptTestSettings(), path, map[string]any{"search": "file1", "context": json.Number("1")})
	if !strings.Contains(out, "call exec") || strings.Contains(out, "file1") || !strings.Contains(out, "nothing else") || !strings.Contains(out, "call read") || strings.Contains(out, "user:") {
		t.Fatalf("search with context: %s", out)
	}
}

func TestTranscriptSearchOnlyMatchesTheFilteredEntries(t *testing.T) {
	path := filterTranscript(t)
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"search": "needle", "role": "user"})
	if !strings.Contains(out, "user: please") || strings.Contains(out, "call read") {
		t.Fatalf("search + role: %s", out)
	}
}

func TestTranscriptReadsTheBackupAndAKeepsItsCursorOnIt(t *testing.T) {
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	backup := filepath.Join(dir, "messages.backup.jsonl")
	writeTestTranscript(t, messages, userLine("live"), answerLine(5, 1, textItem("live answer")))
	lines := []string{userLine("old")}
	for step := 1; step <= 12; step++ {
		lines = append(lines, answerLine(1, step, textItem(fmt.Sprintf("old-%02d %s", step, strings.Repeat("b", 100)))))
	}
	writeTestTranscript(t, backup, lines...)
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 400
	tool, err := NewTranscriptTool(settings, TranscriptFiles{Messages: messages, Backup: backup}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(arguments map[string]any) string {
		out, err := tool.Execute(t.Context(), arguments, tools.ExecutionContext{CallID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := run(map[string]any{"source": "backup", "turns": "1"})
	if !strings.Contains(first, "old-12") || strings.Contains(first, "live answer") {
		t.Fatalf("backup: %s", first)
	}
	cursor := cursorOf(first)
	if cursor == "" {
		t.Fatal("expected a cursor")
	}
	second := run(map[string]any{"cursor": cursor})
	if strings.Contains(second, "live answer") || !strings.Contains(second, "old-") {
		t.Fatalf("a cursor must stay on the file it was made for: %s", second)
	}
	if out := run(map[string]any{"cursor": cursor, "source": "messages"}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("source against the cursor: %s", out)
	}
}

func TestTranscriptBackupMissingSaysSo(t *testing.T) {
	path := threeTurns(t)
	tool, _ := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{Messages: path, Backup: path + ".none"}, nil, nil)
	out, _ := tool.Execute(t.Context(), map[string]any{"source": "backup"}, tools.ExecutionContext{CallID: "c"})
	if !strings.Contains(out, "no_transcript") || !strings.Contains(out, "backup") {
		t.Fatalf("out = %s", out)
	}
}
