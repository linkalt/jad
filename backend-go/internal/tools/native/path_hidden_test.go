package native

import (
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A path in a hidden folder (workspace layout: G/auth, other workspaces' .jad) reads as missing
// to the in-process readers, by name and through a link, with exactly the answer a missing
// file gets, so a reader cannot tell it exists (decider fix C).
func TestHiddenPathsReadAsMissing(t *testing.T) {
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	hidden, absent := filepath.Join(base, "other", ".jad"), filepath.Join(base, "gone")
	linked, dangling := filepath.Join(base, "w1"), filepath.Join(base, "w2")
	writeWorkspaceFile(t, filepath.Join(hidden, "secret"), "abc123")
	writeWorkspaceFile(t, filepath.Join(linked, "keep"), "x")
	writeWorkspaceFile(t, filepath.Join(dangling, "keep"), "x")
	if err := os.Symlink(filepath.Join(hidden, "secret"), filepath.Join(linked, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(absent, "secret"), filepath.Join(dangling, "f")); err != nil {
		t.Fatal(err)
	}
	policy := sessionfs.NativePathPolicy{UnrestrictedPaths: true, Hidden: []string{hidden}}
	same := func(what, root, hiddenPath, otherRoot, missingPath string) {
		t.Helper()
		_, got := resolveExistingTarget(hiddenPath, root, policy, "call")
		_, want := resolveExistingTarget(missingPath, otherRoot, policy, "call")
		if got == "" || got != want {
			t.Fatalf("%s:\n%s\na missing file:\n%s", what, got, want)
		}
	}
	same("a hidden file by name", linked, filepath.Join(hidden, "secret"), linked, filepath.Join(absent, "secret"))
	same("a link into a hidden folder", linked, "f", dangling, "f")
	if _, failure := resolveExistingTarget("keep", linked, policy, "call"); failure != "" {
		t.Fatalf("a visible file: %s", failure)
	}
	if !transcriptDenied(policy, filepath.Join(hidden, "secret")) || transcriptDenied(policy, filepath.Join(linked, "keep")) {
		t.Fatal("transcriptDenied must refuse exactly the hidden paths")
	}
	// Every copy scopedPolicy makes (worktree move, path scope, sandbox mirror) keeps them.
	execution := tools.ExecutionContext{WorkspaceRoot: dangling, WorkspaceOrigin: linked, PathScope: &tools.PathScope{Deny: []string{"x"}},
		Environment: &tools.Environment{Sandbox: &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, ReadPaths: []string{dangling}}}}
	if scoped := scopedPolicy(policy, execution); len(scoped.Hidden) != 1 || scoped.Hidden[0] != hidden {
		t.Fatalf("scoped policy lost Hidden: %+v", scoped)
	}
}

// Other profiles' private folders read as missing, the own one stays readable, also through
// every copy scopedPolicy makes; a link from the own folder to another's is not "own".
func TestOtherProfilesFoldersReadAsMissing(t *testing.T) {
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	root := filepath.Join(base, ".jad", "profile-own")
	own, other := filepath.Join(root, "dev", "a"), filepath.Join(root, "dev", "b")
	writeWorkspaceFile(t, filepath.Join(own, "doc.md"), "mine")
	writeWorkspaceFile(t, filepath.Join(other, "doc.md"), "theirs")
	if err := os.Symlink(filepath.Join(other, "doc.md"), filepath.Join(own, "link.md")); err != nil {
		t.Fatal(err)
	}
	policy := sessionfs.NativePathPolicy{UnrestrictedPaths: true, Hidden: []string{root}, HiddenExcept: []string{own}}
	execution := tools.ExecutionContext{WorkspaceRoot: base, WorkspaceOrigin: filepath.Join(base, "x"), PathScope: &tools.PathScope{Deny: []string{"x"}},
		Environment: &tools.Environment{Sandbox: &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, ReadPaths: []string{base}}}}
	for name, scoped := range map[string]sessionfs.NativePathPolicy{"plain": policy, "scoped": scopedPolicy(policy, execution)} {
		if _, failure := resolveExistingTarget(filepath.Join(own, "doc.md"), base, scoped, "call"); failure != "" {
			t.Fatalf("%s: own document: %s", name, failure)
		}
		for _, path := range []string{filepath.Join(other, "doc.md"), filepath.Join(own, "link.md")} {
			if _, failure := resolveExistingTarget(path, base, scoped, "call"); failure == "" {
				t.Fatalf("%s: %s is readable", name, path)
			}
		}
	}
}
