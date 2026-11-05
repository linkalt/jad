package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// normalizeExecChild reads tools.native.exec and returns its child-process settings, the
// bash-style keys exec shares with tool_smith.
func normalizeExecChild(setting map[string]json.RawMessage, globalRoot, settingPath, cwd, sessionDir string) (*NativeBashSettings, error) {
	settings, err := normalizeNativeExecSettings(setting, globalRoot, settingPath, cwd, sessionDir, "", sessionDir)
	if err != nil || settings == nil {
		return nil, err
	}
	return &settings.Child, nil
}

func TestNormalizeNativeExecChildSettingsDefaults(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	settings, err := normalizeExecChild(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"exec":{}}}`),
	}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir)
	if err != nil {
		t.Fatalf("normalizeExecChild() error = %v", err)
	}
	if settings == nil {
		t.Fatal("normalizeExecChild() returned nil")
	}
	if settings.Shell != "/bin/bash" || settings.LoginShell || settings.TimeoutMs != 120_000 || settings.MaxTimeoutMs != 600_000 || settings.KillGraceMs != 2_000 || settings.KillDeadlineMs != 2_000 || settings.DrainAfterExitMs != 500 || settings.KillProcessGroup != "always" || !settings.KillChildrenOnRunEnd {
		t.Fatalf("default bash execution settings = %+v", settings)
	}
	if settings.MaxBytes != 64*1024 || settings.Stdout != (NativeBashStreamBudget{HeadBytes: 16 * 1024, TailBytes: 32 * 1024}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 4 * 1024, TailBytes: 12 * 1024}) {
		t.Fatalf("default bash output budgets = %+v", settings)
	}
	if !settings.Spill.Enabled || settings.Spill.Keep != "truncated_only" || settings.Spill.CapBytes != 64*1024*1024 || settings.Spill.RetentionDays != 14 || settings.Spill.StorageDir != filepath.Join(cwd, ".exec-output") || settings.MaxLineChars != 2_000 || !settings.StripANSI {
		t.Fatalf("default bash output settings = %+v", settings)
	}
	if settings.Spill.PruneIntervalSeconds != 3600 {
		t.Fatalf("default spill.prune_interval_seconds = %d, want 3600", settings.Spill.PruneIntervalSeconds)
	}
	if settings.StdinMaxBytes != 256*1024 {
		t.Fatalf("default stdin_max_bytes = %d, want %d", settings.StdinMaxBytes, 256*1024)
	}
	if settings.Env["NO_COLOR"] != "1" || settings.Env["GIT_TERMINAL_PROMPT"] != "0" || !reflect.DeepEqual(settings.EnvDeny, []string{}) {
		t.Fatalf("default bash environment = %+v", settings)
	}
	if len(settings.Policy.Paths) != 0 || settings.Policy.UnrestrictedPaths {
		t.Fatalf("exec path policy = %+v, want the empty default", settings.Policy)
	}
}

func TestNormalizeNativeExecChildSettingsOverridesAndLoadsEnvFile(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	envFile := filepath.Join(sessionDir, "command.env")
	contents := "# command settings\nFILE_VALUE=from-file\nOVERRIDDEN=from-file\nQUOTED=\"line one\\nline two # literal\"\nexport DENIED=from-file\n"
	if err := os.WriteFile(envFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"exec":{"shell":" /bin/sh ","login_shell":true,"timeout_ms":30000,"max_timeout_ms":45000,"kill_grace_ms":400,"kill_deadline_ms":900,"drain_after_exit_ms":70,"kill_process_group":"timeout_only","kill_children_on_run_end":"off","max_bytes":1024,"stdout":{"head_bytes":100,"tail_bytes":100},"stderr":{"head_bytes":50,"tail_bytes":50},"spill":{"enabled":false,"keep":"always","prune_interval_seconds":0,"storage_dir":"custom-spill"},"max_line_chars":200,"stdin_max_bytes":4096,"strip_ansi":false,"env_file":"command.env","env":{"OVERRIDDEN":"inline"},"env_deny":[" DENIED "],"paths":{"deny":["private"]}}}}`),
	}
	settings, err := normalizeExecChild(setting, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir)
	if err != nil {
		t.Fatalf("normalizeExecChild() error = %v", err)
	}
	if settings.Shell != "/bin/sh" || !settings.LoginShell || settings.TimeoutMs != 30_000 || settings.MaxTimeoutMs != 45_000 || settings.KillGraceMs != 400 || settings.KillDeadlineMs != 900 || settings.DrainAfterExitMs != 70 || settings.KillProcessGroup != "timeout_only" || settings.KillChildrenOnRunEnd {
		t.Fatalf("normalized execution settings = %+v", settings)
	}
	if settings.MaxBytes != 1024 || settings.Stdout != (NativeBashStreamBudget{HeadBytes: 100, TailBytes: 100}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 50, TailBytes: 50}) || settings.MaxLineChars != 200 || settings.StripANSI {
		t.Fatalf("normalized output settings = %+v", settings)
	}
	if settings.StdinMaxBytes != 4096 {
		t.Fatalf("normalized stdin_max_bytes = %d, want 4096", settings.StdinMaxBytes)
	}
	if settings.Spill.PruneIntervalSeconds != 0 {
		t.Fatalf("normalized spill.prune_interval_seconds = %d, want 0", settings.Spill.PruneIntervalSeconds)
	}
	if settings.Spill.Enabled || settings.Spill.Keep != "always" || settings.Spill.StorageDir != filepath.Join(cwd, "custom-spill") {
		t.Fatalf("normalized spill settings = %+v", settings.Spill)
	}
	if settings.EnvFile != envFile || settings.Env["FILE_VALUE"] != "from-file" || settings.Env["OVERRIDDEN"] != "inline" || settings.Env["QUOTED"] != "line one\nline two # literal" || settings.Env["DENIED"] != "from-file" {
		t.Fatalf("normalized environment = %+v", settings.Env)
	}
	if !reflect.DeepEqual(settings.EnvDeny, []string{"DENIED"}) {
		t.Fatalf("environment denials = %+v", settings.EnvDeny)
	}
	wantPolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(globalRoot, "private")}}, UnrestrictedPaths: true}
	if !reflect.DeepEqual(settings.Policy, wantPolicy) {
		t.Fatalf("exec path policy = %+v, want %+v", settings.Policy, wantPolicy)
	}
}

func TestNormalizeNativeExecChildSettingsValidatesConfiguration(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	cases := []string{
		`{"native":{"exec":{"shell":"  "}}}`,
		`{"native":{"exec":{"login_shell":"yes"}}}`,
		`{"native":{"exec":{"timeout_ms":0}}}`,
		`{"native":{"exec":{"timeout_ms":5000,"max_timeout_ms":1000}}}`,
		`{"native":{"exec":{"kill_grace_ms":-1}}}`,
		`{"native":{"exec":{"kill_deadline_ms":0}}}`,
		`{"native":{"exec":{"kill_deadline_ms":60001}}}`,
		`{"native":{"exec":{"drain_after_exit_ms":0}}}`,
		`{"native":{"exec":{"drain_after_exit_ms":"soon"}}}`,
		`{"native":{"exec":{"kill_process_group":"sometimes"}}}`,
		`{"native":{"exec":{"kill_children_on_run_end":"maybe"}}}`,
		`{"native":{"exec":{"kill_children_on_run_end":true}}}`,
		`{"native":{"exec":{"kill_children_on_run_end":null}}}`,
		`{"native":{"exec":{"max_bytes":0}}}`,
		`{"native":{"exec":{"max_bytes":1024,"stdout":{"head_bytes":2048,"tail_bytes":2048}}}}`,
		`{"native":{"exec":{"stdout":{"head_bytes":-1}}}}`,
		`{"native":{"exec":{"spill":{"stdout_full":"fixed.log"}}}}`,
		`{"native":{"exec":{"spill":{"stdout_full":"{cwd}/{unknown}-{call_id}.log"}}}}`,
		`{"native":{"exec":{"spill":{"stdout_full":"{cwd}/same-{call_id}.log","stderr_full":"{cwd}/same-{call_id}.log"}}}}`,
		`{"native":{"exec":{"spill":{"keep":"sometimes"}}}}`,
		`{"native":{"exec":{"spill":{"cap_bytes":0}}}}`,
		`{"native":{"exec":{"spill":{"prune_interval_seconds":-1}}}}`,
		`{"native":{"exec":{"spill":{"prune_interval_seconds":86401}}}}`,
		`{"native":{"exec":{"spill":{"prune_interval_seconds":"60"}}}}`,
		`{"native":{"exec":{"max_line_chars":5}}}`,
		`{"native":{"exec":{"stdin_max_bytes":0}}}`,
		`{"native":{"exec":{"stdin_max_bytes":-1}}}`,
		`{"native":{"exec":{"stdin_max_bytes":1.5}}}`,
		`{"native":{"exec":{"stdin_max_bytes":"10"}}}`,
		`{"native":{"exec":{"stdin_max_bytes":10485761}}}`,
		`{"native":{"exec":{"strip_ansi":"yes"}}}`,
		`{"native":{"exec":{"env_file":null}}}`,
		`{"native":{"exec":{"env":{"BAD_NAME":1}}}}`,
		`{"native":{"exec":{"env_deny":["1BAD-NAME"]}}}`,
		`{"native":{"exec":{"mystery":true}}}`,
	}
	for _, raw := range cases {
		setting := map[string]json.RawMessage{"tools": json.RawMessage(raw)}
		_, err := normalizeExecChild(setting, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir)
		if err == nil || !strings.Contains(err.Error(), "tools.native.exec") {
			t.Errorf("normalizeExecChild(%s) error = %v; want an exec config error", raw, err)
		}
	}
}

// allow_unreadable let a spill folder sit where read and grep could not reach it; with those
// tools gone the readability check went too, and the key is refused by name.
func TestNativeExecSpillAllowUnreadableIsRefused(t *testing.T) {
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	for _, section := range []string{"tools.native.exec", "tools.native.tool_smith"} {
		for _, raw := range []string{`{"allow_unreadable":true}`, `{"allow_unreadable":false}`} {
			_, err := normalizeNativeBashSpill(json.RawMessage(raw), section, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "")
			if err == nil || !strings.Contains(err.Error(), section+".spill.allow_unreadable was removed") {
				t.Errorf("%s spill %s: error = %v, want a removal error", section, raw, err)
			}
		}
	}
	// A spill folder outside the workspace is no longer refused: nothing reads it back by path rules.
	outside := filepath.Join(t.TempDir(), "spill")
	spill, err := normalizeNativeBashSpill(json.RawMessage(`{"storage_dir":"`+outside+`"}`), "tools.native.exec", filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "")
	if err != nil || spill.StorageDir != outside {
		t.Fatalf("outside storage_dir: %+v, %v", spill, err)
	}
}

func TestNormalizeNativeBashSpillPruneInterval(t *testing.T) {
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	settingPath := filepath.Join(sessionDir, "setting.json")
	for _, section := range []string{"tools.native.tool_smith", "tools.native.exec"} {
		for raw, want := range map[string]int64{
			``:                                 3600,
			`{}`:                               3600,
			`{"prune_interval_seconds":null}`:  3600,
			`{"prune_interval_seconds":60}`:    60,
			`{"prune_interval_seconds":86400}`: 86400,
			`{"prune_interval_seconds":0}`:     0,
		} {
			spill, err := normalizeNativeBashSpill(json.RawMessage(raw), section, settingPath, cwd, sessionDir, "")
			if err != nil {
				t.Fatalf("%s spill %q: error = %v", section, raw, err)
			}
			if spill.PruneIntervalSeconds != want {
				t.Errorf("%s spill %q: prune_interval_seconds = %d, want %d", section, raw, spill.PruneIntervalSeconds, want)
			}
		}
		for _, raw := range []string{`{"prune_interval_seconds":-1}`, `{"prune_interval_seconds":86401}`, `{"prune_interval_seconds":1.5}`, `{"prune_interval_seconds":"60"}`} {
			_, err := normalizeNativeBashSpill(json.RawMessage(raw), section, settingPath, cwd, sessionDir, "")
			if err == nil || !strings.Contains(err.Error(), section+".spill.prune_interval_seconds") {
				t.Errorf("%s spill %q: error = %v; want one naming %s.spill.prune_interval_seconds", section, raw, err, section)
			}
		}
	}
}

func TestParseNativeBashEnvFileQuotesAndRejectsMalformedLines(t *testing.T) {
	values, err := parseNativeBashEnvFile("\uFEFF # comment\r\nPLAIN=hello world\r\nCOMMENT=value # ignored\r\nSINGLE='one\\ntwo'\r\nDOUBLE=\"one\\n\\t\\\\two\" # note\r\nexport EMPTY=\r\n", "test.env")
	if err != nil {
		t.Fatalf("parseNativeBashEnvFile() error = %v", err)
	}
	want := map[string]string{
		"PLAIN":   "hello world",
		"COMMENT": "value",
		"SINGLE":  "one\\ntwo",
		"DOUBLE":  "one\n\t\\two",
		"EMPTY":   "",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("parsed env = %#v, want %#v", values, want)
	}
	for _, contents := range []string{"NOT VALID\n", "BROKEN=\"unterminated\n", "GOOD=value\nBAD='quoted' trailing\n"} {
		if _, err := parseNativeBashEnvFile(contents, "bad.env"); err == nil {
			t.Errorf("parseNativeBashEnvFile(%q) succeeded; want error", contents)
		}
	}
}

// {session_name} is the last element of the session's state folder, which is <state>/<id>.
func TestBashSpillTemplateSessionNameIsTheSessionID(t *testing.T) {
	stateDir := filepath.Join("state", "demo")
	got := resolveBashSpillTemplate("{session_dir}/out/{session_name}-{call_id}.{stream}", "/work", stateDir, "c1", "stdout")
	if want := filepath.Join("state", "demo") + "/out/demo-c1.stdout"; got != want {
		t.Fatalf("resolveBashSpillTemplate = %q, want %q", got, want)
	}
}
