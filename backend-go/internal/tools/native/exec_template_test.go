package native

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// realRuby is the ruby binary behind a version-manager shim, which cannot start in the exec child's
// trimmed environment; it is plain "ruby" when there is no shim to see through.
func realRuby() string {
	path, err := osexec.LookPath("ruby")
	if err != nil {
		return "ruby"
	}
	out, err := osexec.Command(path, "-e", "print RbConfig.ruby").Output()
	if err != nil || len(out) == 0 {
		return "ruby"
	}
	return string(out)
}

// requireRunnableInterpreter skips when the interpreter is missing or cannot start in the exec child's
// environment (a version-manager shim that needs variables the child does not get).
func requireRunnableInterpreter(t *testing.T, lang, interp string) {
	t.Helper()
	requireInterpreters(t, interp)
	root, settings := execTemplateSettings(nil)
	defer os.RemoveAll(root)
	if interp == "ruby" {
		interp = realRuby()
	}
	settings.Langs["plain"] = sessionfs.NativeExecLang{Argv: []string{interp, "-e", "{code}"}, Mode: sessionfs.NativeExecModeArg, Aliases: []string{}}
	settings.LangNames = append(settings.LangNames, "plain")
	if interp == "python3" {
		settings.Langs["plain"] = sessionfs.NativeExecLang{Argv: []string{interp, "-c", "{code}"}, Mode: sessionfs.NativeExecModeArg, Aliases: []string{}}
	}
	if got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "plain", "code": "1"}); !strings.Contains(got, `"status":"ok"`) {
		t.Skipf("%s (%s) cannot start in the exec environment", lang, interp)
	}
}

func execTemplateSettings(files map[string]string) (string, sessionfs.NativeExecSettings) {
	root, settings := execHelpersFixtureFor(files)
	settings.LangNames = append(append([]string{}, settings.LangNames...), "ruby")
	langs := map[string]sessionfs.NativeExecLang{}
	for name, lang := range settings.Langs {
		langs[name] = lang
	}
	langs["ruby"] = sessionfs.NativeExecLang{Argv: []string{realRuby(), "-r", "{template}", "-e", "{code}"}, Mode: sessionfs.NativeExecModeArg, Ext: ".rb", Aliases: []string{"rb"}, LibEnv: "RUBYLIB", Template: sessionfs.NativeExecTemplateRuby}
	settings.Langs = langs
	return root, settings
}

// execHelpersFixtureFor is execHelpersFixture without the testing.T, for table builders.
func execHelpersFixtureFor(files map[string]string) (string, sessionfs.NativeExecSettings) {
	root, err := os.MkdirTemp("", "exec-template-test-")
	if err != nil {
		panic(err)
	}
	base := filepath.Join(root, ".jad", "exec")
	for name, content := range files {
		path := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		mode := os.FileMode(0o644)
		if ext := filepath.Ext(name); ext == ".sh" || ext == "" {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			panic(err)
		}
	}
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec"
	settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes, settings.FunctionMaxBytes = 50, 200, 2048, 4096
	return root, settings
}

type execFnCase struct {
	lang, interp string
	files        map[string]string
	// code calls a helper, a helper that chains through fn, an async helper, and one created while it runs.
	code string
	want string
	// missing is code that reads a helper that does not exist.
	missing string
}

var execFnCases = []execFnCase{
	{
		lang: "ts", interp: "bun",
		files: map[string]string{
			"ts/base.ts": "// Add\nexport const plus = (a: number, b: number): number => a + b;\n",
			"ts/top.ts":  "// Chain through fn\nexport const plus3 = (n: number): number => (globalThis as any).fn.base.plus(n, 3);\nexport const later = async (n: number): Promise<number> => { await 0; return n * 2; };\n",
		},
		code: "import { writeFileSync } from \"node:fs\";\n" +
			"const n: number = (await Bun.stdin.json()).n;\n" +
			"const a = fn.base.plus(n, 1);\n" +
			"const b = fn.top.plus3(n);\n" +
			"const c = await fn.top.later(n);\n" +
			"writeFileSync(process.env.JAD_FUNCTIONS_DIR + \"/ts/late.ts\", \"export const v = () => 42;\");\n" +
			"console.log(a, b, c, fn.late.v());",
		want:    "6 8 10 42\n",
		missing: "fn.nope.x()",
	},
	{
		lang: "node", interp: "node",
		files: map[string]string{
			"node/base.js": "// Add\nexports.plus = (a, b) => a + b;\n",
			"node/top.js":  "// Chain through fn\nexports.plus3 = (n) => fn.base.plus(n, 3);\nexports.later = async (n) => { await 0; return n * 2; };\n",
		},
		code: "const fs = require('fs');\n" +
			"const n = JSON.parse(fs.readFileSync(0, 'utf8')).n;\n" +
			"(async () => {\n" +
			"  const a = fn.base.plus(n, 1), b = fn.top.plus3(n), c = await fn.top.later(n);\n" +
			"  fs.writeFileSync(process.env.JAD_FUNCTIONS_DIR + '/node/late.js', 'exports.v = () => 42');\n" +
			"  console.log(a, b, c, fn.late.v());\n" +
			"})();",
		want:    "6 8 10 42\n",
		missing: "fn.nope.x()",
	},
	{
		lang: "python", interp: "python3",
		files: map[string]string{
			"python/base.py": "# Add\ndef plus(a, b): return a + b\n",
			"python/top.py":  "# Chain through fn\nasync def later(n):\n    return n * 2\n\ndef plus3(n): return fn.base.plus(n, 3)\n",
		},
		code: "import json, os, sys, asyncio\n" +
			"n = json.load(sys.stdin)['n']\n" +
			"a, b, c = fn.base.plus(n, 1), fn.top.plus3(n), asyncio.run(fn.top.later(n))\n" +
			"open(os.environ['JAD_FUNCTIONS_DIR'] + '/python/late.py', 'w').write('def v(): return 42\\n')\n" +
			"print(a, b, c, fn.late.v())",
		want:    "6 8 10 42\n",
		missing: "fn.nope.x()",
	},
	{
		lang: "ruby", interp: "ruby",
		files: map[string]string{
			"ruby/base.rb": "# Add\nmodule Base; def self.plus(a, b) = a + b; end\n",
			"ruby/top.rb":  "# Chain through fn\nmodule Top; def self.plus3(n) = fn.base.plus(n, 3); end\n",
		},
		code: "require 'json'\n" +
			"n = JSON.parse($stdin.read)['n']\n" +
			"File.write(ENV['JAD_FUNCTIONS_DIR'] + '/ruby/late.rb', 'module Late; def self.v = 42; end')\n" +
			"puts [fn.base.plus(n, 1), fn.top.plus3(n), fn.late.v].join(' ')",
		want:    "6 8 42\n",
		missing: "fn.nope.x",
	},
}

func TestExecFnCallsHelpersChainsAndSeesOnesCreatedWhileRunning(t *testing.T) {
	for _, tc := range execFnCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireRunnableInterpreter(t, tc.lang, tc.interp)
			root, settings := execTemplateSettings(tc.files)
			defer os.RemoveAll(root)
			got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": tc.lang, "code": tc.code, "input": map[string]any{"n": 5}})
			if execContent(got) != tc.want {
				t.Fatalf("got %q, want %q\n%s", execContent(got), tc.want, got)
			}
		})
	}
}

type execCallableCase struct {
	lang, interp string
	files        map[string]string
	code, want   string
}

var execCallableCases = []execCallableCase{
	{
		lang: "ts", interp: "bun",
		files: map[string]string{
			"ts/read.ts":    "// Named like the file\nexport const read = (n: number): number => n + 1;\nexport const other = (n: number): number => n * 10;\n",
			"ts/solo.ts":    "// The only function\nexport const whatever = (n: number): number => n + 2;\nexport const VALUE = 7;\n",
			"ts/dflt.ts":    "// Default export\nexport default (n: number): number => n + 3;\nexport const extra = 1;\n",
			"ts/many.ts":    "// Several, none named like the file\nexport const a = (): number => 1;\nexport const b = (): number => 2;\n",
			"ts/asyn.ts":    "// Async entry\nexport default async (n: number): Promise<number> => { await 0; return n * 2; };\n",
			"ts/chain.ts":   "// Chains through fn\nexport default (n: number): number => (globalThis as any).fn.read(n) + (globalThis as any).fn.solo(0);\n",
			"ts/my-help.ts": "// Dashed\nexport const myHelp = (n: number): number => n + 100;\n",
		},
		code: "const n: number = (await Bun.stdin.json()).n;\n" +
			"console.log(fn.read(n), fn.read.other(n), fn.solo(n), fn.solo.VALUE, fn.dflt(n), fn.dflt.extra, await fn.asyn(n), fn.chain(n), fn[\"my-help\"](n));\n" +
			"try { fn.many(); } catch (e) { console.log(String(e.message).split(\"(\")[0] + \"|\" + String(e.message).includes(\"a, b\")); }",
		want: "6 50 7 7 8 1 10 8 105\nfn.many has no single entry to call |true\n",
	},
	{
		lang: "node", interp: "node",
		files: map[string]string{
			"node/read.js":  "// Named like the file\nexports.read = (n) => n + 1;\nexports.other = (n) => n * 10;\n",
			"node/solo.js":  "// The only function\nexports.whatever = (n) => n + 2;\nexports.VALUE = 7;\n",
			"node/dflt.js":  "// Default export\nmodule.exports = (n) => n + 3;\nmodule.exports.extra = 1;\n",
			"node/many.js":  "// Several\nexports.a = () => 1;\nexports.b = () => 2;\n",
			"node/asyn.js":  "// Async entry\nmodule.exports = async (n) => { await 0; return n * 2; };\n",
			"node/chain.js": "// Chains through fn\nmodule.exports = (n) => fn.read(n) + fn.solo(0);\n",
		},
		code: "const n = JSON.parse(require('fs').readFileSync(0, 'utf8')).n;\n" +
			"(async () => {\n" +
			"  console.log(fn.read(n), fn.read.other(n), fn.solo(n), fn.solo.VALUE, fn.dflt(n), fn.dflt.extra, await fn.asyn(n), fn.chain(n));\n" +
			"  try { fn.many(); } catch (e) { console.log(e.message.split('(')[0] + '|' + e.message.includes('a, b')); }\n" +
			"})();",
		want: "6 50 7 7 8 1 10 8\nfn.many has no single entry to call |true\n",
	},
	{
		lang: "python", interp: "python3",
		files: map[string]string{
			"python/read.py":    "# Named like the module\ndef read(n): return n + 1\ndef other(n): return n * 10\n",
			"python/solo.py":    "# The only function\nimport os\nVALUE = 7\ndef whatever(n): return n + 2\n",
			"python/many.py":    "# Several\ndef a(): return 1\ndef b(): return 2\n",
			"python/asyn.py":    "# Async entry\nasync def asyn(n): return n * 2\n",
			"python/chain.py":   "# Chains through fn\ndef chain(n): return fn.read(n) + fn.solo(0)\n",
			"python/my-help.py": "# Dashed\ndef my_help(n): return n + 100\n",
		},
		code: "import json, sys, asyncio\n" +
			"n = json.load(sys.stdin)['n']\n" +
			"print(fn.read(n), fn.read.other(n), fn.solo(n), fn.solo.VALUE, asyncio.run(fn.asyn(n)), fn.chain(n), getattr(fn, 'my-help').my_help(n))\n" +
			"try:\n    fn.many()\nexcept TypeError as e:\n    print(str(e).split('(')[0] + '|' + str('a, b' in str(e)))\n",
		want: "6 50 7 7 10 8 105\nfn.many has no single entry to call |True\n",
	},
	{
		lang: "ruby", interp: "ruby",
		files: map[string]string{
			"ruby/read.rb":  "# Named like the file\nmodule Read; def self.read(n) = n + 1; def self.other(n) = n * 10; end\n",
			"ruby/solo.rb":  "# The only method\nmodule Solo; VALUE = 7; def self.whatever(n) = n + 2; end\n",
			"ruby/dflt.rb":  "# call wins\nmodule Dflt; def self.call(n) = n + 3; def self.extra = 1; end\n",
			"ruby/many.rb":  "# Several\nmodule Many; def self.a = 1; def self.b = 2; end\n",
			"ruby/chain.rb": "# Chains through fn\nmodule Chain; def self.call(n) = fn.read(n) + fn.solo(0); end\n",
		},
		code: "require 'json'\nn = JSON.parse($stdin.read)['n']\n" +
			"puts [fn.read(n), fn.read.other(n), fn.solo(n), fn.solo::VALUE, fn.dflt(n), fn.dflt.extra, fn.chain(n)].join(' ')\n" +
			"begin; fn.many(1); rescue => e; puts e.message.split('(')[0] + '|' + e.message.include?('a, b').to_s; end",
		want: "6 50 7 7 8 1 8\nfn.many has no single entry to call |true\n",
	},
}

func TestExecFnIsCallableThroughItsEntryAndKeepsEveryExportAsAProperty(t *testing.T) {
	for _, tc := range execCallableCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireRunnableInterpreter(t, tc.lang, tc.interp)
			root, settings := execTemplateSettings(tc.files)
			defer os.RemoveAll(root)
			got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": tc.lang, "code": tc.code, "input": map[string]any{"n": 5}})
			if execContent(got) != tc.want {
				t.Fatalf("got %q, want %q\n%s", execContent(got), tc.want, got)
			}
		})
	}
}

func TestExecFnMissingHelperNamesTheHelperAndTheFolder(t *testing.T) {
	for _, tc := range execFnCases {
		t.Run(tc.lang, func(t *testing.T) {
			requireRunnableInterpreter(t, tc.lang, tc.interp)
			root, settings := execTemplateSettings(tc.files)
			defer os.RemoveAll(root)
			got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": tc.lang, "code": tc.missing})
			folder := filepath.Join(root, ".jad", "exec", tc.lang)
			real, _ := filepath.EvalSymlinks(folder)
			if !strings.Contains(got, "no helper fn.nope in "+real) || execField(got, "error_code") != "nonzero_exit" {
				t.Fatalf("missing helper = %s", got)
			}
		})
	}
}

func TestExecFnWithoutALibraryFolderSaysSoAndStillCreatesOne(t *testing.T) {
	requireInterpreters(t, "node", "python3")
	root, settings := execTemplateSettings(map[string]string{"bash/x.sh": "# x\n"})
	defer os.RemoveAll(root)
	bash := bashTestSettings(t, root)
	for lang, code := range map[string]string{"node": "fn.nope", "python": "fn.nope"} {
		got := runExec(t, root, settings, bash, map[string]any{"lang": lang, "code": code})
		if !strings.Contains(got, "no helper fn.nope in ") || !strings.Contains(got, filepath.Join(".jad", "exec", lang)) {
			t.Errorf("%s: %s", lang, got)
		}
	}
	got := runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "require('fs').mkdirSync(process.env.JAD_RUNNER_DIR, {recursive: true}); require('fs').writeFileSync(process.env.JAD_RUNNER_DIR + '/made.js', 'exports.v = 7'); console.log(fn.made.v)"})
	if execContent(got) != "7\n" {
		t.Errorf("helper made in a folder that did not exist: %s", got)
	}
}

func TestExecPythonFnSeesAHelperInAFolderCreatedAfterStart(t *testing.T) {
	requireInterpreters(t, "python3")
	root, settings := execTemplateSettings(map[string]string{"bash/x.sh": "# x\n"})
	defer os.RemoveAll(root)
	code := "import os\nprint(os.path.isdir(os.environ['JAD_RUNNER_DIR']))\n" +
		"try:\n    fn.late\nexcept AttributeError as e:\n    print('first:', e)\n" +
		"os.makedirs(os.environ['JAD_RUNNER_DIR'])\nopen(os.environ['JAD_RUNNER_DIR'] + '/late.py', 'w').write('v = 5\\n')\n" +
		"print(fn.late.v)"
	got := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "python", "code": code}))
	if !strings.HasSuffix(got, "\n5\n") || !strings.HasPrefix(got, "False\nfirst: no helper fn.late in ") {
		t.Fatalf("got %q", got)
	}
}

// TestExecPythonFnSeesAHelperWrittenIntoAListedFolder pins the template's cache reset: Python
// keeps a folder's listing until the folder's mtime changes, so a helper written within the same
// mtime tick (here the mtime is put back) is only found because fn.<name> resets that listing.
func TestExecPythonFnSeesAHelperWrittenIntoAListedFolder(t *testing.T) {
	requireInterpreters(t, "python3")
	root, settings := execTemplateSettings(map[string]string{"python/first.py": "# First\nv = 1\n"})
	defer os.RemoveAll(root)
	// The first import writes __pycache__ into the folder, which changes its mtime; the second
	// lookup lists the folder again, so the listing and the mtime put back below agree.
	code := "import os\nfolder = os.environ['JAD_RUNNER_DIR']\nprint(fn.first.v)\nfn.first\nstat = os.stat(folder)\n" +
		"open(folder + '/second.py', 'w').write('v = 2\\n')\nos.utime(folder, ns=(stat.st_atime_ns, stat.st_mtime_ns))\n" +
		"print(fn.second.v)"
	got := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "python", "code": code}))
	if got != "1\n2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestExecFnLineNumbersOfUserCodeAreUnchanged(t *testing.T) {
	for _, tc := range []struct{ lang, interp, code, want string }{
		{"ts", "bun", "const a = 1;\nconst b = 2;\nthrow new Error('line4');", "[eval]:3"},
		{"node", "node", "const a = 1;\nconst b = 2;\nthrow new Error('line4');", "[eval]:3"},
		{"python", "python3", "a = 1\nb = 2\nraise RuntimeError('x')", "line 3, in <module>"},
		{"ruby", "ruby", "a = 1\nb = 2\nraise 'x'", "-e:3"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			requireRunnableInterpreter(t, tc.lang, tc.interp)
			root, settings := execTemplateSettings(map[string]string{tc.lang + "/x.txt": "# x\n"})
			defer os.RemoveAll(root)
			got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": tc.lang, "code": tc.code})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want %q in:\n%s", tc.want, got)
			}
		})
	}
}

func TestExecFnNamesThatAreNotIdentifiersAreExplained(t *testing.T) {
	requireInterpreters(t, "node", "python3")
	root, settings := execTemplateSettings(map[string]string{
		"node/my-helper.js": "// Dash\nexports.v = 1;\n",
		"python/my_py.py":   "# ok\nv = 2\n",
	})
	defer os.RemoveAll(root)
	bash := bashTestSettings(t, root)
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "console.log(fn['my-helper'].v)"})); got != "1\n" {
		t.Errorf("fn[\"my-helper\"] = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "print(fn.my_py.v, getattr(fn, 'my_py').v)"})); got != "2 2\n" {
		t.Errorf("python = %q", got)
	}
	got := runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "fn['../x']"})
	if !strings.Contains(got, "is not a helper name") {
		t.Errorf("a path-like name must be refused: %s", got)
	}
	got = runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "getattr(fn, '../x')"})
	if !strings.Contains(got, "is not a helper name") {
		t.Errorf("python path-like name: %s", got)
	}
}

func TestExecTemplateFileInTheLibraryReplacesTheBuiltIn(t *testing.T) {
	requireInterpreters(t, "node", "python3", "bun")
	root, settings := execTemplateSettings(map[string]string{
		"node/_template.js":   "globalThis.fn = { who: 'custom-node' };\n",
		"python/_template.py": "import builtins\nbuiltins.fn = type('F', (), {'who': 'custom-python'})\n",
		"ts/_template.ts":     "(globalThis as any).fn = { who: 'custom-ts' };\n",
		"node/real.js":        "// Real\nexports.v = 1;\n",
	})
	defer os.RemoveAll(root)
	bash := bashTestSettings(t, root)
	for lang, code := range map[string]string{"node": "console.log(fn.who)", "python": "print(fn.who)", "ts": "console.log(fn.who)"} {
		want := "custom-" + lang + "\n"
		if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": lang, "code": code})); got != want {
			t.Errorf("%s: %q, want %q", lang, got, want)
		}
	}
	text := execContent(runExec(t, root, settings, bash, map[string]any{}))
	if strings.Contains(text, "_template") || strings.Contains(text, "malformed") || !strings.Contains(text, "real.js") {
		t.Errorf("a template file must not be listed as a helper or a malformed one:\n%s", text)
	}
}

func TestExecTemplateFileThatLeavesTheLibraryIsRefused(t *testing.T) {
	requireInterpreters(t, "node")
	root, settings := execTemplateSettings(map[string]string{"node/real.js": "// Real\n"})
	defer os.RemoveAll(root)
	outside := filepath.Join(t.TempDir(), "evil.js")
	if err := os.WriteFile(outside, []byte("console.log('evil')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".jad", "exec", "node", "_template.js")); err != nil {
		t.Skip("symlinks are not available")
	}
	got := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "node", "code": "console.log(1)"})
	if strings.Contains(got, "evil") || execField(got, "error_code") != "template_failed" {
		t.Fatalf("got %s", got)
	}
}

func TestExecPythonTemplateKeepsUserPythonPathAndChainsAShadowedSitecustomize(t *testing.T) {
	requireInterpreters(t, "python3")
	root, settings := execTemplateSettings(map[string]string{"python/base.py": "# b\nv = 3\n"})
	defer os.RemoveAll(root)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "sitecustomize.py"), []byte("import builtins\nbuiltins.shadowed_ran = 'yes'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bash := bashTestSettings(t, root)
	bash.Env = map[string]string{"PYTHONPATH": other}
	got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "python", "code": "import sys\nprint(fn.base.v, shadowed_ran, sys.path.count(" + shellQuoteForTest(other) + "))"}))
	if got != "3 yes 1\n" {
		t.Fatalf("got %q", got)
	}
}

func TestExecNodeAndTsKeepUserNodePath(t *testing.T) {
	requireInterpreters(t, "node", "bun")
	root, settings := execTemplateSettings(map[string]string{"node/base.js": "// b\nexports.v = 1;\n"})
	defer os.RemoveAll(root)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "mine.js"), []byte("exports.v = 9;"), 0o644); err != nil {
		t.Fatal(err)
	}
	bash := bashTestSettings(t, root)
	bash.Env = map[string]string{"NODE_PATH": other}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "node", "code": "console.log(fn.base.v, require('mine').v)"})); got != "1 9\n" {
		t.Errorf("node = %q", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"lang": "ts", "code": "import { v } from \"mine\"; console.log(v)"})); got != "9\n" {
		t.Errorf("ts = %q", got)
	}
}

func TestExecBashBareNamesAreUnaffectedByTemplates(t *testing.T) {
	requireInterpreters(t, "bash")
	root, settings := execTemplateSettings(map[string]string{"bash/hello.sh": "#!/bin/sh\n# Hello\necho hello\n"})
	defer os.RemoveAll(root)
	if got := execContent(runExec(t, root, settings, bashTestSettings(t, root), map[string]any{"lang": "bash", "code": "hello.sh; echo \"[$JAD_RUNNER_DIR]\" | grep -c bash"})); got != "hello\n1\n" {
		t.Errorf("bash = %q", got)
	}
}

func TestExecTemplateScratchIsRemovedWithTheCall(t *testing.T) {
	requireInterpreters(t, "python3", "node")
	root, settings := execTemplateSettings(map[string]string{"node/x.js": "// x\n"})
	defer os.RemoveAll(root)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	bash := bashTestSettings(t, root)
	for _, lang := range []string{"python", "node"} {
		runExec(t, root, settings, bash, map[string]any{"lang": lang, "code": "1"})
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
}

// TestExecCodeFileWriteErrorCarriesTheCall is P6BM-20: prepare has no call id of its own, so its
// failure used to render "id":"" while every other error of the tool names the call.
func TestExecCodeFileWriteErrorCarriesTheCall(t *testing.T) {
	settings := sessionfs.NativeExecSettings{
		LangNames: []string{"filemode"},
		Langs: map[string]sessionfs.NativeExecLang{
			"filemode": {Argv: []string{"node", "{file}"}, Mode: sessionfs.NativeExecModeFile, Ext: ".js", Aliases: []string{}},
		},
		DefaultLang: "filemode", CodeMaxBytes: 4096, InputMaxBytes: 512, ArgMaxBytes: 4096, ResultMaxBytes: 512,
	}
	plan, problem := planExec(map[string]any{"lang": "filemode", "code": "1"}, settings, tools.ExecutionContext{CallID: "call_file"})
	if problem != "" {
		t.Fatalf("planExec: %s", problem)
	}
	_, _, _, problem = plan.prepare(filepath.Join(t.TempDir(), "missing"))
	if problem == "" {
		t.Fatal("a prepare into a missing directory must fail")
	}
	if !strings.Contains(problem, `"id":"call_file"`) || !strings.Contains(problem, "The code file could not be written") {
		t.Fatalf("the failure does not name the call:\n%s", problem)
	}
}

// TestExecListCapsTheProblemsItShows is P6BM-11: one stray line is added per top-level entry of
// the library that is not a runner, so the count is bounded by the directory and must be capped.
func TestExecListCapsTheProblemsItShows(t *testing.T) {
	root, settings := execHelpersFixtureFor(map[string]string{"node/a.js": "// A\nexports.a = 1;\n"})
	defer os.RemoveAll(root)
	for _, name := range []string{"stray_a", "stray_b", "stray_c", "stray_d", "stray_e"} {
		if err := os.MkdirAll(filepath.Join(root, ".jad", "exec", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settings.ListMaxResults = 2
	content := execContent(executeExecList(map[string]any{}, settings, tools.ExecutionContext{CallID: "list", WorkspaceRoot: root}))
	if got := strings.Count(content, "- stray_"); got != 2 {
		t.Fatalf("listed %d stray entries, want 2:\n%s", got, content)
	}
	if !strings.Contains(content, "malformed entries (5):") || !strings.Contains(content, "- …and 3 more") {
		t.Fatalf("the listing does not say how many entries are hidden:\n%s", content)
	}
}

// TestExecTemplateLeavesTheProgramAlone pins the substitution order: the template is attached
// before the code goes into argv, so a literal {template} (or {file}, or {code}) inside the
// program reaches the runner as written (P6BM-02).
func TestExecTemplateLeavesTheProgramAlone(t *testing.T) {
	root, settings := execTemplateSettings(nil)
	defer os.RemoveAll(root)
	code := `console.log("{template}", "{file}", "{code}")`
	plan, problem := planExec(map[string]any{"lang": "node", "code": code}, settings, tools.ExecutionContext{WorkspaceRoot: root})
	if problem != "" {
		t.Fatalf("planExec: %s", problem)
	}
	workDir := t.TempDir()
	_, argv, _, problem := plan.prepare(workDir)
	if problem != "" {
		t.Fatalf("prepare: %s", problem)
	}
	launch := plan.launch(workDir, plan.code, "code", []byte("// template"), nil, nil)
	if len(launch.templateFiles) != 1 || launch.templateFiles[0].path != filepath.Join(workDir, "template.js") {
		t.Fatalf("template files = %+v", launch.templateFiles)
	}
	want := []string{"-r", filepath.Join(workDir, "template.js"), "-e", code}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func shellQuoteForTest(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\\''") + "'" }
