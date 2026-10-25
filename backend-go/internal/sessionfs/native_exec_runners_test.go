package sessionfs

import (
	"strings"
	"testing"
)

func TestExecRunnersDefaultsDeliverCodeAsAnArgument(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	exec := config.NativeExec
	for _, name := range []string{"python", "node", "bash", "sh", "ruby", "perl"} {
		if exec.Langs[name].Mode != NativeExecModeArg {
			t.Errorf("%s: mode = %q, want arg", name, exec.Langs[name].Mode)
		}
	}
	if exec.InputMaxBytes != 1024*1024 || exec.ArgMaxBytes != 100000 || exec.FunctionsDir != ".jad/exec" {
		t.Errorf("defaults = %d %d %q", exec.InputMaxBytes, exec.ArgMaxBytes, exec.FunctionsDir)
	}
}

func TestExecRunnersKeepStdinModeForAnEntryThatAsksForIt(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{
		"runners": map[string]any{"legacy": map[string]any{"argv": []any{"python3", "-"}, "mode": "stdin"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := config.NativeExec.Langs["legacy"].Mode; got != NativeExecModeStdin {
		t.Errorf("mode = %q", got)
	}
}

func TestExecRunnersExposeTheirHelperFolderThroughLibEnv(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{
		"runners": map[string]any{
			"python": map[string]any{"argv": []any{"python3", "-c", "{code}"}, "lib_env": "MYPATH"},
			"ts":     map[string]any{"argv": []any{"bun", "-e", "{code}"}},
		},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	langs := config.NativeExec.Langs
	if langs["python"].LibEnv != "MYPATH" || langs["ts"].LibEnv != "" {
		t.Errorf("python = %q, ts = %q", langs["python"].LibEnv, langs["ts"].LibEnv)
	}
	for name, want := range map[string]string{"bash": "PATH", "sh": "PATH", "node": "NODE_PATH", "ruby": "RUBYLIB", "perl": "PERL5LIB"} {
		if langs[name].LibEnv != want {
			t.Errorf("%s: lib_env = %q, want %q", name, langs[name].LibEnv, want)
		}
	}
}

func TestExecRunnersRefuseABadLibEnv(t *testing.T) {
	_, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{
		"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{code}"}, "lib_env": "NOT OK"}},
	}}))
	if err == nil || !strings.Contains(err.Error(), "tools.native.exec.runners.x.lib_env must be empty or an environment variable name") {
		t.Fatalf("err = %v", err)
	}
}
