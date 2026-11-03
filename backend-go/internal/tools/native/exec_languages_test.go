package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// builtinExecSettings are the exec settings a session gets with "exec": {}, so these tests run the
// built-in runners exactly as configured, with the test's own child settings.
func builtinExecSettings(t *testing.T, root string) sessionfs.NativeExecSettings {
	t.Helper()
	fixture := layouttest.Write(t, layouttest.Spec{Native: map[string]any{"exec": map[string]any{}}})
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	settings := *config.NativeExec
	settings.Child = bashTestSettings(t, root)
	settings.Child.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 4096, TailBytes: 4096}
	settings.Child.Stderr = sessionfs.NativeBashStreamBudget{HeadBytes: 4096, TailBytes: 4096}
	settings.Child.MaxBytes = 16384
	settings.Child.TimeoutMs = 20_000
	return settings
}

func writeExecHelpers(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, ".jad", "exec", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func execCall(t *testing.T, lang, code string) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"lang": lang, "code": code})
	if err != nil {
		t.Fatal(err)
	}
	return string(arguments)
}

var execLanguageHelpers = map[string]string{
	"perl/double.pl":   "# Doubles a number.\nsub { 2 * $_[0] }\n",
	"perl/my-math.pl":  "# Adds and names.\n{ add => sub { $_[0] + $_[1] }, call => sub { 'entry' } }\n",
	"php/double.php":   "<?php\n// Doubles a number.\nreturn fn ($x) => 2 * $x;\n",
	"php/my-math.php":  "<?php\n// Adds and names.\nreturn ['add' => fn ($a, $b) => $a + $b, 'my_math' => fn () => 'entry'];\n",
	"lua/double.lua":   "-- Doubles a number.\nreturn function(x) return 2 * x end\n",
	"lua/my-math.lua":  "-- Adds and names.\nlocal M = {}\nfunction M.add(a, b) return a + b end\nfunction M.my_math() return 'entry' end\nreturn M\n",
	"r/double.R":       "# Doubles a number.\ndouble <- function(x) 2 * x\n",
	"r/my-math.R":      "# Adds and names.\nadd <- function(a, b) a + b\nmy_math <- function() 'entry'\n",
	"deno/double.ts":   "// Doubles a number.\nexport default (x: number): number => 2 * x;\n",
	"deno/my-math.ts":  "// Adds and names.\nexport const add = (a: number, b: number) => a + b;\nexport function myMath() { return 'entry'; }\n",
	"python/double.py": "# Doubles a number.\ndef double(x):\n    return 2 * x\n",
}

// TestExecNewLanguagesCallTheirHelpers runs each language added with its template: the entry, a
// dashed name, a member other than the entry, and the error for a helper that does not exist.
func TestExecNewLanguagesCallTheirHelpers(t *testing.T) {
	for _, tc := range []struct{ lang, interpreter, code, want string }{
		{"perl", "perl", `use strict; print fn->double(21), " ", fn::call("my-math"), " ", fn::lib("my-math")->{add}->(3, 4), "\n"; eval { fn->nope }; print $@ =~ /no helper fn->nope/ ? "missing\n" : "?$@\n";`, "42 entry 7\nmissing\n"},
		{"php", "php", "<?php\necho $fn->double(21), ' ', $fn->{'my-math'}(), ' ', $fn->{'my-math'}['add'](3, 4), \"\\n\";\nfunction twice() { global $fn; return $fn->double(2); }\necho twice(), \"\\n\";\ntry { $fn->nope(); } catch (Error $e) { echo str_contains($e->getMessage(), 'no helper $fn->nope') ? \"missing\\n\" : $e->getMessage(); }\n", "42 entry 7\n4\nmissing\n"},
		{"lua", "lua", `print(fn.double(21), fn["my-math"](), fn["my-math"].add(3, 4)); print(require("double")(5)); local ok, err = pcall(function() return fn.nope end); print(ok, (err:find("no helper fn.nope", 1, true)) ~= nil)`, "42\tentry\t7\n10\nfalse\ttrue\n"},
		{"r", "Rscript", "cat(fn$double(21), fn$`my-math`(), fn$`my-math`$add(3, 4), '\\n')\ncat(length(ls()), '\\n')\ncat(tryCatch(fn$nope, error = function(e) grepl('no helper fn$nope', conditionMessage(e), fixed = TRUE)), '\\n')\n", "42 entry 7 \n0 \nTRUE \n"},
		{"deno", "deno", "console.log(fn.double(21), fn['my-math'](), fn['my-math'].add(3, 4));\ntry { fn.nope } catch (e) { console.log((e as Error).message.includes('no helper fn.nope')) }\n", "42 entry 7\ntrue\n"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			requireInterpreters(t, "bash", tc.interpreter)
			isolateStandbyPool(t)
			root := t.TempDir()
			writeExecHelpers(t, root, execLanguageHelpers)
			settings := builtinExecSettings(t, root)
			settings.StandbyPool = 0
			got := callExecThroughTheCatalog(t, root, settings, execCall(t, tc.lang, tc.code))
			if execContent(got) != tc.want {
				t.Fatalf("got %q, want %q\n%s", execContent(got), tc.want, got)
			}
		})
	}
}

// TestExecListingShowsEachLanguagesCall: the listing tells the model what to type in each
// language, and reads a php helper's description past its <?php tag.
func TestExecListingShowsEachLanguagesCall(t *testing.T) {
	root := t.TempDir()
	writeExecHelpers(t, root, execLanguageHelpers)
	settings := builtinExecSettings(t, root)
	text := execContent(callExecThroughTheCatalog(t, root, settings, `{}`))
	for _, want := range []string{
		"perl:\n  fn->double(...)   double.pl — Doubles a number.\n  fn::call(\"my-math\", ...)   my-math.pl — Adds and names.",
		"php:\n  $fn->double(...)   double.php — Doubles a number.\n  $fn->{'my-math'}(...)   my-math.php — Adds and names.",
		"lua:\n  fn.double(...)   double.lua — Doubles a number.\n  fn[\"my-math\"](...)   my-math.lua — Adds and names.",
		"deno:\n  fn.double(...)   double.ts — Doubles a number.\n  fn[\"my-math\"](...)   my-math.ts — Adds and names.",
		"r:\n  fn$double(...)   double.R — Doubles a number.\n  fn$`my-math`(...)   my-math.R — Adds and names.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "malformed") {
		t.Errorf("a helper was refused:\n%s", text)
	}
}

// TestExecPipeStandbyRunsLikeAFreshInterpreter is the pipe standby's contract: the interpreter
// started with its own argv while its code file was a named pipe renders the same result as a
// fresh start, including errors, exit codes, input, the result file and reading its own file.
func TestExecPipeStandbyRunsLikeAFreshInterpreter(t *testing.T) {
	corpus := map[string][]string{
		"php": {
			"<?php echo 1, \"\\n\";",
			"<?php $input = json_decode(stream_get_contents(STDIN), true); echo $input['a'], \"\\n\";",
			"<?php file_put_contents(getenv('JAD_RESULT'), 'answer 42');",
			"<?php throw new Exception('x');",
			"<?php echo $undefined_variable;",
			"<?php exit(3);",
			"<?php echo 'x'",
			"<?php echo strlen(file_get_contents(__FILE__)), \"\\n\";",
			"echo 1;",
			"<?php echo $fn->double(4), \"\\n\";",
		},
		"deno": {
			"console.log(1)",
			"const text = new TextDecoder().decode(await new Response(Deno.stdin.readable).arrayBuffer()); console.log(JSON.parse(text).a)",
			"Deno.writeTextFileSync(Deno.env.get('JAD_RESULT')!, 'answer 42')",
			"const x: number = 2; throw new Error('x' + x)",
			"Deno.exit(3)",
			"console.log(",
			"console.log(Deno.readTextFileSync(new URL(import.meta.url)).length)",
			"console.log(fn.double(4))",
		},
		"r": {
			"cat(1, '\\n')",
			"x <- jsonlite_free <- readLines(file('stdin')); cat(nchar(x), '\\n')",
			"writeLines('answer 42', Sys.getenv('JAD_RESULT'))",
			"x <- 1\nprint(x + 1)\n1 + 1",
			"stop('boom')",
			"f <- function() stop('in f')\nf()",
			"quit(status = 3)",
			"print(1 +",
			"cat(length(readLines(sub('^--file=', '', grep('^--file=', commandArgs(FALSE), value = TRUE)))), '\\n')",
			"cat(fn$double(4), '\\n')",
		},
	}
	interpreters := map[string]string{"php": "php", "deno": "deno", "r": "Rscript"}
	for _, lang := range []string{"php", "deno", "r"} {
		t.Run(lang, func(t *testing.T) {
			requireInterpreters(t, "bash", interpreters[lang])
			pool := isolateStandbyPool(t)
			root := t.TempDir()
			writeExecHelpers(t, root, execLanguageHelpers)
			warm := builtinExecSettings(t, root)
			if warm.Langs[lang].Standby != sessionfs.NativeExecStandbyPipe {
				t.Fatalf("%s has standby %q, want pipe", lang, warm.Langs[lang].Standby)
			}
			cold := warm
			cold.StandbyPool = 0
			for _, code := range corpus[lang] {
				arguments := execCallArguments(t, lang, code)
				want := callExecThroughTheCatalog(t, root, cold, arguments)
				callExecThroughTheCatalog(t, root, warm, execCallArguments(t, lang, map[string]string{"php": "<?php", "deno": "1", "r": "1"}[lang]))
				waitForReadyStandby(t, pool, 1)
				_, _, takenBefore := pool.stats()
				got := callExecThroughTheCatalog(t, root, warm, arguments)
				if _, _, taken := pool.stats(); taken != takenBefore+1 {
					t.Fatalf("%q did not run in a standby", code)
				}
				// The code file's path names the call's scratch directory, which differs per call.
				if execComparable(execScratchFree(got)) != execComparable(execScratchFree(want)) {
					t.Errorf("%q differs from a fresh run\nfresh:   %s\nstandby: %s", code, want, got)
				}
			}
		})
	}
}

var execScratchPath = regexp.MustCompile(regexp.QuoteMeta(os.TempDir()) + `/exec-[0-9]+`)

// execScratchFree hides the per-call scratch directory a file runner's messages name.
func execScratchFree(rendered string) string {
	return execScratchPath.ReplaceAllString(rendered, "<scratch>")
}
