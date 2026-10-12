package sessionfs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A tool never reads another tool's settings. Each case sets a value on the source tool
// and checks the dependent tool, which names nothing, holds its own default.
func TestToolsDoNotTakeEachOthersPathRules(t *testing.T) {
	rules := map[string]any{"allow": []any{"shared"}, "deny": []any{"private"}}
	cases := []struct {
		source, dependent string
	}{
		{"read", "grep"}, {"read", "glob"}, {"read", "read_image"},
		{"write", "edit"}, {"write", "bash"}, {"write", "process"},
		{"bash", "process"}, {"bash", "exec"},
	}
	for _, tc := range cases {
		t.Run(tc.source+" to "+tc.dependent, func(t *testing.T) {
			config, err := loadApprovalSetting(t, nativeTools(map[string]any{
				tc.source:    map[string]any{"paths": rules},
				tc.dependent: map[string]any{},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if got := toolPolicy(config, tc.dependent); len(got.Paths) != 0 || got.UnrestrictedPaths {
				t.Fatalf("%s took %s's rules: %+v", tc.dependent, tc.source, got)
			}
			if got := toolPolicy(config, tc.source); len(got.Paths) != 2 {
				t.Fatalf("%s lost its own rules: %+v", tc.source, got)
			}
		})
	}
}

func TestEveryPathToolTakesItsOwnPathRules(t *testing.T) {
	for _, tool := range []string{"read", "write", "edit", "grep", "glob", "read_image", "bash", "process", "exec"} {
		t.Run(tool, func(t *testing.T) {
			config, err := loadApprovalSetting(t, nativeTools(map[string]any{
				tool: map[string]any{"paths": map[string]any{"allow": []any{"mine"}}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			got := toolPolicy(config, tool)
			if len(got.Paths) != 1 || filepath.Base(got.Paths[0].Path) != "mine" {
				t.Fatalf("%s paths = %+v, want its own allow rule", tool, got)
			}
		})
	}
}

func toolPolicy(config SessionConfig, tool string) NativePathPolicy {
	switch tool {
	case "read":
		return config.NativeRead.Policy
	case "write":
		return config.NativeWrite.Policy
	case "edit":
		return config.NativeEdit.Policy
	case "grep":
		return config.NativeGrep.Policy
	case "glob":
		return config.NativeGlob.Policy
	case "read_image":
		return config.NativeReadImage.Policy
	case "bash":
		return config.NativeBash.Policy
	case "process":
		return config.NativeProcess.Policy
	case "exec":
		return config.NativeExec.Child.Policy
	}
	panic("unknown tool " + tool)
}

func TestEditDoesNotTakeWritesBackupSettings(t *testing.T) {
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"write": map[string]any{"backup_max_count": 9, "backup_max_total_bytes": 1, "fsync": false, "backup_dir": "write-backups"},
		"edit":  map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	edit := config.NativeEdit
	if edit.BackupMaxCount != defaultNativeMutationBackupMaxCount || edit.BackupMaxTotalBytes != defaultNativeMutationBackupMaxBytes || !edit.Fsync {
		t.Fatalf("edit took write's values: %+v", edit.NativeMutationSettings)
	}
	if edit.BackupDir == nil || strings.HasSuffix(*edit.BackupDir, "write-backups") {
		t.Fatalf("edit backup dir = %v, want its own default", edit.BackupDir)
	}
}

func TestProcessAndExecDoNotTakeBashsOutputBudgets(t *testing.T) {
	bashBudget := map[string]any{"head_bytes": 11, "tail_bytes": 22}
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"bash":    map[string]any{"stdout": bashBudget, "stderr": bashBudget, "timeout_ms": 5000, "max_timeout_ms": 6000, "max_line_chars": 77, "strip_ansi": false},
		"process": map[string]any{},
		"exec":    map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	wantStdout := NativeBashStreamBudget{HeadBytes: defaultNativeBashStdoutHead, TailBytes: defaultNativeBashStdoutTail}
	wantStderr := NativeBashStreamBudget{HeadBytes: defaultNativeBashStderrHead, TailBytes: defaultNativeBashStderrTail}
	child := config.NativeExec.Child
	if config.NativeProcess.Stdout != wantStdout || config.NativeProcess.Stderr != wantStderr || child.Stdout != wantStdout || child.Stderr != wantStderr {
		t.Fatalf("process %+v/%+v exec %+v/%+v, want the defaults %+v/%+v", config.NativeProcess.Stdout, config.NativeProcess.Stderr, child.Stdout, child.Stderr, wantStdout, wantStderr)
	}
	if child.TimeoutMs != defaultNativeBashTimeoutMs || child.MaxTimeoutMs != defaultNativeBashMaxTimeoutMs || child.MaxLineChars != defaultNativeBashMaxLine || !child.StripANSI {
		t.Fatalf("exec took bash's execution settings: %+v", child)
	}
}

// exec accepts the same child-process keys as bash, under its own name and with its own
// values, and its errors name tools.native.exec.
func TestExecOwnsItsChildProcessSettings(t *testing.T) {
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{
			"shell": "/bin/sh", "login_shell": true, "timeout_ms": 3000, "max_timeout_ms": 4000, "kill_grace_ms": 10,
			"kill_process_group": "timeout_only", "max_bytes": 4096, "stdout": map[string]any{"head_bytes": 100, "tail_bytes": 200},
			"stderr": map[string]any{"head_bytes": 10, "tail_bytes": 20}, "max_line_chars": 90, "stdin_max_bytes": 1000, "strip_ansi": false,
			"env": map[string]any{"EXEC_ENV": "1"}, "env_deny": []any{"SECRET"}, "spill": map[string]any{"enabled": false},
			"approval": "ask", "line_aware": false,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeBash != nil {
		t.Fatalf("exec created a bash tool: %+v", config.NativeBash)
	}
	exec := config.NativeExec
	child := exec.Child
	if child.Shell != "/bin/sh" || !child.LoginShell || child.TimeoutMs != 3000 || child.MaxTimeoutMs != 4000 || child.KillGraceMs != 10 ||
		child.KillProcessGroup != "timeout_only" || child.MaxBytes != 4096 || child.MaxLineChars != 90 || child.StdinMaxBytes != 1000 || child.StripANSI {
		t.Fatalf("exec child settings = %+v", child)
	}
	if child.Stdout != (NativeBashStreamBudget{HeadBytes: 100, TailBytes: 200}) || child.Stderr != (NativeBashStreamBudget{HeadBytes: 10, TailBytes: 20}) {
		t.Fatalf("exec budgets = %+v/%+v", child.Stdout, child.Stderr)
	}
	if child.Env["EXEC_ENV"] != "1" || !reflect.DeepEqual(child.EnvDeny, []string{"SECRET"}) || child.Spill.Enabled {
		t.Fatalf("exec env %v deny %v spill %+v", child.Env, child.EnvDeny, child.Spill)
	}
	if exec.Approval != "ask" || child.Approval != "ask" || exec.LineAware {
		t.Fatalf("exec approval %q/%q line_aware %v", exec.Approval, child.Approval, exec.LineAware)
	}
}

func TestExecChildSettingErrorsNameTheExecKey(t *testing.T) {
	for name, tc := range map[string]struct {
		exec map[string]any
		want string
	}{
		"timeout":    {map[string]any{"timeout_ms": 0}, "tools.native.exec.timeout_ms"},
		"order":      {map[string]any{"timeout_ms": 9000, "max_timeout_ms": 1000}, "tools.native.exec.max_timeout_ms must be greater than or equal to tools.native.exec.timeout_ms"},
		"budget":     {map[string]any{"max_bytes": 10, "stdout": map[string]any{"head_bytes": 100}}, "tools.native.exec stream budgets exceed tools.native.exec.max_bytes"},
		"spill key":  {map[string]any{"spill": map[string]any{"nope": 1}}, "tools.native.exec.spill.nope is not supported"},
		"kill":       {map[string]any{"kill_process_group": "never"}, "tools.native.exec.kill_process_group"},
		"old key":    {map[string]any{"capture_bytes": 1}, "tools.native.exec.capture_bytes is not supported"},
		"env name":   {map[string]any{"env": map[string]any{"bad name": "x"}}, "tools.native.exec.env.bad name"},
		"paths type": {map[string]any{"paths": 3}, "tools.native.exec.paths"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadApprovalSetting(t, nativeTools(map[string]any{"exec": tc.exec}))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "tools.native.bash") {
				t.Fatalf("error = %v, want %q and no mention of bash", err, tc.want)
			}
		})
	}
}
