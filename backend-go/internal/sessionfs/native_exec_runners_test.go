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
	for name, want := range map[string]string{"python": "python", "node": "js", "ts": "js", "ruby": "ruby", "bash": "", "sh": "", "perl": "perl", "php": "php", "lua": "lua", "deno": "deno", "r": "r"} {
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
		"unknown":     {map[string]any{"argv": []any{"x", "{code}"}, "template": "cobol"}, "x.template must be one of js, python, ruby, perl, php, lua, r, deno"},
		"perl marker": {map[string]any{"argv": []any{"x", "{template}", "{code}"}, "template": "perl"}, "the perl template is found through PERL5LIB"},
		"r marker":    {map[string]any{"argv": []any{"x", "{template}", "{file}"}, "mode": "file", "template": "r"}, "the r template is found through R_PROFILE_USER"},
		"php no mark": {map[string]any{"argv": []any{"x", "{file}"}, "mode": "file", "template": "php"}, "exactly once"},
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

func TestExecStandbyDefaultsWarmOnlyTheSlowStartingRunners(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	exec := config.NativeExec
	want := map[string]string{"python": NativeExecStandbyPython, "node": NativeExecStandbyJS, "ruby": NativeExecStandbyRuby, "bash": "", "sh": "", "perl": "", "ts": ""}
	for name, standby := range want {
		if got := exec.Langs[name].Standby; got != standby || exec.Langs[name].StandbyCode != "" {
			t.Errorf("%s: standby = %q, want %q", name, got, standby)
		}
	}
	if exec.StandbyPool != 1 || exec.StandbyIdleMs != 300000 || exec.StandbyMax != 8 {
		t.Errorf("standby defaults = pool %d idle %d max %d", exec.StandbyPool, exec.StandbyIdleMs, exec.StandbyMax)
	}
}

func TestExecStandbySettingsAreReadAndBounded(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{
		"standby_pool": 0, "standby_idle_ms": 1000, "standby_max": 256,
		"runners": map[string]any{
			"python": map[string]any{"argv": []any{"python3.12", "-c", "{code}"}},
			"lua":    map[string]any{"argv": []any{"lua", "{file}"}, "mode": "file", "ext": ".lua", "standby_code": "local f = io.open('/dev/fd/3')"},
			"deno":   map[string]any{"argv": []any{"deno", "eval", "{code}"}, "standby": "js"},
		},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	exec := config.NativeExec
	if exec.StandbyPool != 0 || exec.StandbyIdleMs != 1000 || exec.StandbyMax != 256 {
		t.Errorf("standby settings = pool %d idle %d max %d", exec.StandbyPool, exec.StandbyIdleMs, exec.StandbyMax)
	}
	if exec.Langs["python"].Standby != "" {
		t.Error("an entry that replaces a built-in runner keeps no standby it did not set")
	}
	if exec.Langs["lua"].StandbyCode != "local f = io.open('/dev/fd/3')" || exec.Langs["deno"].Standby != NativeExecStandbyJS {
		t.Errorf("configured shims = %+v %+v", exec.Langs["lua"], exec.Langs["deno"])
	}
	for _, bad := range []struct {
		name, want string
		exec       map[string]any
	}{
		{"pool over", "standby_pool must be a safe integer between 0 and 16", map[string]any{"standby_pool": 17}},
		{"idle under", "standby_idle_ms must be a safe integer between 1000 and 21600000", map[string]any{"standby_idle_ms": 999}},
		{"max zero", "standby_max must be a safe integer between 1 and 256", map[string]any{"standby_max": 0}},
		{"pipe in arg mode", `standby "pipe" needs mode "file"`, map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{code}"}, "standby": "pipe"}}}},
		{"unknown shim", `standby must be "", "python", "js", "ruby" or "pipe"`, map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{code}"}, "standby": "php"}}}},
		{"both shims", "give standby or standby_code, not both", map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{code}"}, "standby": "js", "standby_code": "y"}}}},
		{"blank code", "standby_code must be a program of 1 to 65536 bytes", map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{code}"}, "standby_code": " "}}}},
		{"file mode built-in", `standby needs mode "arg"; a mode "file" runner can use "pipe" or standby_code`, map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x", "{file}"}, "mode": "file", "standby": "python"}}}},
		{"stdin mode", `a runner with mode "stdin" cannot have a standby shim`, map[string]any{"runners": map[string]any{"x": map[string]any{"argv": []any{"x"}, "mode": "stdin", "standby": "python"}}}},
	} {
		_, err := loadSetting(t, nativeTools(map[string]any{"exec": bad.exec}))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: error = %v, want %q", bad.name, err, bad.want)
		}
	}
}

func TestExecRunnersAddPHPLuaDenoAndR(t *testing.T) {
	config, err := loadSetting(t, nativeTools(map[string]any{"exec": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	exec := config.NativeExec
	if got := strings.Join(exec.LangNames, " "); got != "python node bash sh ruby perl ts php lua deno r" {
		t.Fatalf("runner order = %s", got)
	}
	for name, want := range map[string]struct{ argv, mode, ext, standby string }{
		"perl": {"perl -MJadFn -e {code}", NativeExecModeArg, ".pl", ""},
		"php":  {"php -d auto_prepend_file={template} {file}", NativeExecModeFile, ".php", NativeExecStandbyPipe},
		"lua":  {"lua -e dofile[[{template}]] -e {code}", NativeExecModeArg, ".lua", ""},
		"deno": {"deno run --allow-all --preload {template} {file}", NativeExecModeFile, ".ts", NativeExecStandbyPipe},
		"r":    {"Rscript {file}", NativeExecModeFile, ".R", NativeExecStandbyPipe},
	} {
		lang := exec.Langs[name]
		if got := strings.Join(lang.Argv, " "); got != want.argv || lang.Mode != want.mode || lang.Ext != want.ext || lang.Standby != want.standby {
			t.Errorf("%s = %q mode %s ext %s standby %q", name, got, lang.Mode, lang.Ext, lang.Standby)
		}
	}
	if exec.Langs["r"].Aliases[0] != "rscript" {
		t.Errorf("r aliases = %v", exec.Langs["r"].Aliases)
	}
}
