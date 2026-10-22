package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeProcessSettingsDefaultsAndIgnoresBash(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := filepath.Join(globalRoot, "workspace")
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	settings, err := normalizeNativeProcessSettings(
		map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"bash":{"paths":{"deny":["private"]},"stdout":{"head_bytes":10,"tail_bytes":20}},"process":{}}}`)},
		globalRoot,
		settingPath,
		cwd,
		"",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings() error = %v", err)
	}
	if settings == nil {
		t.Fatal("normalizeNativeProcessSettings() returned nil")
	}
	if settings.Shell != defaultNativeBashShell || settings.LoginShell || settings.KillGraceMs != 2_000 || settings.MaxRunning != 8 || settings.MaxCompleted != 100 || settings.RetentionDays != 14 {
		t.Fatalf("normalized process defaults = %+v", settings)
	}
	wantStdout := NativeBashStreamBudget{HeadBytes: defaultNativeBashStdoutHead, TailBytes: defaultNativeBashStdoutTail}
	wantStderr := NativeBashStreamBudget{HeadBytes: defaultNativeBashStderrHead, TailBytes: defaultNativeBashStderrTail}
	if settings.Stdout != wantStdout || settings.Stderr != wantStderr {
		t.Fatalf("process stream budgets = %+v / %+v, want its own defaults %+v / %+v, not bash's", settings.Stdout, settings.Stderr, wantStdout, wantStderr)
	}
	if settings.MaxLineChars != 2_000 || !settings.StripANSI {
		t.Fatalf("process output defaults = max_line_chars %d, strip_ansi %v", settings.MaxLineChars, settings.StripANSI)
	}
	if settings.StdinMaxBytes != 256*1024 {
		t.Fatalf("default process stdin_max_bytes = %d, want %d", settings.StdinMaxBytes, 256*1024)
	}
	if settings.StorageDir != filepath.Join(cwd, ".processes") {
		t.Fatalf("default process storage dir = %q", settings.StorageDir)
	}
	if len(settings.Policy.Paths) != 0 || settings.Policy.UnrestrictedPaths {
		t.Fatalf("process path policy = %+v, want the empty default, not bash's rules", settings.Policy)
	}
	if settings.Env["NO_COLOR"] != "1" || settings.Env["GIT_TERMINAL_PROMPT"] != "0" || len(settings.EnvDeny) != 0 {
		t.Fatalf("default process environment = %+v deny=%v", settings.Env, settings.EnvDeny)
	}
	if !reflect.DeepEqual(settings.StateContext, NativeProcessStateContextSettings{States: []string{"running"}, Limit: 20, Mode: NativeStateContextModeItems}) {
		t.Fatalf("default process statecontext = %+v", settings.StateContext)
	}
}

func TestNormalizeNativeProcessSettingsOverridesAndStorage(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := filepath.Join(globalRoot, "workspace")
	toolStorage := filepath.Join(cwd, "profile-tools")
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"process":{"shell":" /bin/zsh ","login_shell":true,"kill_grace_ms":0,"max_running":4,"max_completed":8,"retention_days":0,"stdout":{"head_bytes":11,"tail_bytes":13},"stderr":{"head_bytes":17,"tail_bytes":19},"log_cap_bytes":512,"max_line_chars":80,"stdin_max_bytes":4096,"strip_ansi":false,"env":{"PROCESS_TEST":"enabled"},"env_deny":["PROCESS_SECRET"],"storage_dir":"runtime","paths":{"allow":["shared"],"deny":["private"]},"statecontext":{"states":["exited","running","exited"],"limit":7}}}}`),
	}
	settings, err := normalizeNativeProcessSettings(
		setting,
		globalRoot,
		settingPath,
		cwd,
		"",
		toolStorage,
		"",
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings(overrides) error = %v", err)
	}
	if settings.Shell != "/bin/zsh" || !settings.LoginShell || settings.KillGraceMs != 0 || settings.MaxRunning != 4 || settings.MaxCompleted != 8 || settings.RetentionDays != 0 {
		t.Fatalf("normalized process overrides = %+v", settings)
	}
	if settings.Stdout != (NativeBashStreamBudget{HeadBytes: 11, TailBytes: 13}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 17, TailBytes: 19}) {
		t.Fatalf("normalized process budgets = %+v/%+v", settings.Stdout, settings.Stderr)
	}
	if settings.StdinMaxBytes != 4096 {
		t.Fatalf("overridden process stdin_max_bytes = %d, want 4096", settings.StdinMaxBytes)
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
	if !reflect.DeepEqual(settings.StateContext, NativeProcessStateContextSettings{States: []string{"exited", "running"}, Limit: 7, Mode: NativeStateContextModeItems}) {
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
		`{"wait_timeout_ms":1000}`,
		`{"max_wait_timeout_ms":999}`,
		`{"stdout":null}`,
		`{"stderr":null}`,
		`{"status_log_bytes":10,"stdout":{"head_bytes":1,"tail_bytes":1}}`,
		`{"stdout":{"head_bytes":10485760,"tail_bytes":1}}`,
		`{"stdout":{"head_bytes":10,"tail_bytes":10},"log_cap_bytes":19}`,
		`{"max_line_chars":19}`,
		`{"stdin_max_bytes":0}`,
		`{"stdin_max_bytes":-1}`,
		`{"stdin_max_bytes":1.5}`,
		`{"stdin_max_bytes":"10"}`,
		`{"stdin_max_bytes":10485761}`,
		`{"strip_ansi":"no"}`,
		`{"stop_orphans":"yes"}`,
		`{"stop_orphans":1}`,
		`{"env":{"BAD-NAME":"x"}}`,
		`{"env":{"PROCESS_TEST":1}}`,
		`{"env_deny":["BAD-NAME"]}`,
		`{"paths":null}`,
		`{"statecontext":null}`,
		`{"statecontext":{"states":["unknown"]}}`,
		`{"statecontext":{"limit":0}}`,
	} {
		_, err := normalizeNativeProcessSettings(base(raw), globalRoot, settingPath, globalRoot, "", "", "")
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
		globalRoot, settingPath, globalRoot, "", "", "",
	)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings(legacy null) error = %v", err)
	}
	if legacy.Stdout != (NativeBashStreamBudget{TailBytes: 1}) || legacy.Stderr != (NativeBashStreamBudget{TailBytes: 1}) {
		t.Fatalf("legacy null budgets = %+v/%+v", legacy.Stdout, legacy.Stderr)
	}
}

func TestNormalizeNativeProcessSettingsLoadsEnvFileBelowInlineEnv(t *testing.T) {
	sessionDir := t.TempDir()
	settingPath := filepath.Join(sessionDir, "setting.json")
	contents := strings.Join([]string{
		"# process-specific configuration",
		"FILE_VALUE=from-file",
		"OVERRIDDEN=from-file",
		`QUOTED="line one\nline two # literal"`,
		"export DENIED=from-file",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "process.env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	setting := map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":{"env_file":"process.env","env":{"OVERRIDDEN":"inline"},"env_deny":["DENIED"]}}}`)}
	settings, err := normalizeNativeProcessSettings(setting, sessionDir, settingPath, sessionDir, sessionDir, "", sessionDir)
	if err != nil {
		t.Fatalf("normalizeNativeProcessSettings() error = %v", err)
	}
	if settings.EnvFile != filepath.Join(sessionDir, "process.env") {
		t.Fatalf("EnvFile = %q", settings.EnvFile)
	}
	want := map[string]string{
		"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0",
		"FILE_VALUE": "from-file", "OVERRIDDEN": "inline", "QUOTED": "line one\nline two # literal", "DENIED": "from-file",
	}
	if !reflect.DeepEqual(settings.Env, want) {
		t.Fatalf("Env = %v, want %v", settings.Env, want)
	}
	if !reflect.DeepEqual(settings.EnvDeny, []string{"DENIED"}) {
		t.Fatalf("EnvDeny = %v", settings.EnvDeny)
	}
}

func TestNormalizeNativeProcessSettingsEnvFileAbsoluteAndErrors(t *testing.T) {
	sessionDir := t.TempDir()
	settingPath := filepath.Join(sessionDir, "setting.json")
	absolute := filepath.Join(sessionDir, "absolute.env")
	if err := os.WriteFile(absolute, []byte("ABSOLUTE_VALUE=available\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "bad.env"), []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func(value string) (*NativeProcessSettings, error) {
		setting := map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":{"env_file":` + value + `}}}`)}
		return normalizeNativeProcessSettings(setting, sessionDir, settingPath, sessionDir, sessionDir, "", sessionDir)
	}
	quoted, _ := json.Marshal(absolute)
	settings, err := load(string(quoted))
	if err != nil {
		t.Fatalf("absolute env_file error = %v", err)
	}
	if settings.EnvFile != absolute || settings.Env["ABSOLUTE_VALUE"] != "available" {
		t.Fatalf("absolute env_file settings = %q %v", settings.EnvFile, settings.Env)
	}
	for value, want := range map[string]string{
		`"missing.env"`: "tools.native.process.env_file: could not read",
		`"   "`:         "tools.native.process: env_file must be a non-empty string",
		`null`:          "tools.native.process: env_file must be a non-empty string",
		`7`:             "tools.native.process: env_file must be a non-empty string",
		`"bad.env"`:     "bad.env:1: expected NAME=value",
	} {
		if _, err := load(value); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("env_file %s error = %v, want %q", value, err, want)
		}
	}
}

func TestProcessStopOrphansSetting(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	read := func(process string) (bool, error) {
		setting := map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"process":` + process + `}}`)}
		got, err := normalizeNativeProcessSettings(setting, globalRoot, settingPath, globalRoot, "", "", "")
		if err != nil {
			return false, err
		}
		return got.StopOrphans, nil
	}
	for name, tc := range map[string]struct {
		setting string
		want    bool
	}{
		"default":  {`{}`, true},
		"null":     {`{"stop_orphans":null}`, true},
		"explicit": {`{"stop_orphans":true}`, true},
		"off":      {`{"stop_orphans":false}`, false},
	} {
		if got, err := read(tc.setting); err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, tc.want)
		}
	}
}
