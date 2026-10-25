package native

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func TestExecWritesTheInputAsJSONToStdin(t *testing.T) {
	requireInterpreters(t, "python3", "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	result := runExec(t, root, settings, bash, map[string]any{
		"lang": "python", "code": "import json,sys\nd=json.load(sys.stdin)\nprint(d['n']+1, d['s'])",
		"input": map[string]any{"n": 41, "s": "é\"q"},
	})
	if got := execContent(result); got != "42 é\"q\n" {
		t.Fatalf("content = %q\n%s", got, result)
	}
	// Any JSON value, not only an object, and one line.
	result = runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "cat", "input": []any{1, "a"}})
	if got := execContent(result); got != "[1,\"a\"]\n" {
		t.Fatalf("array input = %q", got)
	}
}

func TestExecWithoutInputGivesAnEmptyStdin(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "cat | wc -c"})
	if got := strings.TrimSpace(execContent(result)); got != "0" {
		t.Fatalf("stdin bytes = %q\n%s", got, result)
	}
}

func TestExecArgModeKeepsQuotesAndPutsArgsAfterTheCode(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	code := "echo \"it's \\\"quoted\\\" $1 $2\"; echo \"$0\""
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{
		"lang": "bash", "code": code, "args": []any{"a b", "$HOME"},
	})
	if got, want := execContent(result), "it's \"quoted\" a b $HOME\nexec\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestExecRefusesCodeOverTheArgLimitWithoutRunningIt(t *testing.T) {
	root := t.TempDir()
	settings := execTestSettings()
	settings.ArgMaxBytes = 10
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "echo 0123456789"})
	if execField(result, "error_code") != "code_too_large" {
		t.Fatalf("result = %s", result)
	}
}

func TestExecRefusesInputOverTheLimitAndInputForAStdinRunner(t *testing.T) {
	root := t.TempDir()
	settings := execTestSettings()
	settings.InputMaxBytes = 8
	big := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "cat", "input": "0123456789"})
	if execField(big, "error_code") != "input_too_large" {
		t.Fatalf("big input = %s", big)
	}
	settings = execTestSettings()
	settings.LangNames = append(settings.LangNames, "legacy")
	settings.Langs["legacy"] = sessionfs.NativeExecLang{Argv: []string{"bash", "-s", "--"}, Mode: sessionfs.NativeExecModeStdin, Aliases: []string{}}
	refused := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "legacy", "code": "cat", "input": 1})
	if execField(refused, "error_code") != "input_unsupported" || !strings.Contains(execField(refused, "suggestion"), "runners.legacy.mode") {
		t.Fatalf("stdin runner with input = %s", refused)
	}
	requireInterpreters(t, "bash")
	ok := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "legacy", "code": "echo from-stdin"})
	if got := execContent(ok); got != "from-stdin\n" {
		t.Fatalf("stdin runner without input = %q\n%s", got, ok)
	}
}

func TestExecJadResultIsReadBack(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "echo noise; echo answer > \"$JAD_RESULT\"; echo \"old=${EXEC_RESULT-unset}\""})
	got := execContent(result)
	if !strings.HasPrefix(got, "[result]\nanswer\n") || !strings.Contains(got, "old=unset") {
		t.Fatalf("content = %q", got)
	}
}

func TestExecOldNamesAreRefusedWithTheRightKey(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings := execTestSettings()
	for _, tc := range []struct {
		old   string
		value any
		want  string
	}{
		{"command", "ls", "use code with lang"},
		{"language", "node", "use lang"},
		{"source", "ls", "use code"},
		{"script", "ls", "use code"},
		{"program", "ls", "use code"},
		{"stdin", "x", "use input"},
		{"workdir", ".", "use cwd"},
		{"argv", []any{"ls"}, "use args"},
	} {
		got := callExecThroughTheCatalog(t, root, settings, `{"code":"echo hi","`+tc.old+`":`+jsonText(tc.value)+`}`)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "hi\n") {
			t.Errorf("%s: %s", tc.old, got)
		}
	}
}

func jsonText(value any) string {
	switch v := value.(type) {
	case string:
		return "\"" + v + "\""
	case []any:
		return "[\"" + v[0].(string) + "\"]"
	}
	return "null"
}

func countExecScratch(t *testing.T, tmp string) int {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(tmp, "exec-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestExecRemovesItsScratchDirectoryOnEveryExit(t *testing.T) {
	requireInterpreters(t, "bash")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	settings.LangNames = append(settings.LangNames, "viafile")
	settings.Langs["viafile"] = sessionfs.NativeExecLang{Argv: []string{"bash", "{file}"}, Mode: sessionfs.NativeExecModeFile, Ext: ".sh", Aliases: []string{}}
	for name, arguments := range map[string]map[string]any{
		"success":  {"lang": "bash", "code": "echo ok"},
		"failure":  {"lang": "bash", "code": "exit 3"},
		"file":     {"lang": "viafile", "code": "echo ok"},
		"timeout":  {"lang": "bash", "code": "sleep 30", "timeout_ms": 200},
		"refused":  {"lang": "bash", "code": "echo", "input": make([]any, 5000)},
		"bad args": {"lang": "bash", "code": "echo", "args": "x"},
	} {
		settings.InputMaxBytes = 4096
		runExec(t, root, settings, bash, arguments)
		if left := countExecScratch(t, tmp); left != 0 {
			t.Errorf("%s: %d scratch directories left", name, left)
		}
	}
}

// A program that never reads its stdin must not keep the writer goroutine alive.
func TestExecDoesNotLeakTheStdinWriterWhenTheProgramNeverReads(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	settings.InputMaxBytes = 4 << 20
	big := strings.Repeat("x", 1<<20)
	before := runtime.NumGoroutine()
	for range 5 {
		result := runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "echo done", "input": big})
		if got := execContent(result); got != "done\n" {
			t.Fatalf("content = %q\n%s", got, result)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestExecInputWorksThroughAHandle(t *testing.T) {
	f := newExecAsyncFixture(t)
	started := f.run(t, "s1", `{"code":"sleep 0.3; cat","input":{"k":1},"block_ms":10}`)
	execRunIDOf(t, started)
	waited := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, `{\"k\":1}`) && !strings.Contains(waited, `{"k":1}`) {
		t.Fatalf("the handle lost its input:\n%s", waited)
	}
}
