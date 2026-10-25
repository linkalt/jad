package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// execHelpersFixture writes files (keyed "runner/name") under <root>/.jad/exec. A name that
// ends in .sh or has no extension is made executable.
func execHelpersFixture(t *testing.T, files map[string]string) (string, sessionfs.NativeExecSettings) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, ".jad", "exec")
	for name, content := range files {
		path := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if ext := filepath.Ext(name); ext == ".sh" || ext == "" {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec"
	settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes, settings.FunctionMaxBytes = 50, 200, 2048, 4096
	return root, settings
}

func TestExecBashCodeCallsABashHelperAndHelpersChain(t *testing.T) {
	requireInterpreters(t, "bash")
	root, settings := execHelpersFixture(t, map[string]string{
		"bash/inner.sh": "#!/bin/sh\n# Wrap a word\necho \"[$1]\"\n",
		"bash/outer.sh": "#!/bin/bash\n# Call inner twice\necho \"$(inner.sh \"$1\")$(inner.sh again)\"\n",
	})
	got := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "outer.sh hi"}))
	if got != "[hi][again]\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestExecHelpersChainAcrossLanguagesThroughJadFunctionsDir(t *testing.T) {
	requireInterpreters(t, "bash", "python3", "node")
	root, settings := execHelpersFixture(t, map[string]string{
		"python/double.py": "# Double the number on stdin\nimport sys\nprint(int(sys.stdin.read()) * 2)\n",
		"bash/twice.sh":    "#!/bin/bash\n# Double through the python helper, then again\nn=$(echo \"$1\" | python3 \"$JAD_FUNCTIONS_DIR/python/double.py\")\necho \"$n\" | python3 \"$JAD_FUNCTIONS_DIR/python/double.py\"\n",
		"python/viapy.py":  "# Import another python helper\nimport subprocess, os\n\ndef run(n):\n    return subprocess.run([os.environ['JAD_FUNCTIONS_DIR'] + '/bash/twice.sh', str(n)], capture_output=True, text=True).stdout.strip()\n",
		"node/addone.js":   "// Add one, through python\nconst { execFileSync } = require('child_process');\nexports.run = (n) => execFileSync('python3', ['-c', 'import sys; print(int(sys.argv[1]) + 1)', String(n)]).toString().trim();\n",
	})
	bash := bashTestSettings(t, root)
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "twice.sh 5"})); got != "20\n" {
		t.Fatalf("bash -> bash -> python = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "import viapy\nprint(viapy.run(3))"})); got != "12\n" {
		t.Fatalf("python import -> bash -> python = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "console.log(require('addone.js').run(4))"})); got != "5\n" {
		t.Fatalf("node require -> python = %q", got)
	}
}

func TestExecModuleHelpersChainWithinTheirLanguage(t *testing.T) {
	requireInterpreters(t, "python3", "node")
	root, settings := execHelpersFixture(t, map[string]string{
		"python/base.py": "# Base\ndef plus(n): return n + 1\n",
		"python/top.py":  "# Top\nimport base\n\ndef plus2(n): return base.plus(base.plus(n))\n",
		"node/base.js":   "// Base\nexports.plus = (n) => n + 1;\n",
		"node/top.js":    "// Top\nconst base = require('base.js');\nexports.plus2 = (n) => base.plus(base.plus(n));\n",
	})
	bash := bashTestSettings(t, root)
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "import top\nprint(top.plus2(1))"})); got != "3\n" {
		t.Fatalf("python = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "console.log(require('top.js').plus2(1))"})); got != "3\n" {
		t.Fatalf("node = %q", got)
	}
}

func TestExecHelperDirectoryIsPrependedAndKeepsTheExistingValue(t *testing.T) {
	requireInterpreters(t, "bash", "python3")
	root, settings := execHelpersFixture(t, map[string]string{"bash/x.sh": "# x\n", "python/y.py": "# y\n"})
	bash := bashTestSettings(t, root)
	bash.Env = map[string]string{"PYTHONPATH": "/keep/me"}
	real, _ := filepath.EvalSymlinks(root)
	lib := filepath.Join(real, ".jad", "exec")
	got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "echo \"$JAD_FUNCTIONS_DIR\"; echo \"$PATH\" | cut -d: -f1; echo \"$PYTHONPATH\""}))
	if want := lib + "\n" + filepath.Join(lib, "bash") + "\n/keep/me\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "import os; print(os.environ['PYTHONPATH'])"}))
	// The per-call template directory comes first, then the helper folder, then the user's value.
	parts := strings.Split(strings.TrimSpace(got), string(os.PathListSeparator))
	if len(parts) != 3 || filepath.Base(parts[0]) != "template" || parts[1] != filepath.Join(lib, "python") || parts[2] != "/keep/me" {
		t.Fatalf("python path = %q", got)
	}
}

func TestExecWithoutALibraryOrARunnerFolderSetsNothing(t *testing.T) {
	requireInterpreters(t, "bash")
	root := t.TempDir()
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec"
	bash := bashTestSettings(t, root)
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "bash", "code": "echo \"[$JAD_FUNCTIONS_DIR]\""})); got != "[]\n" {
		t.Fatalf("no library: %q", got)
	}
	root, settings = execHelpersFixture(t, map[string]string{"python/y.py": "# y\n"})
	got := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "echo \"$PATH\" | tr ':' '\\n' | grep -c exec$ || true"}))
	if strings.TrimSpace(got) != "0" {
		t.Fatalf("a runner without a folder must not get one on PATH: %q", got)
	}
}

func TestExecHelperFolderThatLeavesTheLibraryIsNotExposed(t *testing.T) {
	requireInterpreters(t, "bash")
	root, settings := execHelpersFixture(t, map[string]string{"python/y.py": "# y\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil.sh"), []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".jad", "exec", "bash")); err != nil {
		t.Skip("symlinks are not available: ", err)
	}
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "evil.sh"})
	if strings.Contains(result, "pwned") {
		t.Fatalf("a runner folder that is a symlink out of the library was put on PATH:\n%s", result)
	}
}

func TestExecListsHelpersPerLanguageAndFiltersNarrowThem(t *testing.T) {
	root, settings := execHelpersFixture(t, map[string]string{
		"python/read_json.py": "#!/usr/bin/env python3\n# Read a JSON file and print its keys\n# input: {path: string}\nimport sys\n",
		"bash/script1.sh":     "#!/bin/sh\n# Summarise the README\necho x\n",
		"bash/build":          "#!/bin/sh\n# Build the project\n",
		"bash/nodoc.sh":       "echo no description\n",
		"bash/.hidden.sh":     "# hidden\n",
		"bash/Bad Name.sh":    "# nope\n",
		"node/read.js":        "// Read a file\n",
		"ghost/lost.sh":       "# folder without a runner\n",
		"loose.sh":            "# a file outside any runner folder\n",
	})
	bash := bashTestSettings(t, root)
	text := execContent(runExec(t, root, settings, bash, map[string]any{}))
	for _, want := range []string{
		"call as: fn.<name>(...) in ts/node/python/ruby",
		"python:\n  fn.read_json(...)   read_json.py — Read a JSON file and print its keys (input: {path: string})",
		"bash:\n  build — Build the project\n  script1.sh — Summarise the README",
		"node:\n  fn.read(...)   read.js — Read a file",
		"malformed entries (",
		"bash/nodoc.sh: missing a leading comment",
		"bash/Bad Name.sh: name must match",
		"ghost: not a runner name",
		"loose.sh: not a runner name",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hidden") {
		t.Errorf("a dotfile was listed:\n%s", text)
	}
	if strings.Index(text, "python:") > strings.Index(text, "node:") || strings.Index(text, "node:") > strings.Index(text, "bash:") {
		t.Errorf("languages must follow the runner order:\n%s", text)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"read*"}})); strings.Contains(got, "script1") || !strings.Contains(got, "read_json.py") || !strings.Contains(got, "read.js") {
		t.Errorf("wildcard filter = %s", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"README"}})); !strings.Contains(got, "script1.sh") || strings.Contains(got, "read_json") {
		t.Errorf("substring filter on the description, any case = %s", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"read*", "*keys*"}})); strings.Contains(got, "read.js") || !strings.Contains(got, "read_json.py") {
		t.Errorf("every filter must match = %s", got)
	}
}

func TestExecListWithoutALibraryOrRunnerFoldersIsEmpty(t *testing.T) {
	root := t.TempDir()
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec"
	settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes, settings.FunctionMaxBytes = 50, 200, 2048, 4096
	bash := bashTestSettings(t, root)
	got := runExec(t, root, settings, bash, map[string]any{})
	if !strings.Contains(got, "0 helpers") || strings.Contains(got, "list_failed") {
		t.Fatalf("no library folder:\n%s", got)
	}
	root, settings = execHelpersFixture(t, map[string]string{"node/read.js": "// Read a file\n"})
	got = runExec(t, root, settings, bashTestSettings(t, root), map[string]any{})
	if !strings.Contains(got, "1 helper") || !strings.Contains(got, "read.js") || strings.Contains(got, "list_failed") {
		t.Fatalf("only some runner folders:\n%s", got)
	}
}

func TestExecListingCutsAtTheConfiguredLimitsAndSaysSo(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		files["bash/"+name+".sh"] = "# helper " + name + "\n"
	}
	root, settings := execHelpersFixture(t, files)
	settings.ListMaxResults = 2
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{})
	if execField(result, "truncated") != "true" || execField(result, "shown") != "2 of 3" {
		t.Fatalf("result = %s", result)
	}
	settings.ListMaxResults, settings.ListMaxFiles = 50, 2
	result = runExec(t, root, settings, bashTestSettings(t, root), map[string]any{})
	if execField(result, "truncated") != "true" || !strings.Contains(execField(result, "shown"), "scanned 2 of 3 files") {
		t.Fatalf("file cut = %s", result)
	}
}

func TestExecListingRefusesAHelperThatIsASymlinkOutOfTheLibrary(t *testing.T) {
	root, settings := execHelpersFixture(t, map[string]string{"bash/ok.sh": "# fine\n"})
	outside := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(outside, []byte("# evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".jad", "exec", "bash", "evil.sh")); err != nil {
		t.Skip("symlinks are not available: ", err)
	}
	list := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{}))
	if strings.Contains(list, "evil.sh —") || !strings.Contains(list, "bash/evil.sh: symlink leaves the functions directory") {
		t.Fatalf("listing = %s", list)
	}
}

func TestExecListingOfATooLargeHelperIsMalformed(t *testing.T) {
	root, settings := execHelpersFixture(t, map[string]string{"bash/big.sh": "# Big\n" + strings.Repeat("x", 5000)})
	if list := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{})); !strings.Contains(list, "over function_max_bytes") {
		t.Fatalf("listing = %s", list)
	}
}

func TestExecHelperCallsRunThroughAHandleAndStopKillsTheHelper(t *testing.T) {
	f := newExecAsyncFixture(t)
	helperDir := filepath.Join(f.root, ".jad", "exec", "bash")
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, "helper-alive")
	script := "#!/bin/bash\n# Slow helper\necho $$ > " + marker + "\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(helperDir, "slow.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := execTestSettings()
	settings.FunctionsDir, settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes, settings.FunctionMaxBytes = ".jad/exec", 50, 200, 2048, 4096
	settings.Child = bashTestSettings(t, f.root)
	settings.Child.TimeoutMs, settings.Child.MaxTimeoutMs = 20_000, 30_000
	tool, err := NewExecTool(settings, "", f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.exec = tool
	id := execRunIDOf(t, f.run(t, "s1", `{"code":"slow.sh","block_ms":0}`))
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(marker); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	if pid == 0 {
		t.Fatal("the helper never started")
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
	f.waitState(t, "s1", id, "stopped")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the helper process %d survived the stop", pid)
}

func TestExecAcceptsNoFunctionArgvArgsOrTimeoutInTheSchema(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(execInputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"function", "argv", "args", "timeout_ms"} {
		if _, has := schema.Properties[key]; has {
			t.Errorf("the schema still has %s", key)
		}
	}
}

func TestExecTsCodeImportsTsHelpersThatChainAndCallBash(t *testing.T) {
	requireInterpreters(t, "bun", "bash")
	root, settings := execHelpersFixture(t, map[string]string{
		"bash/shout.sh": "#!/bin/sh\n# Upper-case the word\necho \"$1\" | tr a-z A-Z\n",
		"ts/base.ts":    "// Add one\nexport const plus = (n: number): number => n + 1;\n",
		"ts/top.ts":     "// Add two through base\nimport { plus } from \"base\";\nexport const plus2 = (n: number): number => plus(plus(n));\n",
		"ts/viabash.ts": "// Shout through the bash helper\nimport { execFileSync } from \"node:child_process\";\nexport const shout = (w: string): string => execFileSync(process.env.JAD_FUNCTIONS_DIR + \"/bash/shout.sh\", [w]).toString().trim();\n",
	})
	bash := bashTestSettings(t, root)
	code := "import { plus2 } from \"top\";\nimport { shout } from \"viabash\";\nconst n: number = (await Bun.stdin.json()).n;\nconsole.log(plus2(n), shout(\"hi\"));"
	got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "ts", "code": code, "input": map[string]any{"n": 5}}))
	if got != "7 HI\n" {
		t.Fatalf("ts import chain = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "typescript", "code": "import { plus } from \"base.ts\"; console.log(plus(1))"})); got != "2\n" {
		t.Fatalf("ts alias with extension = %q", got)
	}
}

func TestExecHelperCallExpressionPerRunnerAndName(t *testing.T) {
	js := sessionfs.NativeExecLang{Ext: ".ts", Template: sessionfs.NativeExecTemplateJS}
	py := sessionfs.NativeExecLang{Ext: ".py", Template: sessionfs.NativeExecTemplatePython}
	rb := sessionfs.NativeExecLang{Ext: ".rb", Template: sessionfs.NativeExecTemplateRuby}
	sh := sessionfs.NativeExecLang{Ext: ".sh", LibEnv: "PATH"}
	for _, tc := range []struct {
		file string
		lang sessionfs.NativeExecLang
		want string
	}{
		{"read.ts", js, "fn.read(...)   read.ts"},
		{"my-helper.ts", js, `fn["my-helper"](...)   my-helper.ts`},
		{"2fast.ts", js, `fn["2fast"](...)   2fast.ts`},
		{"grep.py", py, "fn.grep(...)   grep.py"},
		{"my-helper.py", py, `getattr(fn, "my-helper")(...)   my-helper.py`},
		{"my_helper.rb", rb, "fn.my_helper(...)   my_helper.rb"},
		{"my-helper.rb", rb, `fn.public_send("my-helper", ...)   my-helper.rb`},
		{"script1.sh", sh, "script1.sh"},
	} {
		if got := execHelperCall(tc.file, tc.lang); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestExecListsTsHelpers(t *testing.T) {
	root, settings := execHelpersFixture(t, map[string]string{"ts/read.ts": "// Read a thing\nexport {};\n"})
	text := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{}))
	if !strings.Contains(text, "ts:\n  fn.read(...)   read.ts — Read a thing") {
		t.Fatalf("listing = %q", text)
	}
}
