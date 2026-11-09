package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func fakeLSPSettings(t *testing.T) sessionfs.NativeLSPSettings {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return sessionfs.NativeLSPSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Servers:          []sessionfs.NativeLSPServer{{Name: "fake", Command: []string{executable, "-test.run=^$"}, Extensions: []string{".fk"}, LanguageID: "fake", Env: map[string]string{"JAD_FAKE_LSP": "1"}}},
		RequestTimeoutMs: 10000, DiagnosticsWaitMs: 2000, IdleShutdownMs: 60000, MaxResults: 3, MaxFileBytes: 1 << 20,
	}
}

func callLSP(t *testing.T, settings sessionfs.NativeLSPSettings, root string, arguments map[string]any) string {
	t.Helper()
	tool, err := NewLSPTool(settings)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "call-1", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func wantAll(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in:\n%s", want, output)
		}
	}
}

func TestLSPWithAFakeServer(t *testing.T) {
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	settings := fakeLSPSettings(t)
	file := filepath.Join(root, "main.fk")
	// é is one rune but 1 UTF-16 unit; 𝒳 is one rune but 2 units: columns must convert both ways.
	if err := os.WriteFile(file, []byte("var é𝒳 = target\nok BAD here\nline three\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "main.fk"})
	wantAll(t, output, "1 diagnostics in main.fk", "server: fake", "main.fk:2:4: error: bad thing v1 [fake F1]")

	// Unchanged file: the stored diagnostics come back at once.
	started := time.Now()
	output = callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "main.fk"})
	wantAll(t, output, "bad thing v1")
	if time.Since(started) > time.Second {
		t.Fatalf("an unchanged file waited %v for diagnostics", time.Since(started))
	}
	// A change on disk is sent as a new version, and its diagnostics replace the old ones.
	if err := os.WriteFile(file, []byte("var é𝒳 = target\nfine\nBAD BAD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output = callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "main.fk"})
	wantAll(t, output, "main.fk:3:1: error: bad thing v2")

	// The symbol's rune column (8, after "var é𝒳 = ") goes out as UTF-16 offset 9 and comes back as 8+1.
	output = callLSP(t, settings, root, map[string]any{"action": "definition", "path": "main.fk", "line": 1, "symbol": "target"})
	wantAll(t, output, "2 locations", "main.fk:1:10  var é𝒳 = target", "/nonexistent/lib/x.go:10:5\n")
	output = callLSP(t, settings, root, map[string]any{"action": "type_definition", "path": "main.fk", "line": 1, "column": 10})
	wantAll(t, output, "main.fk:1:10  var é𝒳 = target")

	output = callLSP(t, settings, root, map[string]any{"action": "references", "path": "main.fk", "line": 1, "column": 1})
	wantAll(t, output, "results: 5", "truncated: only the first results are shown", "main.fk:3:1  BAD BAD")
	if strings.Contains(output, "main.fk:4:1") {
		t.Errorf("max_results 3 showed a fourth result:\n%s", output)
	}

	output = callLSP(t, settings, root, map[string]any{"action": "hover", "path": "main.fk", "line": 1, "symbol": "target"})
	wantAll(t, output, "func target() int")
	output = callLSP(t, settings, root, map[string]any{"action": "symbols", "path": "main.fk"})
	wantAll(t, output, "struct Server struct  1:6\n  method Start  2:2\n")
	output = callLSP(t, settings, root, map[string]any{"action": "workspace_symbols", "path": "main.fk", "query": "Do"})
	wantAll(t, output, `1 symbols match \"Do\"`, "function DoThing (in pkg)  /nonexistent/lib/y.go:3:6")
}

func TestLSPRefusalsAndServerFailures(t *testing.T) {
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	settings := fakeLSPSettings(t)
	_ = os.WriteFile(filepath.Join(root, "a.fk"), []byte("one\nCRASH\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	for name, test := range map[string]struct {
		arguments map[string]any
		want      string
	}{
		"no server":        {map[string]any{"action": "symbols", "path": "a.txt"}, "error_code: no_language_server"},
		"no position":      {map[string]any{"action": "definition", "path": "a.fk"}, "error_code: position_required"},
		"no column":        {map[string]any{"action": "definition", "path": "a.fk", "line": 1}, "error_code: position_required"},
		"line too far":     {map[string]any{"action": "hover", "path": "a.fk", "line": 9, "column": 1}, "error_code: line_out_of_range"},
		"symbol not there": {map[string]any{"action": "hover", "path": "a.fk", "line": 1, "symbol": "two"}, "error_code: symbol_not_on_line"},
		"column too far":   {map[string]any{"action": "hover", "path": "a.fk", "line": 1, "column": 9}, "error_code: column_out_of_range"},
		"outside":          {map[string]any{"action": "symbols", "path": filepath.Join(t.TempDir(), "b.fk")}, "error_code: path_outside_workspace"},
	} {
		if output := callLSP(t, settings, root, test.arguments); !strings.Contains(output, test.want) {
			t.Errorf("%s: missing %q in:\n%s", name, test.want, output)
		}
	}
	// A server that dies reports its stderr; the next call starts a new one.
	output := callLSP(t, settings, root, map[string]any{"action": "hover", "path": "a.fk", "line": 2, "column": 1})
	wantAll(t, output, "error_code: server_error", "exited", "fake crashed on purpose")
	_ = os.WriteFile(filepath.Join(root, "a.fk"), []byte("one\nfine\n"), 0o644)
	output = callLSP(t, settings, root, map[string]any{"action": "hover", "path": "a.fk", "line": 2, "column": 1})
	wantAll(t, output, "func target() int")

	missing := settings
	missing.Servers = []sessionfs.NativeLSPServer{{Name: "gone", Command: []string{"jad-no-such-language-server"}, Extensions: []string{".fk"}, LanguageID: "x"}}
	output = callLSP(t, missing, root, map[string]any{"action": "symbols", "path": "a.fk"})
	wantAll(t, output, "error_code: server_unavailable", "jad-no-such-language-server")
}

// A server that publishes nothing: the call waits diagnostics_wait_ms, then says so.
func TestLSPDiagnosticsWaitIsBounded(t *testing.T) {
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	settings := fakeLSPSettings(t)
	settings.DiagnosticsWaitMs = 200
	_ = os.WriteFile(filepath.Join(root, "q.fk"), []byte("SILENT\n"), 0o644)
	started := time.Now()
	output := callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "q.fk"})
	wantAll(t, output, "0 diagnostics", "published nothing new within 200 ms")
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("waited %v", elapsed)
	}
}

// One server per workspace is reused across calls and catalogs, and stops when idle.
func TestLSPServerIsSharedAndStopsWhenIdle(t *testing.T) {
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	settings := fakeLSPSettings(t)
	settings.IdleShutdownMs = 300
	_ = os.WriteFile(filepath.Join(root, "a.fk"), []byte("x\n"), 0o644)
	callLSP(t, settings, root, map[string]any{"action": "symbols", "path": "a.fk"})
	key := lspKey(settings.Servers[0], resolvedRoot(t, root), nil, sandboxPlace{})
	first := slotClient(key)
	callLSP(t, settings, root, map[string]any{"action": "symbols", "path": "a.fk"})
	if first == nil || slotClient(key) != first {
		t.Fatal("the second call did not reuse the server")
	}
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle server did not stop")
	}
	callLSP(t, settings, root, map[string]any{"action": "symbols", "path": "a.fk"})
	if second := slotClient(key); second == nil || second == first {
		t.Fatal("a call after the idle stop did not start a new server")
	}
}

func resolvedRoot(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func slotClient(key string) *lspClient {
	languageServers.mu.Lock()
	slot := languageServers.slots[key]
	languageServers.mu.Unlock()
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.client
}

func TestLSPWithGopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls is not installed")
	}
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/demo\n\ngo 1.22\n",
		"util.go": "package demo\n\n// Double returns twice n.\nfunc Double(n int) int { return n * 2 }\n",
		"main.go": "package demo\n\nfunc Use() int {\n\treturn Double(21)\n}\n\nfunc Broken() string {\n\treturn 42\n}\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	settings := sessionfs.NativeLSPSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Servers:          []sessionfs.NativeLSPServer{{Name: "go", Command: []string{"gopls"}, Extensions: []string{".go"}, LanguageID: "go", Env: map[string]string{"GOCACHE": filepath.Join(t.TempDir(), "cache"), "GOPATH": filepath.Join(t.TempDir(), "gopath"), "GOFLAGS": "-mod=mod", "GOTOOLCHAIN": "local"}}},
		RequestTimeoutMs: 60000, DiagnosticsWaitMs: 30000, IdleShutdownMs: 60000, MaxResults: 50, MaxFileBytes: 1 << 20,
	}
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "main.go"}), "main.go:8:9: error: cannot use 42")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "definition", "path": "main.go", "line": 4, "symbol": "Double"}), "util.go:4:6  func Double(n int) int { return n * 2 }")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "references", "path": "util.go", "line": 4, "symbol": "Double"}), "main.go:4:9  return Double(21)", "util.go:4:6")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "hover", "path": "main.go", "line": 4, "symbol": "Double"}), "func Double(n int) int", "Double returns twice n.")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "symbols", "path": "main.go"}), "function Use", "function Broken")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "workspace_symbols", "path": "main.go", "query": "Double"}), "function Double", "util.go:4:6")
}

func TestLSPWithPyright(t *testing.T) {
	if _, err := exec.LookPath("pyright-langserver"); err != nil {
		t.Skip("pyright-langserver is not installed")
	}
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "util.py"), []byte("def double(n: int) -> int:\n    \"\"\"Return twice n.\"\"\"\n    return n * 2\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "main.py"), []byte("from util import double\n\nvalue: str = double(21)\n"), 0o644)
	settings := sessionfs.NativeLSPSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Servers:          []sessionfs.NativeLSPServer{{Name: "python", Command: []string{"pyright-langserver", "--stdio"}, Extensions: []string{".py"}, LanguageID: "python"}},
		RequestTimeoutMs: 60000, DiagnosticsWaitMs: 30000, IdleShutdownMs: 60000, MaxResults: 50, MaxFileBytes: 1 << 20,
	}
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "diagnostics", "path": "main.py"}), "main.py:3:14: error:", `"int" is not assignable to declared type "str"`)
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "definition", "path": "main.py", "line": 3, "symbol": "double"}), "util.py:1:5  def double(n: int) -> int:")
	wantAll(t, callLSP(t, settings, root, map[string]any{"action": "hover", "path": "main.py", "line": 3, "symbol": "double"}), "def double(n: int) -> int", "Return twice n.")
}

func TestLSPIsReadOnlyAndLoadsFromTheSampleManifest(t *testing.T) {
	root := t.TempDir()
	settings := fakeLSPSettings(t)
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "setting.json"), ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "lsp.json")},
		NativeLSP: &settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "lsp" || definitions[0].Description != lspDescription || !jsonEqual(t, definitions[0].InputSchema, lspInputSchema) {
		t.Fatalf("definitions = %+v", definitions)
	}
	tool, _ := NewLSPTool(settings)
	if !tool.ReadOnly || tool.MayChangeFiles != nil || !tool.LeafBudget {
		t.Fatal("lsp changes no file: it must be read-only")
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
