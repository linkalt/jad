package native

import (
	"context"
	"runtime"
	"sync"
	"time"

	"fmt"
	"jad/backend-go/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func detailTranscript(t *testing.T, result string) string {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	reasoning := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "weighing options"}}}
	writeTestTranscript(t, path,
		userLine("do the thing"),
		answerLine(1, 1, reasoning, callItem("c1", "read", `{"path":"a.go"}`)),
		resultLine("c1", result),
		answerLine(1, 2, textItem("all done")),
		userLine("second"),
		answerLine(2, 1, textItem("ok")),
	)
	return path
}

func TestTranscriptDetailShowsACallWithItsResultWhole(t *testing.T) {
	result := envelope("ok", "the whole result") + "\n" + strings.Repeat("line of output\n", 50)
	out := runTranscript(t, transcriptTestSettings(), detailTranscript(t, result), map[string]any{"step": "1.2"})
	for _, want := range []string{"step 1.2, part both; bytes 0-", "== 1.2 call read [c1] ok ", `arguments: {"path":"a.go"}`, "result:\n" + result} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "all done") || strings.Contains(out, "weighing") || cursorOf(out) != "" {
		t.Errorf("only the asked step, no cursor: %s", out)
	}
}

func TestTranscriptDetailPartsSelectCallResultOrReasoning(t *testing.T) {
	path := detailTranscript(t, "RESULT-TEXT")
	call := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.2", "part": "call"})
	result := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.2", "part": "result"})
	if !strings.Contains(call, "arguments:") || strings.Contains(call, "RESULT-TEXT") || strings.Contains(call, "result:") {
		t.Errorf("call: %s", call)
	}
	if strings.Contains(result, "arguments:") || !strings.Contains(result, "RESULT-TEXT") {
		t.Errorf("result: %s", result)
	}
	reasoning := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.1", "part": "reasoning"})
	if !strings.Contains(reasoning, "weighing options") || !strings.Contains(reasoning, "== 1.1 reasoning") {
		t.Errorf("reasoning: %s", reasoning)
	}
	if out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.1"}); !strings.Contains(out, "no_such_step") || strings.Contains(out, "weighing") {
		t.Errorf("reasoning is only shown on request: %s", out)
	}
	if out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.3", "part": "result"}); !strings.Contains(out, "no_such_step") {
		t.Errorf("a message has no result part: %s", out)
	}
}

func TestTranscriptDetailRangeCrossesTurnsAndSkipsReasoning(t *testing.T) {
	out := runTranscript(t, transcriptTestSettings(), detailTranscript(t, "R"), map[string]any{"step": "1.0-2.1"})
	for _, want := range []string{"== 1.0 user\ndo the thing", "== 1.2 call read", "== 1.3 assistant\nall done", "== 2.0 user\nsecond", "== 2.1 assistant\nok"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "weighing") || strings.Contains(out, "== 1.1") {
		t.Errorf("reasoning only on request: %s", out)
	}
	both := runTranscript(t, transcriptTestSettings(), detailTranscript(t, "R"), map[string]any{"step": "1.1-1.2", "part": "reasoning"})
	if !strings.Contains(both, "weighing") || strings.Contains(both, "== 1.2") {
		t.Errorf("reasoning range: %s", both)
	}
}

func TestTranscriptDetailPagesABigResultWithoutLosingAByte(t *testing.T) {
	result := envelope("ok", "big") + "\n"
	for i := 0; i < 400; i++ {
		result += fmt.Sprintf("row %03d héllo ✓ 日本語\n", i)
	}
	path := detailTranscript(t, result)
	whole := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.2", "part": "result"})
	settings := transcriptTestSettings()
	settings.MaxDetailBytes = 500
	var joined strings.Builder
	arguments := map[string]any{"step": "1.2", "part": "result"}
	pages := 0
	for ; pages < 200; pages++ {
		out := runTranscript(t, settings, path, arguments)
		_, body, _ := strings.Cut(out, "encoding=utf8 ---\n")
		body, _, _ = strings.Cut(body, "\n--- END CONTENT")
		if !utf8.ValidString(body) || len(body) > 500 {
			t.Fatalf("page %d: %d bytes, valid=%v", pages, len(body), utf8.ValidString(body))
		}
		joined.WriteString(body)
		if !strings.Contains(out, "bytes ") {
			t.Fatalf("no byte range in the header: %s", out)
		}
		cursor := cursorOf(out)
		if cursor == "" {
			break
		}
		if !strings.Contains(out, "truncated: true") {
			t.Fatalf("a cut page says so: %s", out)
		}
		arguments = map[string]any{"cursor": cursor}
	}
	_, wholeBody, _ := strings.Cut(whole, "encoding=utf8 ---\n")
	wholeBody, _, _ = strings.Cut(wholeBody, "\n--- END CONTENT")
	if pages < 10 || joined.String() != wholeBody {
		t.Fatalf("%d pages; joined %d bytes, whole %d bytes", pages+1, joined.Len(), len(wholeBody))
	}
}

func TestTranscriptDetailPageNeverSplitsARuneEvenWithATinyBudget(t *testing.T) {
	text := "日本語😀é"
	for size := 1; size < 8; size++ {
		var got strings.Builder
		for offset := 0; offset < len(text); {
			page, next := pageOfText(text, offset, size)
			if next <= offset || !utf8.ValidString(page) {
				t.Fatalf("size %d offset %d: %q next %d", size, offset, page, next)
			}
			got.WriteString(page)
			offset = next
		}
		if got.String() != text {
			t.Fatalf("size %d: %q", size, got.String())
		}
	}
}

func TestTranscriptDetailRejectsBadAndConflictingArguments(t *testing.T) {
	path := detailTranscript(t, "R")
	for _, tc := range []struct {
		arguments map[string]any
		code      string
	}{
		{map[string]any{"step": "3"}, "invalid_step"},
		{map[string]any{"step": "1.x"}, "invalid_step"},
		{map[string]any{"step": "1.3-1.1"}, "invalid_step"},
		{map[string]any{"step": "9.1"}, "no_such_step"},
		{map[string]any{"step": "1.99"}, "no_such_step"},
		{map[string]any{"step": "1.2", "turns": "1"}, "conflicting_selectors"},
		{map[string]any{"step": "1.2", "role": "tool"}, "conflicting_selectors"},
		{map[string]any{"step": "1.2", "search": "x"}, "conflicting_selectors"},
		{map[string]any{"part": "call"}, "conflicting_selectors"},
	} {
		out := runTranscript(t, transcriptTestSettings(), path, tc.arguments)
		if !strings.Contains(out, tc.code) {
			t.Errorf("%v: want %s, got %s", tc.arguments, tc.code, out)
		}
	}
}

func TestTranscriptDetailCursorIsStaleAfterACompactionAndRefusesNewSelectors(t *testing.T) {
	path := detailTranscript(t, strings.Repeat("r", 3000))
	settings := transcriptTestSettings()
	settings.MaxDetailBytes = 400
	cursor := cursorOf(runTranscript(t, settings, path, map[string]any{"step": "1.2"}))
	if cursor == "" {
		t.Fatal("expected a cursor")
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor, "step": "1.2"}); !strings.Contains(out, "conflicting_selectors") {
		t.Errorf("step with a cursor: %s", out)
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor}); strings.Contains(out, "cursor_stale") || !strings.Contains(out, "bytes 400-") {
		t.Errorf("the cursor continues: %s", out)
	}
	other := filepath.Join(filepath.Dir(path), "other")
	writeTestTranscript(t, other, userLine("different head"), answerLine(9, 1, textItem("x")))
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if out := runTranscript(t, settings, path, map[string]any{"cursor": cursor}); !strings.Contains(out, "cursor_stale") {
		t.Errorf("want cursor_stale: %s", out)
	}
}

func TestTranscriptDetailReplacesDataURLsAndShowsAnOrphanResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	image := "data:image/png;base64," + strings.Repeat("QUJD", 100)
	writeTestTranscript(t, path, userLine("q"), resultLine("gone", "orphan "+image))
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.1"})
	if !strings.Contains(out, "== 1.1 result [gone]") || !strings.Contains(out, "[data URL") || strings.Contains(out, "QUJD") {
		t.Fatalf("out = %s", out)
	}
}

func TestTranscriptDetailKeepsTheTurnNumbersOfACompactedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("kept"), answerLine(41, 1, textItem("answer")))
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "41.1"})
	if !strings.Contains(out, "== 41.1 assistant\nanswer") {
		t.Fatalf("a compacted transcript keeps its turn numbers: %s", out)
	}
	if out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.1"}); !strings.Contains(out, "no_such_step") {
		t.Fatalf("turn 1 is not in this file: %s", out)
	}
}

func TestTranscriptListAndDetailReadWhileARunAppendsWithoutRaces(t *testing.T) {
	path := detailTranscript(t, strings.Repeat("r", 2000))
	settings := transcriptTestSettings()
	settings.MaxDetailBytes = 300
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer file.Close()
		for i := 0; i < 200; i++ {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			_, _ = file.WriteString(answerLine(2, 2+i, textItem(fmt.Sprintf("late %d", i))) + "\n")
		}
	}()
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 30 {
				if out := runTranscript(t, settings, path, map[string]any{"step": "1.2"}); !strings.Contains(out, "== 1.2 call read") {
					t.Errorf("detail: %s", out)
					return
				}
				if out := runTranscript(t, settings, path, map[string]any{"turns": "1"}); !strings.Contains(out, "1.2 call read unknown 2000B") {
					t.Errorf("list: %s", out)
					return
				}
			}
		}()
	}
	group.Wait()
	close(stop)
	<-done
}

func TestTranscriptDetailScanStopsOnCancelAndLeaksNoGoroutine(t *testing.T) {
	path := detailTranscript(t, "R")
	before := runtime.NumGoroutine()
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		tool, _ := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{Messages: path}, nil, nil)
		if out, _ := tool.Execute(ctx, map[string]any{"step": "1.2"}, tools.ExecutionContext{CallID: "c"}); !strings.Contains(out, "transcript_cancelled") {
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

func TestTranscriptDetailNeverWritesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	content := userLine("q") + "\n" + answerLine(1, 1, textItem("done")) + "\n" + `{"type":"response.comple`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runTranscript(t, transcriptTestSettings(), path, map[string]any{"step": "1.1"}); !strings.Contains(out, "== 1.1 assistant\ndone") {
		t.Fatalf("out = %s", out)
	}
	if after, _ := os.ReadFile(path); string(after) != content {
		t.Fatalf("the reader changed the transcript: %q", after)
	}
}
