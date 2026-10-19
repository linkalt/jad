package native

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func listTestSettings(dir string) sessionfs.NativeToolSmithSettings {
	return sessionfs.NativeToolSmithSettings{FunctionsDir: dir, ListMaxResults: 50, ListMaxFiles: 200, ListHeaderBytes: 2048}
}

func listTestFunction(name, description string) string {
	return fmt.Sprintf("// %s\nexport async function %s(input: {a: string}) {\n  return 1;\n}\n", description, name)
}

func listTestWrite(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runToolSmithList(root string, settings sessionfs.NativeToolSmithSettings, filters ...string) string {
	items := make([]any, len(filters))
	for index, filter := range filters {
		items[index] = filter
	}
	return executeToolSmithList(map[string]any{"list": map[string]any{"filters": items}}, settings, tools.ExecutionContext{CallID: "list-call", WorkspaceRoot: root})
}

func TestToolSmithListParsesHeaderAndSignature(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "fn/trace_callers.ts", "\n// Find callers of a symbol\n// second line\n\nexport async function trace_callers(\n")
	listTestWrite(t, root, "fn/inspect.ts", "// Search project code\r\nexport async function inspect(input: {query: string, path?: string}): Promise<string> {\r\n}\r\n")
	listTestWrite(t, root, "fn/notes.txt", "ignored")
	result := runToolSmithList(root, listTestSettings("fn"))
	content := execContent(result)
	want := "inspect(input: {query: string, path?: string}) — Search project code\ntrace_callers() — Find callers of a symbol"
	if !strings.Contains(result, `"status":"ok"`) || content != want {
		t.Fatalf("content = %q\nresult:\n%s", content, result)
	}
}

func TestToolSmithListFiltersMustAllMatchNameOrDescription(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "fn/alpha_one.ts", listTestFunction("alpha_one", "Search the Web"))
	listTestWrite(t, root, "fn/alpha_two.ts", listTestFunction("alpha_two", "Other"))
	listTestWrite(t, root, "fn/beta.ts", listTestFunction("beta", "search things"))
	settings := listTestSettings("fn")
	if got := execContent(runToolSmithList(root, settings, "ALPHA")); !strings.Contains(got, "alpha_one") || !strings.Contains(got, "alpha_two") || strings.Contains(got, "beta") {
		t.Fatalf("single filter: %q", got)
	}
	if got := execContent(runToolSmithList(root, settings, "alpha", "web")); !strings.HasPrefix(got, "alpha_one(") || strings.Contains(got, "alpha_two") {
		t.Fatalf("two filters: %q", got)
	}
	result := runToolSmithList(root, settings, "nothing-matches")
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(result, "0 functions") || execContent(result) != "" {
		t.Fatalf("no match:\n%s", result)
	}
}

func TestToolSmithListCapsResultsAndFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		listTestWrite(t, root, "fn/"+name+".ts", listTestFunction(name, "Function "+name))
	}
	settings := listTestSettings("fn")
	settings.ListMaxResults = 2
	result := runToolSmithList(root, settings)
	if got, shown := execField(result, "truncated"), execField(result, "shown"); got != "true" || shown != "2 of 4" {
		t.Fatalf("truncated = %q, shown = %q\n%s", got, shown, result)
	}
	if !strings.Contains(execField(result, "suggestion"), "filters") || strings.Contains(execContent(result), "c(") {
		t.Fatalf("result:\n%s", result)
	}

	settings = listTestSettings("fn")
	settings.ListMaxFiles = 3
	result = runToolSmithList(root, settings)
	if got, shown := execField(result, "truncated"), execField(result, "shown"); got != "true" || shown != "3 of 3; scanned 3 of 4 files" {
		t.Fatalf("truncated = %q, shown = %q\n%s", got, shown, result)
	}
	if suggestion := execField(result, "suggestion"); !strings.Contains(suggestion, "list_max_files") || strings.Contains(suggestion, "add list.filters") {
		t.Fatalf("suggestion = %q", suggestion)
	}
	if strings.Contains(execContent(result), "d(") {
		t.Fatalf("file past the cap listed:\n%s", result)
	}

	if got := execField(runToolSmithList(root, listTestSettings("fn")), "truncated"); got != "" {
		t.Fatalf("uncut result truncated = %q", got)
	}
}

func TestToolSmithListReadsOnlyTheHeaderBytes(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "fn/late.ts", "// Late\n"+strings.Repeat("// pad\n", 20)+"export async function late(input) {}\n")
	settings := listTestSettings("fn")
	settings.ListHeaderBytes = 40
	result := runToolSmithList(root, settings)
	if !strings.Contains(execContent(result), "late.ts: no `export async function late(`") {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithListEmptyLibraryIsOkAndMissingDirectoryIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := runToolSmithList(root, listTestSettings("fn"))
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(result, "0 functions") || execContent(result) != "" {
		t.Fatalf("empty:\n%s", result)
	}
	result = runToolSmithList(root, listTestSettings("missing"))
	if !strings.Contains(result, `"status":"error"`) || execField(result, "error_code") != "list_failed" {
		t.Fatalf("missing:\n%s", result)
	}
	listTestWrite(t, root, "plain", "x")
	if result = runToolSmithList(root, listTestSettings("plain")); execField(result, "error_code") != "list_failed" {
		t.Fatalf("file as directory:\n%s", result)
	}
}

func TestToolSmithListReportsAllMalformedFilesInOneSection(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "fn/good.ts", listTestFunction("good", "Fine"))
	listTestWrite(t, root, "fn/BadName.ts", listTestFunction("BadName", "Bad"))
	listTestWrite(t, root, "fn/nohead.ts", "export async function nohead() {}\n")
	listTestWrite(t, root, "fn/wrongfn.ts", "// Described\nexport async function other() {}\n")
	result := runToolSmithList(root, listTestSettings("fn"))
	content := execContent(result)
	if !strings.HasPrefix(content, "good(input: {a: string}) — Fine\n\nmalformed files (3):\n") {
		t.Fatalf("content = %q", content)
	}
	for _, want := range []string{"- BadName.ts: name must match", "- nohead.ts: missing leading // header", "- wrongfn.ts: no `export async function wrongfn(`"} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in %q", want, content)
		}
	}
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(result, "1 function, 3 malformed files") {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithListRejectsPathsThatEscapeTheWorkspace(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	listTestWrite(t, root, "keep", "x")
	listTestWrite(t, parent, "outside/out.ts", listTestFunction("out", "Outside"))
	if err := os.Symlink(filepath.Join(parent, "outside"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, dir := range map[string]string{
		"absolute":     filepath.Join(parent, "outside"),
		"dotdot":       "../outside",
		"inner dotdot": "fn/../../outside",
		"symlink":      "linked",
	} {
		result := runToolSmithList(root, listTestSettings(dir))
		if execField(result, "error_code") != "list_failed" || strings.Contains(result, "out(") {
			t.Errorf("%s:\n%s", name, result)
		}
	}
}

func TestToolSmithListDoesNotFollowFileSymlinksOutOfTheLibrary(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	listTestWrite(t, root, "fn/good.ts", listTestFunction("good", "Fine"))
	listTestWrite(t, parent, "secret.ts", listTestFunction("leak", "Secret"))
	if err := os.Symlink(filepath.Join(parent, "secret.ts"), filepath.Join(root, "fn", "leak.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	content := execContent(runToolSmithList(root, listTestSettings("fn")))
	if !strings.Contains(content, "- leak.ts: symlink leaves the functions directory") || strings.Contains(content, "Secret") {
		t.Fatalf("content = %q", content)
	}
}

func TestToolSmithListRejectsNonStringFilters(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "fn/a.ts", listTestFunction("a", "A"))
	result := executeToolSmithList(map[string]any{"list": map[string]any{"filters": []any{"a", 123.0}}}, listTestSettings("fn"), tools.ExecutionContext{CallID: "list-call", WorkspaceRoot: root})
	if execField(result, "error_code") != "invalid_input" || !strings.Contains(result, "list.filters must be strings") {
		t.Fatalf("result:\n%s", result)
	}
}
