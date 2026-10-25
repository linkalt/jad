package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

type execAsyncFixture struct {
	host  *AsyncHost
	exec  tools.Tool
	async tools.Tool
	root  string
}

func newExecAsyncFixture(t *testing.T) execAsyncFixture {
	t.Helper()
	return newExecAsyncFixtureTimeout(t, 20_000)
}

func newExecAsyncFixtureTimeout(t *testing.T, timeoutMs int64) execAsyncFixture {
	t.Helper()
	requireInterpreters(t, "bash")
	root := t.TempDir()
	host := newTestAsyncHost(t)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.TimeoutMs = timeoutMs
	settings.Child.MaxTimeoutMs = 30_000
	execTool, err := NewExecTool(settings, "", host)
	if err != nil {
		t.Fatal(err)
	}
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return execAsyncFixture{host: host, exec: execTool, async: asyncTool, root: root}
}

func (f execAsyncFixture) runCtx(ctx context.Context, t *testing.T, owner string, arguments string) string {
	t.Helper()
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	out, err := f.exec.Execute(ctx, f.exec.NormalizeArguments(decoded), tools.ExecutionContext{CallID: "exec", WorkspaceRoot: f.root, SessionDir: f.root, SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f execAsyncFixture) run(t *testing.T, owner string, arguments string) string {
	t.Helper()
	return f.runCtx(context.Background(), t, owner, arguments)
}

func execRunIDOf(t *testing.T, result string) string {
	t.Helper()
	id := execField(result, "run")
	if !execRunID.MatchString(id) {
		t.Fatalf("no exec run id in:\n%s", result)
	}
	return id
}

func (f execAsyncFixture) waitState(t *testing.T, owner, id string, want asyncmgr.State) asyncmgr.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, err := f.host.Manager().Status(owner, id)
		if err == nil && entry.State == want {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle %s = %+v %v, want %s", id, entry, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecFinishingWithinBlockMsReturnsInlineAndConsumesTheSignal(t *testing.T) {
	f := newExecAsyncFixture(t)
	out := f.run(t, "s1", `{"code":"echo quick","block_ms":5000}`)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(execContent(out), "quick") || execField(out, "run") != "" {
		t.Fatalf("result:\n%s", out)
	}
	if later := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(later, "exec_") {
		t.Fatalf("the signal was delivered twice:\n%s", later)
	}
	if list := callAsync(t, f.async, "s1", map[string]any{"action": "list", "state": "all"}); !strings.Contains(list, "exec_") || !strings.Contains(list, "exec") {
		t.Fatalf("the finished run is not listed as an exec handle:\n%s", list)
	}
}

func TestExecOutlastingBlockMsGivesAHandleDeliveredOnceAndLeavesNothingBehind(t *testing.T) {
	tmp := useOwnTempDir(t)
	f := newExecAsyncFixture(t)
	baseline := runtime.NumGoroutine()
	started := f.run(t, "s1", `{"code":"sleep 1; echo slow-answer","block_ms":50}`)
	id := execRunIDOf(t, started)
	if execField(started, "state") != "running" || !strings.Contains(started, "async tool") {
		t.Fatalf("expected a running handle:\n%s", started)
	}
	if entry, err := f.host.Manager().Status("s1", id); err != nil || entry.State != asyncmgr.StateRunning || entry.Kind != asyncKindExec {
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
	if data, err := os.ReadFile(entry.Ref); err != nil || !strings.Contains(string(data), "slow-answer") || filepath.Base(filepath.Dir(entry.Ref)) != "exec" {
		t.Fatalf("the ref file %s: %q %v", entry.Ref, data, err)
	}
	requireGoroutinesSettle(t, baseline+2)
	requireNoLeftovers(t, tmp)
}

func TestExecHandleSurvivesTheCallIsStoppedByAsyncStopAndByTheOwnerFanOut(t *testing.T) {
	tmp := useOwnTempDir(t)
	f := newExecAsyncFixture(t)
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	first := execRunIDOf(t, f.runCtx(ctx, t, "s1", `{"code":"sleep 30","block_ms":0}`))
	cancel()
	if entry, err := f.host.Manager().Status("s1", first); err != nil || entry.State != asyncmgr.StateRunning {
		t.Fatalf("the program did not survive the call: %+v %v", entry, err)
	}
	second := execRunIDOf(t, f.run(t, "s1", `{"code":"sleep 30","block_ms":0}`))
	if stop := callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": second}); !strings.Contains(stop, "stopped") {
		t.Fatalf("stop = %s", stop)
	}
	f.waitState(t, "s1", second, asyncmgr.StateStopped)
	if n := f.host.Manager().CancelOwner("s1", "session_stopped"); n != 1 {
		t.Fatalf("CancelOwner reached %d handles, want the one still running", n)
	}
	entry := f.waitState(t, "s1", first, asyncmgr.StateStopped)
	if entry.Reason != "session_stopped" {
		t.Fatalf("reason = %q", entry.Reason)
	}
	requireGoroutinesSettle(t, baseline+2)
	requireNoLeftovers(t, tmp)
}

func TestExecHandleFoundRunningAfterARestartIsLost(t *testing.T) {
	f := newExecAsyncFixture(t)
	id := execRunIDOf(t, f.run(t, "s1", `{"code":"sleep 30","block_ms":0}`))
	second, err := NewAsyncHost(f.host.dir, asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.attachShell(execAsync)
	entry, err := second.Manager().Status("s1", id)
	if err != nil || entry.State != asyncmgr.StateLost || entry.Reason != asyncmgr.ReasonRestarted {
		t.Fatalf("after a restart the handle = %+v, %v; want lost", entry, err)
	}
	f.host.Manager().CancelOwner("s1", "test_cleanup")
}

func TestExecTimeoutIsTimedOutAndAFailedStartIsFailed(t *testing.T) {
	f := newExecAsyncFixture(t)
	short := newExecAsyncFixtureTimeout(t, 1)
	slow := execRunIDOf(t, short.run(t, "s1", `{"code":"sleep 30","block_ms":0}`))
	if entry := short.waitState(t, "s1", slow, asyncmgr.StateTimedOut); entry.Kind != asyncKindExec {
		t.Fatalf("entry = %+v", entry)
	}
	failing := execRunIDOf(t, f.run(t, "s1", `{"code":"echo oops >&2; exit 3","block_ms":0}`))
	waited := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "ids": []any{failing}, "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, failing) || !strings.Contains(waited, `"code":"3"`) || strings.Contains(waited, `"ok":true`) {
		t.Fatalf("a non-zero exit must not be ok:\n%s", waited)
	}
}

func TestExecWithoutAHostOrAnOwnerRunsToItsEnd(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	for name, host := range map[string]*AsyncHost{"no host": nil, "no owner": newTestAsyncHost(t)} {
		tool, err := NewExecTool(settings, "", host)
		if err != nil {
			t.Fatal(err)
		}
		owner := "s1"
		if host != nil {
			owner = ""
		}
		out, err := tool.Execute(context.Background(), tool.NormalizeArguments(map[string]any{"code": "sleep 0.3; echo inline-result", "block_ms": json.Number("0")}),
			tools.ExecutionContext{CallID: "exec", WorkspaceRoot: root, SessionDir: root, SessionKey: owner})
		if err != nil || !strings.Contains(execContent(out), "inline-result") || execField(out, "run") != "" {
			t.Fatalf("%s: must run to its end: %s %v", name, out, err)
		}
	}
}

func TestExecRefusedCallsRegisterNoHandle(t *testing.T) {
	f := newExecAsyncFixture(t)
	out := f.run(t, "s1", `{"lang":"nosuchlang","code":"echo x","block_ms":0}`)
	if !strings.Contains(out, "unknown_lang") {
		t.Fatalf("result:\n%s", out)
	}
	list, err := f.host.Manager().List(asyncmgr.ListQuery{Owner: "s1", State: "all", Limit: 10})
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("a refused call left handles: %+v %v", list, err)
	}
}

func TestExecSchemaAndDescriptionOfferBlockMs(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Minimum *int `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(execInputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if block, has := schema.Properties["block_ms"]; !has || block.Minimum == nil || *block.Minimum != 0 {
		t.Fatalf("block_ms in the schema: %+v", schema.Properties["block_ms"])
	}
	if !strings.Contains(execDescription, "block_ms") || !strings.Contains(execDescription, "async tool") {
		t.Fatal("the description must tell the model about block_ms and the async tool")
	}
	normalized := normalizeExecArguments(map[string]any{"code": "x", "block_ms": json.Number("-5")}, "bash")
	if normalized["block_ms"] != json.Number("0") {
		t.Fatalf("block_ms = %v, want 0", normalized["block_ms"])
	}
}
