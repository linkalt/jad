package native

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

func nativeGrepSettings(binary string) sessionfs.NativeGrepSettings {
	return sessionfs.NativeGrepSettings{
		Policy:            sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary:            binary,
		MaxResults:        200,
		MaxMatchesPerFile: 20,
		MaxBytes:          64 * 1024,
		MaxLineChars:      240,
		MaxFileBytes:      8 * 1024 * 1024,
		TimeoutMs:         10_000,
	}
}

func newGrepTestCatalog(t *testing.T, settings sessionfs.NativeGrepSettings) *tools.Catalog {
	t.Helper()
	tool, err := NewGrepTool(settings)
	if err != nil {
		t.Fatalf("NewGrepTool() error = %v", err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func executeGrepTestCall(t *testing.T, catalog *tools.Catalog, root, callID string, arguments map[string]any) string {
	t.Helper()
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      "grep",
		"arguments": string(encodedArguments),
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: root})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	var response struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode function-call output: %v", err)
	}
	return response.Output
}

func installedRipgrep(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	return binary
}

func TestNativeGrepFindsAndGroupsMatches(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"src/a.ts":      "alpha\nbeta TODO\ngamma\ndelta\n",
		"src/b.ts":      "nothing here\n",
		"src/sub/c.ts":  "TODO again\n",
		"src/notes.md":  "TODO in markdown\n",
		".hidden/d.txt": "hidden TODO\n",
		"visible.txt":   "visible TODO\n",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := newGrepTestCatalog(t, nativeGrepSettings(installedRipgrep(t)))
	output := executeGrepTestCall(t, catalog, root, "grep-basic", map[string]any{"pattern": "TODO"})
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, `"summary":"4 matches in 4 files for TODO"`) {
		t.Fatalf("grep summary/status = %s", output)
	}
	for _, expected := range []string{
		"src/a.ts\n2:beta TODO",
		"src/notes.md\n1:TODO in markdown",
		"src/sub/c.ts\n1:TODO again",
		"visible.txt\n1:visible TODO",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("grep output missing %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, ".hidden/d.txt") {
		t.Fatalf("default traversal searched a hidden file: %s", output)
	}

	included := executeGrepTestCall(t, catalog, root, "grep-include", map[string]any{"pattern": "TODO", "include": "*.md"})
	if !strings.Contains(included, "include: *.md") || !strings.Contains(included, "src/notes.md\n1:TODO in markdown") || strings.Contains(included, "src/a.ts") {
		t.Fatalf("include glob result = %s", included)
	}
	literal := executeGrepTestCall(t, catalog, root, "grep-literal", map[string]any{"pattern": "a.b", "literal": true})
	if !strings.Contains(literal, "literal: true") {
		t.Fatalf("literal option missing from result: %s", literal)
	}
}

func TestNativeGrepExcludeGlob(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"src/a.ts":      "beta TODO\n",
		"src/a.test.ts": "TODO in test\n",
		"src/sub/c.ts":  "TODO again\n",
		"src/notes.md":  "TODO in markdown\n",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := newGrepTestCatalog(t, nativeGrepSettings(installedRipgrep(t)))
	run := func(id string, arguments map[string]any) string {
		return executeGrepTestCall(t, catalog, root, id, arguments)
	}

	excluded := run("grep-exclude", map[string]any{"pattern": "TODO", "exclude": "*.md"})
	if !strings.Contains(excluded, "exclude: *.md") || strings.Contains(excluded, "notes.md") || !strings.Contains(excluded, "src/a.ts\n1:beta TODO") {
		t.Fatalf("exclude result = %s", excluded)
	}
	both := run("grep-include-exclude", map[string]any{"pattern": "TODO", "include": "*.ts", "exclude": "*.test.ts"})
	if !strings.Contains(both, "files: 2\n") || strings.Contains(both, "a.test.ts") || strings.Contains(both, "notes.md") || !strings.Contains(both, "src/sub/c.ts") {
		t.Fatalf("include+exclude result = %s", both)
	}
	directory := run("grep-exclude-dir", map[string]any{"pattern": "TODO", "exclude": "src/sub/**"})
	if !strings.Contains(directory, "files: 3\n") || strings.Contains(directory, "TODO again") {
		t.Fatalf("directory exclude result = %s", directory)
	}
	braces := run("grep-exclude-braces", map[string]any{"pattern": "TODO", "exclude": "*.{md,test.ts}"})
	if !strings.Contains(braces, "files: 2\n") || strings.Contains(braces, "notes.md") || strings.Contains(braces, "a.test.ts") {
		t.Fatalf("brace exclude result = %s", braces)
	}
	negated := run("grep-exclude-bang", map[string]any{"pattern": "TODO", "exclude": " !*.md "})
	if !strings.Contains(negated, "exclude: *.md\n") || strings.Contains(negated, "notes.md") {
		t.Fatalf("negated exclude result = %s", negated)
	}
	for id, exclude := range map[string]string{"grep-exclude-blank": "  ", "grep-exclude-bangs": "!"} {
		blank := run(id, map[string]any{"pattern": "TODO", "exclude": exclude})
		if strings.Contains(blank, "exclude:") || !strings.Contains(blank, "files: 4\n") {
			t.Fatalf("blank exclude %q result = %s", exclude, blank)
		}
	}
}

func TestNativeGrepContextLimitsAndPathPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src.txt"), []byte("alpha\nbeta TODO\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret", "hidden.txt"), []byte("TODO secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := nativeGrepSettings(installedRipgrep(t))
	settings.MaxResults = 1
	settings.Policy.Paths = []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(root, "secret")}}
	catalog := newGrepTestCatalog(t, settings)

	output := executeGrepTestCall(t, catalog, root, "grep-deny-budget", map[string]any{"pattern": "TODO", "context": 1})
	if !strings.Contains(output, `"summary":"1 matches in 1 files for TODO"`) || !strings.Contains(output, "1-alpha\n2:beta TODO\n3-gamma") {
		t.Fatalf("context result = %s", output)
	}
	if strings.Contains(output, "secret") || strings.Contains(output, "truncation_reason") {
		t.Fatalf("denied path leaked or consumed the result budget: %s", output)
	}

	cappedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(cappedRoot, "a.txt"), []byte("TODO first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cappedRoot, "z.txt"), []byte("TODO later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cappedSettings := nativeGrepSettings(installedRipgrep(t))
	cappedSettings.MaxResults = 1
	cappedCatalog := newGrepTestCatalog(t, cappedSettings)
	capped := executeGrepTestCall(t, cappedCatalog, cappedRoot, "grep-capped", map[string]any{"pattern": "TODO"})
	if !strings.Contains(capped, "truncated: true") || !strings.Contains(capped, "truncation_reason: max_results") {
		t.Fatalf("result cap was not reported: %s", capped)
	}

	perFileSettings := nativeGrepSettings(installedRipgrep(t))
	perFileSettings.MaxMatchesPerFile = 2
	perFileCatalog := newGrepTestCatalog(t, perFileSettings)
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte("TODO 1\nTODO 2\nTODO 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	perFile := executeGrepTestCall(t, perFileCatalog, root, "grep-per-file", map[string]any{"pattern": "TODO", "path": "many.txt"})
	if !strings.Contains(perFile, "per_file_limit_reached: true") || strings.Contains(perFile, "3:TODO 3") {
		t.Fatalf("per-file limit result = %s", perFile)
	}
}

func TestNativeGrepErrorsAndArgumentNormalization(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("value a.b here\nvalue axb here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newGrepTestCatalog(t, nativeGrepSettings(installedRipgrep(t)))

	malformed := executeGrepTestCall(t, catalog, root, "grep-bad-pattern", map[string]any{"pattern": "a("})
	if !strings.Contains(malformed, `"status":"invalid_input"`) || !strings.Contains(malformed, "error_code: invalid_pattern") || !strings.Contains(malformed, "regex parse error") {
		t.Fatalf("malformed pattern result = %s", malformed)
	}
	missing := executeGrepTestCall(t, catalog, root, "grep-missing", map[string]any{"pattern": "TODO", "path": "missing"})
	if !strings.Contains(missing, `"status":"not_found"`) {
		t.Fatalf("missing path result = %s", missing)
	}
	outside := executeGrepTestCall(t, catalog, root, "grep-outside", map[string]any{"pattern": "TODO", "path": "../outside"})
	if !strings.Contains(outside, `"status":"forbidden"`) || !strings.Contains(outside, "path_outside_workspace") {
		t.Fatalf("outside path result = %s", outside)
	}
	literal := executeGrepTestCall(t, catalog, root, "grep-literal", map[string]any{"pattern": "a.b", "literal": true})
	if strings.Contains(literal, "2:value axb here") || !strings.Contains(literal, "1:value a.b here") {
		t.Fatalf("literal search result = %s", literal)
	}
	clamped := executeGrepTestCall(t, catalog, root, "grep-context-clamped", map[string]any{"pattern": "a.b", "context": 21})
	if !strings.Contains(clamped, "context: 20") {
		t.Fatalf("context was not clamped before schema validation: %s", clamped)
	}
}

func TestRunRipgrepTimeoutKeepsPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fixture")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "fake-rg")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"fixture.txt\"},\"lines\":{\"text\":\"TODO\\n\"},\"line_number\":1}}'\nsleep 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := nativeGrepSettings(binary)
	settings.TimeoutMs = 100
	result := runRipgrep(context.Background(), settings, nil, "TODO", []string{"."}, directory)
	if !result.truncated || result.truncationReason != "timeout" || result.matches != 1 {
		t.Fatalf("timeout result = %+v", result)
	}
	if len(result.groups) != 1 || result.groups[0].path != "fixture.txt" {
		t.Fatalf("partial groups = %+v", result.groups)
	}
}

func TestRunRipgrepReapsTermIgnoringDescendantAfterNormalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fixture")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "fake-rg")
	marker := filepath.Join(directory, "orphan-marker")
	script := "#!/bin/sh\n(trap '' TERM; sleep 1; touch " + marker + ") </dev/null >/dev/null 2>&1 &\nexit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := nativeGrepSettings(binary)
	settings.TimeoutMs = 2_000
	result := runRipgrep(context.Background(), settings, nil, "unused", []string{"."}, directory)
	if result.failure != nil {
		t.Fatalf("normal no-match result has failure: %+v", result.failure)
	}
	time.Sleep(1_200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("TERM-ignoring descendant survived and wrote marker: stat error = %v", err)
	}
}

func TestNativeGrepReportsUnavailableExecutable(t *testing.T) {
	root := t.TempDir()
	settings := nativeGrepSettings(filepath.Join(root, "missing-rg"))
	output := executeGrepTestCall(t, newGrepTestCatalog(t, settings), root, "grep-no-binary", map[string]any{"pattern": "TODO"})
	if !strings.Contains(output, "ripgrep_unavailable") || !strings.Contains(output, "install ripgrep") {
		t.Fatalf("unavailable binary result = %s", output)
	}
}

func TestEnvironmentWithoutServerToken(t *testing.T) {
	t.Setenv("AGENT_API_TOKEN", "test-secret")
	for _, value := range environmentWithoutServerToken() {
		if strings.HasPrefix(value, "AGENT_API_TOKEN=") {
			t.Fatalf("server token leaked into child environment: %q", value)
		}
	}
}
