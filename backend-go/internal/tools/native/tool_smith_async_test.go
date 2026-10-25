package native

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fakeToolSmithSettings runs the script through a shell that ignores the script file and
// writes its own answer, so these tests need neither bun nor node.
func fakeToolSmithSettings(t *testing.T, root, shellScript string) sessionfs.NativeToolSmithSettings {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	settings := toolSmithTestSettings0(t, root)
	settings.Interpreter = []string{"/bin/sh", "-c", shellScript, "{file}"}
	return settings
}

func toolSmithTestSettings0(t *testing.T, root string) sessionfs.NativeToolSmithSettings {
	t.Helper()
	child := bashTestSettings(t, root)
	child.TimeoutMs = 5_000
	child.MaxTimeoutMs = 10_000
	return sessionfs.NativeToolSmithSettings{Child: child, CodeMaxBytes: 4096, ResultMaxBytes: 1024}
}

type toolSmithAsyncFixture struct {
	host  *AsyncHost
	smith tools.Tool
	async tools.Tool
	root  string
}

func newToolSmithAsyncFixture(t *testing.T, shellScript string) toolSmithAsyncFixture {
	t.Helper()
	root := t.TempDir()
	host := newTestAsyncHost(t)
	smith, err := NewToolSmithTool(fakeToolSmithSettings(t, root, shellScript), "", nil, host)
	if err != nil {
		t.Fatal(err)
	}
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return toolSmithAsyncFixture{host: host, smith: smith, async: asyncTool, root: root}
}

func (f toolSmithAsyncFixture) run(t *testing.T, owner string, arguments string) string {
	t.Helper()
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	out, err := f.smith.Execute(context.Background(), f.smith.NormalizeArguments(decoded), tools.ExecutionContext{CallID: "smith", WorkspaceRoot: f.root, SessionDir: f.root, SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func smithRunIDOf(t *testing.T, result string) string {
	t.Helper()
	id := execField(result, "run")
	if !toolSmithRunID.MatchString(id) {
		t.Fatalf("no script run id in:\n%s", result)
	}
	return id
}

func TestToolSmithWithoutParametersListsTheLibrary(t *testing.T) {
	root := t.TempDir()
	listTestWrite(t, root, ".jad/functions/read_file.ts", listTestFunction("read_file", "Read a file"))
	listTestWrite(t, root, ".jad/functions/write_file.ts", listTestFunction("write_file", "Write a file"))
	listTestWrite(t, root, ".jad/functions/grep_all.ts", listTestFunction("grep_all", "Search"))
	settings := listTestSettings(".jad/functions")
	call := func(arguments map[string]any) string {
		out, err := executeToolSmith(context.Background(), arguments, settings, "", nil, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return execContent(out)
	}
	if got := call(map[string]any{}); !strings.Contains(got, "read_file") || !strings.Contains(got, "write_file") || !strings.Contains(got, "grep_all") {
		t.Fatalf("no parameters:\n%s", got)
	}
	got := call(map[string]any{"filters": []any{"read*"}})
	if !strings.Contains(got, "read_file") || strings.Contains(got, "write_file") || strings.Contains(got, "grep_all") {
		t.Fatalf("read* filter:\n%s", got)
	}
}

func TestToolSmithFilterWildcardsAndSubstrings(t *testing.T) {
	entry := toolSmithListEntry{name: "read_file", description: "Reads A File"}
	for filter, want := range map[string]bool{
		"read*": true, "*file*": true, "*_file": true, "r?ad_file": true, "read_fil?": true, "read": true, "FILE": true,
		"read?": false, "file*": false, "*x*": false, "*": true, "reads a*": true, "a*b*c": false, "": true,
	} {
		filters := []toolSmithFilter{}
		if filter != "" {
			filters = append(filters, newToolSmithFilter(strings.ToLower(filter)))
		}
		if got := toolSmithMatchesFilters(entry, filters); got != want {
			t.Errorf("filter %q matches = %v, want %v", filter, got, want)
		}
	}
}

func TestToolSmithSchemaHasNoListObject(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(toolSmithInputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, has := schema.Properties["list"]; has {
		t.Fatal("the schema still has a list object")
	}
	for _, name := range []string{"filters", "code", "input", "block_ms"} {
		if _, has := schema.Properties[name]; !has {
			t.Fatalf("the schema lacks %s", name)
		}
	}
	if _, has := schema.Properties["run"]; has {
		t.Fatal("the schema still has a run object")
	}
	for name, raw := range schema.Properties {
		if strings.Contains(string(raw), `"properties"`) {
			t.Fatalf("%s is a nested object schema: %s", name, raw)
		}
	}
}

func TestToolSmithRunWithinBlockMsReturnsTheResultAndConsumesTheSignal(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `echo quick > "$EXEC_RESULT"`)
	out := f.run(t, "s1", `{"code":"return 1;","block_ms":5000}`)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(execContent(out), "quick") {
		t.Fatalf("result:\n%s", out)
	}
	later := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")})
	if strings.Contains(later, "smith_") {
		t.Fatalf("the signal was delivered twice:\n%s", later)
	}
	list := callAsync(t, f.async, "s1", map[string]any{"action": "list", "state": "all"})
	if !strings.Contains(list, "smith_") || !strings.Contains(list, "tool_smith") {
		t.Fatalf("the finished script is not listed as a tool_smith handle:\n%s", list)
	}
}

func TestToolSmithEndToEndBlockMsHandleWaitOnceStopAndNoGoroutineLeak(t *testing.T) {
	tmp := useOwnTempDir(t)
	f := newToolSmithAsyncFixture(t, `sleep 1; echo slow-answer > "$EXEC_RESULT"`)
	baseline := runtime.NumGoroutine()
	started := f.run(t, "s1", `{"code":"return 1;","block_ms":50}`)
	id := smithRunIDOf(t, started)
	if execField(started, "state") != "running" || !strings.Contains(started, "async tool") {
		t.Fatalf("expected a running handle:\n%s", started)
	}
	if entry, err := f.host.Manager().Status("s1", id); err != nil || entry.State != asyncmgr.StateRunning || entry.Kind != asyncKindToolSmith {
		t.Fatalf("handle after the call returned: %+v %v", entry, err)
	}
	if _, err := f.host.Manager().Status("other", id); err == nil {
		t.Fatal("another owner can see the handle")
	}
	waited := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("10000")})
	if strings.Count(waited, `"id":"`+id+`"`) != 1 || !strings.Contains(waited, "slow-answer") || !strings.Contains(waited, `"ok":true`) {
		t.Fatalf("wait lacks the result, once:\n%s", waited)
	}
	if again := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(again, `"id":"`+id+`"`) {
		t.Fatalf("the result was returned twice:\n%s", again)
	}
	entry, err := f.host.Manager().Status("s1", id)
	if err != nil || entry.Ref == "" {
		t.Fatalf("status: %+v %v", entry, err)
	}
	if data, err := os.ReadFile(entry.Ref); err != nil || !strings.Contains(string(data), "slow-answer") {
		t.Fatalf("the ref file %s: %q %v", entry.Ref, data, err)
	}

	second := smithRunIDOf(t, f.run(t, "s1", `{"code":"return 2;","block_ms":0}`))
	stop := callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": second})
	if !strings.Contains(stop, "stopped") {
		t.Fatalf("stop = %s", stop)
	}
	if entry, err := f.host.Manager().Status("s1", second); err != nil || entry.State != asyncmgr.StateStopped {
		t.Fatalf("after stop: %+v %v", entry, err)
	}
	requireGoroutinesSettle(t, baseline+2)
	requireNoLeftovers(t, tmp)
}

func TestToolSmithRunSurvivesTheEndOfTheCallAndIsStoppedByTheOwnerFanOut(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `sleep 30`)
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		out, _ := f.smith.Execute(ctx, f.smith.NormalizeArguments(map[string]any{"code": "return 1;", "block_ms": json.Number("0")}),
			tools.ExecutionContext{CallID: "smith", WorkspaceRoot: f.root, SessionDir: f.root, SessionKey: "s1"})
		done <- out
	}()
	id := smithRunIDOf(t, <-done)
	cancel()
	if entry, err := f.host.Manager().Status("s1", id); err != nil || entry.State != asyncmgr.StateRunning {
		t.Fatalf("the script did not survive the call: %+v %v", entry, err)
	}
	if n := f.host.Manager().CancelOwner("s1", "session_stopped"); n != 1 {
		t.Fatalf("CancelOwner reached %d handles, want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entry, err := f.host.Manager().Status("s1", id); err == nil && entry.State == asyncmgr.StateStopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry, err := f.host.Manager().Status("s1", id); err != nil || entry.State != asyncmgr.StateStopped {
		t.Fatalf("after the fan-out: %+v %v", entry, err)
	}
	requireGoroutinesSettle(t, baseline+2)
}

func TestToolSmithRunWithoutAHostRunsToItsEnd(t *testing.T) {
	root := t.TempDir()
	smith, err := NewToolSmithTool(fakeToolSmithSettings(t, root, `sleep 0.3; echo blocked-result > "$EXEC_RESULT"`), "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := smith.Execute(context.Background(), smith.NormalizeArguments(map[string]any{"code": "return 1;", "block_ms": json.Number("0")}),
		tools.ExecutionContext{CallID: "smith", WorkspaceRoot: root, SessionDir: root, SessionKey: "s1"})
	if err != nil || !strings.Contains(execContent(out), "blocked-result") {
		t.Fatalf("a session without the async tool must run to its end: %s %v", out, err)
	}
}

func TestToolSmithRunTimeoutAndFailureStates(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `echo oops >&2; exit 3`)
	id := smithRunIDOf(t, f.run(t, "s1", `{"code":"return 1;","block_ms":0}`))
	waited := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, id) || !strings.Contains(waited, `"code":"3"`) || strings.Contains(waited, `"ok":true`) {
		t.Fatalf("a non-zero exit must not be ok:\n%s", waited)
	}
}

func TestToolSmithRunIDsAreNotAcceptedAsPathsForDetail(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `echo x > "$EXEC_RESULT"`)
	driver := &shellDriver{host: f.host, spec: toolSmithAsync}
	for _, id := range []string{"../../etc/passwd", "smith_../x", "smith_zzzzzzzz", ""} {
		if got := driver.Detail(id, 100); got != "" {
			t.Errorf("Detail(%q) = %q", id, got)
		}
	}
}

func TestToolSmithSummariesSayScriptAndNoReturnMeansNoResultBlock(t *testing.T) {
	f := newToolSmithAsyncFixture(t, `echo printed`)
	out := f.run(t, "s1", `{"code":"console.log(1);","block_ms":5000}`)
	if !strings.Contains(out, `"summary":"Script completed in `) || strings.Contains(out, "Command") {
		t.Fatalf("summary wording:\n%s", out)
	}
	if strings.Contains(execContent(out), "[result]") || !strings.Contains(execContent(out), "printed") {
		t.Fatalf("a body without a return value must show only its output:\n%s", out)
	}
	failing := newToolSmithAsyncFixture(t, `exit 3`)
	out = failing.run(t, "s1", `{"code":"return 1;","block_ms":5000}`)
	if !strings.Contains(out, `"summary":"Script exited with 3"`) {
		t.Fatalf("failure wording:\n%s", out)
	}
}

func TestToolSmithDescriptionStatesThatNoReturnGivesNoResultBlock(t *testing.T) {
	if !strings.Contains(toolSmithDescription, "without return") {
		t.Fatal("the description must say what a body without return gives")
	}
}
