package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parity golden records only whether a setting loads, so the value a name resolves
// to is checked here against what the TypeScript loader stores.
func TestExecDefaultLangResolvesToTheTableNameAndDefaultsToBash(t *testing.T) {
	cases := []struct {
		name string
		exec map[string]any
		want string
	}{
		{"absent", map[string]any{}, "bash"},
		{"null", map[string]any{"default_lang": nil}, "bash"},
		{"a name", map[string]any{"default_lang": "python"}, "python"},
		{"an alias, any case, padded", map[string]any{"default_lang": " PY "}, "python"},
		{"a configured language", map[string]any{"default_lang": "mine", "langs": map[string]any{"mine": map[string]any{"argv": []any{"bash", "-s", "--"}}}}, "mine"},
		{"a replaced built-in keeps its name", map[string]any{"langs": map[string]any{"bash": map[string]any{"argv": []any{"bash", "-s"}}}}, "bash"},
	}
	for _, tc := range cases {
		config, err := loadApprovalSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"bash": map[string]any{}, "exec": tc.exec}},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := config.NativeExec.DefaultLang; got != tc.want {
			t.Errorf("%s: DefaultLang = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExecDefaultLangRefusesWhatIsNotInTheTable(t *testing.T) {
	for name, value := range map[string]any{"unknown": "cobol", "empty": "", "a number": float64(3)} {
		_, err := loadApprovalSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"bash": map[string]any{}, "exec": map[string]any{"default_lang": value}}},
		})
		if err == nil {
			t.Errorf("%s: default_lang %v was accepted", name, value)
		}
	}
}

func writeExecEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// exec reads its own env_file and env. Nothing of bash's file or inline values reaches it,
// and exec's reach bash no more than the other way round.
func TestExecEnvironmentIsItsOwnAndNeverBashs(t *testing.T) {
	bashFile := writeExecEnvFile(t, "BASH_ONLY=1\nSHARED=bash\nWIN=bash-file\n")
	execFile := writeExecEnvFile(t, "EXEC_ONLY=1\nSHARED=exec\nWIN=exec-file\n")
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"bash": map[string]any{"env_file": bashFile, "env": map[string]any{"WIN": "bash-inline", "BASH_INLINE": "1"}},
		"exec": map[string]any{"env_file": execFile, "env": map[string]any{"WIN": "exec-inline"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	bash, exec := config.NativeBash.Env, config.NativeExec.Child.Env
	for name, want := range map[string]string{"BASH_ONLY": "1", "SHARED": "bash", "WIN": "bash-inline", "BASH_INLINE": "1", "NO_COLOR": "1"} {
		if bash[name] != want {
			t.Errorf("bash env %s = %q, want %q (all: %v)", name, bash[name], want, bash)
		}
	}
	for name, want := range map[string]string{"EXEC_ONLY": "1", "SHARED": "exec", "WIN": "exec-inline", "NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"} {
		if exec[name] != want {
			t.Errorf("exec env %s = %q, want %q (all: %v)", name, exec[name], want, exec)
		}
	}
	for _, leaked := range []string{"BASH_ONLY", "BASH_INLINE"} {
		if _, found := exec[leaked]; found {
			t.Errorf("bash's %s leaked into exec: %v", leaked, exec)
		}
	}
	if _, found := bash["EXEC_ONLY"]; found {
		t.Errorf("exec's file value leaked into bash: %v", bash)
	}
	if config.NativeExec.Child.EnvFile != execFile {
		t.Errorf("exec env file = %q, want %q", config.NativeExec.Child.EnvFile, execFile)
	}
}

// Without its own env_file, exec runs with the built-in environment, not bash's file.
func TestExecWithoutEnvFileDoesNotTakeBashsEnvironment(t *testing.T) {
	bashFile := writeExecEnvFile(t, "BASH_ONLY=1\n")
	config, err := loadApprovalSetting(t, nativeTools(map[string]any{
		"bash": map[string]any{"env_file": bashFile},
		"exec": map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	env := config.NativeExec.Child.Env
	if _, leaked := env["BASH_ONLY"]; leaked || config.NativeExec.Child.EnvFile != "" || env["NO_COLOR"] != "1" {
		t.Fatalf("exec env = %v file = %q, want only the built-in defaults", env, config.NativeExec.Child.EnvFile)
	}
}

func TestExecEnvFileErrorsNameTheExecKey(t *testing.T) {
	for name, value := range map[string]any{"missing file": filepath.Join(t.TempDir(), "none.env"), "empty": " ", "null": nil, "number": 3} {
		_, err := loadApprovalSetting(t, nativeTools(map[string]any{
			"bash": map[string]any{},
			"exec": map[string]any{"env_file": value},
		}))
		if err == nil || !strings.Contains(err.Error(), "tools.native.exec") || !strings.Contains(err.Error(), "env_file") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}
