package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleAsyncManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "async.json")

type asyncFakeDriver struct {
	host *AsyncHost
	mu   sync.Mutex
	stop []string
}

func (d *asyncFakeDriver) Kind() asyncmgr.Kind { return "fake" }

func (d *asyncFakeDriver) Stop(id, reason string) {
	d.mu.Lock()
	d.stop = append(d.stop, id)
	d.mu.Unlock()
	_, _ = d.host.Manager().Complete(id, asyncmgr.Result{State: asyncmgr.StateStopped, Reason: reason})
}

func asyncConfig(t *testing.T) sessionfs.SessionConfig {
	t.Helper()
	dir := t.TempDir()
	return sessionfs.SessionConfig{
		SettingPath:         filepath.Join(dir, "setting.json"),
		ToolDefinitionPaths: []string{sampleAsyncManifest},
		StoragePaths:        sessionfs.SessionStoragePaths{ToolStorage: filepath.Join(dir, "tools")},
		NativeAsync:         &sessionfs.NativeAsyncSettings{Config: asyncmgr.DefaultConfig()},
	}
}

func newAsyncToolFixture(t *testing.T) (tools.Tool, *AsyncHost, *asyncFakeDriver) {
	t.Helper()
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	driver := &asyncFakeDriver{host: host}
	if err := host.Manager().AddDriver(driver); err != nil {
		t.Fatal(err)
	}
	tool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return tool, host, driver
}

func callAsync(t *testing.T, tool tools.Tool, owner string, arguments map[string]any) string {
	t.Helper()
	if tool.NormalizeArguments != nil {
		arguments = tool.NormalizeArguments(arguments)
	}
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c1", SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestAsyncToolIsRegisteredFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(asyncConfig(t))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != asyncToolID || definitions[0].Name != "async" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestAsyncToolRequiresItsManifest(t *testing.T) {
	config := asyncConfig(t)
	config.ToolDefinitionPaths = nil
	if _, err := BuildCatalog(config); err == nil || !strings.Contains(err.Error(), "tools.native.async is configured but native.async is missing from tool_definition_paths") {
		t.Fatalf("error = %v", err)
	}
}

func TestAsyncToolAbsentWhenNotConfigured(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: "setting.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Definitions()) != 0 {
		t.Fatalf("definitions = %v", catalog.Definitions())
	}
}

func TestAsyncToolChangesStateOnlyForStop(t *testing.T) {
	catalog, err := BuildCatalog(asyncConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		`{"action":"wait","wait_ms":5}`: false, `{"action":"list"}`: false, `{"action":"status","id":"x"}`: false,
		`{"action":"stop","id":"x"}`: true, `{"action":"kill","id":"x"}`: true, `{"id":"x"}`: false, `{}`: false, `{"wait_ms":1}`: false,
	} {
		raw, _ := json.Marshal(map[string]string{"name": "async", "arguments": name})
		if got := catalog.ChangesState(raw); got != want {
			t.Errorf("ChangesState(%s) = %t, want %t", name, got, want)
		}
	}
}

func TestAsyncToolIsNeverInASubagentPoolOrAScript(t *testing.T) {
	tool, _, _ := newAsyncToolFixture(t)
	if !tool.ReadOnly {
		t.Fatal("async must be ReadOnly for the exclusion to matter")
	}
	readOnly, changers := childToolPools(sessionfs.NativeSubagentSettings{AllowWrite: true}, []tools.Tool{readOnlyTool("read"), tool})
	for _, pooled := range append(readOnly, changers...) {
		if pooled.Definition.ID == asyncToolID {
			t.Fatalf("a sub-agent pool contains async: %v", poolNames(readOnly))
		}
	}
	if !containsName(sessionfs.NativeToolSmithExcludedTools(), "async") {
		t.Fatal("tool_smith scripts may call async")
	}
}

func TestAsyncToolWaitReturnsEveryReadyHandleAndConsumesThem(t *testing.T) {
	tool, host, _ := newAsyncToolFixture(t)
	m := host.Manager()
	for _, id := range []string{"h1", "h2", "h3"} {
		if err := m.Register(asyncmgr.Registration{ID: id, Owner: "s1", Kind: "fake", Label: "job " + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"h1", "h2"} {
		if _, err := m.Complete(id, asyncmgr.Result{State: asyncmgr.StateDone, OK: true, Code: "0", Summary: "out-" + id, Ref: "/r/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	out := callAsync(t, tool, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("2000")})
	for _, want := range []string{"out-h1", "out-h2", "pending: 1", "/r/h1"} {
		if !strings.Contains(out, want) {
			t.Errorf("wait output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "out-h3") {
		t.Errorf("wait returned an unfinished handle:\n%s", out)
	}
	again := callAsync(t, tool, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")})
	if strings.Contains(again, "out-h1") || !strings.Contains(again, "pending: 1") {
		t.Errorf("a delivered signal came back:\n%s", again)
	}
}

func TestAsyncToolWaitWakesOnCompletion(t *testing.T) {
	tool, host, _ := newAsyncToolFixture(t)
	m := host.Manager()
	if err := m.Register(asyncmgr.Registration{ID: "slow", Owner: "s1", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = m.Complete("slow", asyncmgr.Result{State: asyncmgr.StateDone, OK: true, Summary: "woke"})
	}()
	begun := time.Now()
	out := callAsync(t, tool, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("10000")})
	if !strings.Contains(out, "woke") || time.Since(begun) > 5*time.Second {
		t.Fatalf("wait did not wake on completion (%v):\n%s", time.Since(begun), out)
	}
}

func TestAsyncToolOwnerIsolation(t *testing.T) {
	tool, host, _ := newAsyncToolFixture(t)
	if err := host.Manager().Register(asyncmgr.Registration{ID: "mine", Owner: "s1", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"status", "stop"} {
		out := callAsync(t, tool, "s2", map[string]any{"action": action, "id": "mine"})
		if !strings.Contains(out, "unknown_handle") {
			t.Errorf("%s by another session = %s", action, out)
		}
	}
	if list := callAsync(t, tool, "s2", map[string]any{"action": "list"}); strings.Contains(list, "mine") {
		t.Errorf("another session lists the handle:\n%s", list)
	}
}

func TestAsyncToolStopStatusAndList(t *testing.T) {
	tool, host, driver := newAsyncToolFixture(t)
	if err := host.Manager().Register(asyncmgr.Registration{ID: "job", Owner: "s1", Kind: "fake", Label: "a job"}); err != nil {
		t.Fatal(err)
	}
	if out := callAsync(t, tool, "s1", map[string]any{"id": "job"}); !strings.Contains(out, "state\":\"running") && !strings.Contains(out, "running") {
		t.Errorf("status = %s", out)
	}
	list := callAsync(t, tool, "s1", map[string]any{})
	if !strings.Contains(list, "a job") || !strings.Contains(list, "active") {
		t.Errorf("list = %s", list)
	}
	stopped := callAsync(t, tool, "s1", map[string]any{"action": "stop", "id": "job"})
	if !strings.Contains(stopped, "stopped") || len(driver.stop) != 1 {
		t.Errorf("stop = %s (driver stops %v)", stopped, driver.stop)
	}
	again := callAsync(t, tool, "s1", map[string]any{"action": "stop", "id": "job"})
	if !strings.Contains(again, "stopped") || len(driver.stop) != 1 {
		t.Errorf("a second stop must be idempotent: %s (driver stops %v)", again, driver.stop)
	}
	if done := callAsync(t, tool, "s1", map[string]any{"action": "list", "state": "stopped"}); !strings.Contains(done, "job") {
		t.Errorf("state filter list = %s", done)
	}
}

func TestAsyncToolRejectsFieldsOfOtherActions(t *testing.T) {
	tool, _, _ := newAsyncToolFixture(t)
	for name, arguments := range map[string]map[string]any{
		"list with wait_ms": {"action": "list", "wait_ms": json.Number("1")},
		"wait with id":      {"action": "wait", "id": "x"},
		"status with kind":  {"action": "status", "id": "x", "kind": "process"},
	} {
		if out := callAsync(t, tool, "s1", arguments); !strings.Contains(out, "invalid_field") {
			t.Errorf("%s = %s", name, out)
		}
	}
	if out := callAsync(t, tool, "s1", map[string]any{"action": "stop"}); !strings.Contains(out, "invalid_target") {
		t.Errorf("stop without id = %s", out)
	}
	if out := callAsync(t, tool, "s1", map[string]any{"action": "dance"}); !strings.Contains(out, "invalid_action") {
		t.Errorf("unknown action = %s", out)
	}
}

func TestAsyncToolWaitEndsWhenTheCallIsCancelled(t *testing.T) {
	tool, host, _ := newAsyncToolFixture(t)
	if err := host.Manager().Register(asyncmgr.Registration{ID: "j", Owner: "s1", Kind: "fake"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := tool.Execute(ctx, map[string]any{"action": "wait", "wait_ms": json.Number("60000")}, tools.ExecutionContext{SessionKey: "s1"})
	if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if c := host.Manager().Counts("s1"); c.Active != 1 {
		t.Fatalf("cancelled wait changed the handle: %+v", c)
	}
}
