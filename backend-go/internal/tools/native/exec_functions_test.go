package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func execFunctionsFixture(t *testing.T, files map[string]string) (string, sessionfs.NativeExecSettings) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".jad", "exec-functions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec-functions"
	settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes, settings.FunctionMaxBytes = 50, 200, 2048, 4096
	return root, settings
}

func TestExecWithNoParametersListsSavedFunctionsAndFiltersNarrowThem(t *testing.T) {
	root, settings := execFunctionsFixture(t, map[string]string{
		"read_json.py":  "#!/usr/bin/env python3\n# Read a JSON file and print its keys\n# input: {path: string}\nimport sys\n",
		"readme_sum.sh": "# Summarise the README\necho x\n",
		"build.sh":      "# Build the project\necho build\n",
		"broken.py":     "import sys\n",
		"Bad-Name.py":   "# nope\n",
		"notes.txt":     "# not a runner extension\n",
	})
	bash := bashTestSettings(t, root)
	all := runExec(t, root, settings, bash, map[string]any{})
	text := execContent(all)
	for _, want := range []string{"build [bash] — Build the project", "read_json [python] — Read a JSON file and print its keys (input: {path: string})", "readme_sum [bash]", "malformed files (2):", "broken.py: missing a leading comment", "Bad-Name.py: name must match"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "notes") {
		t.Errorf("a file without a runner extension was listed:\n%s", text)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"read*"}})); strings.Contains(got, "build") || !strings.Contains(got, "read_json") || !strings.Contains(got, "readme_sum") {
		t.Errorf("wildcard filter = %s", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"README"}})); !strings.Contains(got, "readme_sum") || strings.Contains(got, "read_json") {
		t.Errorf("substring filter on name or description, any case = %s", got)
	}
	if got := execContent(runExec(t, root, settings, bash, map[string]any{"filters": []any{"read*", "*sum"}})); strings.Contains(got, "read_json") || !strings.Contains(got, "readme_sum") {
		t.Errorf("every filter must match = %s", got)
	}
}

func TestExecListingCutsAtTheConfiguredLimitsAndSaysSo(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		files[name+".sh"] = "# function " + name + "\n"
	}
	root, settings := execFunctionsFixture(t, files)
	settings.ListMaxResults = 2
	result := runExec(t, root, settings, bashTestSettings(t, root), map[string]any{})
	if execField(result, "truncated") != "true" || execField(result, "shown") != "2 of 3" {
		t.Fatalf("result = %s", result)
	}
}

func TestExecRunsASavedFunctionByNameWithInput(t *testing.T) {
	requireInterpreters(t, "python3", "bash")
	root, settings := execFunctionsFixture(t, map[string]string{
		"double.py":   "# Double a number\nimport json, sys\nprint(json.load(sys.stdin)['n'] * 2)\n",
		"argsdemo.sh": "# Echo args\necho \"$1-$2\"\n",
	})
	bash := bashTestSettings(t, root)
	result := runExec(t, root, settings, bash, map[string]any{"function": "double", "input": map[string]any{"n": 21}})
	if got := execContent(result); got != "42\n" {
		t.Fatalf("content = %q\n%s", got, result)
	}
	result = runExec(t, root, settings, bash, map[string]any{"function": "argsdemo", "args": []any{"x", "y"}})
	if got := execContent(result); got != "x-y\n" {
		t.Fatalf("args = %q\n%s", got, result)
	}
}

func TestExecFunctionErrorsNameTheKeysAndTheCandidates(t *testing.T) {
	root, settings := execFunctionsFixture(t, map[string]string{
		"read_json.py": "# Read\n",
		"read_json.sh": "# Read again\n",
		"summary.sh":   "# Summary\n",
		"big.sh":       "# Big\n" + strings.Repeat("x", 5000),
	})
	bash := bashTestSettings(t, root)
	for name, tc := range map[string]struct {
		arguments map[string]any
		want      string
	}{
		"code and function": {map[string]any{"function": "summary", "code": "echo"}, "give either code or function, not both"},
		"function and argv": {map[string]any{"function": "summary", "argv": []any{"ls"}}, "give either function or argv, not both"},
		"function and lang": {map[string]any{"function": "summary", "lang": "python"}, "lang is only for code; a saved function brings its own language"},
		"filters with run":  {map[string]any{"function": "summary", "filters": []any{"x"}}, "filters is only for listing"},
		"unknown":           {map[string]any{"function": "summar"}, "similar: summary"},
		"bad name":          {map[string]any{"function": "../etc/passwd"}, "function must match"},
		"ambiguous":         {map[string]any{"function": "read_json"}, "Several files are called read_json"},
		"too big":           {map[string]any{"function": "big"}, "over function_max_bytes"},
	} {
		result := execNoDefaultLang(t, root, settings, bash, tc.arguments)
		if !strings.Contains(result, tc.want) || execField(result, "error_code") == "" {
			t.Errorf("%s: want %q in\n%s", name, tc.want, result)
		}
	}
	list := execNoDefaultLang(t, root, settings, bash, map[string]any{"lang": "python"})
	if !strings.Contains(list, "lang is only for running") {
		t.Errorf("lang without a mode: %s", list)
	}
}

func TestExecFunctionLibraryRefusesSymlinksOutOfTheDirectory(t *testing.T) {
	requireInterpreters(t, "bash")
	root, settings := execFunctionsFixture(t, map[string]string{"ok.sh": "# fine\necho fine\n"})
	outside := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(outside, []byte("# evil\necho pwned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".jad", "exec-functions", "evil.sh")); err != nil {
		t.Skip("symlinks are not available: ", err)
	}
	bash := bashTestSettings(t, root)
	run := runExec(t, root, settings, bash, map[string]any{"function": "evil"})
	if strings.Contains(run, "pwned") || !strings.Contains(run, "symlink leaves the functions directory") {
		t.Fatalf("a symlink out of the library ran: %s", run)
	}
	if list := execContent(runExec(t, root, settings, bash, map[string]any{})); strings.Contains(list, "evil [") {
		t.Fatalf("a symlink out of the library was listed: %s", list)
	}
}

func TestExecFunctionRunsThroughAHandleAndAFunctionEditedMidRunDoesNotChangeIt(t *testing.T) {
	f := newExecAsyncFixture(t)
	dir := filepath.Join(f.root, ".jad", "exec-functions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(file, []byte("# Slow\nsleep 0.3; echo first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := execTestSettings()
	settings.FunctionsDir = ".jad/exec-functions"
	settings.FunctionMaxBytes, settings.ListMaxResults, settings.ListMaxFiles, settings.ListHeaderBytes = 4096, 50, 200, 2048
	settings.Child = bashTestSettings(t, f.root)
	settings.Child.TimeoutMs, settings.Child.MaxTimeoutMs = 20_000, 30_000
	tool, err := NewExecTool(settings, "", f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.exec = tool
	started := f.run(t, "s1", `{"function":"slow","block_ms":10}`)
	execRunIDOf(t, started)
	if err := os.WriteFile(file, []byte("# Slow\necho second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waited := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("10000")})
	if !strings.Contains(waited, "first") || strings.Contains(waited, "second") {
		t.Fatalf("handle result:\n%s", waited)
	}
}
