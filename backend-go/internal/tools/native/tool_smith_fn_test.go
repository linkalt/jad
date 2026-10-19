package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func fnStageSettings() sessionfs.NativeToolSmithSettings {
	settings := listTestSettings(".jad/functions")
	settings.FunctionMaxBytes = 4096
	return settings
}

func fnRunSettings(t *testing.T, root string) sessionfs.NativeToolSmithSettings {
	t.Helper()
	settings := toolSmithTestSettings(t, root)
	settings.FunctionsDir = ".jad/functions"
	settings.ListMaxFiles = 200
	settings.ListMaxResults = 50
	settings.ListHeaderBytes = 2048
	settings.FunctionMaxBytes = 4096
	return settings
}

func fnSource(name, body string) string {
	return fmt.Sprintf("// Function %s\nexport async function %s(input: {n: number}) {\n%s\n}\n", name, name, body)
}

func fnWrite(t *testing.T, root, name, content string) {
	t.Helper()
	listTestWrite(t, root, ".jad/functions/"+name+".ts", content)
}

func fnStage(t *testing.T, root string, settings sessionfs.NativeToolSmithSettings) (workDir string, manifest toolSmithFunctionsManifest) {
	t.Helper()
	workDir = t.TempDir()
	if err := stageToolSmithFunctions(workDir, settings, root); err != nil {
		t.Fatalf("stage: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "fn-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest %s: %v", data, err)
	}
	return workDir, manifest
}

func fnRun(t *testing.T, root string, settings sessionfs.NativeToolSmithSettings, function string) string {
	t.Helper()
	return runToolSmith(context.Background(), root, settings, smithRun(function, nil))
}

func TestToolSmithStageCopiesValidFilesPrivately(t *testing.T) {
	root := t.TempDir()
	fnWrite(t, root, "double", fnSource("double", "  return input.n * 2;"))
	workDir, manifest := fnStage(t, root, fnStageSettings())
	if len(manifest.Available) != 1 || manifest.Available[0] != "double" || len(manifest.Malformed) != 0 || manifest.LibraryError != "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	copied, err := os.ReadFile(filepath.Join(workDir, "fn", "double.ts"))
	if err != nil || string(copied) != fnSource("double", "  return input.n * 2;") {
		t.Fatalf("copy = %q, %v", copied, err)
	}
	info, err := os.Stat(filepath.Join(workDir, "fn", "double.ts"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Join(workDir, "fn"))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v", dirInfo, err)
	}
}

func TestToolSmithStageDoesNotCopyMalformedFiles(t *testing.T) {
	root := t.TempDir()
	fnWrite(t, root, "good", fnSource("good", "  return 1;"))
	fnWrite(t, root, "noheader", "export async function noheader(input) {}\n")
	fnWrite(t, root, "wrongsig", "// Has a header\nexport async function other(input) {}\n")
	fnWrite(t, root, "Bad-Name", fnSource("Bad-Name", "  return 1;"))
	fnWrite(t, root, "big", "// Big one\nexport async function big(input) {}\n"+strings.Repeat("// pad\n", 1000))
	workDir, manifest := fnStage(t, root, fnStageSettings())
	if len(manifest.Available) != 1 || manifest.Available[0] != "good" {
		t.Fatalf("available = %v", manifest.Available)
	}
	for name, want := range map[string]string{
		"noheader": "missing leading // header",
		"wrongsig": "no `export async function wrongsig(`",
		"Bad-Name": "name must match",
		"big":      "over function_max_bytes 4096",
	} {
		if got := manifest.Malformed[name]; !strings.Contains(got, want) {
			t.Errorf("malformed[%s] = %q, want %q", name, got, want)
		}
		if _, err := os.Stat(filepath.Join(workDir, "fn", name+".ts")); !os.IsNotExist(err) {
			t.Errorf("%s was copied: %v", name, err)
		}
	}
}

func TestToolSmithStageDoesNotCopyASymlinkLeavingTheLibrary(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, "outside/escape.ts", fnSource("escape", "  return 1;"))
	fnWrite(t, root, "good", fnSource("good", "  return 1;"))
	if err := os.Symlink(filepath.Join(root, "outside", "escape.ts"), filepath.Join(root, ".jad", "functions", "escape.ts")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	workDir, manifest := fnStage(t, root, fnStageSettings())
	if !strings.Contains(manifest.Malformed["escape"], "symlink leaves the functions directory") {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(workDir, "fn", "escape.ts")); !os.IsNotExist(err) {
		t.Fatalf("escaping symlink was copied: %v", err)
	}
}

func TestToolSmithStageCutsAvailableAndScannedFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		fnWrite(t, root, name, fnSource(name, "  return 1;"))
	}
	settings := fnStageSettings()
	settings.ListMaxResults = 2
	workDir, manifest := fnStage(t, root, settings)
	if len(manifest.Available) != 2 || manifest.More != 2 || manifest.ScanCut != 0 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(workDir, "fn", "d.ts")); err != nil {
		t.Fatalf("a name cut from the list is still staged: %v", err)
	}

	settings = fnStageSettings()
	settings.ListMaxFiles = 3
	workDir, manifest = fnStage(t, root, settings)
	if len(manifest.Available) != 3 || manifest.ScanCut != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(workDir, "fn", "d.ts")); !os.IsNotExist(err) {
		t.Fatalf("a file past list_max_files was copied: %v", err)
	}
}

func TestToolSmithStageReportsAnUnusableLibraryInTheManifest(t *testing.T) {
	root := t.TempDir()
	for name, dir := range map[string]string{"empty": "", "missing": ".jad/functions"} {
		settings := fnStageSettings()
		settings.FunctionsDir = dir
		workDir, manifest := fnStage(t, root, settings)
		if manifest.LibraryError == "" || len(manifest.Available) != 0 {
			t.Errorf("%s: manifest = %+v", name, manifest)
		}
		if _, err := os.Stat(filepath.Join(workDir, "fn")); !os.IsNotExist(err) {
			t.Errorf("%s: fn dir exists: %v", name, err)
		}
	}
}

func TestToolSmithFnCallsASavedFunction(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "double", fnSource("double", "  return { value: input.n * 2 };"))
	result := fnRun(t, root, settings, "return await fn.double({ n: 21 });")
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(execContent(result), `{"value":42}`) {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithFnSavedFunctionSeesGlobalToolsAndFn(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "inner", fnSource("inner", "  return input.n + 1;"))
	fnWrite(t, root, "outer", fnSource("outer", "  return { tools: typeof tools.read, inner: await fn.inner({ n: input.n }) };"))
	result := fnRun(t, root, settings, "return await fn.outer({ n: 1 });")
	if got := execContent(result); !strings.Contains(got, `"tools":"function"`) || !strings.Contains(got, `"inner":2`) {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithFnUnknownNameListsTheAvailableOnes(t *testing.T) {
	root, tmp := t.TempDir(), useOwnTempDir(t)
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "alpha", fnSource("alpha", "  return 1;"))
	fnWrite(t, root, "beta", fnSource("beta", "  return 1;"))
	result := fnRun(t, root, settings, "return await fn.nope({});")
	got := execContent(result)
	if !strings.Contains(result, `"status":"error"`) || !strings.Contains(got, "no saved function nope") || !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)

	settings.ListMaxResults = 1
	got = execContent(fnRun(t, root, settings, "return await fn.nope({});"))
	if !strings.Contains(got, "and 1 more") || strings.Contains(got, "beta") {
		t.Fatalf("content = %q", got)
	}
}

func TestToolSmithFnUnknownNameWithoutALibraryExplainsIt(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	result := fnRun(t, root, settings, "return await fn.nope({});")
	if !strings.Contains(result, `"status":"error"`) || !strings.Contains(execContent(result), "the function library is unavailable") {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithFnMalformedFunctionsAreReported(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "plain", "export async function plain(input) {}\n")
	fnWrite(t, root, "big", "// Big one\nexport async function big(input) {}\n"+strings.Repeat("// pad\n", 1000))
	got := execContent(fnRun(t, root, settings, "return await fn.plain({});"))
	if !strings.Contains(got, "function plain is malformed: missing leading // header") {
		t.Fatalf("content = %q", got)
	}
	got = execContent(fnRun(t, root, settings, "return await fn.big({});"))
	if !strings.Contains(got, "function big is malformed:") || !strings.Contains(got, "over function_max_bytes") {
		t.Fatalf("content = %q", got)
	}
}

func TestToolSmithFnRefusesAPathLikeName(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "double", fnSource("double", "  return 1;"))
	got := execContent(fnRun(t, root, settings, `return await fn["../x"]({});`))
	if !strings.Contains(got, "no saved function ../x") {
		t.Fatalf("content = %q", got)
	}
}

func TestToolSmithFnSeesAnEditedLibraryOnTheNextRun(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "value", fnSource("value", "  return 'first';"))
	if got := execContent(fnRun(t, root, settings, "return await fn.value({});")); !strings.Contains(got, "first") {
		t.Fatalf("first run: %q", got)
	}
	fnWrite(t, root, "value", fnSource("value", "  return 'second';"))
	if got := execContent(fnRun(t, root, settings, "return await fn.value({});")); !strings.Contains(got, "second") {
		t.Fatalf("second run: %q", got)
	}
}

func TestToolSmithFnSavedFunctionCallsToolsThroughTheBridge(t *testing.T) {
	root, tmp := t.TempDir(), shortTempDir(t)
	echo := bridgeTestTool(t, "read", true, func(arguments map[string]any) string { return "got:" + arguments["path"].(string) })
	settings := bridgeSettings(t, root, "read")
	settings.FunctionsDir = ".jad/functions"
	settings.ListMaxFiles = 200
	settings.ListMaxResults = 50
	settings.ListHeaderBytes = 2048
	settings.FunctionMaxBytes = 4096
	fnWrite(t, root, "peek", fnSource("peek", "  const r = await tools.read({ path: 'p' });\n  return r.content;"))
	result := runWithBridge(context.Background(), root, settings, bridgeNested(echo), tools.ExecutionContext{}, "return await fn.peek({ n: 1 });")
	if !strings.Contains(execContent(result), "got:p") || execField(result, "nested_calls") != "1" {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithFnBridgeCallsCountTowardMaxNestedCalls(t *testing.T) {
	root := t.TempDir()
	shortTempDir(t)
	echo := bridgeTestTool(t, "read", true, func(map[string]any) string { return "ok" })
	settings := bridgeSettings(t, root, "read")
	settings.MaxNestedCalls = 1
	settings.FunctionsDir = ".jad/functions"
	settings.ListMaxFiles = 200
	settings.ListMaxResults = 50
	settings.ListHeaderBytes = 2048
	settings.FunctionMaxBytes = 4096
	fnWrite(t, root, "peek", fnSource("peek", "  const a = await tools.read({ path: 'a' });\n  const b = await tools.read({ path: 'b' });\n  return [a.ok, b.ok];"))
	result := runWithBridge(context.Background(), root, settings, bridgeNested(echo), tools.ExecutionContext{}, "return await fn.peek({ n: 1 });")
	if !strings.Contains(execContent(result), "max_nested_calls of 1 reached") || execField(result, "nested_calls") != "1" {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithStageManifestListsAreNeverNull(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".jad", "functions"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir, _ := fnStage(t, root, fnStageSettings())
	data, err := os.ReadFile(filepath.Join(workDir, "fn-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"available":[]`) || !strings.Contains(string(data), `"callable":[]`) {
		t.Fatalf("manifest = %s", data)
	}
}

func TestToolSmithFnNameOnAnEmptyOrAllMalformedLibraryIsUnknown(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	if err := os.MkdirAll(filepath.Join(root, ".jad", "functions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"empty", "all malformed"} {
		result := fnRun(t, root, settings, "return await fn.x({});")
		got := execContent(result)
		if !strings.Contains(result, `"status":"error"`) || !strings.Contains(got, "no saved function x") || !strings.Contains(got, "available: (none)") {
			t.Fatalf("%s:\n%s", label, result)
		}
		fnWrite(t, root, "noheader", "export async function noheader(input) {}\n")
	}
}

func TestToolSmithFnCallsAFunctionCutFromTheAvailableList(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	settings.ListMaxResults = 1
	fnWrite(t, root, "alpha", fnSource("alpha", "  return 'a';"))
	fnWrite(t, root, "beta", fnSource("beta", "  return 'b';"))
	result := fnRun(t, root, settings, "return await fn.beta({});")
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(execContent(result), "b") {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithFnPrototypeNamesAreNotFunctions(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "double", fnSource("double", "  return 1;"))
	for _, name := range []string{"constructor", "__proto__", "toString"} {
		got := execContent(fnRun(t, root, settings, "return await fn."+name+"({});"))
		if !strings.Contains(got, "no saved function "+name) {
			t.Errorf("%s: content = %q", name, got)
		}
	}
}

func TestToolSmithFnAwaitingFnDoesNotHang(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	result := fnRun(t, root, settings, "await fn; return 'done';")
	if !strings.Contains(result, `"status":"ok"`) || !strings.Contains(execContent(result), "done") {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithFnSavedFunctionFailuresAreErrorResults(t *testing.T) {
	root := t.TempDir()
	settings := fnRunSettings(t, root)
	fnWrite(t, root, "boom", fnSource("boom", "  throw new Error('exploded');"))
	fnWrite(t, root, "broken", fnSource("broken", "  return (;"))
	result := fnRun(t, root, settings, "return await fn.boom({});")
	if !strings.Contains(result, `"status":"error"`) || !strings.Contains(execContent(result), "exploded") {
		t.Fatalf("throwing function:\n%s", result)
	}
	result = fnRun(t, root, settings, "return await fn.broken({});")
	if !strings.Contains(result, `"status":"error"`) {
		t.Fatalf("syntax error function:\n%s", result)
	}
}

func TestToolSmithStageReportsAFifoAsMalformedWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fnWrite(t, root, "good", fnSource("good", "  return 1;"))
	if err := syscall.Mkfifo(filepath.Join(root, ".jad", "functions", "x.ts"), 0o600); err != nil {
		t.Skipf("no fifo: %v", err)
	}
	workDir, manifest := fnStage(t, root, fnStageSettings())
	if !strings.Contains(manifest.Malformed["x"], "not a regular file") {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(workDir, "fn", "x.ts")); !os.IsNotExist(err) {
		t.Fatalf("fifo was copied: %v", err)
	}
}
