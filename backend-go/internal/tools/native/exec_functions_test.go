package native

import (
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
