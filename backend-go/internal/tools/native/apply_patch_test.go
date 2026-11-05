package native

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleApplyPatchManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "apply_patch.json")

func testApplyPatchSettings(root string) sessionfs.NativeApplyPatchSettings {
	write := testWriteSettings(root, 1<<20)
	return sessionfs.NativeApplyPatchSettings{Policy: write.Policy, MaxBytes: 1 << 20, MaxFiles: 10, MaxPatchBytes: 1 << 20, NativeMutationSettings: write.NativeMutationSettings}
}

func runApplyPatch(t *testing.T, settings sessionfs.NativeApplyPatchSettings, root, patch string) string {
	t.Helper()
	tool, err := NewApplyPatchTool(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	return runNativeMutation(t, tool, root, "call-1", map[string]any{"patch": patch})
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplyPatchAddsUpdatesMovesAndDeletesInOneCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	writeTestFile(t, filepath.Join(workspace, "main.go"), "package main\n\nfunc a() {}\n\nfunc b() {}\n\nfunc c() {}\n")
	writeTestFile(t, filepath.Join(workspace, "old.txt"), "one\ntwo\n")
	writeTestFile(t, filepath.Join(workspace, "gone.txt"), "bye\n")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: docs/new.md",
		"+# New",
		"+text",
		"*** Update File: main.go",
		"@@ func a() {}",
		" ",
		"-func b() {}",
		"+func b() { return }",
		"@@",
		" func c() {}",
		"+",
		"+func d() {}",
		"*** Update File: old.txt",
		"*** Move to: moved/new.txt",
		"@@",
		" one",
		"-two",
		"+TWO",
		"*** Delete File: gone.txt",
		"*** End Patch",
	}, "\n")
	settings := testApplyPatchSettings(root)
	var changes []string
	tool, err := NewApplyPatchTool(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"patch": patch}, tools.ExecutionContext{CallID: "call-1", WorkspaceRoot: workspace, OnFileChange: func(path, action string) {
		changes = append(changes, action+" "+path)
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertMutationStatus(t, output, "ok")
	if !strings.Contains(output, "Applied the patch to 4 files: 1 added, 1 updated, 1 moved, 1 deleted") {
		t.Fatalf("output:\n%s", output)
	}
	if got := readTestFile(t, filepath.Join(workspace, "docs", "new.md")); got != "# New\ntext\n" {
		t.Fatalf("added file = %q", got)
	}
	if got := readTestFile(t, filepath.Join(workspace, "main.go")); got != "package main\n\nfunc a() {}\n\nfunc b() { return }\n\nfunc c() {}\n\nfunc d() {}\n" {
		t.Fatalf("updated file = %q", got)
	}
	if got := readTestFile(t, filepath.Join(workspace, "moved", "new.txt")); got != "one\nTWO\n" {
		t.Fatalf("moved file = %q", got)
	}
	for _, name := range []string{"old.txt", "gone.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", name, err)
		}
	}
	want := []string{"created docs/new.md", "edited main.go", "created moved/new.txt", "deleted old.txt", "deleted gone.txt"}
	if !slices.Equal(changes, want) {
		t.Fatalf("file changes = %q, want %q", changes, want)
	}
	if !strings.Contains(output, "gone.txt (backup ") {
		t.Fatalf("the deleted file names no backup:\n%s", output)
	}
}

func TestApplyPatchChangesNothingWhenOneFileDoesNotApply(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "alpha\n")
	writeTestFile(t, filepath.Join(root, "b.txt"), "beta\n")
	patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n-alpha\n+ALPHA\n*** Add File: c.txt\n+new\n*** Update File: b.txt\n@@\n-missing\n+x\n*** End Patch"
	output := runApplyPatch(t, testApplyPatchSettings(t.TempDir()), root, patch)
	assertMutationStatus(t, output, "not_found")
	for _, want := range []string{"no_match", "b.txt", "hunk: 1", "no file was changed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output misses %q:\n%s", want, output)
		}
	}
	if readTestFile(t, filepath.Join(root, "a.txt")) != "alpha\n" {
		t.Fatal("a.txt was changed by a patch that did not apply")
	}
	if _, err := os.Stat(filepath.Join(root, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt was created by a patch that did not apply")
	}
}

func TestApplyPatchKeepsCRLFAndMatchesLooseWhitespace(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "win.txt"), "first\r\n  second  \r\nthird\r\n")
	output := runApplyPatch(t, testApplyPatchSettings(t.TempDir()), root, "*** Begin Patch\n*** Update File: win.txt\n first\n-second\n+SECOND\n third\n*** End Patch\n")
	assertMutationStatus(t, output, "ok")
	if got := readTestFile(t, filepath.Join(root, "win.txt")); got != "first\r\nSECOND\r\nthird\r\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestApplyPatchEndOfFileAnchorsTheLastMatch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "x.txt"), "end\nmiddle\nend")
	output := runApplyPatch(t, testApplyPatchSettings(t.TempDir()), root, "*** Begin Patch\n*** Update File: x.txt\n@@\n-end\n+END\n*** End of File\n*** End Patch")
	assertMutationStatus(t, output, "ok")
	if got := readTestFile(t, filepath.Join(root, "x.txt")); got != "end\nmiddle\nEND" {
		t.Fatalf("file = %q, want the last line changed and no newline added", got)
	}
}

func TestApplyPatchRefusals(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "exists.txt"), "x\n")
	settings := testApplyPatchSettings(t.TempDir())
	small := settings
	small.MaxFiles = 1
	tiny := settings
	tiny.MaxPatchBytes = 10
	cases := []struct {
		name     string
		settings sessionfs.NativeApplyPatchSettings
		patch    string
		want     []string
	}{
		{"no begin", settings, "*** Update File: a\n-x\n+y\n*** End Patch", []string{"invalid_patch", "must start with"}},
		{"bad add line", settings, "*** Begin Patch\n*** Add File: n.txt\nmissing plus\n*** End Patch", []string{"invalid_patch", "patch_line: 3"}},
		{"no change", settings, "*** Begin Patch\n*** Update File: exists.txt\n x\n*** End Patch", []string{"invalid_patch", "changes nothing"}},
		{"add over existing", settings, "*** Begin Patch\n*** Add File: exists.txt\n+y\n*** End Patch", []string{"already_exists", "Update File"}},
		{"delete missing", settings, "*** Begin Patch\n*** Delete File: none.txt\n*** End Patch", []string{"not_found", "none.txt"}},
		{"same file twice", settings, "*** Begin Patch\n*** Delete File: exists.txt\n*** Update File: exists.txt\n-x\n+y\n*** End Patch", []string{"duplicate_path"}},
		{"outside workspace", settings, "*** Begin Patch\n*** Add File: ../escape.txt\n+y\n*** End Patch", []string{"operation for ../escape.txt"}},
		{"too many files", small, "*** Begin Patch\n*** Add File: a.txt\n+a\n*** Add File: b.txt\n+b\n*** End Patch", []string{"too_many_files", "max_files: 1"}},
		{"patch too large", tiny, "*** Begin Patch\n*** Add File: a.txt\n+a\n*** End Patch", []string{"size_limit", "max_patch_bytes: 10"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := runApplyPatch(t, test.settings, root, test.patch)
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Errorf("output misses %q:\n%s", want, output)
				}
			}
			if readTestFile(t, filepath.Join(root, "exists.txt")) != "x\n" {
				t.Fatal("exists.txt changed")
			}
		})
	}
}

func TestApplyPatchRunsPostEditChecksOnEveryChangedFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "a\n")
	settings := testApplyPatchSettings(t.TempDir())
	tool, err := NewApplyPatchTool(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool = WithPostEditHooks(tool, hookTestSettings(shHook("show", `echo "checked $0"; exit 1`, ".txt")), "")
	output, err := tool.Execute(context.Background(), map[string]any{"patch": "*** Begin Patch\n*** Update File: a.txt\n-a\n+A\n*** Add File: b.txt\n+b\n*** Delete File: a.txt\n*** End Patch"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	// The same file twice is refused, so this patch changes nothing and runs no check.
	if !strings.Contains(output, "duplicate_path") || strings.Contains(output, "post-edit checks") {
		t.Fatalf("output:\n%s", output)
	}
	output, err = tool.Execute(context.Background(), map[string]any{"patch": "*** Begin Patch\n*** Update File: a.txt\n-a\n+A\n*** Add File: b.txt\n+b\n*** End Patch"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"post-edit checks for a.txt", "post-edit checks for b.txt"} {
		if !strings.Contains(output, want) {
			t.Errorf("output misses %q:\n%s", want, output)
		}
	}
}

func TestApplyPatchSampleManifestMatchesTheCode(t *testing.T) {
	root := t.TempDir()
	settings := testApplyPatchSettings(root)
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "setting.json"), ToolDefinitionPaths: []string{sampleApplyPatchManifest},
		NativeApplyPatch: &settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "apply_patch" || definitions[0].Description != applyPatchDescription {
		t.Fatalf("definitions = %+v", definitions)
	}
}
