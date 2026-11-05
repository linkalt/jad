package native

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// bashTestSettings are child-process settings for tests of the shared shell runner (exec,
// tool_smith, hooks): bash from PATH, small budgets, no spill.
func bashTestSettings(t *testing.T, root string) sessionfs.NativeBashSettings {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	return sessionfs.NativeBashSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:            shell,
		TimeoutMs:        2_000,
		MaxTimeoutMs:     5_000,
		KillGraceMs:      100,
		KillProcessGroup: "always",
		MaxBytes:         2048,
		Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
		Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
		Spill: sessionfs.NativeBashSpillSettings{
			Enabled: false, Keep: "truncated_only", CapBytes: 1024, RetentionDays: 0,
			StorageDir: filepath.Join(root, ".bash-output"),
		},
		MaxLineChars:  2000,
		StdinMaxBytes: 64 * 1024,
		StripANSI:     true,
		Env:           map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"},
		EnvDeny:       []string{},
	}
}

// executeNativeBash runs command with the settings' shell through the shared runner, as
// the bash tool did before exec replaced it.
func executeNativeBash(ctx context.Context, arguments map[string]any, settings sessionfs.NativeBashSettings, apiKeyEnv string, execution tools.ExecutionContext) string {
	command, _ := arguments["command"].(string)
	stdin, _ := arguments["stdin"].(string)
	cwd := "."
	if value, ok := arguments["cwd"].(string); ok {
		cwd = value
	}
	var timeout *int64
	if value, ok := safeInteger(arguments["timeout_ms"]); ok {
		timeout = &value
	}
	argv := []string{"-c", command}
	if settings.LoginShell {
		argv = []string{"-l", "-c", command}
	}
	settings.Policy = workspacePolicy(settings.Policy, execution)
	return runShellChild(ctx, shellChildRun{file: settings.Shell, argv: argv, stdin: stdin, cwd: cwd, timeoutMs: timeout}, settings, apiKeyEnv, execution)
}

func executeBashTest(t *testing.T, root, callID, command, cwd string, settings sessionfs.NativeBashSettings) string {
	t.Helper()
	arguments := map[string]any{"command": command}
	if cwd != "" {
		arguments["cwd"] = cwd
	}
	return executeNativeBash(context.Background(), arguments, settings, "", tools.ExecutionContext{
		CallID: callID, WorkspaceRoot: root, SessionDir: root,
	})
}
