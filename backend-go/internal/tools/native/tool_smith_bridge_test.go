package native

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// shortTempDir points TMPDIR at a short directory: a Unix socket path is limited to about 100 bytes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tsb")
	if err != nil {
		t.Skipf("no short temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	return dir
}

var bridgeAnySchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)

func bridgeTestTool(t *testing.T, name string, readOnly bool, run func(arguments map[string]any) string) tools.Tool {
	t.Helper()
	validator, err := tools.CompileSchema(bridgeAnySchema)
	if err != nil {
		t.Fatal(err)
	}
	return tools.Tool{
		Definition: tools.Definition{ID: "native." + name, Source: "native", Name: name, Description: name, InputSchema: bridgeAnySchema},
		Validator:  validator,
		ReadOnly:   readOnly,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			text := run(arguments)
			result, err := tools.RenderToolResult(tools.ResultOptions{
				ID: execution.CallID, Status: tools.StatusOK, Summary: "done " + name,
				Information: []tools.Information{{Key: "kind", Value: "file"}},
				Content:     &tools.Content{Text: text},
			})
			return result, err
		},
	}
}

func bridgeSettings(t *testing.T, root string, names ...string) sessionfs.NativeToolSmithSettings {
	t.Helper()
	settings := toolSmithTestSettings(t, root)
	settings.ResultMaxBytes = 4096
	settings.NestedResultMaxBytes = 4096
	settings.NestedRequestMaxBytes = 65536
	settings.MaxNestedCalls = 100
	settings.MaxParallelCalls = 4
	settings.Tools = names
	return settings
}

func bridgeNested(list ...tools.Tool) *ToolSmithNested {
	nested := &ToolSmithNested{}
	nested.Bind(list, sessionfs.ProfileToolSettings{})
	return nested
}

func runWithBridge(ctx context.Context, root string, settings sessionfs.NativeToolSmithSettings, nested *ToolSmithNested, execution tools.ExecutionContext, function string) string {
	execution.CallID, execution.WorkspaceRoot, execution.SessionDir = "smith-call", root, root
	result, _ := executeToolSmith(ctx, normalizeToolSmithArguments(smithRun(function, nil)), settings, "", nested, execution, nil)
	return result
}

func requireGoroutinesSettle(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
}

func TestToolSmithBridgePromiseAllOfReads(t *testing.T) {
	root := t.TempDir()
	echo := bridgeTestTool(t, "read", true, func(arguments map[string]any) string { return arguments["path"].(string) })
	settings := bridgeSettings(t, root, "read")
	result := runWithBridge(context.Background(), root, settings, bridgeNested(echo), tools.ExecutionContext{}, `
const all = await Promise.all(['x', 'y', 'z'].map((p) => tools.read({ path: p })));
return all.map((r) => r.content).join(',');`)
	if !strings.Contains(execContent(result), "x,y,z") || execField(result, "nested_calls") != "3" {
		t.Fatalf("result:\n%s", result)
	}
}

func TestToolSmithBridgeRefusesToolsOutsideTheAllowlistAndTheExcludedOnes(t *testing.T) {
	root := t.TempDir()
	excluded := sessionfs.NativeToolSmithExcludedTools()
	if len(excluded) != 6 {
		t.Fatalf("excluded list = %v", excluded)
	}
	list := []tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" }), bridgeTestTool(t, "write", false, func(map[string]any) string { return "w" })}
	for _, name := range excluded {
		list = append(list, bridgeTestTool(t, name, false, func(map[string]any) string { return "ran" }))
	}
	// The excluded names are in the settings on purpose: the config loader refuses them, the bridge must too.
	settings := bridgeSettings(t, root, append([]string{"read"}, excluded...)...)
	result := runWithBridge(context.Background(), root, settings, bridgeNested(list...), tools.ExecutionContext{}, `
const out = [];
for (const name of ['write', 'exec', 'async', 'subagent', 'tool_smith', 'task', 'load_tool']) {
  const r = await tools[name]({});
  out.push(name + ':' + r.status + ':' + r.ok + ':' + r.summary.includes('not allowed; allowed: read'));
}
return out.join('|');`)
	content := execContent(result)
	for _, name := range append([]string{"write"}, excluded...) {
		if !strings.Contains(content, name+":invalid_input:false:true") {
			t.Fatalf("%s was not refused:\n%s", name, result)
		}
	}
	if execField(result, "nested_calls") != "" {
		t.Fatalf("refused calls were counted:\n%s", result)
	}
}

func TestToolSmithNestedCatalogHoldsOnlyAllowedNativeTools(t *testing.T) {
	list := []tools.Tool{bridgeTestTool(t, "read", true, nil), bridgeTestTool(t, "grep", true, nil), bridgeTestTool(t, "exec", false, nil), mcpReadOnlyTool("read_mcp")}
	nested := &ToolSmithNested{}
	nested.Bind(list, sessionfs.ProfileToolSettings{Restricted: true, Deny: []string{"grep"}})
	got := poolNames(nested.allowed(sessionfs.NativeToolSmithSettings{Tools: []string{"exec", "grep", "read", "read_mcp"}}))
	if len(got) != 1 || got[0] != "read" {
		t.Fatalf("allowed = %v", got)
	}
	if (*ToolSmithNested)(nil).allowed(sessionfs.NativeToolSmithSettings{Tools: []string{"read"}}) != nil {
		t.Fatal("a nil ToolSmithNested gave tools")
	}
}

func TestToolSmithRunWithoutToolsOpensNoBridge(t *testing.T) {
	root, tmp := t.TempDir(), shortTempDir(t)
	settings := bridgeSettings(t, root)
	result := runWithBridge(context.Background(), root, settings, bridgeNested(), tools.ExecutionContext{}, `
const r = await tools.read({ path: 'a' });
return typeof process.env.TOOL_SMITH_SOCKET + ':' + r.status;`)
	if !strings.Contains(execContent(result), "undefined:invalid_input") {
		t.Fatalf("result:\n%s", result)
	}
	requireNoLeftovers(t, tmp)
}

func TestToolSmithBridgeRejectsAWrongTokenAndAMalformedFrame(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	var ran atomic.Int64
	echo := bridgeTestTool(t, "read", true, func(map[string]any) string { ran.Add(1); return "ok" })
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "read"), []tools.Tool{echo}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	exchange := func(frame string) map[string]any {
		t.Helper()
		if _, err := conn.Write([]byte(frame + "\n")); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no reply to %q: %v", frame, err)
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			t.Fatalf("reply %q: %v", line, err)
		}
		return reply
	}
	if reply := exchange(`{"id":1,"token":"nope","tool":"read","arguments":{}}`); reply["bridge_error"] != "wrong token" || reply["ok"] != false {
		t.Fatalf("wrong token reply = %v", reply)
	}
	if reply := exchange(`{"id":2,"tool":"read","arguments":{}}`); reply["bridge_error"] != "wrong token" {
		t.Fatalf("missing token reply = %v", reply)
	}
	if reply := exchange(`this is not json`); reply["bridge_error"] == nil || !strings.Contains(reply["bridge_error"].(string), "malformed frame") {
		t.Fatalf("malformed reply = %v", reply)
	}
	if reply := exchange(`{"token":"` + bridge.token + `","tool":"read"}`); reply["bridge_error"] == nil {
		t.Fatalf("frame without id reply = %v", reply)
	}
	if ran.Load() != 0 {
		t.Fatal("a refused frame ran a tool")
	}
	// The connection is still usable after the refusals.
	if reply := exchange(`{"id":3,"token":"` + bridge.token + `","tool":"read","arguments":{}}`); reply["ok"] != true || reply["status"] != "ok" || reply["content"] != "ok" {
		t.Fatalf("good frame reply = %v", reply)
	}
}

func TestToolSmithBridgeStartsNoMoreHandlersThanMaxNestedCalls(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	release := make(chan struct{})
	var ran atomic.Int64
	slow := bridgeTestTool(t, "read", true, func(map[string]any) string { ran.Add(1); <-release; return "ok" })
	settings := bridgeSettings(t, root, "read")
	settings.MaxNestedCalls = 3
	const maxCalls = 3
	bridge, err := startToolSmithBridge(context.Background(), dir, settings, []tools.Tool{slow}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const frames = 50
	before := runtime.NumGoroutine()
	for id := 1; id <= frames; id++ {
		frame := `{"id":` + strconv.Itoa(id) + `,"token":"` + bridge.token + `","tool":"read","arguments":{}}` + "\n"
		if _, err := conn.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < frames-maxCalls; i++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
		var reply map[string]any
		if err := json.Unmarshal(line, &reply); err != nil {
			t.Fatal(err)
		}
		if reply["bridge_error"] != "max_nested_calls of 3 reached" {
			t.Fatalf("extra frame reply = %v", reply)
		}
	}
	if grown := runtime.NumGoroutine() - before; grown > maxCalls+10 {
		t.Fatalf("goroutines grew by %d for %d frames", grown, frames)
	}
	close(release)
	for i := 0; i < maxCalls; i++ {
		if _, err := reader.ReadBytes('\n'); err != nil {
			t.Fatalf("handler reply %d: %v", i, err)
		}
	}
	if ran.Load() > maxCalls {
		t.Fatalf("%d handlers ran, max %d", ran.Load(), settings.MaxNestedCalls)
	}
}

func TestToolSmithBridgeWaitsForUnawaitedNestedWrites(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "late.txt")
	write := bridgeTestTool(t, "write", false, func(map[string]any) string {
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(target, []byte("x"), 0o600)
		return "w"
	})
	result := runWithBridge(context.Background(), root, bridgeSettings(t, root, "write"), bridgeNested(write), tools.ExecutionContext{}, `
tools.write({});
return 1;`)
	if _, err := os.Stat(target); err != nil || execField(result, "nested_writes") != "1" {
		t.Fatalf("stat err=%v result:\n%s", err, result)
	}
}

type concurrencyProbe struct {
	active, peak atomic.Int64
}

func (p *concurrencyProbe) run(sleep time.Duration) func(map[string]any) string {
	return func(map[string]any) string {
		now := p.active.Add(1)
		for {
			peak := p.peak.Load()
			if now <= peak || p.peak.CompareAndSwap(peak, now) {
				break
			}
		}
		time.Sleep(sleep)
		p.active.Add(-1)
		return "done"
	}
}

func TestToolSmithBridgeSerializesConcurrentNestedWrites(t *testing.T) {
	root := t.TempDir()
	probe := &concurrencyProbe{}
	write := bridgeTestTool(t, "write", false, probe.run(60*time.Millisecond))
	result := runWithBridge(context.Background(), root, bridgeSettings(t, root, "write"), bridgeNested(write), tools.ExecutionContext{}, `
const all = await Promise.all([1, 2, 3].map(() => tools.write({})));
return all.every((r) => r.ok);`)
	if !strings.Contains(execContent(result), "true") || probe.peak.Load() != 1 || execField(result, "nested_writes") != "3" {
		t.Fatalf("peak=%d result:\n%s", probe.peak.Load(), result)
	}
}

func TestToolSmithBridgeBoundsParallelReads(t *testing.T) {
	root := t.TempDir()
	probe := &concurrencyProbe{}
	read := bridgeTestTool(t, "read", true, probe.run(150*time.Millisecond))
	settings := bridgeSettings(t, root, "read")
	settings.MaxParallelCalls = 2
	result := runWithBridge(context.Background(), root, settings, bridgeNested(read), tools.ExecutionContext{}, `
const all = await Promise.all([1, 2, 3, 4, 5, 6].map(() => tools.read({})));
return all.length;`)
	if !strings.Contains(execContent(result), "6") || probe.peak.Load() != 2 {
		t.Fatalf("peak=%d result:\n%s", probe.peak.Load(), result)
	}
}

func TestToolSmithBridgeRefusesCallsOverMaxNestedCalls(t *testing.T) {
	root := t.TempDir()
	read := bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })
	settings := bridgeSettings(t, root, "read")
	settings.MaxNestedCalls = 2
	result := runWithBridge(context.Background(), root, settings, bridgeNested(read), tools.ExecutionContext{}, `
let done = 0;
try {
  for (let i = 0; i < 5; i++) { await tools.read({}); done++; }
} catch (error) {
  return done + ':' + error.message;
}
return 'no error';`)
	if got := execContent(result); !strings.Contains(got, "2:") || !strings.Contains(got, "max_nested_calls") {
		t.Fatalf("result:\n%s", result)
	}
	if execField(result, "nested_calls") != "2" {
		t.Fatalf("counter:\n%s", result)
	}
}

func TestToolSmithBridgeCutsANestedResultAtAUTF8Boundary(t *testing.T) {
	root := t.TempDir()
	long := bridgeTestTool(t, "read", true, func(map[string]any) string { return strings.Repeat("\u00e9", 20) })
	settings := bridgeSettings(t, root, "read")
	settings.NestedResultMaxBytes = 11
	result := runWithBridge(context.Background(), root, settings, bridgeNested(long), tools.ExecutionContext{}, `
const r = await tools.read({});
return { content: r.content, truncated: r.info.truncated };`)
	var value struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimPrefix(execContent(result), "[result]\n"), "\n[output]", 2)[0]), &value); err != nil {
		t.Fatalf("result:\n%s\n%v", result, err)
	}
	if !value.Truncated || len(value.Content) > 11 || len(value.Content) == 0 || !utf8.ValidString(value.Content) || strings.ContainsRune(value.Content, '\uFFFD') {
		t.Fatalf("value = %+v", value)
	}
	if execField(result, "nested_bytes_returned") != "10" {
		t.Fatalf("bytes counter:\n%s", result)
	}
}

func TestToolSmithBridgeRemovesSocketAndDirectoryOnEveryPath(t *testing.T) {
	root := t.TempDir()
	read := bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })
	hang := `await tools.read({}); await new Promise(() => setInterval(() => {}, 1000));`
	cases := map[string]func(t *testing.T, settings *sessionfs.NativeToolSmithSettings) (context.Context, string){
		"success": func(t *testing.T, _ *sessionfs.NativeToolSmithSettings) (context.Context, string) {
			return context.Background(), `await tools.read({}); return 1;`
		},
		"timeout": func(t *testing.T, settings *sessionfs.NativeToolSmithSettings) (context.Context, string) {
			settings.Child.TimeoutMs = 500
			return context.Background(), hang
		},
		"cancel": func(t *testing.T, _ *sessionfs.NativeToolSmithSettings) (context.Context, string) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(900*time.Millisecond, cancel)
			return ctx, hang
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := shortTempDir(t)
			before := runtime.NumGoroutine()
			settings := bridgeSettings(t, root, "read")
			ctx, function := setup(t, &settings)
			runWithBridge(ctx, root, settings, bridgeNested(read), tools.ExecutionContext{}, function)
			requireNoLeftovers(t, tmp)
			requireGoroutinesSettle(t, before)
		})
	}
}

func TestToolSmithBridgeSocketAndDirectoryArePrivate(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	work, err := os.MkdirTemp(dir, "tool-smith-")
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := startToolSmithBridge(context.Background(), work, bridgeSettings(t, root, "read"), []tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	info, err := os.Stat(bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
	if err := requirePrivateDirectory(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requirePrivateDirectory(work); err == nil {
		t.Fatal("a 0755 directory was accepted as private")
	}
	if _, err := startToolSmithBridge(context.Background(), work, bridgeSettings(t, root, "read"), []tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })}, tools.ExecutionContext{}); err == nil {
		t.Fatal("the bridge started in a directory that is not private")
	}
}

func TestToolSmithBridgeRefusesAFrameOverTheLimitAndKeepsThatConnection(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	settings := bridgeSettings(t, root, "read")
	settings.NestedRequestMaxBytes = 256
	bridge, err := startToolSmithBridge(context.Background(), dir, settings, []tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "ok" })}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	small := `{"id":1,"token":"` + bridge.token + `","tool":"read","arguments":{}}`
	withID := `{"id":7,"pad":"` + strings.Repeat("x", 100_000) + `"}`
	withoutID := strings.Repeat("x", 100_000)
	go conn.Write([]byte(withID + "\n" + withoutID + "\n" + small + "\n"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil || !strings.Contains(string(line), `"id":7`) || !strings.Contains(string(line), `"bridge_error":"frame too large"`) {
		t.Fatalf("reply %q: %v", line, err)
	}
	line, err = reader.ReadBytes('\n')
	if err != nil || strings.Contains(string(line), `"id":7`) || !strings.Contains(string(line), `"bridge_error":"frame too large"`) {
		t.Fatalf("reply %q: %v", line, err)
	}
	answer, err := reader.ReadBytes('\n')
	if err != nil || !strings.Contains(string(answer), `"id":1`) || !strings.Contains(string(answer), `"ok":true`) {
		t.Fatalf("reply %q: %v", answer, err)
	}
	// Another connection still works too.
	other, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Write([]byte(small + "\n")); err != nil {
		t.Fatal(err)
	}
	other.SetReadDeadline(time.Now().Add(5 * time.Second))
	answer, err = bufio.NewReader(other).ReadBytes('\n')
	if err != nil || !strings.Contains(string(answer), `"ok":true`) {
		t.Fatalf("reply %q: %v", answer, err)
	}
}

func TestToolSmithBridgeForwardsTheExecutionHooksOfTheOuterCall(t *testing.T) {
	root := t.TempDir()
	write := bridgeTestTool(t, "write", false, nil)
	write.Execute = func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
		if execution.OnFileChange != nil {
			execution.OnFileChange("out.txt", "created")
		}
		if execution.ProcessOwner != "owner-1" || execution.RequestInteraction == nil || execution.OnInteractionResolved == nil {
			return tools.ExecutionError(execution.CallID, tools.StatusError, "missing_hooks", "the nested context lost a field"), nil
		}
		return tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: "done"})
	}
	var mu sync.Mutex
	var changes []string
	execution := tools.ExecutionContext{
		OnFileChange: func(path, action string) {
			mu.Lock()
			defer mu.Unlock()
			changes = append(changes, path+":"+action)
		},
		ProcessOwner: "owner-1",
		RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{}, nil
		},
		OnInteractionResolved: func(tools.InteractionIdentity, tools.InteractionAnswer, json.RawMessage) error { return nil },
	}
	result := runWithBridge(context.Background(), root, bridgeSettings(t, root, "write"), bridgeNested(write), execution, `
const r = await tools.write({});
return r.status + ':' + r.ok;`)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(execContent(result), "ok:true") || len(changes) != 1 || changes[0] != "out.txt:created" {
		t.Fatalf("changes = %v, result:\n%s", changes, result)
	}
}

func TestToolSmithBridgeDoesNotCountACallThatNeverRan(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	write := bridgeTestTool(t, "write", false, func(map[string]any) string {
		started <- struct{}{}
		<-release
		return "w"
	})
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "write"), []tools.Tool{write}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for id := 1; id <= 2; id++ {
		frame := `{"id":` + string(rune('0'+id)) + `,"token":"` + bridge.token + `","tool":"write","arguments":{}}`
		if _, err := conn.Write([]byte(frame + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	// The counter the bridge keeps says both frames were read before Close, and the closed flag
	// says Close has reached its wait: two sleeps here could pass for the wrong reason on a
	// loaded machine (P6BM-22).
	eventually(t, "the second frame to reach the bridge", func() bool { return bridge.started.Load() >= 2 })
	statsDone := make(chan toolSmithBridgeStats, 1)
	go func() { statsDone <- bridge.Close() }()
	eventually(t, "Close to wait for the running call", func() bool {
		bridge.connMu.Lock()
		defer bridge.connMu.Unlock()
		return bridge.closed
	})
	close(release)
	stats := <-statsDone
	if stats.calls != 1 || stats.writes != 1 || stats.perTool["write"] != 1 {
		t.Fatalf("the queued call was counted: %+v", stats)
	}
}

func TestAppendToolSmithInformationAddsABlockWhenTheHeaderIsMissing(t *testing.T) {
	lines := []string{"nested_calls: 1"}
	for name, result := range map[string]string{
		"with newline": `{"status":"error","summary":"x"}` + "\n",
		"no newline":   `{"status":"error","summary":"x"}`,
	} {
		got := appendToolSmithInformation(result, lines)
		want := `{"status":"error","summary":"x"}` + "\n--- INFORMATION ---\nnested_calls: 1\n"
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	if got := appendToolSmithInformation("abc", nil); got != "abc" {
		t.Errorf("no lines changed the result: %q", got)
	}
}

func TestToolSmithBridgeCloseWaitsForRunningCallsAndLeaksNothing(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	before := runtime.NumGoroutine()
	started := make(chan struct{}, 1)
	slow := bridgeTestTool(t, "read", true, func(map[string]any) string {
		started <- struct{}{}
		time.Sleep(200 * time.Millisecond)
		return "late"
	})
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "read"), []tools.Tool{slow}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"id":1,"token":"` + bridge.token + `","tool":"read","arguments":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	<-started
	stats := bridge.Close()
	if stats.calls != 1 {
		t.Fatalf("calls = %d", stats.calls)
	}
	if _, err := os.Stat(bridge.socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket still there: %v", err)
	}
	if _, err := net.Dial("unix", bridge.socketPath); err == nil {
		t.Fatal("the closed bridge still accepts")
	}
	conn.Close()
	requireGoroutinesSettle(t, before)
}

func TestToolSmithBridgeAnswersAPanicInsideAFrameWithAnInternalError(t *testing.T) {
	root := t.TempDir()
	dir := shortTempDir(t)
	boom := bridgeTestTool(t, "read", true, func(map[string]any) string { return "ok" })
	boom.NormalizeArguments = func(map[string]any) map[string]any { panic("boom") }
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "read"), []tools.Tool{boom}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	conn, err := net.Dial("unix", bridge.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"id":7,"token":"` + bridge.token + `","tool":"read","arguments":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["bridge_error"] != "internal error" || reply["id"] != float64(7) {
		t.Fatalf("reply = %v", reply)
	}
}

func TestToolSmithUnboundNestedGivesNoTools(t *testing.T) {
	if got := (&ToolSmithNested{}).allowed(sessionfs.NativeToolSmithSettings{Tools: []string{"read"}}); len(got) != 0 {
		t.Fatalf("an unbound ToolSmithNested gave %v", poolNames(got))
	}
}

func TestToolSmithWorkDirFallsBackWhenTheSocketPathIsTooLong(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets only")
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	short, err := os.MkdirTemp("/tmp", "ts")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	dir, err := makeToolSmithWorkDir(os.TempDir(), short)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if strings.HasPrefix(dir, long) {
		t.Fatalf("work dir %q was not moved out of the long TMPDIR", dir)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fallback dir not private: %v %v", info, err)
	}
	root := t.TempDir()
	bridge, err := startToolSmithBridge(context.Background(), dir, bridgeSettings(t, root, "read"), []tools.Tool{bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatalf("bridge did not start: %v", err)
	}
	bridge.Close()
}

func TestToolSmithWorkDirNamesTheLimitWhenNoPathFits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets only")
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := makeToolSmithWorkDir(long, long)
	if err == nil || !strings.Contains(err.Error(), "unix socket limit") {
		t.Fatalf("expected a clear limit error, got %v", err)
	}
}
