package native

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// T-009: a child process of a call hides the key variable of the tool's build-time model
// (apiKeyEnv) and of every model in the call's ExecutionContext.ModelKeyEnvs, also when the
// configured env sets them, so one tool shared by runs on different models hides from each call
// its own run's key.

// keyEnv sets the build-time key, two run keys and an ordinary variable.
var keyEnv = map[string]string{"OWNER_KEY": "owner-secret", "KEY_A": "a-secret", "KEY_B": "b-secret", "KEY_VISIBLE": "visible"}

const keyProbe = `echo "O=[$OWNER_KEY] A=[$KEY_A] B=[$KEY_B] V=[$KEY_VISIBLE]"`

func withKeys(execution tools.ExecutionContext, keys ...string) tools.ExecutionContext {
	execution.ModelKeyEnvs = keys
	return execution
}

func TestNativeChildEnvironmentHidesEveryModelKey(t *testing.T) {
	base := []string{"PATH=/bin", "KEY_A=from-process", "HOME=/home/x"}
	settings := sessionfs.NativeBashSettings{Env: map[string]string{"KEY_A": "a", "KEY_B": "b", "KEY_C": "c", "DENIED": "d", "JAD_API_TOKEN": "t"}, EnvDeny: []string{"DENIED"}}
	got := buildNativeBashEnvironment(base, settings, "KEY_A", "", "KEY_B")
	want := []string{"HOME=/home/x", "KEY_C=c", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("environment = %v, want %v", got, want)
	}
	if got := buildNativeBashEnvironment(base, settings); !slices.Contains(got, "KEY_A=a") || !slices.Contains(got, "KEY_B=b") {
		t.Fatalf("with no model key a configured key stays: %v", got)
	}
	if got := callModelKeys("OWNER_KEY", withKeys(tools.ExecutionContext{}, "KEY_A", "KEY_B")); !slices.Equal(got, []string{"OWNER_KEY", "KEY_A", "KEY_B"}) {
		t.Fatalf("callModelKeys = %v", got)
	}
}

// One exec tool, built with OWNER_KEY, runs calls of two runs at once (KEY_A and KEY_B): each call
// hides the owner's key and its own run's, and shows the other run's.
func TestASharedExecToolHidesEachConcurrentCallsOwnRunKey(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.Env = keyEnv
	tool, err := NewExecTool(settings, "OWNER_KEY", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"KEY_A": "O=[] A=[] B=[b-secret] V=[visible]", "KEY_B": "O=[] A=[a-secret] B=[] V=[visible]"}
	const calls = 16
	start := make(chan struct{})
	var group sync.WaitGroup
	errs := make(chan string, calls)
	for index := range calls {
		key := []string{"KEY_A", "KEY_B"}[index%2]
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			execution := withKeys(tools.ExecutionContext{CallID: fmt.Sprintf("c%d", index), WorkspaceRoot: root, SessionDir: root}, key)
			output, err := tool.Execute(context.Background(), map[string]any{"lang": "bash", "code": keyProbe}, execution)
			if err != nil {
				errs <- err.Error()
				return
			}
			if got := strings.TrimSpace(execContent(output)); got != want[key] {
				errs <- fmt.Sprintf("a call of the %s run sees %q, want %q", key, got, want[key])
			}
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for problem := range errs {
		t.Error(problem)
	}
}

// A standby started for a call of one run is not taken by a call of a run on another model: its
// environment is part of the standby's key. A call of the same run takes it and still hides the
// key. Both kinds of standby: a shim (python) and a named-pipe code file (a bash file runner).
func TestAnExecStandbyIsNotSharedAcrossRunsOnDifferentModels(t *testing.T) {
	probes := map[string]string{
		"python": `import os; print("A=[%s] B=[%s]" % (os.environ.get("KEY_A", ""), os.environ.get("KEY_B", "")))`,
		"pipesh": `echo "A=[$KEY_A] B=[$KEY_B]"`,
	}
	for _, lang := range []string{"python", "pipesh"} {
		t.Run(lang, func(t *testing.T) {
			requireInterpreters(t, "bash", "python3")
			pool := isolateStandbyPool(t)
			root := t.TempDir()
			settings := standbyExecSettings()
			settings.LangNames = append(settings.LangNames, "pipesh")
			settings.Langs["pipesh"] = sessionfs.NativeExecLang{Argv: []string{"bash", "{file}"}, Mode: sessionfs.NativeExecModeFile, Ext: ".sh", Aliases: []string{}, LibEnv: "PATH", Standby: sessionfs.NativeExecStandbyPipe}
			settings.Child = bashTestSettings(t, root)
			settings.Child.Env = keyEnv
			tool, err := NewExecTool(settings, "OWNER_KEY", nil)
			if err != nil {
				t.Fatal(err)
			}
			call := func(key string) string {
				t.Helper()
				execution := withKeys(tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: root}, key)
				output, err := tool.Execute(context.Background(), map[string]any{"lang": lang, "code": probes[lang]}, execution)
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(execContent(output))
			}
			if got := call("KEY_A"); got != "A=[] B=[b-secret]" {
				t.Fatalf("the first call of run A sees %q", got)
			}
			waitForReadyStandby(t, pool, 1)
			if got := call("KEY_B"); got != "A=[a-secret] B=[]" {
				t.Fatalf("a call of run B sees %q", got)
			}
			if _, _, taken := pool.stats(); taken != 0 {
				t.Fatal("a call of run B took the standby started for run A")
			}
			waitForReadyStandby(t, pool, 2)
			if got := call("KEY_A"); got != "A=[] B=[b-secret]" {
				t.Fatalf("run A's call in its standby sees %q", got)
			}
			if _, _, taken := pool.stats(); taken != 1 {
				t.Fatalf("taken = %d, want run A's second call to take its standby", taken)
			}
		})
	}
}

// The same for a tool_smith standby.
func TestAToolSmithStandbyIsNotSharedAcrossRunsOnDifferentModels(t *testing.T) {
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := bridgeSettings(t, root)
	settings.Child.Env = keyEnv
	settings.StandbyPool, settings.StandbyIdleMs, settings.StandbyMax = 1, 60_000, 8
	call := func(key string) string {
		t.Helper()
		execution := withKeys(tools.ExecutionContext{CallID: "smith-call", WorkspaceRoot: root, SessionDir: root}, key)
		code := "console.log(`A=[${process.env.KEY_A ?? ''}] B=[${process.env.KEY_B ?? ''}]`);"
		result, err := executeToolSmith(context.Background(), normalizeToolSmithArguments(map[string]any{"code": code}), settings, "OWNER_KEY", nil, execution, nil)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(execContent(result))
	}
	if got := call("KEY_A"); got != "A=[] B=[b-secret]" {
		t.Fatalf("the first call of run A sees %q", got)
	}
	waitForReadyStandby(t, pool, 1)
	if got := call("KEY_B"); got != "A=[a-secret] B=[]" {
		t.Fatalf("a call of run B sees %q", got)
	}
	if _, _, taken := pool.stats(); taken != 0 {
		t.Fatal("a call of run B took the standby started for run A")
	}
	waitForReadyStandby(t, pool, 2)
	if got := call("KEY_A"); got != "A=[] B=[b-secret]" {
		t.Fatalf("run A's call in its standby sees %q", got)
	}
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatalf("taken = %d, want run A's second call to take its standby", taken)
	}
}

// tool_smith's script hides the call's run key as exec does.
func TestToolSmithScriptHidesTheCallsRunKey(t *testing.T) {
	root := t.TempDir()
	settings := fakeToolSmithSettings(t, root, keyProbe)
	settings.Child.Env = keyEnv
	execution := withKeys(tools.ExecutionContext{CallID: "smith-call", WorkspaceRoot: root, SessionDir: root}, "KEY_B")
	result, err := executeToolSmith(context.Background(), normalizeToolSmithArguments(map[string]any{"code": "return 1;"}), settings, "OWNER_KEY", nil, execution, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "O=[] A=[a-secret] B=[] V=[visible]") {
		t.Fatalf("the script's environment is wrong:\n%s", result)
	}
}

// A tool's pre_tool, post_tool and post_edit commands hide the call's run key beside the key the
// hooks were built with; a run event's commands hide the event's ModelKeyEnvs.
func TestHookCommandsHideTheCallsRunKey(t *testing.T) {
	want := "O=[] A=[a-secret] B=[] V=[visible]"
	root := scopeWorkspace(t)
	settings := eventSettings([]sessionfs.ToolEventHook{eventHook("pre", "probe", keyProbe)}, []sessionfs.ToolEventHook{eventHook("post", "probe", keyProbe)})
	settings.Env = keyEnv
	tool, _ := recordingTool("probe", "test.probe", "native")
	hooked := WithToolEventHooks(tool, settings, "OWNER_KEY")
	output, err := hooked.Execute(context.Background(), map[string]any{}, withKeys(tools.ExecutionContext{CallID: "c1", WorkspaceRoot: root}, "KEY_B"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(output, want); count != 2 {
		t.Fatalf("want both tool-event commands to see %q:\n%s", want, output)
	}

	edit := hookTestSettings(shHook("lint", keyProbe, ".txt"))
	edit.Env = keyEnv
	write := WithPostEditHooks(fileChangeTool(false), edit, "OWNER_KEY")
	output, err = write.Execute(context.Background(), map[string]any{"path": "docs/k.txt", "content": "x\n"}, withKeys(tools.ExecutionContext{CallID: "c2", WorkspaceRoot: root, SessionDir: root}, "KEY_B"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, want) {
		t.Fatalf("the post_edit command's environment is wrong:\n%s", output)
	}

	stop := hookTestSettings()
	stop.Env = keyEnv
	stop.MaxPayloadBytes = 4096
	stop.SubagentStop = []sessionfs.RunEventHook{{Name: "stop", Argv: []string{"sh", "-c", keyProbe}}}
	event := RunEvent{Event: HookEventSubagentStop, WorkspaceRoot: root, ModelKeyEnvs: []string{"KEY_B"}}
	reports := FireRunEvent(context.Background(), stop, "OWNER_KEY", event)
	if len(reports) != 1 || !strings.Contains(reports[0], want) {
		t.Fatalf("the subagent_stop command's environment is wrong: %v", reports)
	}
	encoded := encodeRunEvent(event, 4096)
	var payload map[string]any
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil || strings.Contains(encoded, "KEY_B") {
		t.Fatalf("the payload carries the key names: %s (%v)", encoded, err)
	}
}
