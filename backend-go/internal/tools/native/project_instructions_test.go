package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestSubfolderInstructionsAreAddedOncePerRunToFileToolResults(t *testing.T) {
	root := t.TempDir()
	for path, text := range map[string]string{
		"AGENTS.md":                "ROOT (already in the prompt)",
		"pkg/AGENTS.md":            "PKG RULES",
		"pkg/inner/CLAUDE.md":      "INNER RULES",
		"pkg/inner/code.go":        "package inner",
		"docs/guide.md":            "guide",
		"other/AGENTS.md/keep.txt": "a folder named like the file is no file",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := sessionfs.DefaultProjectInstructionsSettings()
	settings.Enabled, settings.Subfolders = true, true
	plain := func(name string) tools.Tool {
		return tools.Tool{Definition: tools.Definition{Source: "native", Name: name}, Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			return "result of " + name + "\n", nil
		}}
	}
	loaded := withProjectInstructionsAll([]tools.Tool{plain("read"), plain("apply_patch"), plain("grep")}, settings)
	execution := tools.ExecutionContext{WorkspaceRoot: root}
	call := func(tool tools.Tool, arguments map[string]any) string {
		t.Helper()
		result, err := tool.Execute(context.Background(), arguments, execution)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := call(loaded[0], map[string]any{"path": "pkg/inner/code.go"})
	if !strings.Contains(first, "PKG RULES") || !strings.Contains(first, "INNER RULES") || strings.Contains(first, "ROOT") {
		t.Fatalf("first read:\n%s", first)
	}
	if strings.Index(first, "PKG RULES") > strings.Index(first, "INNER RULES") || !strings.Contains(first, "# Project instructions: pkg/inner/CLAUDE.md") || !strings.Contains(first, "apply to the files under pkg/inner/") {
		t.Fatalf("order or headings:\n%s", first)
	}
	if again := call(loaded[0], map[string]any{"path": filepath.Join(root, "pkg", "inner", "code.go")}); strings.Contains(again, "RULES") {
		t.Fatalf("a file was given twice in one run:\n%s", again)
	}
	for _, path := range []string{"docs/guide.md", "README.md", "../outside.txt", "other/AGENTS.md/keep.txt"} {
		if result := call(loaded[0], map[string]any{"path": path}); result != "result of read\n" {
			t.Fatalf("%s:\n%s", path, result)
		}
	}
	if result := call(loaded[2], map[string]any{"path": "pkg/inner"}); result != "result of grep\n" {
		t.Fatalf("grep is not a file tool:\n%s", result)
	}

	// A fresh catalog (the next run) gives the files again; apply_patch names its files in the patch.
	loaded = withProjectInstructionsAll([]tools.Tool{plain("read"), plain("apply_patch")}, settings)
	patched := call(loaded[1], map[string]any{"patch": "*** Begin Patch\n*** Add File: pkg/new.go\n+package pkg\n*** End Patch"})
	if !strings.Contains(patched, "PKG RULES") || strings.Contains(patched, "INNER RULES") {
		t.Fatalf("apply_patch:\n%s", patched)
	}

	settings.Subfolders = false
	if wrapped := withProjectInstructionsAll([]tools.Tool{plain("read")}, settings); call(wrapped[0], map[string]any{"path": "pkg/inner/code.go"}) != "result of read\n" {
		t.Fatal("subfolders off still added a file")
	}
}
