package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// argumentTool records the arguments and raw arguments it ran with.
func argumentTool(t *testing.T) (tools.Tool, *map[string]any, *string) {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(`{"type":"object","properties":{"pattern":{"type":"string"},"limit":{"type":"integer"}},"required":["pattern"],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]any{}
	raw := ""
	tool := tools.Tool{
		Definition: tools.Definition{ID: "grep", Source: "native", Name: "grep"},
		Validator:  validator,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			seen = arguments
			raw = string(execution.RawArguments)
			return `{"id":"c","status":"ok","summary":"done"}` + "\n", nil
		},
	}
	return tool, &seen, &raw
}

func rewriteHook(name, script string) sessionfs.ToolEventHook {
	hook := eventHook(name, "*", script)
	hook.Rewrite = true
	return hook
}

func TestPreToolRewriteChangesTheArgumentsTheCallRunsWith(t *testing.T) {
	root := t.TempDir()
	seenByLater := filepath.Join(root, "later.json")
	long := strings.Repeat("x", 3000)
	settings := eventSettings([]sessionfs.ToolEventHook{
		rewriteHook("narrow", `echo '{"arguments":{"pattern":"`+long+`","limit":5}}'`),
		quietEventHook("later", "*", `cat > `+seenByLater),
	}, nil)
	settings.MaxPayloadBytes = 64 * 1024
	settings.MaxOutputBytes = 16 * 1024
	tool, seen, raw := argumentTool(t)
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{"pattern": "wide"})
	if (*seen)["pattern"] != long || (*seen)["limit"] != json.Number("5") || !strings.Contains(*raw, `"limit":5`) {
		t.Fatalf("the call ran with %v (raw %s)", *seen, *raw)
	}
	if !strings.Contains(output, "[narrow] rewrote the arguments") {
		t.Fatalf("output:\n%s", output)
	}
	payload, err := os.ReadFile(seenByLater)
	if err != nil || !strings.Contains(string(payload), `"limit":5`) {
		t.Fatalf("the later command did not read the new arguments: %s %v", payload, err)
	}
}

func TestPreToolRewriteThatCannotBeUsedBlocksTheCall(t *testing.T) {
	for name, script := range map[string]string{
		"not json":       `echo nope`,
		"no arguments":   `echo '{"args":{}}'`,
		"schema refuses": `echo '{"arguments":{"pattern":"p","extra":1}}'`,
		"not an object":  `echo '{"arguments":[1]}'`,
	} {
		t.Run(name, func(t *testing.T) {
			settings := eventSettings([]sessionfs.ToolEventHook{rewriteHook("fix", script)}, nil)
			tool, seen, _ := argumentTool(t)
			output := runEventTool(t, WithToolEventHooks(tool, settings, ""), t.TempDir(), map[string]any{"pattern": "wide"})
			if len(*seen) != 0 || !strings.Contains(output, "blocked_by_hook") || !strings.Contains(output, "rewrite refused") {
				t.Fatalf("seen %v, output:\n%s", *seen, output)
			}
		})
	}
}

func TestPreToolRewriteWithNoOutputOrAFailingExitKeepsTheArguments(t *testing.T) {
	for name, script := range map[string]string{"empty": `true`, "failing": `echo '{"arguments":{"pattern":"x"}}'; exit 3`} {
		t.Run(name, func(t *testing.T) {
			settings := eventSettings([]sessionfs.ToolEventHook{rewriteHook("maybe", script)}, nil)
			tool, seen, _ := argumentTool(t)
			runEventTool(t, WithToolEventHooks(tool, settings, ""), t.TempDir(), map[string]any{"pattern": "wide"})
			if (*seen)["pattern"] != "wide" {
				t.Fatalf("the call ran with %v", *seen)
			}
		})
	}
}

func eventListSettings(event string, hooks ...sessionfs.RunEventHook) sessionfs.HooksSettings {
	settings := hookTestSettings()
	settings.MaxPayloadBytes = 4096
	switch event {
	case HookEventPromptSubmit:
		settings.PromptSubmit = hooks
	case HookEventStop:
		settings.Stop = hooks
	case HookEventSessionStart:
		settings.SessionStart = hooks
	case HookEventNotification:
		settings.Notification = hooks
	}
	return settings
}

func TestPromptSubmitRewriteAndStopContinue(t *testing.T) {
	root := t.TempDir()
	rewrite := sessionfs.RunEventHook{Name: "rewrite", Argv: []string{"sh", "-c", `echo '{"prompt":"better prompt"}'`}, Rewrite: true}
	result := FireRunEventResult(context.Background(), eventListSettings(HookEventPromptSubmit, rewrite), "", RunEvent{Event: HookEventPromptSubmit, WorkspaceRoot: root, Prompt: "p"})
	if result.Prompt == nil || *result.Prompt != "better prompt" || result.RewrittenBy != "rewrite" || result.BlockedBy != "" {
		t.Fatalf("rewrite result = %+v", result)
	}
	bad := sessionfs.RunEventHook{Name: "bad", Argv: []string{"sh", "-c", `echo '{"prompt":"  "}'`}, Rewrite: true}
	result = FireRunEventResult(context.Background(), eventListSettings(HookEventPromptSubmit, bad), "", RunEvent{Event: HookEventPromptSubmit, WorkspaceRoot: root})
	if result.BlockedBy != "bad" || result.Prompt != nil || !strings.Contains(strings.Join(result.Reports, "\n"), "the new prompt is empty") {
		t.Fatalf("empty rewrite result = %+v", result)
	}
	payload := filepath.Join(root, "stop.json")
	silent := sessionfs.RunEventHook{Name: "silent", Argv: []string{"sh", "-c", `cat > ` + payload}}
	again := sessionfs.RunEventHook{Name: "again", Argv: []string{"sh", "-c", `echo '{"continue":"run the tests too"}'`}}
	never := sessionfs.RunEventHook{Name: "never", Argv: []string{"sh", "-c", `echo '{"continue":"unreachable"}'`}}
	result = FireRunEventResult(context.Background(), eventListSettings(HookEventStop, silent, again, never), "", RunEvent{Event: HookEventStop, WorkspaceRoot: root, Answer: "final words", Continues: 1})
	if result.Continue != "run the tests too" || result.ContinuedBy != "again" {
		t.Fatalf("stop result = %+v", result)
	}
	data, err := os.ReadFile(payload)
	if err != nil || !strings.Contains(string(data), `"answer":"final words"`) || !strings.Contains(string(data), `"continues":1`) {
		t.Fatalf("stop payload = %s, %v", data, err)
	}
	failing := sessionfs.RunEventHook{Name: "failing", Argv: []string{"sh", "-c", `echo '{"continue":"x"}'; exit 1`}}
	if result := FireRunEventResult(context.Background(), eventListSettings(HookEventStop, failing), "", RunEvent{Event: HookEventStop, WorkspaceRoot: root}); result.Continue != "" {
		t.Fatalf("a failing stop command continued the run: %+v", result)
	}
}

func TestShellChangeScanReportsTheFilesACommandChanged(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"keep.txt": "same", "edit.go": "old", "gone.txt": "x", "node_modules/dep.js": "a"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shell := tools.Tool{
		Definition: tools.Definition{ID: "bash", Source: "native", Name: "bash"},
		Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			_ = os.WriteFile(filepath.Join(execution.WorkspaceRoot, "edit.go"), []byte("new content"), 0o644)
			_ = os.WriteFile(filepath.Join(execution.WorkspaceRoot, "made.go"), []byte("x"), 0o644)
			_ = os.Remove(filepath.Join(execution.WorkspaceRoot, "gone.txt"))
			_ = os.WriteFile(filepath.Join(execution.WorkspaceRoot, "node_modules", "dep.js"), []byte("bbbb"), 0o644)
			return `{"id":"c","status":"ok","summary":"ran"}` + "\n", nil
		},
	}
	settings := hookTestSettings(shHook("vet", `echo "vetted $0"`, ".go"))
	settings.ShellChanges = sessionfs.HookShellChangesScan
	settings.ShellScanMaxFiles = 100
	settings.ShellMaxChangedFiles = 10
	settings.ShellScanSkipDirs = []string{"node_modules"}
	var changes []string
	wrapped := WithPostEditHooks(WithShellChangeScan(shell, settings), settings, "")
	output, err := wrapped.Execute(context.Background(), map[string]any{}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, OnFileChange: func(path, action string) {
		changes = append(changes, action+" "+path)
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "updated edit.go,deleted gone.txt,created made.go"
	if strings.Join(changes, ",") != want {
		t.Fatalf("changes = %q, want %q", changes, want)
	}
	for _, check := range []string{"post-edit checks for edit.go", "post-edit checks for made.go"} {
		if !strings.Contains(output, check) {
			t.Errorf("output misses %q:\n%s", check, output)
		}
	}

	// Past the file limit nothing is reported and the result says why.
	settings.ShellScanMaxFiles = 1
	changes = nil
	output, err = WithShellChangeScan(shell, settings).Execute(context.Background(), map[string]any{}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, OnFileChange: func(path, action string) {
		changes = append(changes, path)
	}})
	if err != nil || len(changes) != 0 || !strings.Contains(output, "hooks.shell_scan_max_files") {
		t.Fatalf("changes %v, output:\n%s", changes, output)
	}
}

func TestShellChangeScanIsOffByDefault(t *testing.T) {
	tool, _ := recordingTool("bash", "bash", "native")
	settings := hookTestSettings()
	settings.ShellChanges = sessionfs.HookShellChangesOff
	// With shell_changes off the tool is returned as it is: calling it reports nothing.
	called := false
	output, err := WithShellChangeScan(tool, settings).Execute(context.Background(), nil, tools.ExecutionContext{CallID: "c", WorkspaceRoot: t.TempDir(), OnFileChange: func(string, string) { called = true }})
	if err != nil || called || strings.Contains(output, "shell_scan") {
		t.Fatalf("called %t output %s", called, output)
	}
}
