package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeProcessSettingsDefaultsAndInheritsBashPolicy(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := filepath.Join(globalRoot, "workspace")
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	bashPolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(cwd, "private")}}}
	bash := &NativeBashSettings{
		Policy:       bashPolicy,
		Stdout:       NativeBashStreamBudget{HeadBytes: 10, TailBytes: 20},
		Stderr:       NativeBashStreamBudget{HeadBytes: 5, TailBytes: 15},
		MaxLineChars: 333,
		StripANSI:    false,
	}
	settings, err := normalizeNativeProcessSettings(
		map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":{}}}`)},
		globalRoot,
		settingPath,
		cwd,
		"",
		bash,
		nil,
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings() error = %v", err)
	}
	if settings == nil {
		t.Fatal("normalizeNativeProcessSettings() returned nil")
	}
	if settings.Shell != defaultNativeBashShell || settings.LoginShell || settings.KillGraceMs != 2_000 || settings.MaxRunning != 8 || settings.MaxCompleted != 100 || settings.RetentionDays != 14 || settings.WaitTimeoutMs != 10_000 || settings.MaxWaitTimeoutMs != 120_000 {
		t.Fatalf("normalized process defaults = %+v", settings)
	}
	if settings.Stdout != bash.Stdout || settings.Stderr != bash.Stderr {
		t.Fatalf("process stream budgets = %+v / %+v, want Bash inheritance %+v / %+v", settings.Stdout, settings.Stderr, bash.Stdout, bash.Stderr)
	}
	if settings.MaxLineChars != 2_000 || !settings.StripANSI {
		t.Fatalf("process output defaults = max_line_chars %d, strip_ansi %v", settings.MaxLineChars, settings.StripANSI)
	}
	if settings.StorageDir != filepath.Join(cwd, ".processes") {
		t.Fatalf("default process storage dir = %q", settings.StorageDir)
	}
	if !reflect.DeepEqual(settings.Policy, bashPolicy) {
		t.Fatalf("inherited process path policy = %+v, want %+v", settings.Policy, bashPolicy)
	}
	if settings.Env["NO_COLOR"] != "1" || settings.Env["GIT_TERMINAL_PROMPT"] != "0" || len(settings.EnvDeny) != 0 {
		t.Fatalf("default process environment = %+v deny=%v", settings.Env, settings.EnvDeny)
	}
	if !reflect.DeepEqual(settings.StateContext, NativeProcessStateContextSettings{States: []string{"running"}, Limit: 20}) {
		t.Fatalf("default process statecontext = %+v", settings.StateContext)
	}
}

func TestNormalizeNativeProcessSettingsOverridesAndStorage(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := filepath.Join(globalRoot, "workspace")
	toolStorage := filepath.Join(cwd, "profile-tools")
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"process":{"shell":" /bin/zsh ","login_shell":true,"kill_grace_ms":0,"max_running":4,"max_completed":8,"retention_days":0,"wait_timeout_ms":1200,"max_wait_timeout_ms":3000,"stdout":{"head_bytes":11,"tail_bytes":13},"stderr":{"head_bytes":17,"tail_bytes":19},"log_cap_bytes":512,"max_line_chars":80,"strip_ansi":false,"env":{"PROCESS_TEST":"enabled"},"env_deny":["PROCESS_SECRET"],"storage_dir":"runtime","paths":{"allow":["shared"],"deny":["private"]},"statecontext":{"states":["exited","running","exited"],"limit":7}}}}`),
	}
	writePolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "allow", Path: filepath.Join(cwd, "legacy")}}}
	settings, err := normalizeNativeProcessSettings(
		setting,
		globalRoot,
		settingPath,
		cwd,
		toolStorage,
		nil,
		&NativeWriteSettings{Policy: writePolicy},
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings(overrides) error = %v", err)
	}
	if settings.Shell != "/bin/zsh" || !settings.LoginShell || settings.KillGraceMs != 0 || settings.MaxRunning != 4 || settings.MaxCompleted != 8 || settings.RetentionDays != 0 || settings.WaitTimeoutMs != 1200 || settings.MaxWaitTimeoutMs != 3000 {
		t.Fatalf("normalized process overrides = %+v", settings)
	}
	if settings.Stdout != (NativeBashStreamBudget{HeadBytes: 11, TailBytes: 13}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 17, TailBytes: 19}) {
		t.Fatalf("normalized process budgets = %+v/%+v", settings.Stdout, settings.Stderr)
	}
	if settings.LogCapBytes != 512 || settings.MaxLineChars != 80 || settings.StripANSI {
		t.Fatalf("normalized process output settings = %+v", settings)
	}
	if settings.Env["PROCESS_TEST"] != "enabled" || settings.EnvDeny[0] != "PROCESS_SECRET" {
		t.Fatalf("normalized process environment = %+v deny=%v", settings.Env, settings.EnvDeny)
	}
	if settings.StorageDir != filepath.Join(toolStorage, "runtime") {
		t.Fatalf("process storage dir = %q, want %q", settings.StorageDir, filepath.Join(toolStorage, "runtime"))
	}
	wantPolicy := NativePathPolicy{
		Paths: []NativePathRule{
			{Action: "allow", Path: filepath.Join(globalRoot, "shared")},
			{Action: "deny", Path: filepath.Join(globalRoot, "private")},
		},
	}
	if !reflect.DeepEqual(settings.Policy, wantPolicy) {
		t.Fatalf("explicit process path policy = %+v, want %+v", settings.Policy, wantPolicy)
	}
	if !reflect.DeepEqual(settings.StateContext, NativeProcessStateContextSettings{States: []string{"exited", "running"}, Limit: 7}) {
		t.Fatalf("normalized process statecontext = %+v", settings.StateContext)
	}
}

func TestNormalizeNativeProcessSettingsValidatesUnsupportedValues(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "setting.json")
	base := func(process string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":` + process + `}}`)}
	}
	for _, raw := range []string{
		`{"unknown":true}`,
		`{"shell":"  "}`,
		`{"login_shell":"yes"}`,
		`{"kill_grace_ms":-1}`,
		`{"max_running":0}`,
		`{"max_completed":0}`,
		`{"retention_days":-1}`,
		`{"wait_timeout_ms":0}`,
		`{"wait_timeout_ms":1000,"max_wait_timeout_ms":999}`,
		`{"stdout":null}`,
		`{"stderr":null}`,
		`{"status_log_bytes":10,"stdout":{"head_bytes":1,"tail_bytes":1}}`,
		`{"stdout":{"head_bytes":10485760,"tail_bytes":1}}`,
		`{"stdout":{"head_bytes":10,"tail_bytes":10},"log_cap_bytes":19}`,
		`{"max_line_chars":19}`,
		`{"strip_ansi":"no"}`,
		`{"env":{"BAD-NAME":"x"}}`,
		`{"env":{"PROCESS_TEST":1}}`,
		`{"env_deny":["BAD-NAME"]}`,
		`{"paths":null}`,
		`{"statecontext":null}`,
		`{"statecontext":{"states":["unknown"]}}`,
		`{"statecontext":{"limit":0}}`,
	} {
		_, err := normalizeNativeProcessSettings(base(raw), globalRoot, settingPath, globalRoot, "", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "tools.native.process") {
			t.Errorf("normalizeNativeProcessSettings(%s) error = %v, want process config error", raw, err)
		}
	}
}

func TestNormalizeNativeProcessLegacyStatusLogBytesDefaultsNull(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "setting.json")
	legacy, err := normalizeNativeProcessSettings(
		map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":{"status_log_bytes":null}}}`)},
		globalRoot, settingPath, globalRoot, "", nil, nil,
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings(legacy null) error = %v", err)
	}
	if legacy.Stdout != (NativeBashStreamBudget{TailBytes: 1}) || legacy.Stderr != (NativeBashStreamBudget{TailBytes: 1}) {
		t.Fatalf("legacy null budgets = %+v/%+v", legacy.Stdout, legacy.Stderr)
	}
}
