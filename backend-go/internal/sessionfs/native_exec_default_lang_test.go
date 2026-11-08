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
		{"a configured language", map[string]any{"default_lang": "mine", "runners": map[string]any{"mine": map[string]any{"argv": []any{"bash", "-c", "{code}"}}}}, "mine"},
		{"a replaced built-in keeps its name", map[string]any{"runners": map[string]any{"bash": map[string]any{"argv": []any{"bash", "-c", "{code}"}}}}, "bash"},
	}
	for _, tc := range cases {
		config, err := loadSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"exec": tc.exec}},
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
		_, err := loadSetting(t, map[string]any{
			"tools": map[string]any{"native": map[string]any{"exec": map[string]any{"default_lang": value}}},
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

// exec reads its own env_file and env. Nothing of tool_smith's file or inline values (the
// other tool with the same child-process keys) reaches it, and exec's reach tool_smith no
// more than the other way round.
func TestExecEnvironmentIsItsOwnAndNeverToolSmiths(t *testing.T) {
	bashFile := writeExecEnvFile(t, "BASH_ONLY=1\nSHARED=bash\nWIN=bash-file\n")
	execFile := writeExecEnvFile(t, "EXEC_ONLY=1\nSHARED=exec\nWIN=exec-file\n")
	config, err := loadSetting(t, nativeTools(map[string]any{
		"tool_smith": map[string]any{"env_file": bashFile, "env": map[string]any{"WIN": "bash-inline", "BASH_INLINE": "1"}},
		"exec":       map[string]any{"env_file": execFile, "env": map[string]any{"WIN": "exec-inline"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	smith, exec := config.NativeToolSmith.Child.Env, config.NativeExec.Child.Env
	for name, want := range map[string]string{"BASH_ONLY": "1", "SHARED": "bash", "WIN": "bash-inline", "BASH_INLINE": "1", "NO_COLOR": "1"} {
		if smith[name] != want {
			t.Errorf("tool_smith env %s = %q, want %q (all: %v)", name, smith[name], want, smith)
		}
	}
	for name, want := range map[string]string{"EXEC_ONLY": "1", "SHARED": "exec", "WIN": "exec-inline", "NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"} {
		if exec[name] != want {
			t.Errorf("exec env %s = %q, want %q (all: %v)", name, exec[name], want, exec)
		}
	}
	for _, leaked := range []string{"BASH_ONLY", "BASH_INLINE"} {
		if _, found := exec[leaked]; found {
			t.Errorf("tool_smith's %s leaked into exec: %v", leaked, exec)
		}
	}
	if _, found := smith["EXEC_ONLY"]; found {
		t.Errorf("exec's file value leaked into tool_smith: %v", smith)
	}
	if config.NativeExec.Child.EnvFile != execFile {
		t.Errorf("exec env file = %q, want %q", config.NativeExec.Child.EnvFile, execFile)
	}
}

// Without its own env_file, exec runs with the built-in environment, not tool_smith's file.
func TestExecWithoutEnvFileDoesNotTakeToolSmithsEnvironment(t *testing.T) {
	bashFile := writeExecEnvFile(t, "BASH_ONLY=1\n")
	config, err := loadSetting(t, nativeTools(map[string]any{
		"tool_smith": map[string]any{"env_file": bashFile},
		"exec":       map[string]any{},
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
		_, err := loadSetting(t, nativeTools(map[string]any{
			"exec": map[string]any{"env_file": value},
		}))
		if err == nil || !strings.Contains(err.Error(), "tools.native.exec") || !strings.Contains(err.Error(), "env_file") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestExecSpillDefaultsToItsOwnDirectory(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{
		"tool_smith": map[string]any{},
		"exec":       map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	smith, exec := config.NativeToolSmith.Child.Spill.StorageDir, config.NativeExec.Child.Spill.StorageDir
	if filepath.Base(smith) != ".bash-output" || filepath.Base(exec) != ".exec-output" || filepath.Dir(smith) != filepath.Dir(exec) {
		t.Fatalf("tool_smith spill = %q, exec spill = %q; want sibling .bash-output and .exec-output", smith, exec)
	}
}

func TestExecSpillFilesAreNamedForTheTool(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{
		"tool_smith": map[string]any{},
		"exec":       map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := config.NativeToolSmith.Child.Spill.FilePrefix; got != "bash" {
		t.Fatalf("tool_smith spill file prefix = %q, want bash", got)
	}
	if got := config.NativeExec.Child.Spill.FilePrefix; got != "exec" {
		t.Fatalf("exec spill file prefix = %q, want exec", got)
	}
	legacy, err := loadSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{"spill": map[string]any{"stdout_full": "{cwd}/.exec-output/{call_id}.{stream}"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := legacy.NativeExec.Child.Spill.FilePrefix; got != "exec" {
		t.Fatalf("exec spill file prefix with a legacy template = %q, want exec", got)
	}
	defaultTemplate, err := loadSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{"spill": map[string]any{"stdout_full": "{cwd}/x/{call_id}.out"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultTemplate.NativeExec.Child.Spill.StderrFull; !strings.Contains(got, "/.exec-output/exec-") {
		t.Fatalf("exec default stderr template = %q, want the .exec-output/exec- file name", got)
	}
}

func TestExecSpillStorageDirAndLegacyTemplatesStayConfigurable(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{"spill": map[string]any{"storage_dir": "custom-out"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.NativeExec.Child.Spill.StorageDir, filepath.Join(config.WorkingDir, "custom-out"); got != want {
		t.Fatalf("exec storage_dir = %q, want %q (a configured relative path is from the cwd)", got, want)
	}
	config, err = loadSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{"spill": map[string]any{"storage_dir": ".exec-output", "stdout_full": "{cwd}/x/{call_id}.out", "stderr_full": "{cwd}/x/{call_id}.err"}},
	}))
	if err != nil {
		t.Fatalf("explicit default storage_dir with templates: %v", err)
	}
	if config.NativeExec.Child.Spill.StdoutFull == "" {
		t.Fatal("legacy stdout_full was not kept")
	}
	_, err = loadSetting(t, nativeTools(map[string]any{
		"exec": map[string]any{"spill": map[string]any{"storage_dir": ".bash-output", "stdout_full": "{cwd}/x/{call_id}.out"}},
	}))
	if err == nil || !strings.Contains(err.Error(), "tools.native.exec.spill.storage_dir cannot be combined") {
		t.Fatalf("bash's directory name is a custom storage_dir for exec; error = %v", err)
	}
}
