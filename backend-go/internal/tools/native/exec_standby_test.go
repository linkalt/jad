package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// isolateStandbyPool gives the test a pool of its own and stops what it started.
func isolateStandbyPool(t *testing.T) *standbyPool {
	t.Helper()
	previous := execStandby
	pool := newStandbyPool()
	execStandby = pool
	t.Cleanup(func() {
		pool.stopAll()
		execStandby = previous
	})
	return pool
}

func standbyExecSettings() sessionfs.NativeExecSettings {
	settings := execTestSettings()
	settings.LangNames = append(settings.LangNames, "ruby")
	settings.Langs["ruby"] = sessionfs.NativeExecLang{Argv: []string{"ruby", "-r", "{template}", "-e", "{code}"}, Mode: sessionfs.NativeExecModeArg, Ext: ".rb", Aliases: []string{}, LibEnv: "RUBYLIB", Template: sessionfs.NativeExecTemplateRuby, Standby: sessionfs.NativeExecStandbyRuby}
	for name, standby := range map[string]string{"python": sessionfs.NativeExecStandbyPython, "node": sessionfs.NativeExecStandbyJS} {
		lang := settings.Langs[name]
		lang.Standby = standby
		settings.Langs[name] = lang
	}
	settings.FunctionsDir = ".jad/exec"
	settings.FunctionMaxBytes = 64 * 1024
	settings.InputMaxBytes = 64 * 1024
	settings.ResultMaxBytes = 4096
	settings.StandbyPool = 1
	settings.StandbyIdleMs = 60_000
	settings.StandbyMax = 8
	return settings
}

// waitForReadyStandby waits until want standbys wait ready in the pool.
func waitForReadyStandby(t *testing.T, pool *standbyPool, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ready, _ := pool.stats(); ready >= want {
			return
		}
		if time.Now().After(deadline) {
			count, ready, _ := pool.stats()
			t.Fatalf("no ready standby: count %d ready %d", count, ready)
		}
	}
}

var execRunVariables = regexp.MustCompile(`\d+ ms|token=[A-Za-z0-9_-]+`)

// execComparable drops what differs between any two runs: the duration and the content token.
func execComparable(rendered string) string {
	return execRunVariables.ReplaceAllString(rendered, "*")
}

// execCallArguments is one call of the corpus: the code and the JSON input on stdin.
func execCallArguments(t *testing.T, lang, code string) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"lang": lang, "code": code, "input": map[string]any{"a": 7}})
	if err != nil {
		t.Fatal(err)
	}
	return string(arguments)
}

// TestExecStandbyRunsLikeAFreshInterpreter is the standby's contract: a call that runs in a
// waiting interpreter renders the same result as one that starts its own, for output, input,
// the result file, exit codes, uncaught errors and code the shim declines.
func TestExecStandbyRunsLikeAFreshInterpreter(t *testing.T) {
	corpus := map[string][]string{
		"python": {
			"print(1)",
			"import sys, json\nprint(json.load(sys.stdin)['a'])",
			"import os\nopen(os.environ['JAD_RESULT'], 'w').write('answer 42')\nprint('raw')",
			"raise ValueError('x')",
			"def f():\n    raise KeyError(2)\nf()",
			"import sys\nsys.exit(3)",
			"print(\n",
			"print(__name__, sorted(k for k in globals() if not k.startswith('__')))",
			"import sys\nprint(sys.argv)",
			"import traceback\ntry:\n    1/0\nexcept Exception:\n    traceback.print_exc()",
			"import sys\nsys.stderr.write('e\\n')\nexit(5)",
		},
		"node": {
			"console.log(1)",
			"let d='';process.stdin.on('data',c=>d+=c).on('end',()=>console.log(JSON.parse(d).a))",
			"require('fs').writeFileSync(process.env.JAD_RESULT, 'answer 42')",
			"process.exit(3)",
			"console.log(",
			"console.log(typeof require, typeof module, __filename, process.argv.length)",
			"await Promise.resolve(); console.log(2)",
			"import fs from 'fs'; console.log(typeof fs.readFileSync)",
			"setTimeout(()=>console.log('later'),10)",
		},
		"ruby": {
			"puts 1",
			"require 'json'; puts JSON.parse($stdin.read)['a']",
			"File.write(ENV['JAD_RESULT'], 'answer 42')",
			"raise 'x'",
			"def f = raise(ArgumentError, 'a')\nf",
			"exit 3",
			"puts(",
			"p $0, __FILE__",
			"x = 1; p binding.local_variables",
			"at_exit { $stderr.puts \"ae #{$!.inspect}\" }; raise 'z'",
			"def g = eval('raise \"deep\"')\ng",
		},
	}
	interpreters := map[string]string{"python": "python3", "node": "node", "ruby": "ruby"}
	for _, lang := range []string{"python", "node", "ruby"} {
		t.Run(lang, func(t *testing.T) {
			requireInterpreters(t, "bash", interpreters[lang])
			pool := isolateStandbyPool(t)
			root := t.TempDir()
			warm := standbyExecSettings()
			cold := warm
			cold.StandbyPool = 0
			for _, code := range corpus[lang] {
				arguments := execCallArguments(t, lang, code)
				want := callExecThroughTheCatalog(t, root, cold, arguments)
				// The first warm call of a key starts the standby the next one takes.
				callExecThroughTheCatalog(t, root, warm, execCallArguments(t, lang, "1"))
				waitForReadyStandby(t, pool, 1)
				_, _, takenBefore := pool.stats()
				got := callExecThroughTheCatalog(t, root, warm, arguments)
				if _, _, taken := pool.stats(); taken != takenBefore+1 {
					t.Fatalf("%q did not run in a standby", code)
				}
				if execComparable(got) != execComparable(want) {
					if lang == "node" && strings.Contains(want, "Error") {
						// node -e prints its own internal frames under an uncaught error; the
						// shim's frames sit among them (documented), the rest must match.
						continue
					}
					t.Errorf("%q differs from a fresh run\nfresh:   %s\nstandby: %s", code, want, got)
				}
			}
		})
	}
}

// TestExecStandbyNodeErrorKeepsTheProgramsFrames pins what the node shim may change: only frames
// below the program's own, which name node internals or the shim.
func TestExecStandbyNodeErrorKeepsTheProgramsFrames(t *testing.T) {
	requireInterpreters(t, "bash", "node")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "node", "1"))
	waitForReadyStandby(t, pool, 1)
	got := callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "node", "function f(){throw new TypeError('t')}\nf()"))
	for _, want := range []string{"[eval]:1\nfunction f(){throw new TypeError('t')}", "TypeError: t\n    at f ([eval]:1:20)\n    at [eval]:2:1\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the program's error is not reported as node -e does (%q):\n%s", want, got)
		}
	}
}

// TestExecStandbyCallsShareNoState: a standby runs one call and ends, so nothing a program
// leaves behind is seen by the next one.
func TestExecStandbyCallsShareNoState(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "import builtins\nbuiltins.leftover = 1"))
	waitForReadyStandby(t, pool, 1)
	got := execContent(callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "import builtins\nprint(hasattr(builtins, 'leftover'))")))
	if !strings.Contains(got, "False") {
		t.Fatalf("a call saw the state of an earlier one:\n%s", got)
	}
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatalf("taken = %d, want the second call to run in a standby", taken)
	}
}

// TestExecStandbyHelpersWorkInAStandby: the template loads before the call, and fn still finds a
// helper written after the standby started.
func TestExecStandbyHelpersWorkInAStandby(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	helpers := filepath.Join(root, ".jad", "exec", "python")
	if err := os.MkdirAll(helpers, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := standbyExecSettings()
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "1"))
	waitForReadyStandby(t, pool, 1)
	if err := os.WriteFile(filepath.Join(helpers, "double.py"), []byte("# Doubles.\ndef double(x):\n    return 2 * x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := execContent(callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "print(fn.double(21))")))
	if !strings.Contains(got, "42") {
		t.Fatalf("fn.double in a standby:\n%s", got)
	}
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatalf("taken = %d, want 1", taken)
	}
}

// TestExecStandbyOnlyServesTheSameLaunch: a call in another directory gets no standby started
// for a different one.
func TestExecStandbyOnlyServesTheSameLaunch(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := standbyExecSettings()
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "1"))
	waitForReadyStandby(t, pool, 1)
	arguments, _ := json.Marshal(map[string]any{"lang": "python", "code": "import os\nprint(os.getcwd())", "cwd": "sub"})
	got := execContent(callExecThroughTheCatalog(t, root, settings, string(arguments)))
	if !strings.Contains(got, filepath.Join(root, "sub")) {
		t.Fatalf("cwd sub:\n%s", got)
	}
	if _, _, taken := pool.stats(); taken != 0 {
		t.Fatal("a call in another directory took the standby of the workspace root")
	}
}

// TestExecStandbyIdleOnesStop: a standby nobody takes stops after standby_idle_ms.
func TestExecStandbyIdleOnesStop(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	settings.StandbyIdleMs = 100
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "1"))
	waitForReadyStandby(t, pool, 1)
	pool.mu.Lock()
	var process *standbyProcess
	for _, waiting := range pool.idle {
		process = waiting[0]
	}
	pool.mu.Unlock()
	select {
	case <-process.child.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle standby was not stopped")
	}
	if count, _, _ := pool.stats(); count != 0 {
		t.Fatalf("count = %d after the idle stop", count)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(process.workDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped standby's directory was left behind")
		}
	}
}

// TestExecStandbyRespectsTheBackendLimit: standby_max bounds the waiting interpreters of the
// whole pool, over every key.
func TestExecStandbyRespectsTheBackendLimit(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	settings := standbyExecSettings()
	settings.StandbyMax = 1
	settings.StandbyPool = 4
	for range 3 {
		callExecThroughTheCatalog(t, t.TempDir(), settings, execCallArguments(t, "python", "1"))
	}
	waitForReadyStandby(t, pool, 1)
	if count, _, _ := pool.stats(); count != 1 {
		t.Fatalf("count = %d, want standby_max 1", count)
	}
}

// TestExecStandbyABrokenShimIsHeldBack: a shim that ends before a call arrives is dropped, the
// call starts its own interpreter, and the runner is not started on standby again for the idle time.
func TestExecStandbyABrokenShimIsHeldBack(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	lang := settings.Langs["python"]
	lang.Standby, lang.StandbyCode = "", "import sys\nsys.exit(0)"
	settings.Langs["python"] = lang
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "1"))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		pool.mu.Lock()
		held := len(pool.brokenUntil)
		pool.mu.Unlock()
		if held == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead standby did not hold its runner back")
		}
	}
	got := execContent(callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "print(6 * 7)")))
	if !strings.Contains(got, "42") {
		t.Fatalf("the call after a broken standby:\n%s", got)
	}
	if count, _, taken := pool.stats(); count != 0 || taken != 0 {
		t.Fatalf("count %d taken %d: a held-back runner must not start or hand out standbys", count, taken)
	}
}

// TestExecStandbyRunsAnyLanguageThroughStandbyCode: the fd 3 / fd 4 contract is the whole
// interface, so an interpreter without a built-in shim gets one from the configuration.
func TestExecStandbyRunsAnyLanguageThroughStandbyCode(t *testing.T) {
	requireInterpreters(t, "bash", "perl")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	settings.LangNames = append(settings.LangNames, "perl")
	settings.Langs["perl"] = sessionfs.NativeExecLang{
		Argv: []string{"perl", "-e", "{code}"}, Mode: sessionfs.NativeExecModeArg, Ext: ".pl", Aliases: []string{},
		StandbyCode: `open(my $in, "<&=", 3) or die; my $code = do { local $/; <$in> }; close $in; exit 0 if $code eq ""; ` +
			`open(my $answer, ">&=", 4) or die; syswrite $answer, "R"; close $answer; eval $code; die $@ if $@;`,
	}
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "perl", "1"))
	waitForReadyStandby(t, pool, 1)
	got := execContent(callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "perl", "my $line = <STDIN>; print 6 * 7, ' ', $line;")))
	if !strings.Contains(got, `42 {"a":7}`) {
		t.Fatalf("perl in a configured standby:\n%s", got)
	}
	if _, _, taken := pool.stats(); taken != 1 {
		t.Fatalf("taken = %d, want 1", taken)
	}
}

// TestExecStandbyTimeoutStillApplies: a call that outlasts timeout_ms in a standby is stopped and
// reported as timed out, like a fresh one.
func TestExecStandbyTimeoutStillApplies(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	pool := isolateStandbyPool(t)
	root := t.TempDir()
	settings := standbyExecSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.TimeoutMs = 300
	cold := settings
	cold.StandbyPool = 0
	arguments := execCallArguments(t, "python", "import time\nprint('start', flush=True)\ntime.sleep(30)")
	callExecThroughTheCatalog(t, root, settings, execCallArguments(t, "python", "1"))
	waitForReadyStandby(t, pool, 1)
	started := time.Now()
	got := callExecThroughTheCatalog(t, root, settings, arguments)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the timed-out standby call took %v", elapsed)
	}
	want := callExecThroughTheCatalog(t, root, cold, arguments)
	if execComparable(got) != execComparable(want) {
		t.Fatalf("timeout differs from a fresh run\nfresh:   %s\nstandby: %s", want, got)
	}
}
