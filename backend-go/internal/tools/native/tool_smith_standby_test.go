package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// toolSmithVariable hides what differs between any two runs: the scratch directory, durations (the
// summary's "N ms" and the information block's "nested_duration_ms: N", the time the bridge took)
// and the content token.
var toolSmithVariable = regexp.MustCompile(`tool-smith-[0-9]+|\d+ ms|nested_duration_ms: \d+|token=[A-Za-z0-9_-]+`)

func toolSmithComparable(rendered string) string {
	return toolSmithVariable.ReplaceAllString(rendered, "*")
}

// A bridged run reports how long its nested calls took; that is timing, not behaviour, so two
// renderings that differ only in it compare equal, and one that differs in a count does not.
func TestToolSmithComparableIgnoresTheBridgeDuration(t *testing.T) {
	render := func(summary int, nested string, duration int) string {
		return fmt.Sprintf("Script completed in %d ms\nnested_calls: %s\nnested_duration_ms: %d\ntoken=r_abc", summary, nested, duration)
	}
	if toolSmithComparable(render(19, "1", 0)) != toolSmithComparable(render(6, "1", 17)) {
		t.Error("durations make two equal runs differ")
	}
	if toolSmithComparable(render(6, "1", 0)) == toolSmithComparable(render(6, "2", 0)) {
		t.Error("a different nested call count compares equal")
	}
}

func runToolSmithWith(t *testing.T, root string, settings sessionfs.NativeToolSmithSettings, nested *ToolSmithNested, code string, input map[string]any) string {
	t.Helper()
	arguments := map[string]any{"code": code}
	if input != nil {
		arguments["input"] = input
	}
	result, _ := executeToolSmith(context.Background(), normalizeToolSmithArguments(arguments), settings, "", nested,
		tools.ExecutionContext{CallID: "smith-call", WorkspaceRoot: root, SessionDir: root}, nil)
	return result
}

// TestToolSmithStandbyRunsLikeAFreshStart is the standby's contract for tool_smith: a run in an
// interpreter that loaded the prelude before the call renders the same result as a fresh start,
// for output, returned values, errors, exit codes, input and saved functions.
func TestToolSmithStandbyRunsLikeAFreshStart(t *testing.T) {
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	library := filepath.Join(root, ".jad", "functions")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, "double.ts"), []byte("// Doubles x.\nexport async function double(input: { x: number }) {\n  return input.x * 2;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warm := bridgeSettings(t, root)
	warm.FunctionsDir = ".jad/functions"
	warm.ListMaxFiles, warm.ListMaxResults, warm.ListHeaderBytes, warm.FunctionMaxBytes = 50, 50, 2048, 65536
	warm.Child.Stderr.HeadBytes, warm.Child.Stderr.TailBytes, warm.Child.MaxBytes = 4096, 4096, 16384
	warm.StandbyPool, warm.StandbyIdleMs, warm.StandbyMax = 1, 60_000, 8
	cold := warm
	cold.StandbyPool = 0
	for index, code := range []string{
		"console.log(1);",
		"return {a: input.x * 2};",
		"throw new Error('boom');",
		"const f = () => { throw new TypeError('t'); };\nf();",
		"console.error('to stderr'); process.exitCode = 3;",
		"console.log(process.argv.length, typeof input, JSON.stringify(input));",
		"return await fn.double({x: input.x});",
		"await fn.nope({});",
		"const x: number = ;",
		"undefinedName.call();",
		"return await new Promise((r) => setTimeout(() => r('later'), 10));",
	} {
		input := map[string]any{"x": index}
		want := runToolSmithWith(t, root, cold, nil, code, input)
		runToolSmithWith(t, root, warm, nil, "1;", nil)
		waitForReadyStandby(t, pool, 1)
		_, _, takenBefore := pool.stats()
		got := runToolSmithWith(t, root, warm, nil, code, input)
		if _, _, taken := pool.stats(); taken != takenBefore+1 {
			t.Fatalf("%q did not run in a standby", code)
		}
		if toolSmithComparable(got) != toolSmithComparable(want) {
			t.Errorf("%q differs from a fresh start\nfresh:   %s\nstandby: %s", code, want, got)
		}
	}
}

// TestToolSmithStandbyBridgeUsesTheStandbysToken: a script in a standby calls nested tools over
// the bridge the call starts with the token the standby was started with.
func TestToolSmithStandbyBridgeUsesTheStandbysToken(t *testing.T) {
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	nested := bridgeNested(bridgeTestTool(t, "read", true, func(arguments map[string]any) string { return "read " + arguments["path"].(string) }))
	warm := bridgeSettings(t, root, "read")
	warm.StandbyPool, warm.StandbyIdleMs, warm.StandbyMax = 1, 60_000, 8
	cold := warm
	cold.StandbyPool = 0
	code := "const r = await tools.read({path: 'a.txt'}); console.log(r.ok, r.content);"
	want := runToolSmithWith(t, root, cold, nested, code, nil)
	runToolSmithWith(t, root, warm, nested, "1;", nil)
	waitForReadyStandby(t, pool, 1)
	got := runToolSmithWith(t, root, warm, nested, code, nil)
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatalf("taken = %d, want the bridged run in a standby", taken)
	}
	if !strings.Contains(execContent(got), "true read a.txt") {
		t.Fatalf("the nested call failed in a standby:\n%s", got)
	}
	if toolSmithComparable(got) != toolSmithComparable(want) {
		t.Fatalf("bridged run differs from a fresh start\nfresh:   %s\nstandby: %s", want, got)
	}
	// A run without nested tools has no bridge in its environment, so it takes no bridged standby.
	runToolSmithWith(t, root, cold, nil, "1;", nil)
	unbridged := warm
	unbridged.Tools = nil
	runToolSmithWith(t, root, unbridged, nil, "console.log(typeof process.env.TOOL_SMITH_SOCKET);", nil)
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatal("a run without a bridge took a standby started with one")
	}
}

// TestToolSmithStandbySeesALibraryChangedAfterItStarted: saved functions are staged by the call,
// not by the standby, so a function written after the standby started is callable.
func TestToolSmithStandbySeesALibraryChangedAfterItStarted(t *testing.T) {
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	library := filepath.Join(root, ".jad", "functions")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := bridgeSettings(t, root)
	settings.FunctionsDir = ".jad/functions"
	settings.ListMaxFiles, settings.ListMaxResults, settings.ListHeaderBytes, settings.FunctionMaxBytes = 50, 50, 2048, 65536
	settings.StandbyPool, settings.StandbyIdleMs, settings.StandbyMax = 1, 60_000, 8
	runToolSmithWith(t, root, settings, nil, "1;", nil)
	waitForReadyStandby(t, pool, 1)
	if err := os.WriteFile(filepath.Join(library, "late.ts"), []byte("// Late.\nexport async function late() {\n  return 'late answer';\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runToolSmithWith(t, root, settings, nil, "console.log(await fn.late());", nil)
	if !strings.Contains(execContent(got), "late answer") {
		t.Fatalf("a function written after the standby started:\n%s", got)
	}
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatal("the run did not take the standby")
	}
}
