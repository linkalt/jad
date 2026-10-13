package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

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

func TestNativeBashRunsCommandAndUsesRequestedCWD(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "src")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, root)
	output := executeBashTest(t, root, "echo-call", "printf 'hello-from-bash\\n'", "src", settings)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, `"summary":"Command completed in `) {
		t.Fatalf("bash result = %s", output)
	}
	if !strings.Contains(output, "cwd: src") || !strings.Contains(output, "hello-from-bash") {
		t.Fatalf("bash result omitted cwd or content: %s", output)
	}
}

func TestNativeBashResolvesShellFromFilteredPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script requires Unix semantics")
	}
	root := t.TempDir()
	binaryDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binaryDir, "custom-shell"), []byte(`#!/bin/sh
printf 'custom-shell-ran\n'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := sessionfs.NativeBashSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:            "custom-shell",
		TimeoutMs:        1_000,
		MaxTimeoutMs:     2_000,
		KillGraceMs:      100,
		KillProcessGroup: "always",
		MaxBytes:         1024,
		Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 0},
		Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 0, TailBytes: 0},
		MaxLineChars:     200,
		StripANSI:        true,
		Env:              map[string]string{"PATH": binaryDir, "NO_COLOR": "1"},
		EnvDeny:          []string{},
	}
	output := executeBashTest(t, root, "custom-shell", "true", ".", settings)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "custom-shell-ran") {
		t.Fatalf("shell from filtered PATH result = %s", output)
	}
	settings.EnvDeny = []string{"PATH"}
	denied := executeBashTest(t, root, "custom-shell-denied", "true", ".", settings)
	if !strings.Contains(denied, "error_code: shell_unavailable") {
		t.Fatalf("denied PATH still resolved a shell: %s", denied)
	}
}

func TestNativeBashMissingAbsoluteShellIsUnavailable(t *testing.T) {
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	settings.Shell = filepath.Join(root, "missing-shell")
	output := executeBashTest(t, root, "missing-shell", "true", ".", settings)
	if !strings.Contains(output, "error_code: shell_unavailable") {
		t.Fatalf("missing shell result = %s", output)
	}
}

func TestNativeBashEnforcesCWDPathPolicy(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, root)
	settings.Policy.Paths = []sessionfs.NativePathRule{{Action: "deny", Path: private}}
	denied := executeBashTest(t, root, "denied-cwd", "pwd", "private", settings)
	if !strings.Contains(denied, `"status":"forbidden"`) || !strings.Contains(denied, "error_code: path_denied") {
		t.Fatalf("denied CWD result = %s", denied)
	}
	file := executeBashTest(t, root, "file-cwd", "pwd", "file.txt", settings)
	if !strings.Contains(file, `"status":"invalid_input"`) || !strings.Contains(file, "error_code: not_a_directory") {
		t.Fatalf("file CWD result = %s", file)
	}
}

func TestNormalizeBashArgumentsAcceptsNearMissAliases(t *testing.T) {
	normalized := normalizeBashArguments(map[string]any{"cmd": "  pwd  ", "workdir": "src"})
	if normalized["command"] != "pwd" || normalized["cwd"] != "src" {
		t.Fatalf("normalized arguments = %#v", normalized)
	}
	for _, key := range []string{"cmd", "workdir", "dir"} {
		if _, exists := normalized[key]; exists {
			t.Errorf("normalized arguments retained alias %q: %#v", key, normalized)
		}
	}
	withExplicitCWD := normalizeBashArguments(map[string]any{"command": "pwd", "cwd": nil, "dir": "src"})
	if withExplicitCWD["cwd"] != nil {
		t.Fatalf("explicit null cwd was overwritten: %#v", withExplicitCWD)
	}
}

func TestBuildNativeBashEnvironmentFiltersCredentialsAndUnlistedVariables(t *testing.T) {
	settings := sessionfs.NativeBashSettings{
		Env:     map[string]string{"EXTRA_ALLOWED": "configured", "FIXTURE_SECRET": "configured-secret", "JAD_API_TOKEN": "server-secret"},
		EnvDeny: []string{"DENIED_BY_SETTING"},
	}
	base := []string{
		"PATH=/usr/bin:/bin", "HOME=/tmp", "LANG=C", "FIXTURE_SECRET=ambient-secret",
		"JAD_API_TOKEN=server-secret", "UNLISTED_SECRET=do-not-pass", "DENIED_BY_SETTING=denied",
	}
	environment := buildNativeBashEnvironment(base, settings, "FIXTURE_SECRET")
	values := make(map[string]string)
	for _, item := range environment {
		name, value, found := strings.Cut(item, "=")
		if found {
			values[name] = value
		}
	}
	if values["PATH"] != "/usr/bin:/bin" || values["HOME"] != "/tmp" || values["LANG"] != "C" || values["EXTRA_ALLOWED"] != "configured" {
		t.Fatalf("allowed environment entries = %#v", values)
	}
	for _, name := range []string{"FIXTURE_SECRET", "JAD_API_TOKEN", "UNLISTED_SECRET", "DENIED_BY_SETTING"} {
		if _, exists := values[name]; exists {
			t.Errorf("environment unexpectedly contains %s: %#v", name, values)
		}
	}
}

func TestEffectiveBashTimeoutClampsPerCallRequest(t *testing.T) {
	settings := sessionfs.NativeBashSettings{TimeoutMs: 60_000, MaxTimeoutMs: 120_000}
	requested := int64(1)
	if got := effectiveBashTimeoutMs(&requested, settings); got != sessionfs.NativeBashMinCallTimeoutMs {
		t.Fatalf("effectiveBashTimeoutMs(%d) = %d", requested, got)
	}
	requested = 5_000_000
	if got := effectiveBashTimeoutMs(&requested, settings); got != 120_000 {
		t.Fatalf("effectiveBashTimeoutMs(%d) = %d", requested, got)
	}
	if got := effectiveBashTimeoutMs(nil, settings); got != 60_000 {
		t.Fatalf("effectiveBashTimeoutMs(nil) = %d", got)
	}
}

func TestBashOutputCollectorKeepsHeadAndTailPerStream(t *testing.T) {
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: 4, tailBytes: 4},
		bashStreamLimits{headBytes: 2, tailBytes: 2},
		100,
		true,
	)
	collector.push(bashStdout, []byte("abcdefghijk"))
	collector.push(bashStderr, []byte("err"))
	collector.end()
	output := collector.render()
	if output.stdout.producedBytes != 11 || output.stdout.shownBytes != 8 || output.stdout.omittedBytes != 3 || !output.stdout.truncated {
		t.Fatalf("stdout accounting = %+v", output.stdout)
	}
	if !strings.Contains(output.text, "[stdout]\nabcd\n[... 3 bytes omitted ...]\nhijk") || !strings.Contains(output.text, "[stderr]\nerr") {
		t.Fatalf("rendered output = %q", output.text)
	}
	if output.returnedBytes != int64(len(output.text)) {
		t.Fatalf("returnedBytes = %d; text bytes = %d", output.returnedBytes, len(output.text))
	}
}

func TestNativeBashTimeoutKillsTheCommandAndMarksPartialOutput(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "too-late")
	settings := bashTestSettings(t, root)
	settings.TimeoutMs = 50
	settings.MaxTimeoutMs = 1_000
	settings.KillGraceMs = 50
	output := executeBashTest(t, root, "timeout-call", "printf start; sleep 0.4; touch too-late", ".", settings)
	if !strings.Contains(output, `"status":"error"`) || !strings.Contains(output, "error_code: timeout") || !strings.Contains(output, "timed_out: true") || !strings.Contains(output, "truncation_reason: timeout") {
		t.Fatalf("timeout result = %s", output)
	}
	if !strings.Contains(output, "start") {
		t.Fatalf("timeout result lost partial output: %s", output)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("timed-out shell left command running; marker stat error = %v", err)
	}
}

func TestNativeBashTimeoutEscalatesToSIGKILL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group signal semantics are Unix-specific")
	}
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	settings.TimeoutMs = 75
	settings.MaxTimeoutMs = 1_000
	settings.KillGraceMs = 75
	output := executeBashTest(t, root, "stubborn-timeout", "trap '' TERM; while :; do sleep 0.02; done", ".", settings)
	if !strings.Contains(output, "error_code: timeout") || !strings.Contains(output, "signal: SIGKILL") {
		t.Fatalf("SIGTERM-ignoring command result = %s", output)
	}
}

func TestNativeBashCleansBackgroundProcessAfterCommandCompletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup is Unix-specific")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "background-marker")
	settings := bashTestSettings(t, root)
	settings.KillGraceMs = 50
	output := executeBashTest(t, root, "background-cleanup", "(sleep 0.35; touch background-marker) </dev/null >/dev/null 2>&1 & echo started", ".", settings)
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "started") {
		t.Fatalf("background command result = %s", output)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background process survived successful command; marker stat error = %v", err)
	}
}

func TestNativeBashReturnsBinaryOutputMarker(t *testing.T) {
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	output := executeBashTest(t, root, "binary-call", "printf '\\000\\377A'", ".", settings)
	if !strings.Contains(output, "binary_output: true") || !strings.Contains(output, "�A") || strings.Contains(output, "\x00") {
		t.Fatalf("binary output result = %q", output)
	}
}

func TestNativeBashSpillKeepsFullOutputOnlyWhenConfigured(t *testing.T) {
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	settings.Spill = sessionfs.NativeBashSpillSettings{
		Enabled: true, Keep: "always", CapBytes: 1024, StorageDir: filepath.Join(root, ".bash-output"),
	}
	output := executeBashTest(t, root, "spill-call", "printf 'kept\\n'", ".", settings)
	path := filepath.Join(root, ".bash-output", "bash-spill-call.stdout.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spill %s: %v; result=%s", path, err, output)
	}
	if string(data) != "kept\n" || !strings.Contains(output, "stdout_full: .bash-output/bash-spill-call.stdout.log") {
		t.Fatalf("spill content=%q result=%s", data, output)
	}

	settings.Spill.Keep = "truncated_only"
	short := executeBashTest(t, root, "spill-short", "printf 'small\\n'", ".", settings)
	if strings.Contains(short, "stdout_full:") {
		t.Fatalf("untruncated output kept a spill file: %s", short)
	}
	if _, err := os.Stat(filepath.Join(root, ".bash-output", "bash-spill-short.stdout.log")); !os.IsNotExist(err) {
		t.Fatalf("untruncated spill was not removed: %v", err)
	}
}

func TestNativeSpillFileNameFollowsTheConfiguredPrefix(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct{ prefix, want string }{{"", "bash"}, {"bash", "bash"}, {"exec", "exec"}} {
		settings := bashTestSettings(t, root)
		settings.Spill = sessionfs.NativeBashSpillSettings{
			Enabled: true, Keep: "always", CapBytes: 1024, StorageDir: filepath.Join(root, "out-"+test.want), FilePrefix: test.prefix,
		}
		output := executeBashTest(t, root, "name-call", "printf 'kept\\n'", ".", settings)
		path := filepath.Join(root, "out-"+test.want, test.want+"-name-call.stdout.log")
		if _, err := os.Stat(path); err != nil || !strings.Contains(output, test.want+"-name-call.stdout.log") {
			t.Fatalf("prefix %q: spill %s missing (%v); result=%s", test.prefix, path, err, output)
		}
	}
}

func TestNativeBashSpillCapsAndReportsFailuresWithoutFailingCommand(t *testing.T) {
	root := t.TempDir()
	settings := bashTestSettings(t, root)
	settings.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 4, TailBytes: 4}
	settings.Stderr = sessionfs.NativeBashStreamBudget{HeadBytes: 0, TailBytes: 0}
	settings.MaxBytes = 8
	settings.Spill = sessionfs.NativeBashSpillSettings{
		Enabled: true, Keep: "always", CapBytes: 4, StorageDir: filepath.Join(root, ".bash-output"),
	}
	capped := executeBashTest(t, root, "spill-cap", "printf '0123456789'", ".", settings)
	written, err := os.ReadFile(filepath.Join(root, ".bash-output", "bash-spill-cap.stdout.log"))
	if err != nil {
		t.Fatalf("read capped spill: %v; result=%s", err, capped)
	}
	if string(written) != "0123" || !strings.Contains(capped, "stdout_full_capped: true") {
		t.Fatalf("capped spill=%q result=%s", written, capped)
	}

	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings.Spill.StorageDir = filepath.Join(blocked, "output")
	settings.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 10, TailBytes: 0}
	settings.MaxBytes = 10
	failed := executeBashTest(t, root, "spill-failed", "printf 'still-runs'", ".", settings)
	if !strings.Contains(failed, `"status":"ok"`) || !strings.Contains(failed, "spill_error: stdout:") || !strings.Contains(failed, "still-runs") {
		t.Fatalf("spill failure affected command: %s", failed)
	}
}

func TestBashArgumentIntegerAcceptsJSONInteger(t *testing.T) {
	value, err := json.Number("42").Int64()
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := bashArgumentInteger(json.Number("42"))
	if !ok || parsed != value {
		t.Fatalf("bashArgumentInteger() = %d, %v", parsed, ok)
	}
}

func BenchmarkNativeBashShortCommand(b *testing.B) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		b.Skip("bash is not installed")
	}
	root := b.TempDir()
	settings := sessionfs.NativeBashSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:            shell,
		TimeoutMs:        1_000,
		MaxTimeoutMs:     1_000,
		KillGraceMs:      50,
		KillProcessGroup: "timeout_only",
		MaxBytes:         1024,
		Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 0},
		Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 0, TailBytes: 0},
		Spill:            sessionfs.NativeBashSpillSettings{Enabled: false},
		MaxLineChars:     200,
		StripANSI:        true,
		Env:              map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"},
		EnvDeny:          []string{},
	}
	arguments := map[string]any{"command": "printf ok"}
	execution := tools.ExecutionContext{CallID: "bash-benchmark", WorkspaceRoot: root, SessionDir: root}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		output := executeNativeBash(context.Background(), arguments, settings, "", execution)
		if !strings.Contains(output, "ok") {
			b.Fatalf("bash command result = %s", output)
		}
	}
}

// A script reads its own name as $0. The interpreter is found through PATH, so it
// is started from an absolute path, but argv[0] has to stay the name that was
// configured, as it does when Node starts it. Otherwise the same call prints
// /usr/bin/bash on one backend and bash on the other. The interpreter here is a
// link to the real bash, because a #! script would report its own path whatever
// argv[0] is.
func TestNativeBashKeepsTheConfiguredNameAsArgvZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script requires Unix semantics")
	}
	root := t.TempDir()
	binaryDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := bashTestSettings(t, root)
	if err := os.Symlink(settings.Shell, filepath.Join(binaryDir, "argv0-shell")); err != nil {
		t.Fatal(err)
	}
	settings.Shell = "argv0-shell"
	settings.Env = map[string]string{"PATH": binaryDir}
	output := executeBashTest(t, root, "argv0", `printf 'name=%s\n' "$0"`, ".", settings)
	if !strings.Contains(output, "name=argv0-shell\n") || strings.Contains(output, binaryDir) {
		t.Fatalf("the script saw its resolved path instead of the configured name: %s", output)
	}
}
