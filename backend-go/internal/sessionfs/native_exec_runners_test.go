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
	for _, name := range []string{"python", "node", "bash", "sh", "ruby", "perl", "ts"} {
		if exec.Langs[name].Mode != NativeExecModeArg {
			t.Errorf("%s: mode = %q, want arg", name, exec.Langs[name].Mode)
		}
	}
	if got := strings.Join(exec.Langs["ts"].Argv, " "); got != "bun --no-install --preload {template} -e {code}" || exec.Langs["ts"].Ext != ".ts" || exec.Langs["ts"].Template != NativeExecTemplateJS {
		t.Errorf("ts runner = %q ext %q", got, exec.Langs["ts"].Ext)
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
			"deno":   map[string]any{"argv": []any{"deno", "eval", "{code}"}},
		},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	langs := config.NativeExec.Langs
	if langs["python"].LibEnv != "MYPATH" || langs["deno"].LibEnv != "" {
		t.Errorf("python = %q, deno = %q", langs["python"].LibEnv, langs["deno"].LibEnv)
	}
	for name, want := range map[string]string{"bash": "PATH", "sh": "PATH", "node": "NODE_PATH", "ts": "NODE_PATH", "ruby": "RUBYLIB", "perl": "PERL5LIB"} {
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

func TestExecRunnersDefaultTemplates(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	langs := config.NativeExec.Langs
	for name, want := range map[string]string{"python": "python", "node": "js", "ts": "js", "ruby": "ruby", "bash": "", "sh": "", "perl": ""} {
		if langs[name].Template != want {
			t.Errorf("%s: template = %q, want %q", name, langs[name].Template, want)
		}
	}
}

func TestExecRunnersRefuseABadTemplate(t *testing.T) {
	for name, tc := range map[string]struct {
		entry map[string]any
		want  string
	}{
		"unknown":     {map[string]any{"argv": []any{"x", "{code}"}, "template": "perl"}, "x.template must be"},
		"no marker":   {map[string]any{"argv": []any{"x", "{code}"}, "template": "js"}, "x.argv must contain {template} exactly once"},
		"two markers": {map[string]any{"argv": []any{"x", "{template}", "{template}", "{code}"}, "template": "ruby"}, "exactly once"},
		"python":      {map[string]any{"argv": []any{"x", "{template}", "{code}"}, "template": "python"}, "must not contain {template}"},
		"orphan":      {map[string]any{"argv": []any{"x", "{template}", "{code}"}}, "has no template"},
	} {
		_, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{
			"runners": map[string]any{"x": tc.entry},
		}}))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestExecRunnersAcceptACustomTemplateRunner(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"bash": map[string]any{}, "exec": map[string]any{
		"runners": map[string]any{"deno": map[string]any{"argv": []any{"deno", "eval", "--preload", "{template}", "{code}"}, "template": "js", "ext": ".ts"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeExec.Langs["deno"].Template != "js" {
		t.Errorf("deno = %+v", config.NativeExec.Langs["deno"])
	}
}
