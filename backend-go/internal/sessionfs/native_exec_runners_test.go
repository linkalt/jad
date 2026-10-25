package sessionfs

import "testing"

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
	if exec.InputMaxBytes != 1024*1024 || exec.ArgMaxBytes != 100000 || exec.FunctionsDir != ".jad/exec-functions" {
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
