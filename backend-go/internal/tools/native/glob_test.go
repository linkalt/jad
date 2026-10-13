package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestCompileGlobMatchesDocumentedPatterns(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		match   bool
	}{
		{"*.ts", "src/a.test.ts", true},
		{"*.ts", "src/sub/b.test.ts", true},
		{"*.ts", "src/c.tsx", false},
		{"*.ts", "package.json", false},
		{"**/*.ts", "src/a.test.ts", true},
		{"**/*.ts", "a.ts", true},
		{"src/**/*.ts", "src/sub/b.test.ts", true},
		{"src/**/*.ts", "src/a.ts", true},
		{"src/*/*.ts", "src/sub/b.test.ts", true},
		{"src/*.ts", "src/sub/b.test.ts", false},
		{"*.{ts,tsx}", "src/c.tsx", true},
		{"*.{ts,tsx}", "src/notes.md", false},
		{"a?test.ts", "a.test.ts", true},
		{"*.{ts", "*.{ts", true},
	}
	for _, testCase := range cases {
		compiled, err := compileGlob(testCase.pattern)
		if err != nil {
			t.Fatalf("compileGlob(%q) error = %v", testCase.pattern, err)
		}
		matched := compiled.matches(filepath.Base(testCase.path), testCase.path)
		if matched != testCase.match {
			t.Errorf("pattern %q against %q matched = %v, want %v", testCase.pattern, testCase.path, matched, testCase.match)
		}
	}
	if compiled, err := compileGlob("  "); err != nil || compiled != nil {
		t.Fatalf("compileGlob(blank) = %v, %v; want nil, nil", compiled, err)
	}
	if got := expandGlobBraces("*.{ts,tsx}", 64); len(got) != 2 || got[0] != "*.ts" || got[1] != "*.tsx" {
		t.Fatalf("brace alternatives = %#v", got)
	}
	if got := expandGlobBraces("*.{ts", 64); len(got) != 1 || got[0] != "*.{ts" {
		t.Fatalf("malformed brace alternatives = %#v", got)
	}
	if got := expandGlobBraces("{a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u,v,w,x,y,z}", 4); len(got) != 4 {
		t.Fatalf("bounded brace expansion returned %d alternatives, want 4", len(got))
	}
}

func TestParseGlobListedFilesPreservesNamesAndDropsTruncatedTail(t *testing.T) {
	if got, want := parseGlobListedFiles("a.ts\x00b/c.ts\x00", false), []string{"a.ts", "b/c.ts"}; !equalStrings(got, want) {
		t.Fatalf("parse complete list = %#v, want %#v", got, want)
	}
	if got, want := parseGlobListedFiles("  spaced name  \x00rg: file\x00first\nsecond\x00", false), []string{"  spaced name  ", "rg: file", "first\nsecond"}; !equalStrings(got, want) {
		t.Fatalf("parse unusual names = %#v, want %#v", got, want)
	}
	if got, want := parseGlobListedFiles("a.ts\x00b/c", true), []string{"a.ts"}; !equalStrings(got, want) {
		t.Fatalf("parse truncated list = %#v, want %#v", got, want)
	}
}

func TestNormalizeGlobArgumentsSupportsAliases(t *testing.T) {
	got := normalizeGlobArguments(map[string]any{"glob": "  *.ts  ", "dir": "src", "include": "*.go"})
	if got["pattern"] != "*.ts" || got["path"] != "src" {
		t.Fatalf("normalized aliases = %#v", got)
	}
	for _, alias := range []string{"glob", "include", "dir"} {
		if _, exists := got[alias]; exists {
			t.Errorf("normalized arguments retained alias %q", alias)
		}
	}
	got = normalizeGlobArguments(map[string]any{"glob": false, "include": "*.go"})
	if _, exists := got["pattern"]; exists {
		t.Fatalf("non-string non-null glob alias should suppress include fallback, got %#v", got)
	}
	got = normalizeGlobArguments(map[string]any{"glob": nil, "include": "*.go", "dir": "src"})
	if got["pattern"] != "*.go" || got["path"] != "src" {
		t.Fatalf("nullish alias fallback = %#v", got)
	}
}

func TestExecuteNativeGlobListsMatchesAndHonorsHiddenSetting(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	workspace := t.TempDir()
	writeGlobFixture(t, workspace)

	output := executeGlobForTest(t, workspace, globSettingsForTest(binary, true), map[string]any{"pattern": "*.test.ts"})
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, `"summary":"2 files matching *.test.ts"`) {
		t.Fatalf("glob result header = %s", output)
	}
	if !strings.Contains(output, "src/a.test.ts") || !strings.Contains(output, "src/sub/b.test.ts") || strings.Contains(output, "src/c.tsx") {
		t.Fatalf("glob result content = %s", output)
	}
	if !strings.Contains(output, "files: 2") || !strings.Contains(output, "truncated: false") {
		t.Fatalf("glob result information = %s", output)
	}

	hidden := executeGlobForTest(t, workspace, globSettingsForTest(binary, true), map[string]any{"pattern": "*.{ts,yml,env}"})
	if !strings.Contains(hidden, ".env") || !strings.Contains(hidden, ".hidden/d.ts") || !strings.Contains(hidden, ".github/workflows/ci.yml") {
		t.Fatalf("hidden files were not listed: %s", hidden)
	}
	visible := executeGlobForTest(t, workspace, globSettingsForTest(binary, false), map[string]any{"pattern": "*"})
	if strings.Contains(visible, ".env") || strings.Contains(visible, ".github/") || !strings.Contains(visible, "package.json") {
		t.Fatalf("hidden files were not suppressed: %s", visible)
	}
}

func TestExecuteNativeGlobRespectsGitIgnoreOnlyInsideRepository(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	repository := t.TempDir()
	writeGlobFile(t, filepath.Join(repository, "kept.ts"), "x\n")
	writeGlobFile(t, filepath.Join(repository, "ignored.ts"), "x\n")
	writeGlobFile(t, filepath.Join(repository, ".gitignore"), "ignored.ts\n")
	init := exec.Command(git, "init", "-q")
	init.Dir = repository
	if output, err := init.CombinedOutput(); err != nil {
		t.Skipf("git repository fixture could not be initialized: %v: %s", err, output)
	}
	inside := executeGlobForTest(t, repository, globSettingsForTest(binary, true), map[string]any{"pattern": "*.ts"})
	if !strings.Contains(inside, "kept.ts") || strings.Contains(inside, "ignored.ts") {
		t.Fatalf("repository listing did not apply gitignore: %s", inside)
	}

	plain := t.TempDir()
	writeGlobFile(t, filepath.Join(plain, "kept.ts"), "x\n")
	writeGlobFile(t, filepath.Join(plain, "ignored.ts"), "x\n")
	writeGlobFile(t, filepath.Join(plain, ".gitignore"), "ignored.ts\n")
	outside := executeGlobForTest(t, plain, globSettingsForTest(binary, true), map[string]any{"pattern": "*.ts"})
	if !strings.Contains(outside, "kept.ts") || !strings.Contains(outside, "ignored.ts") {
		t.Fatalf("plain-tree listing unexpectedly applied gitignore: %s", outside)
	}
}

func TestExecuteNativeGlobAppliesPathPolicyAndRejectsNonDirectories(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	workspace := t.TempDir()
	writeGlobFixture(t, workspace)
	secret := filepath.Join(workspace, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGlobFile(t, filepath.Join(secret, "hidden.ts"), "secret\n")
	settings := globSettingsForTest(binary, true)
	settings.Policy.Paths = []sessionfs.NativePathRule{{Action: "deny", Path: secret}}

	broad := executeGlobForTest(t, workspace, settings, map[string]any{"pattern": "*.ts"})
	if !strings.Contains(broad, "src/a.test.ts") || strings.Contains(broad, "hidden.ts") || strings.Contains(broad, "secret") {
		t.Fatalf("broad listing leaked denied paths: %s", broad)
	}
	denied := executeGlobForTest(t, workspace, settings, map[string]any{"path": "secret"})
	if !strings.Contains(denied, `"status":"forbidden"`) || !strings.Contains(denied, "path_denied") {
		t.Fatalf("denied path result = %s", denied)
	}
	file := executeGlobForTest(t, workspace, settings, map[string]any{"path": "package.json"})
	if !strings.Contains(file, `"status":"invalid_input"`) || !strings.Contains(file, "not_a_directory") {
		t.Fatalf("file path result = %s", file)
	}
}

func TestExecuteNativeGlobReportsResultAndByteCaps(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	workspace := t.TempDir()
	writeGlobFile(t, filepath.Join(workspace, "a.txt"), "a\n")
	writeGlobFile(t, filepath.Join(workspace, "b.txt"), "b\n")

	limited := globSettingsForTest(binary, true)
	limited.MaxResults = 1
	resultCap := executeGlobForTest(t, workspace, limited, map[string]any{"pattern": "*.txt"})
	if !strings.Contains(resultCap, "truncation_reason: max_results") || !strings.Contains(resultCap, "truncated: true") || !strings.Contains(resultCap, "files: 1") {
		t.Fatalf("result cap result = %s", resultCap)
	}

	byteLimited := globSettingsForTest(binary, true)
	byteLimited.MaxBytes = 8
	byteCap := executeGlobForTest(t, workspace, byteLimited, map[string]any{"pattern": "*.txt"})
	if !strings.Contains(byteCap, "truncation_reason: max_bytes") || !strings.Contains(byteCap, "truncated: true") || !strings.Contains(byteCap, "a.txt") || strings.Contains(byteCap, "b.txt") {
		t.Fatalf("byte cap result = %s", byteCap)
	}
}

func TestExecuteNativeGlobTruncatesRenderedPathsAfterParsingNULList(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	workspace := t.TempDir()
	filename := strings.Repeat("a", 23) + "😀" + strings.Repeat("tail", 8) + ".txt"
	writeGlobFile(t, filepath.Join(workspace, filename), "x\n")
	settings := globSettingsForTest(binary, true)
	settings.MaxBytes = 4096
	settings.MaxLineChars = 24

	output := executeGlobForTest(t, workspace, settings, map[string]any{"pattern": "*.txt"})
	if !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "files: 1") {
		t.Fatalf("long path was not parsed from the NUL-delimited listing: %s", output)
	}
	wantPrefix := strings.Repeat("a", 23) + "😀" + globTruncatedLineMarker
	if !strings.Contains(output, wantPrefix) {
		t.Fatalf("rendered path does not honor max_line_chars: %s", output)
	}
	if strings.Contains(output, filename) {
		t.Fatalf("full long path unexpectedly appears in output: %s", output)
	}
}

func TestExecuteNativeGlobReportsTimeoutAndUnavailableBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("timeout fixture uses a POSIX executable")
	}
	workspace := t.TempDir()
	binary := filepath.Join(workspace, "rg-timeout")
	writeGlobFile(t, binary, "#!/bin/sh\nwhile :; do sleep 1; done\n")
	if err := os.Chmod(binary, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := globSettingsForTest(binary, true)
	settings.TimeoutMs = 100
	started := time.Now()
	timeout := executeGlobForTest(t, workspace, settings, map[string]any{"pattern": "*"})
	if !strings.Contains(timeout, `"status":"ok"`) || !strings.Contains(timeout, "truncation_reason: timeout") {
		t.Fatalf("timeout result = %s", timeout)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("glob timeout took %s, expected bounded cleanup", elapsed)
	}

	missing := globSettingsForTest(filepath.Join(workspace, "missing-rg"), true)
	unavailable := executeGlobForTest(t, workspace, missing, map[string]any{})
	if !strings.Contains(unavailable, "ripgrep_unavailable") || !strings.Contains(unavailable, "install ripgrep") {
		t.Fatalf("unavailable binary result = %s", unavailable)
	}
}

func TestExecuteNativeGlobQuotesPathsContainingNewlines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filesystems do not support newlines in file names")
	}
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	workspace := t.TempDir()
	writeGlobFile(t, filepath.Join(workspace, "line\nname.txt"), "content\n")
	output := executeGlobForTest(t, workspace, globSettingsForTest(binary, true), map[string]any{"pattern": "*.txt"})
	if !strings.Contains(output, `"line\nname.txt"`) {
		t.Fatalf("newline path was not JSON-quoted: %s", output)
	}
}

func TestExecuteNativeGlobReapsDescendantsAfterNormalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup fixture uses a POSIX executable")
	}
	workspace := t.TempDir()
	binary := filepath.Join(workspace, "fake-rg")
	marker := filepath.Join(workspace, "orphan-marker")
	script := "#!/bin/sh\n(trap '' TERM; touch cleanup-ready; sleep 3; touch orphan-marker) </dev/null >/dev/null 2>&1 &\nwhile [ ! -f cleanup-ready ]; do :; done\nexit 0\n"
	writeGlobFile(t, binary, script)
	if err := os.Chmod(binary, 0o700); err != nil {
		t.Fatal(err)
	}
	output := executeGlobForTest(t, workspace, globSettingsForTest(binary, true), map[string]any{})
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("glob cleanup result = %s", output)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("TERM-ignoring descendant survived glob process-group cleanup")
	}
}

func globSettingsForTest(binary string, includeHidden bool) sessionfs.NativeGlobSettings {
	return sessionfs.NativeGlobSettings{
		Policy:        sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary:        binary,
		IncludeHidden: includeHidden,
		MaxResults:    500,
		MaxBytes:      64 * 1024,
		MaxLineChars:  1000,
		TimeoutMs:     10_000,
	}
}

func executeGlobForTest(t *testing.T, workspace string, settings sessionfs.NativeGlobSettings, arguments map[string]any) string {
	t.Helper()
	return executeNativeGlob(context.Background(), arguments, settings, tools.ExecutionContext{
		CallID:        "glob-test",
		WorkspaceRoot: workspace,
	})
}

func writeGlobFixture(t *testing.T, workspace string) {
	t.Helper()
	for _, directory := range []string{"src/sub", ".github/workflows", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"src/a.test.ts":            "x\n",
		"src/sub/b.test.ts":        "x\n",
		"src/c.tsx":                "x\n",
		"src/notes.md":             "x\n",
		"package.json":             "{}\n",
		".env":                     "SECRET=1\n",
		".github/workflows/ci.yml": "on: push\n",
		".hidden/d.ts":             "x\n",
	} {
		writeGlobFile(t, filepath.Join(workspace, path), content)
	}
}

func writeGlobFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func BenchmarkExecuteNativeGlob(b *testing.B) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		b.Skip("ripgrep is not installed")
	}
	workspace := b.TempDir()
	for index := 0; index < 128; index++ {
		path := filepath.Join(workspace, "src", "file-"+strconv.Itoa(index)+".txt")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	settings := globSettingsForTest(binary, true)
	execution := tools.ExecutionContext{CallID: "glob-benchmark", WorkspaceRoot: workspace}
	arguments := map[string]any{"pattern": "*.txt"}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := executeNativeGlob(context.Background(), arguments, settings, execution)
		if !strings.Contains(result, "files: 128") {
			b.Fatalf("benchmark result did not include all files: %s", result)
		}
	}
}
