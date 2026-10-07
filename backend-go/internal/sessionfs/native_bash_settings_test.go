package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeBashSettingsDefaultsAndInheritsWritePolicy(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	writePolicy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(cwd, "private")}}}
	settings, err := normalizeNativeBashSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"bash":{}}}`),
	}, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "", &NativeWriteSettings{Policy: writePolicy}, NativePathPolicy{}, NativePathPolicy{})
	if err != nil {
		t.Fatalf("normalizeNativeBashSettings() error = %v", err)
	}
	if settings == nil {
		t.Fatal("normalizeNativeBashSettings() returned nil")
	}
	if settings.Shell != "/bin/bash" || settings.LoginShell || settings.TimeoutMs != 120_000 || settings.MaxTimeoutMs != 600_000 || settings.KillGraceMs != 2_000 || settings.KillProcessGroup != "always" {
		t.Fatalf("default bash execution settings = %+v", settings)
	}
	if settings.MaxBytes != 64*1024 || settings.Stdout != (NativeBashStreamBudget{HeadBytes: 16 * 1024, TailBytes: 32 * 1024}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 4 * 1024, TailBytes: 12 * 1024}) {
		t.Fatalf("default bash output budgets = %+v", settings)
	}
	if !settings.Spill.Enabled || settings.Spill.Keep != "truncated_only" || settings.Spill.CapBytes != 64*1024*1024 || settings.Spill.RetentionDays != 14 || settings.Spill.StorageDir != filepath.Join(cwd, ".bash-output") || settings.MaxLineChars != 2_000 || !settings.StripANSI {
		t.Fatalf("default bash output settings = %+v", settings)
	}
	if settings.StdinMaxBytes != 256*1024 {
		t.Fatalf("default stdin_max_bytes = %d, want %d", settings.StdinMaxBytes, 256*1024)
	}
	if settings.Env["NO_COLOR"] != "1" || settings.Env["GIT_TERMINAL_PROMPT"] != "0" || !reflect.DeepEqual(settings.EnvDeny, []string{}) {
		t.Fatalf("default bash environment = %+v", settings)
	}
	if !reflect.DeepEqual(settings.Policy, writePolicy) {
		t.Fatalf("bash path policy = %+v, want inherited write policy %+v", settings.Policy, writePolicy)
	}
}

func TestNormalizeNativeBashSettingsOverridesAndLoadsEnvFile(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	envFile := filepath.Join(sessionDir, "command.env")
	contents := "# command settings\nFILE_VALUE=from-file\nOVERRIDDEN=from-file\nQUOTED=\"line one\\nline two # literal\"\nexport DENIED=from-file\n"
	if err := os.WriteFile(envFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"bash":{"shell":" /bin/sh ","login_shell":true,"timeout_ms":30000,"max_timeout_ms":45000,"kill_grace_ms":400,"kill_process_group":"timeout_only","max_bytes":1024,"stdout":{"head_bytes":100,"tail_bytes":100},"stderr":{"head_bytes":50,"tail_bytes":50},"spill":{"enabled":false,"keep":"always","storage_dir":"custom-spill"},"max_line_chars":200,"stdin_max_bytes":4096,"strip_ansi":false,"env_file":"command.env","env":{"OVERRIDDEN":"inline"},"env_deny":[" DENIED "],"paths":{"deny":["private"]}}}}`),
	}
	settings, err := normalizeNativeBashSettings(setting, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "", nil, NativePathPolicy{}, NativePathPolicy{})
	if err != nil {
		t.Fatalf("normalizeNativeBashSettings() error = %v", err)
	}
	if settings.Shell != "/bin/sh" || !settings.LoginShell || settings.TimeoutMs != 30_000 || settings.MaxTimeoutMs != 45_000 || settings.KillGraceMs != 400 || settings.KillProcessGroup != "timeout_only" {
		t.Fatalf("normalized execution settings = %+v", settings)
	}
	if settings.MaxBytes != 1024 || settings.Stdout != (NativeBashStreamBudget{HeadBytes: 100, TailBytes: 100}) || settings.Stderr != (NativeBashStreamBudget{HeadBytes: 50, TailBytes: 50}) || settings.MaxLineChars != 200 || settings.StripANSI {
		t.Fatalf("normalized output settings = %+v", settings)
	}
	if settings.StdinMaxBytes != 4096 {
		t.Fatalf("normalized stdin_max_bytes = %d, want 4096", settings.StdinMaxBytes)
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
		t.Fatalf("bash path policy = %+v, want %+v", settings.Policy, wantPolicy)
	}
}

func TestNormalizeNativeBashSettingsValidatesConfiguration(t *testing.T) {
	globalRoot := t.TempDir()
	cwd := t.TempDir()
	sessionDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "spill")
	cases := []string{
		`{"native":{"bash":{"shell":"  "}}}`,
		`{"native":{"bash":{"login_shell":"yes"}}}`,
		`{"native":{"bash":{"timeout_ms":0}}}`,
		`{"native":{"bash":{"timeout_ms":5000,"max_timeout_ms":1000}}}`,
		`{"native":{"bash":{"kill_grace_ms":-1}}}`,
		`{"native":{"bash":{"kill_process_group":"sometimes"}}}`,
		`{"native":{"bash":{"max_bytes":0}}}`,
		`{"native":{"bash":{"max_bytes":1024,"stdout":{"head_bytes":2048,"tail_bytes":2048}}}}`,
		`{"native":{"bash":{"stdout":{"head_bytes":-1}}}}`,
		`{"native":{"bash":{"spill":{"stdout_full":"fixed.log"}}}}`,
		`{"native":{"bash":{"spill":{"stdout_full":"{cwd}/{unknown}-{call_id}.log"}}}}`,
		`{"native":{"bash":{"spill":{"stdout_full":"{cwd}/same-{call_id}.log","stderr_full":"{cwd}/same-{call_id}.log"}}}}`,
		`{"native":{"bash":{"spill":{"keep":"sometimes"}}}}`,
		`{"native":{"bash":{"spill":{"cap_bytes":0}}}}`,
		`{"native":{"bash":{"spill":{"storage_dir":"` + outside + `"}}}}`,
		`{"native":{"bash":{"max_line_chars":5}}}`,
		`{"native":{"bash":{"stdin_max_bytes":0}}}`,
		`{"native":{"bash":{"stdin_max_bytes":-1}}}`,
		`{"native":{"bash":{"stdin_max_bytes":1.5}}}`,
		`{"native":{"bash":{"stdin_max_bytes":"10"}}}`,
		`{"native":{"bash":{"stdin_max_bytes":10485761}}}`,
		`{"native":{"bash":{"strip_ansi":"yes"}}}`,
		`{"native":{"bash":{"env_file":null}}}`,
		`{"native":{"bash":{"env":{"BAD_NAME":1}}}}`,
		`{"native":{"bash":{"env_deny":["1BAD-NAME"]}}}`,
		`{"native":{"bash":{"mystery":true}}}`,
	}
	for _, raw := range cases {
		setting := map[string]json.RawMessage{"tools": json.RawMessage(raw)}
		_, err := normalizeNativeBashSettings(setting, globalRoot, filepath.Join(sessionDir, "setting.json"), cwd, sessionDir, "", nil, NativePathPolicy{}, NativePathPolicy{})
		if err == nil || !strings.Contains(err.Error(), "tools.native.bash") {
			t.Errorf("normalizeNativeBashSettings(%s) error = %v; want a bash config error", raw, err)
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
