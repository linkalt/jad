package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func hookTestSettings(hooks ...sessionfs.PostEditHook) sessionfs.HooksSettings {
	return sessionfs.HooksSettings{
		Enabled: true, TimeoutMS: 5_000, MaxOutputBytes: 2048, PostEdit: hooks,
		KillGraceMs: 100, KillProcessGroup: "always", MaxLineChars: 2000, StripANSI: true,
	}
}

func shHook(name, script string, extensions ...string) sessionfs.PostEditHook {
	return sessionfs.PostEditHook{Name: name, Ext: extensions, Argv: []string{"sh", "-c", script, "{rel}"}, Cwd: sessionfs.HookCwdFileDir}
}

// fileChangeTool is a stand-in for a tool that changes one file per call and tells
// OnFileChange about it, as exec does under hooks.shell_changes scan: a write (path,
// content) or, with edit, a replacement (path, oldText, newText). It refuses a path
// outside the workspace or outside the call's PathScope allow list, and reports an
// unchanged file without calling OnFileChange.
func fileChangeTool(edit bool) tools.Tool {
	return tools.Tool{Definition: tools.Definition{ID: "test.change", Source: "native", Name: "change"}, Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		rel, _ := arguments["path"].(string)
		target := filepath.Join(execution.WorkspaceRoot, filepath.FromSlash(rel))
		allowed := sessionfs.PathWithin(execution.WorkspaceRoot, target)
		if scope := execution.PathScope; allowed && scope != nil && len(scope.Allow) > 0 {
			allowed = false
			for _, entry := range scope.Allow {
				if sessionfs.PathWithin(filepath.Join(execution.WorkspaceRoot, entry), target) {
					allowed = true
				}
			}
		}
		if !allowed {
			return tools.ExecutionError(execution.CallID, tools.StatusForbidden, "path_outside_workspace", "The requested path is outside the workspace"), nil
		}
		old, readErr := os.ReadFile(target)
		content, _ := arguments["content"].(string)
		verb, action := "Created", "created"
		if readErr == nil {
			verb, action = "Updated", "updated"
		}
		if edit {
			from, _ := arguments["oldText"].(string)
			to, _ := arguments["newText"].(string)
			if readErr != nil || !strings.Contains(string(old), from) {
				return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "no_match", "oldText was not found"), nil
			}
			content, verb, action = strings.Replace(string(old), from, to, 1), "Edited", "edited"
		}
		if readErr == nil && string(old) == content {
			return tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: "Unchanged " + rel})
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			return tools.ExecutionError(execution.CallID, tools.StatusError, "write_failed", err.Error()), nil
		}
		if execution.OnFileChange != nil {
			execution.OnFileChange(rel, action)
		}
		return tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: verb + " " + rel})
	}}
}

// hookedTools returns a write and an edit stand-in wrapped with hooks, over a workspace
// with src/ and docs/.
func hookedTools(t *testing.T, hooks sessionfs.HooksSettings) (write, edit tools.Tool, root string) {
	t.Helper()
	root = scopeWorkspace(t)
	return WithPostEditHooks(fileChangeTool(false), hooks, ""), WithPostEditHooks(fileChangeTool(true), hooks, ""), root
}

func assertMutationStatus(t *testing.T, output, want string) {
	t.Helper()
	line, _, _ := strings.Cut(output, "\n")
	var header struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		t.Fatalf("invalid result header %q: %v", line, err)
	}
	if header.Status != want {
		t.Fatalf("status = %q, want %q; output: %s", header.Status, want, output)
	}
}

func callTool(t *testing.T, tool tools.Tool, root string, arguments map[string]any, onChange func(string, string)) string {
	t.Helper()
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: root, OnFileChange: onChange})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestHooksAppendTheFailingCheckAfterAWrite(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(shHook("lint", `echo "bad: $0"; exit 3`, ".go")))
	output := callTool(t, write, root, map[string]any{"path": "src/a.go", "content": "package a\n"}, nil)
	assertMutationStatus(t, output, "ok")
	for _, want := range []string{"Created src/a.go", "post-edit checks for src/a.go", "[lint] failed, exit 3", "bad: a.go", "--- CONTENT token="} {
		if !strings.Contains(output, want) {
			t.Errorf("result lacks %q:\n%s", want, output)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "src", "a.go")); string(data) != "package a\n" {
		t.Fatalf("the file was not written: %q", data)
	}
}

func TestHooksRunAfterAnEditAndNameTheFile(t *testing.T) {
	write, edit, root := hookedTools(t, hookTestSettings(shHook("lint", `echo "checked $0"`, ".go")))
	callTool(t, write, root, map[string]any{"path": "src/b.go", "content": "one\n"}, nil)
	output := callTool(t, edit, root, map[string]any{"path": "src/b.go", "oldText": "one", "newText": "two"}, nil)
	for _, want := range []string{"Edited src/b.go", "[lint] passed", "checked b.go"} {
		if !strings.Contains(output, want) {
			t.Errorf("edit result lacks %q:\n%s", want, output)
		}
	}
}

func TestHooksLeaveTheResultAloneWhenNothingShouldRun(t *testing.T) {
	hooks := hookTestSettings(shHook("lint", `echo ran > ran.txt`, ".go"))
	write, edit, root := hookedTools(t, hooks)
	plainWrite := fileChangeTool(false)
	ran := filepath.Join(root, "src", "ran.txt")

	// A file with another extension: byte-identical to the unhooked tool, no check started.
	got := callTool(t, write, root, map[string]any{"path": "docs/readme.md", "content": "x\n"}, nil)
	want := callTool(t, plainWrite, root, map[string]any{"path": "docs/readme2.md", "content": "x\n"}, nil)
	if strings.ReplaceAll(got, "readme.md", "readme2.md") != want {
		t.Errorf("a non-matching extension changed the result:\n%s\n---\n%s", got, want)
	}
	// An unchanged file, a no-match edit and a rejected path run nothing.
	callTool(t, write, root, map[string]any{"path": "src/same.go", "content": "x\n"}, nil)
	os.Remove(ran)
	for name, output := range map[string]string{
		"unchanged": callTool(t, write, root, map[string]any{"path": "src/same.go", "content": "x\n"}, nil),
		"no match":  callTool(t, edit, root, map[string]any{"path": "src/same.go", "oldText": "absent", "newText": "y"}, nil),
		"escape":    callTool(t, write, root, map[string]any{"path": "../outside.go", "content": "x\n"}, nil),
	} {
		if strings.Contains(output, "post-edit") {
			t.Errorf("%s: a check ran:\n%s", name, output)
		}
	}
	if pathExists(ran) {
		t.Fatal("a check was started for a call that changed nothing")
	}
}

func TestHooksOffMeansTheToolIsReturnedAsIs(t *testing.T) {
	plain := fileChangeTool(false)
	for name, hooks := range map[string]sessionfs.HooksSettings{
		"disabled":   {Enabled: false, PostEdit: []sessionfs.PostEditHook{shHook("x", "true", ".go")}},
		"no entries": {Enabled: true},
		"zero value": {},
	} {
		wrapped := WithPostEditHooks(plain, hooks, "")
		if reflect.ValueOf(wrapped.Execute).Pointer() != reflect.ValueOf(plain.Execute).Pointer() {
			t.Errorf("%s: the tool was wrapped although no check can run", name)
		}
	}
	on := WithPostEditHooks(plain, hookTestSettings(shHook("x", "true", ".go")), "")
	if reflect.ValueOf(on.Execute).Pointer() == reflect.ValueOf(plain.Execute).Pointer() {
		t.Error("an active hook set did not wrap the tool")
	}
}

func TestHooksOnFailureOnlyHidesAPassingCheck(t *testing.T) {
	quiet := shHook("quiet", `echo fine`, ".txt")
	quiet.OnFailureOnly = true
	write, _, root := hookedTools(t, hookTestSettings(quiet))
	output := callTool(t, write, root, map[string]any{"path": "docs/n.txt", "content": "x\n"}, nil)
	if strings.Contains(output, "post-edit") || strings.Contains(output, "fine") {
		t.Fatalf("a passing on_failure_only check was reported:\n%s", output)
	}
	loud := shHook("quiet", `exit 1`, ".txt")
	loud.OnFailureOnly = true
	write, _, root = hookedTools(t, hookTestSettings(loud))
	output = callTool(t, write, root, map[string]any{"path": "docs/n.txt", "content": "x\n"}, nil)
	if !strings.Contains(output, "[quiet] failed, exit 1") {
		t.Fatalf("a failing on_failure_only check was hidden:\n%s", output)
	}
}

func TestHooksMatchExtensionsWithoutCaseAndRunEveryMatchInOrder(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(
		shHook("first", `echo one`, ".TXT"),
		shHook("skipped", `echo never`, ".go"),
		shHook("second", `echo two`, ".txt", ".md"),
	))
	output := callTool(t, write, root, map[string]any{"path": "docs/a.txt", "content": "x\n"}, nil)
	first, second := strings.Index(output, "[first] passed"), strings.Index(output, "[second] passed")
	if first < 0 || second < first || strings.Contains(output, "never") {
		t.Fatalf("wrong checks or order:\n%s", output)
	}
}

func TestHooksReportATimeoutAndAMissingProgramWithoutFailingTheCall(t *testing.T) {
	hooks := hookTestSettings(
		shHook("slow", `echo started; sleep 30`, ".txt"),
		sessionfs.PostEditHook{Name: "gone", Ext: []string{".txt"}, Argv: []string{"no-such-checker-xyz", "{file}"}, Cwd: sessionfs.HookCwdFileDir},
	)
	hooks.TimeoutMS = 1_000
	write, _, root := hookedTools(t, hooks)
	output := callTool(t, write, root, map[string]any{"path": "docs/t.txt", "content": "x\n"}, nil)
	assertMutationStatus(t, output, "ok")
	for _, want := range []string{"[slow] timed out after 1000 ms (setting: hooks.timeout_ms)", "started", "[gone] could not start", "hooks.post_edit[gone].argv"} {
		if !strings.Contains(output, want) {
			t.Errorf("result lacks %q:\n%s", want, output)
		}
	}
}

func TestHooksCutLongOutputAndSayWhichSettingCutIt(t *testing.T) {
	hooks := hookTestSettings(shHook("noisy", `i=0; while [ $i -lt 2000 ]; do echo "line $i of noise"; i=$((i+1)); done`, ".txt"))
	hooks.MaxOutputBytes = 400
	write, _, root := hookedTools(t, hooks)
	output := callTool(t, write, root, map[string]any{"path": "docs/n.txt", "content": "x\n"}, nil)
	if !strings.Contains(output, "line 0 of noise") || !strings.Contains(output, "line 1999 of noise") || !strings.Contains(output, "bytes omitted") {
		t.Fatalf("head and tail were not both kept:\n%s", output)
	}
	if len(output) > 2500 {
		t.Fatalf("the result is %d bytes although the check output is capped at 400", len(output))
	}
}

func TestHooksNearestMarkerAndWorkspaceDirectories(t *testing.T) {
	near := shHook("near", `pwd; echo "rel=$0"`, ".txt")
	near.Cwd = "nearest:marker.json"
	near.Argv[3] = "{rel}"
	work := shHook("work", `pwd`, ".txt")
	work.Cwd = sessionfs.HookCwdWorkspace
	missing := shHook("missing", `pwd`, ".txt")
	missing.Cwd = "nearest:absent.json"
	write, _, root := hookedTools(t, hookTestSettings(near, work, missing))
	if err := os.MkdirAll(filepath.Join(root, "src", "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "marker.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := callTool(t, write, root, map[string]any{"path": "src/deep/er/f.txt", "content": "x\n"}, nil)
	for _, want := range []string{
		"[near] passed", filepath.Join(root, "src"), "rel=deep/er/f.txt",
		"[work] passed (", "in .)",
		"[missing] skipped: no absent.json between the file and the workspace root",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("result lacks %q:\n%s", want, output)
		}
	}
}

func TestHooksReportWhenACheckRewroteTheFile(t *testing.T) {
	fixer := shHook("fmt", `printf 'fixed\n' > "$0"`, ".txt")
	fixer.Argv[3] = "{file}"
	fixer.OnFailureOnly = true
	write, _, root := hookedTools(t, hookTestSettings(fixer))
	output := callTool(t, write, root, map[string]any{"path": "docs/f.txt", "content": "x\n"}, nil)
	if !strings.Contains(output, "the check changed the file, read it again before the next edit") {
		t.Fatalf("a rewrite by the check was not reported (and on_failure_only must not hide it):\n%s", output)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "docs", "f.txt")); string(data) != "fixed\n" {
		t.Fatalf("the fixer did not run: %q", data)
	}
}

func TestHooksStillReportTheChangeToTheCaller(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(shHook("lint", `true`, ".txt")))
	var seen []string
	callTool(t, write, root, map[string]any{"path": "docs/r.txt", "content": "x\n"}, func(path, action string) { seen = append(seen, path+":"+action) })
	if len(seen) != 1 || seen[0] != "docs/r.txt:created" {
		t.Fatalf("the original OnFileChange was not called once: %v", seen)
	}
}

func TestHooksRespectTheAgentPathScope(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(shHook("lint", `echo ran > ../ran.flag`, ".txt")))
	output, err := write.Execute(context.Background(), map[string]any{"path": "docs/denied.txt", "content": "x\n"}, tools.ExecutionContext{
		CallID: "c", WorkspaceRoot: root, PathScope: &tools.PathScope{Allow: []string{"src"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "post-edit") || pathExists(filepath.Join(root, "ran.flag")) || pathExists(filepath.Join(root, "docs", "denied.txt")) {
		t.Fatalf("a call outside the agent's paths ran a check or wrote:\n%s", output)
	}
}

func TestHookArgumentsReplacePlaceholdersButNeverTheProgram(t *testing.T) {
	hook := sessionfs.PostEditHook{Name: "x", Argv: []string{"prog", "{file}|{dir}|{rel}|{reldir}", "{file}"}}
	got := hookArguments(hook, "/ws/pkg", "/ws/pkg/sub/a.go")
	want := []string{"prog", "/ws/pkg/sub/a.go|/ws/pkg/sub|sub/a.go|sub", "/ws/pkg/sub/a.go"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHooksGetNoShellSoAFileNameIsNeverParsed(t *testing.T) {
	hook := sessionfs.PostEditHook{Name: "cat", Ext: []string{".txt"}, Argv: []string{"cat", "{file}"}, Cwd: sessionfs.HookCwdFileDir}
	write, _, root := hookedTools(t, hookTestSettings(hook))
	output := callTool(t, write, root, map[string]any{"path": "docs/$(touch pwned); x.txt", "content": "payload\n"}, nil)
	if !strings.Contains(output, "payload") || pathExists(filepath.Join(root, "docs", "pwned")) {
		t.Fatalf("the file name was interpreted by a shell:\n%s", output)
	}
}

func hookExecCatalog(t *testing.T, root string, hooks sessionfs.HooksSettings) *tools.Catalog {
	t.Helper()
	requireInterpreters(t, "bash")
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "exec.json"))
	if err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(root, "exec.json")
	if err := os.WriteFile(definition, data, 0o600); err != nil {
		t.Fatal(err)
	}
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	config := sessionfs.SessionConfig{SettingPath: "setting.json", ToolDefinitionPaths: []string{definition}, NativeExec: &settings, Hooks: hooks}
	catalog, err := BuildCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func callCatalog(t *testing.T, catalog *tools.Catalog, root, name, arguments string) string {
	t.Helper()
	item := `{"type":"function_call","call_id":"c","name":"` + name + `","arguments":` + jsonString(arguments) + `}`
	output, err := catalog.ExecuteFunctionCall(context.Background(), []byte(item), tools.ExecutionContext{CallID: "c", ToolName: name, WorkspaceRoot: root, SessionDir: root, OnFileChange: func(string, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// The catalog hooks exec: with hooks.shell_changes scan, a file an exec call wrote gets its
// post-edit checks.
func TestHooksAreWiredIntoExecWithTheChangeScan(t *testing.T) {
	root := scopeWorkspace(t)
	hooks := hookTestSettings(shHook("lint", `echo "saw $0"`, ".txt"))
	hooks.ShellChanges = sessionfs.HookShellChangesScan
	hooks.ShellScanMaxFiles = 1000
	hooks.ShellMaxChangedFiles = 10
	catalog := hookExecCatalog(t, root, hooks)
	if out := callCatalog(t, catalog, root, "exec", `{"lang":"bash","code":"printf 'x\\n' > docs/d.txt"}`); !strings.Contains(out, "[lint] passed") || !strings.Contains(out, "saw d.txt") {
		t.Fatalf("exec from the catalog ran no check:\n%s", out)
	}
}

// Without the scan exec names no file, so no check runs.
func TestHooksDoNotRunForExecWithoutTheChangeScan(t *testing.T) {
	root := scopeWorkspace(t)
	hooks := hookTestSettings(shHook("lint", `echo "saw $0"`, ".txt"))
	hooks.ShellChanges = sessionfs.HookShellChangesOff
	catalog := hookExecCatalog(t, root, hooks)
	if out := callCatalog(t, catalog, root, "exec", `{"lang":"bash","code":"printf 'x\\n' > docs/e.txt"}`); strings.Contains(out, "post-edit") {
		t.Fatalf("exec without the scan ran a post-edit check:\n%s", out)
	}
}

func TestHooksUseTheirOwnChildSettings(t *testing.T) {
	hooks := hookTestSettings(shHook("env", `echo "v=$HOOK_VAR d=${HOOK_DENIED:-unset}"`, ".txt"))
	hooks.Env = map[string]string{"HOOK_VAR": "from-hooks", "HOOK_DENIED": "x"}
	hooks.EnvDeny = []string{"HOOK_DENIED"}
	write, _, root := hookedTools(t, hooks)
	output := callTool(t, write, root, map[string]any{"path": "docs/e.txt", "content": "x\n"}, nil)
	if !strings.Contains(output, "v=from-hooks d=unset") {
		t.Fatalf("hooks ignored their own env/env_deny:\n%s", output)
	}

	long := hookTestSettings(shHook("wide", `printf '%0300d\n' 0`, ".txt"))
	long.MaxLineChars = 40
	write, _, root = hookedTools(t, long)
	output = callTool(t, write, root, map[string]any{"path": "docs/f.txt", "content": "x\n"}, nil)
	if strings.Contains(output, strings.Repeat("0", 100)) {
		t.Fatalf("hooks ignored their own max_line_chars:\n%s", output)
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestHooksNearestMarkerStopsAtTheWorkspaceRoot(t *testing.T) {
	near := shHook("near", `pwd`, ".txt")
	near.Cwd = "nearest:outside.json"
	write, _, root := hookedTools(t, hookTestSettings(near))
	// The marker sits in the directory above the workspace, which the search must not reach.
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "outside.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := callTool(t, write, root, map[string]any{"path": "src/x.txt", "content": "x\n"}, nil)
	if !strings.Contains(output, "[near] skipped: no outside.json between the file and the workspace root") {
		t.Fatalf("the search left the workspace:\n%s", output)
	}
}

func TestHooksKeepTheCallsErrorEvenWhenTheFileChanged(t *testing.T) {
	failing := tools.Tool{Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
		execution.OnFileChange("docs/e.txt", "created")
		return "", os.ErrInvalid
	}}
	wrapped := WithPostEditHooks(failing, hookTestSettings(shHook("lint", `echo ran > ran.flag`, ".txt")), "")
	root := scopeWorkspace(t)
	output, err := wrapped.Execute(context.Background(), nil, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != os.ErrInvalid || output != "" || pathExists(filepath.Join(root, "docs", "ran.flag")) {
		t.Fatalf("err=%v output=%q: an error from the call must pass through untouched, with no check run", err, output)
	}
}

func TestHooksNeverRunForAnUnchangedWrite(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(shHook("lint", `echo ran >> ran.log`, ".txt")))
	first := callTool(t, write, root, map[string]any{"path": "src/a.txt", "content": "x\n"}, nil)
	if !strings.Contains(first, "[lint] passed") {
		t.Fatalf("the first write must run the check:\n%s", first)
	}
	again := callTool(t, write, root, map[string]any{"path": "src/a.txt", "content": "x\n"}, nil)
	if strings.Contains(again, "post-edit") || !strings.Contains(again, "Unchanged") {
		t.Fatalf("an unchanged write ran a check:\n%s", again)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "src", "ran.log")); string(data) != "ran\n" {
		t.Fatalf("the check ran %d times in total, want once", strings.Count(string(data), "ran"))
	}
}

func TestHooksStopARunningCheckWhenTheRunIsCancelled(t *testing.T) {
	write, _, root := hookedTools(t, hookTestSettings(shHook("slow", `echo started; sleep 30`, ".txt")))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	output, err := write.Execute(ctx, map[string]any{"path": "docs/c.txt", "content": "x\n"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the call took %v after a cancel; the check kept running", took)
	}
	if !strings.Contains(output, "[slow] stopped before it finished") || !strings.Contains(output, "Created docs/c.txt") {
		t.Fatalf("a cancelled check must be reported and the write kept:\n%s", output)
	}
}

func TestHooksNearestSaysSoForAFileOutsideTheWorkspace(t *testing.T) {
	near := shHook("near", `pwd`, ".txt")
	near.Cwd = "nearest:go.mod"
	root := scopeWorkspace(t)
	outside := t.TempDir()
	output := runPostEditHooks(context.Background(), hookTestSettings(near), "", tools.ExecutionContext{WorkspaceRoot: root}, filepath.Join(outside, "o.txt"))
	if !strings.Contains(output, "the file is outside the workspace") {
		t.Fatalf("got:\n%s", output)
	}
}

// The guard against an empty changed path only matters when the workspace root itself
// carries a hooked extension: the path "" then resolves to the root and would match.
func TestHooksNeverRunWithoutAChangedFileEvenWhenTheRootLooksLikeOne(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "ws.txt")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := fileChangeTool(false)
	write := WithPostEditHooks(plain, hookTestSettings(shHook("lint", `echo ran`, ".txt")), "")
	callTool(t, write, root, map[string]any{"path": "src/a.dat", "content": "x\n"}, nil)
	output := callTool(t, write, root, map[string]any{"path": "src/a.dat", "content": "x\n"}, nil)
	if strings.Contains(output, "post-edit") || !strings.Contains(output, "Unchanged") {
		t.Fatalf("an unchanged write ran a check:\n%s", output)
	}
}
