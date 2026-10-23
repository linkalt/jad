package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// toolSmithInterpreter returns an argv for bun, or for node when it can run a .ts file.
func toolSmithInterpreter(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err == nil {
		return []string{"bun", "run", "{file}"}
	}
	if _, err := exec.LookPath("node"); err == nil {
		probe := filepath.Join(t.TempDir(), "probe.ts")
		if os.WriteFile(probe, []byte("const x: number = 1;\nconsole.log(x);\n"), 0o600) == nil {
			if out, err := exec.Command("node", probe).Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
				return []string{"node", "{file}"}
			}
		}
	}
	t.Skip("neither bun nor node (with TypeScript support) is installed")
	return nil
}

func toolSmithTestSettings(t *testing.T, root string) sessionfs.NativeToolSmithSettings {
	t.Helper()
	child := bashTestSettings(t, root)
	child.TimeoutMs = 5_000
	child.MaxTimeoutMs = 10_000
	return sessionfs.NativeToolSmithSettings{
		Child:          child,
		CodeMaxBytes:   4096,
		ResultMaxBytes: 64,
		Interpreter:    toolSmithInterpreter(t),
	}
}

func runToolSmith(ctx context.Context, root string, settings sessionfs.NativeToolSmithSettings, arguments map[string]any) string {
	result, _ := executeToolSmith(ctx, normalizeToolSmithArguments(arguments), settings, "", nil, tools.ExecutionContext{
		CallID: "smith-call", WorkspaceRoot: root, SessionDir: root,
	}, nil)
	return result
}

func smithRun(function string, input map[string]any) map[string]any {
	arguments := map[string]any{"function": function}
	if input != nil {
		arguments["input"] = input
	}
	return arguments
}

// useOwnTempDir points os.TempDir at an empty directory and returns it.
func useOwnTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func requireNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temp dir %s not clean: %v %v", dir, entries, err)
	}
}

func TestToolSmithRunReturnsTheValueAndRemovesItsDirectory(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := toolSmithTestSettings(t, root)
	result := runToolSmith(context.Background(), root, settings, smithRun("return { sum: input.a + input.b, text: 'h\u00e9' };", map[string]any{"a": 1, "b": 2}))
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(execContent(result), `{"sum":3,"text":"hé"}`) {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRejectsTheRunKeyAndNamesFunction(t *testing.T) {
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		`{"run":{"function":"return 1;"}}`,
		`{"run":"return 1;"}`,
		`{"function":"return 1;","run":{"function":"return 2;"}}`,
		`{"script":"return 1;"}`,
	} {
		output := executeCatalogCall(t, catalog, "tool_smith", arguments)
		if !strings.Contains(output, `"status":"invalid_input"`) {
			t.Errorf("arguments %s were accepted:\n%s", arguments, output)
		}
	}
	output := executeCatalogCall(t, catalog, "tool_smith", `{"run":{"function":"return 1;"}}`)
	if !strings.Contains(output, "top-level function string") || !strings.Contains(output, `{"function":"return 1;"}`) {
		t.Errorf("the error does not name function:\n%s", output)
	}
}

func TestToolSmithRunReturnsAStringRaw(t *testing.T) {
	root := t.TempDir()
	result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), smithRun("return 'plain';", nil))
	if !strings.Contains(execContent(result), "plain") || strings.Contains(execContent(result), `"plain"`) {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithRunShowsStdoutAfterTheResult(t *testing.T) {
	root := t.TempDir()
	result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), smithRun("console.log('log line'); return 7;", nil))
	if got := execContent(result); !strings.HasPrefix(got, "[result]\n7\n[output]\n") || !strings.Contains(got, "log line") {
		t.Fatalf("content = %q", got)
	}
}

func TestToolSmithRunReportsASyntaxError(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), smithRun("return (;", nil))
	if !strings.Contains(result, `"status":"error"`) || execField(result, "exit_code") == "" || execContent(result) == "" {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRunReportsAThrownError(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), smithRun("throw new Error('kaboom');", nil))
	if !strings.Contains(result, `"status":"error"`) || execField(result, "exit_code") != "1" || !strings.Contains(execContent(result), "kaboom") {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRunTimesOut(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := toolSmithTestSettings(t, root)
	settings.Child.TimeoutMs = 400
	result := runToolSmith(context.Background(), root, settings, smithRun("await new Promise(() => setInterval(() => {}, 1000));", nil))
	if execField(result, "timed_out") != "true" || execField(result, "error_code") != "timeout" {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithCatalogRejectsATimeoutArgument(t *testing.T) {
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		`{"function":"return 1;","timeout_ms":60000}`,
	} {
		output := executeCatalogCall(t, catalog, "tool_smith", arguments)
		if !strings.Contains(output, `"status":"invalid_input"`) {
			t.Errorf("arguments %s were accepted:\n%s", arguments, output)
		}
	}
}

func TestToolSmithRunCutsALongValueWithAMarker(t *testing.T) {
	root := t.TempDir()
	settings := toolSmithTestSettings(t, root)
	result := runToolSmith(context.Background(), root, settings, smithRun("return '\u00e9'.repeat(100);", nil))
	content := execContent(result)
	if execField(result, "truncated") != "true" || !strings.Contains(content, "[result cut: ") {
		t.Fatalf("result:\n%s", result)
	}
	body := strings.SplitN(strings.TrimPrefix(content, "[result]\n"), "\n[result cut", 2)[0]
	if len(body) > int(settings.ResultMaxBytes) || strings.ContainsRune(body, '\uFFFD') {
		t.Fatalf("cut body %d bytes: %q", len(body), body)
	}
}

func TestToolSmithRunReportsAMissingInterpreter(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := toolSmithTestSettings(t, root)
	settings.Interpreter = []string{"definitely-not-installed-interpreter", "{file}"}
	result := runToolSmith(context.Background(), root, settings, smithRun("return 1;", nil))
	if execField(result, "error_code") != "interpreter_unavailable" || !strings.Contains(result, "tools.native.tool_smith.interpreter") {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRunCancelLeavesNoChild(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := toolSmithTestSettings(t, root)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	runToolSmith(ctx, root, settings, smithRun(
		"await Bun_or_node_write(input.pidFile); await new Promise(() => setInterval(() => {}, 1000));\nasync function Bun_or_node_write(p) { (await import('node:fs')).writeFileSync(p, String(process.pid)); }",
		map[string]any{"pidFile": pidFile}))
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the script never wrote its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("child %d still runs after cancel", pid)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRunRefusesBadParameters(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := toolSmithTestSettings(t, root)
	for name, arguments := range map[string]map[string]any{
		"empty function":  {"function": ""},
		"blank function":  {"function": "  \n"},
		"too large":       smithRun(strings.Repeat("x", int(settings.CodeMaxBytes)), nil),
		"input too large": smithRun("return 1;", map[string]any{"big": strings.Repeat("x", int(settings.CodeMaxBytes))}),
	} {
		result := runToolSmith(context.Background(), root, settings, arguments)
		if !strings.Contains(result, `"status":"invalid_input"`) {
			t.Fatalf("%s:\n%s", name, result)
		}
		if name != "too large" && name != "input too large" && !strings.Contains(result, `{"function":"return 1;"}`) {
			t.Fatalf("%s has no example:\n%s", name, result)
		}
	}
	requireNoLeftovers(t, tmp)
}

// A call with neither run nor filters is a list, so it is not an input error; it fails only
// when the library cannot be read.
func TestToolSmithCallWithNeitherRunNorFiltersListsInsteadOfRefusing(t *testing.T) {
	root := t.TempDir()
	settings := toolSmithTestSettings(t, root)
	if result := runToolSmith(context.Background(), root, settings, map[string]any{}); strings.Contains(result, `"status":"invalid_input"`) {
		t.Fatalf("neither is a list now:\n%s", result)
	}
}

func TestToolSmithRunRejectsANonObjectInput(t *testing.T) {
	root := t.TempDir()
	for name, input := range map[string]any{"string": "x", "array": []any{1.0}} {
		result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), map[string]any{"function": "return 1;", "input": input})
		if execField(result, "error_code") != "invalid_input" || !strings.Contains(result, "input must be an object") {
			t.Errorf("%s:\n%s", name, result)
		}
	}
}

func TestToolSmithRunWinsOverFiltersAndSaysSo(t *testing.T) {
	root := t.TempDir()
	arguments := smithRun("return 5;", nil)
	arguments["filters"] = []any{"read*"}
	result := runToolSmith(context.Background(), root, toolSmithTestSettings(t, root), arguments)
	if !strings.Contains(execContent(result), "5") || !strings.Contains(result, "ignored: filters") {
		t.Fatalf("result:\n%s", result)
	}
}
