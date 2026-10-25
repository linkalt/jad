package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func execTestSettings() sessionfs.NativeExecSettings {
	names := []string{"python", "node", "bash", "sh"}
	return sessionfs.NativeExecSettings{
		LangNames: names,
		Langs: map[string]sessionfs.NativeExecLang{
			"python": {Argv: []string{"python3", "-"}, Code: sessionfs.NativeExecCodeStdin, Ext: ".py", Aliases: []string{"python3", "py"}},
			"node":   {Argv: []string{"node", "-"}, Code: sessionfs.NativeExecCodeStdin, Ext: ".js", Aliases: []string{"js"}},
			"bash":   {Argv: []string{"bash", "-s", "--"}, Code: sessionfs.NativeExecCodeStdin, Ext: ".sh", Aliases: []string{"shell"}},
			"sh":     {Argv: []string{"sh", "-s", "--"}, Code: sessionfs.NativeExecCodeStdin, Ext: ".sh", Aliases: []string{}},
		},
		DefaultLang:    "bash",
		CodeMaxBytes:   64 * 1024,
		ResultMaxBytes: 1024,
		LineAware:      true,
	}
}

func requireInterpreters(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
}

func runExec(t *testing.T, root string, settings sessionfs.NativeExecSettings, bash sessionfs.NativeBashSettings, arguments map[string]any) string {
	t.Helper()
	normalized := normalizeExecArguments(arguments, settings.DefaultLang)
	settings.Child = bash
	return executeNativeExec(context.Background(), normalized, settings, "", tools.ExecutionContext{
		CallID: "exec-call", WorkspaceRoot: root, SessionDir: root,
	})
}

// execContent returns the text between the content fences of a rendered tool result.
func execContent(result string) string {
	marker := "--- CONTENT token="
	start := strings.Index(result, marker)
	if start < 0 {
		return ""
	}
	start = strings.Index(result[start:], "\n") + start + 1
	end := strings.Index(result, "\n--- END CONTENT")
	if end < 0 || end < start {
		return ""
	}
	return result[start:end]
}

// execField reads one `key: value` line of the information block, which ends at the first
// blank line after its header.
func execField(result, key string) string {
	_, block, found := strings.Cut(result, "--- INFORMATION ---\n")
	if !found {
		return ""
	}
	for _, line := range strings.Split(block, "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, key+": ") {
			return strings.TrimPrefix(line, key+": ")
		}
	}
	return ""
}

func TestExecRunsTheSmallestCallInEachBuiltInInterpreter(t *testing.T) {
	requireInterpreters(t, "python3", "node", "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	for _, tc := range []struct{ lang, code, want string }{
		{"python", "print(1 + 2)", "3\n"},
		{"node", "console.log(6 * 7)", "42\n"},
		{"bash", "echo $((6 * 7))", "42\n"},
		{"Python3", "print('p')", "p\n"},
		{" JS ", "console.log('j')", "j\n"},
	} {
		result := runExec(t, root, settings, bash, map[string]any{"lang": tc.lang, "code": tc.code})
		if got := execContent(result); got != tc.want || !strings.Contains(result, `"status":"ok"`) {
			t.Fatalf("%s: content %q, want %q\n%s", tc.lang, got, tc.want, result)
		}
	}
}

func TestExecPassesArgsAsArgvWithoutShellQuoting(t *testing.T) {
	requireInterpreters(t, "python3")
	root := t.TempDir()
	code := "import sys\nfor value in sys.argv[1:]:\n    print(repr(value))"
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{
		"lang": "python", "code": code, "args": []any{"a b", "$HOME", "it's", "x;y", ""},
	})
	if got, want := execContent(result), "'a b'\n'$HOME'\n\"it's\"\n'x;y'\n''\n"; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestExecReportsANonZeroExitWithItsOutput(t *testing.T) {
	requireInterpreters(t, "python3")
	root := t.TempDir()
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{
		"lang": "python", "code": "import sys\nprint('partial')\nsys.stderr.write('boom\\n')\nsys.exit(3)",
	})
	if !strings.Contains(result, `"status":"error"`) || execField(result, "error_code") != "nonzero_exit" || execField(result, "exit_code") != "3" {
		t.Fatalf("result = %s", result)
	}
	if got := execContent(result); got != "[stdout]\npartial\n[stderr]\nboom\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestExecFailureInTheDefaultLanguageSaysWhatRanAndHowToPickAnother(t *testing.T) {
	requireInterpreters(t, "python3", "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	// Python code that reached bash, as when a model assumes Python and leaves lang out.
	result := runExec(t, root, settings, bash, map[string]any{"code": "import json\nprint(json.dumps({'a': 1}))"})
	if !strings.Contains(result, `"status":"error"`) || execField(result, "error_code") != "nonzero_exit" {
		t.Fatalf("result = %s", result)
	}
	want := "ran as lang=bash, the default when lang is omitted; if the code is for another interpreter, pass lang (available: python, node, bash, sh)"
	if got := execField(result, "suggestion"); got != want {
		t.Fatalf("suggestion = %q, want %q", got, want)
	}
	// The same code with its own language works, so the advice leads somewhere.
	fixed := runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "import json\nprint(json.dumps({'a': 1}))"})
	if !strings.Contains(fixed, `"status":"ok"`) {
		t.Fatalf("python run = %s", fixed)
	}
}

func TestExecDefaultLanguageAdviceFollowsTheSettingAndIsAbsentOtherwise(t *testing.T) {
	requireInterpreters(t, "python3", "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	settings.DefaultLang = "python"
	failed := runExec(t, root, settings, bash, map[string]any{"code": "raise SystemExit(2)"})
	if got := execField(failed, "suggestion"); !strings.HasPrefix(got, "ran as lang=python, the default when lang is omitted;") {
		t.Fatalf("suggestion = %q\n%s", got, failed)
	}
	if ok := runExec(t, root, settings, bash, map[string]any{"code": "print(1)"}); execField(ok, "suggestion") != "" {
		t.Fatalf("a successful call carries advice: %s", ok)
	}
	// A call that names another language than the default is not advised about the default.
	if other := runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "exit 3"}); execField(other, "suggestion") != "" {
		t.Fatalf("a call in another language carries advice: %s", other)
	}
}

func TestExecShowsTheResultFileAheadOfTheRawOutput(t *testing.T) {
	requireInterpreters(t, "python3", "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	code := "import os\nprint('noisy line')\nopen(os.environ['EXEC_RESULT'], 'w').write('the answer is 42\\n')"
	if got, want := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": code})), "[result]\nthe answer is 42\n[output]\nnoisy line\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": `echo -n "just this" > "$EXEC_RESULT"`})); got != "[result]\njust this" {
		t.Fatalf("result only = %q", got)
	}
	// An empty result file is the same as none.
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": `echo shown; : > "$EXEC_RESULT"`})); got != "shown\n" {
		t.Fatalf("empty result file = %q", got)
	}
}

func TestExecCutsALongResultAtACharacterAndSaysHowMuchWasDropped(t *testing.T) {
	requireInterpreters(t, "python3")
	root := t.TempDir()
	settings := execTestSettings()
	// 11 is odd, so the cut lands in the middle of the sixth two-byte character.
	settings.ResultMaxBytes = 11
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{
		"lang": "python", "code": "import os\nopen(os.environ['EXEC_RESULT'], 'w').write('é' * 20)",
	})
	if got, want := execContent(result), "[result]\n"+strings.Repeat("é", 5); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if got := execField(result, "result_truncated_bytes"); got != "30" {
		t.Fatalf("result_truncated_bytes = %q", got)
	}
}

func TestExecRefusesCodeOverTheLimitWholeAndCountsBytes(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	settings.CodeMaxBytes = 20
	marker := filepath.Join(root, "must-not-exist")
	result := runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "touch " + marker + "; echo xxxxxxxxxxxx"})
	if execField(result, "error_code") != "code_too_large" {
		t.Fatalf("result = %s", result)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the program started although its code was refused")
	}
	settings.CodeMaxBytes = 12
	if execField(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": ": ✓✓✓✓✓"}), "error_code") != "code_too_large" {
		t.Fatal("5 three-byte characters are 15 bytes and must be over a limit of 12")
	}
	if got := runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": ": ✓✓✓"}); !strings.Contains(got, `"status":"ok"`) {
		t.Fatalf("3 three-byte characters are 9 bytes and must fit: %s", got)
	}
}

func TestExecListsTheAvailableLanguagesForAnUnknownOne(t *testing.T) {
	root := t.TempDir()
	result := runExec(t, root, execTestSettings(), bashTestSettings(t, root), map[string]any{"lang": "cobol", "code": "x"})
	if execField(result, "error_code") != "unknown_lang" || !strings.Contains(execField(result, "available"), "python, node, bash") {
		t.Fatalf("result = %s", result)
	}
}

func TestExecNamesTheSettingForAnInterpreterThatIsNotInstalled(t *testing.T) {
	root := t.TempDir()
	settings := execTestSettings()
	settings.LangNames = append(settings.LangNames, "ghost")
	settings.Langs["ghost"] = sessionfs.NativeExecLang{Argv: []string{"definitely-not-installed-xyz", "-"}, Code: sessionfs.NativeExecCodeStdin, Aliases: []string{}}
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "ghost", "code": "x"})
	if execField(result, "error_code") != "interpreter_unavailable" || execField(result, "interpreter") != "definitely-not-installed-xyz" ||
		!strings.Contains(execField(result, "suggestion"), "tools.native.exec.langs.ghost.argv") {
		t.Fatalf("result = %s", result)
	}
}

func TestExecDeliversCodeAsAFileThatIsRemovedAfterwards(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings := execTestSettings()
	settings.LangNames = append(settings.LangNames, "viafile")
	settings.Langs["viafile"] = sessionfs.NativeExecLang{Argv: []string{"bash", "{file}"}, Code: sessionfs.NativeExecCodeFile, Ext: ".sh", Aliases: []string{}}
	code := `echo "ran $(basename "$0") $1"; echo "dir=$(dirname "$0")" >&2`
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "viafile", "code": code, "args": []any{"z"}})
	text := execContent(result)
	if !strings.Contains(text, "ran code.sh z") {
		t.Fatalf("content = %q", text)
	}
	match := regexp.MustCompile(`dir=(\S+)`).FindStringSubmatch(text)
	if match == nil || !strings.Contains(match[1], "exec-") {
		t.Fatalf("no scratch directory reported in %q", text)
	}
	if _, err := os.Stat(match[1]); err == nil {
		t.Fatalf("scratch directory %s was left behind", match[1])
	}
}

func TestExecUsesBashsEnvironmentAndNeverShowsTheHarnessCredential(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	bash := bashTestSettings(t, root)
	bash.Env["EXEC_VISIBLE"] = "yes"
	t.Setenv("JAD_API_TOKEN", "secret-token-value")
	result := runExec(t, root, execTestSettings(), bash, map[string]any{"lang": "bash", "code": `echo "visible=$EXEC_VISIBLE token=${JAD_API_TOKEN:-none}"`})
	if got := execContent(result); got != "visible=yes token=none\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestExecTimesOutUnderBashsTimeoutPolicy(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	bash := bashTestSettings(t, root)
	bash.TimeoutMs, bash.MaxTimeoutMs = 1000, 1000
	result := runExec(t, root, execTestSettings(), bash, map[string]any{"lang": "bash", "code": "sleep 5"})
	if execField(result, "error_code") != "timeout" {
		t.Fatalf("result = %s", result)
	}
	if suggestion := execField(result, "suggestion"); !strings.Contains(suggestion, "tools.native.exec.timeout_ms") || strings.Contains(suggestion, "tools.native.bash") {
		t.Fatalf("timeout advice = %q, want it to name tools.native.exec.timeout_ms", suggestion)
	}
}

func TestExecCutsByLineWhileBashWithTheSameBudgetDoesNot(t *testing.T) {
	requireInterpreters(t, "python3")
	root := t.TempDir()
	bash := bashTestSettings(t, root)
	// 43 is not a multiple of the 8-byte rows, so a plain cut lands inside a row.
	bash.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 43, TailBytes: 43}
	bash.Stderr = sessionfs.NativeBashStreamBudget{HeadBytes: 43, TailBytes: 43}
	bash.MaxBytes = 4096
	code := "for i in range(100):\n    print('row-%03d' % i)"
	whole := regexp.MustCompile(`^(row-\d{3}|\[\.\.\. \d+ bytes omitted \.\.\.\]|)$`)
	settings := execTestSettings()
	for _, line := range strings.Split(execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": code})), "\n") {
		if !whole.MatchString(line) {
			t.Fatalf("line-aware output shows a partial line %q", line)
		}
	}
	settings.LineAware = false
	partial := false
	for _, line := range strings.Split(execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": code})), "\n") {
		if !whole.MatchString(line) {
			partial = true
		}
	}
	if !partial {
		t.Fatal("a plain cut at a budget that is not a multiple of the row size should show a partial row")
	}
}

func TestLineAwareWindowsDropOnlyPartialLines(t *testing.T) {
	head, tail := lineAwareWindows("a\nb\nc", "d\ne\nf\n")
	if head != "a\nb\n" || tail != "e\nf\n" {
		t.Fatalf("head %q tail %q", head, tail)
	}
	// One very long line has no break to cut at and is left alone.
	head, tail = lineAwareWindows("xxxxxxxxxx", "yyyyyyyyyy")
	if head != "xxxxxxxxxx" || tail != "yyyyyyyyyy" {
		t.Fatalf("long line: head %q tail %q", head, tail)
	}
	// A window that ends exactly on a break keeps everything.
	head, tail = lineAwareWindows("a\nb\n", "\nc\n")
	if head != "a\nb\n" || tail != "c\n" {
		t.Fatalf("on the break: head %q tail %q", head, tail)
	}
}

func TestLineAwareCountsEveryDroppedByteInTheMarker(t *testing.T) {
	limits := bashStreamLimits{headBytes: 25, tailBytes: 25}
	var builder strings.Builder
	for i := 0; i < 40; i++ {
		builder.WriteString("line-" + strings.Repeat("0", 2-len(itoa(i))) + itoa(i) + "\n")
	}
	lines := builder.String()
	plain := newBashOutputCollector(limits, bashStreamLimits{}, 2000, true)
	aware := newBashOutputCollector(limits, bashStreamLimits{}, 2000, true)
	aware.lineAware = true
	for _, collector := range []*bashOutputCollector{plain, aware} {
		collector.push(bashStdout, []byte(lines))
		collector.end()
	}
	plainOut, awareOut := plain.render(), aware.render()
	if awareOut.omittedBytes <= plainOut.omittedBytes {
		t.Fatalf("dropping partial lines must drop more bytes: aware %d, plain %d", awareOut.omittedBytes, plainOut.omittedBytes)
	}
	shown := int64(len(regexp.MustCompile(`\[\.\.\. \d+ bytes omitted \.\.\.\]\n?`).ReplaceAllString(awareOut.text, "")))
	if want := int64(len(lines)) - shown; awareOut.omittedBytes != want {
		t.Fatalf("omitted %d, want produced-shown = %d", awareOut.omittedBytes, want)
	}
}

func itoa(value int) string {
	return strings.TrimSpace(strings.Join([]string{string(rune('0' + value/10)), string(rune('0' + value%10))}, ""))
}

func TestNormalizeExecArgumentsTurnsNearMissNamesIntoTheSchemaForm(t *testing.T) {
	got := normalizeExecArguments(map[string]any{"language": " Python ", "source": "print(1)", "argv": []any{float64(1), true, "x"}, "workdir": "sub"}, "bash")
	if got["lang"] != "Python" || got["code"] != "print(1)" || got["cwd"] != "sub" {
		t.Fatalf("normalized = %v", got)
	}
	args, _ := got["args"].([]any)
	if len(args) != 3 || args[0] != "1" || args[1] != "true" || args[2] != "x" {
		t.Fatalf("args = %v", got["args"])
	}
	for _, key := range []string{"language", "source", "argv", "workdir"} {
		if _, kept := got[key]; kept {
			t.Fatalf("%s was not removed from %v", key, got)
		}
	}
	got = normalizeExecArguments(map[string]any{"lang": "node", "code": "1", "args": []any{"a"}, "script": "ignored"}, "bash")
	if got["code"] != "1" {
		t.Fatalf("an explicit code must win over an alias: %v", got)
	}
}

func TestExecWithoutALanguageRunsTheConfiguredDefault(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	root := t.TempDir()
	settings, bash := execTestSettings(), bashTestSettings(t, root)
	// The smallest call: code alone runs in bash.
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"code": "echo from-$0-shell; echo $((6*7))"})); !strings.Contains(got, "42") {
		t.Fatalf("code alone did not run in bash: %q", got)
	}
	// null, an empty string and a blank string all mean "not given".
	for _, lang := range []any{nil, "", "   "} {
		if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": lang, "code": "echo $((6*7))"})); !strings.Contains(got, "42") {
			t.Fatalf("lang %#v did not fall back to bash: %q", lang, got)
		}
	}
	// A named language still wins over the default.
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "print(6 * 7)"})); !strings.Contains(got, "42") {
		t.Fatalf("an explicit lang must still run: %q", got)
	}
	// The default is configuration: with python as default, code alone is python.
	settings.DefaultLang = "python"
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"code": "print(6 * 7)"})); !strings.Contains(got, "42") {
		t.Fatalf("a configured default_lang of python was not used: %q", got)
	}
}

// callExecThroughTheCatalog sends one function call the way the run loop does: raw JSON
// arguments, the tool's own validator and its own normalizer.
func callExecThroughTheCatalog(t *testing.T, root string, settings sessionfs.NativeExecSettings, arguments string) string {
	t.Helper()
	settings.Child = bashTestSettings(t, root)
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{"type": "function_call", "call_id": "exec-call", "name": "exec", "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	execution := tools.ExecutionContext{WorkspaceRoot: root, SessionDir: root}
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
	if err != nil {
		t.Fatal(err)
	}
	// The catalog wraps the rendered result in a function_call_output item.
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(output, &item); err != nil {
		t.Fatalf("function_call_output: %v: %s", err, output)
	}
	return item.Output
}

func TestExecThroughTheCatalogAcceptsCodeAlone(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	root := t.TempDir()
	settings := execTestSettings()
	// The schema the model sees requires code only.
	settings.Child = bashTestSettings(t, root)
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Definition.InputSchema, &schema); err != nil || len(schema.Required) != 1 || schema.Required[0] != "code" {
		t.Fatalf("required = %v (%v), want [code]", schema.Required, err)
	}
	if got := execContent(callExecThroughTheCatalog(t, root, settings, `{"code":"echo $((6*7))"}`)); !strings.Contains(got, "42") {
		t.Fatalf("code alone through the catalog: %q", got)
	}
	// null is "not given" and is repaired before validation, so it costs no retry.
	if got := execContent(callExecThroughTheCatalog(t, root, settings, `{"lang":null,"code":"echo $((6*7))"}`)); !strings.Contains(got, "42") {
		t.Fatalf("lang null through the catalog: %q", got)
	}
	// The configured default, not a hard-coded bash, is what a call without lang runs.
	settings.DefaultLang = "python"
	if got := execContent(callExecThroughTheCatalog(t, root, settings, `{"code":"print(6*7)"}`)); !strings.Contains(got, "42") {
		t.Fatalf("a configured default python through the catalog: %q", got)
	}
}

func TestExecNormalizationFillsTheDefaultLanguage(t *testing.T) {
	got := normalizeExecArguments(map[string]any{"code": "ls"}, "bash")
	if got["lang"] != "bash" {
		t.Fatalf("lang = %v, want bash", got["lang"])
	}
	if got := normalizeExecArguments(map[string]any{"code": "ls", "language": "node"}, "bash"); got["lang"] != "node" {
		t.Fatalf("an alias must win over the default: %v", got)
	}
	if got := normalizeExecArguments(map[string]any{"code": "ls"}, ""); got["lang"] != nil {
		t.Fatalf("lang = %v, want absent", got["lang"])
	}
}

func TestFindExecLangMatchesNamesAndAliasesOnly(t *testing.T) {
	settings := execTestSettings()
	if name, _, ok := findExecLang(settings, "PY"); !ok || name != "python" {
		t.Fatalf("PY -> %q %v", name, ok)
	}
	if _, _, ok := findExecLang(settings, "pyth"); ok {
		t.Fatal("a prefix must not match")
	}
}

// exec runs with the environment of its own Child settings, not with bash's.
func TestExecUsesItsOwnEnvironment(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	child := bashTestSettings(t, root)
	child.Env = map[string]string{"WHO": "exec"}

	result := runExec(t, root, execTestSettings(), child, map[string]any{"lang": "bash", "code": "echo $WHO"})
	if got := execContent(result); got != "exec\n" {
		t.Fatalf("exec environment: %q\n%s", got, result)
	}
}
