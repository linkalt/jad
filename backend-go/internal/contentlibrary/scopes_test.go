package contentlibrary

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func writeScopeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A skill or command name in both scopes is never picked: reading it is an AmbiguousError
// (also a ConfigError) naming both, the skill tool's read too; a name in one scope reads.
// A kind global.json does not set uses the built-in default folder under G.
func TestWorkspaceLibraryRefusesANameInBothScopes(t *testing.T) {
	base := t.TempDir()
	global, jad := filepath.Join(base, "g"), filepath.Join(base, "w", ".jad")
	writeScopeFile(t, filepath.Join(global, ".agent", "skills", "both", "SKILL.md"), "---\nname: both\ndescription: G.\n---\nG.\n")
	writeScopeFile(t, filepath.Join(global, ".agent", "commands", "review.md"), "G.\n")
	writeScopeFile(t, filepath.Join(jad, "skills", "both", "SKILL.md"), "---\nname: both\ndescription: W.\n---\nW.\n")
	writeScopeFile(t, filepath.Join(jad, "skills", "own", "SKILL.md"), "---\nname: own\ndescription: W.\n---\nW.\n")
	writeScopeFile(t, filepath.Join(jad, "commands", "review.md"), "W.\n")
	libraries := &sessionfs.Libraries{GlobalDir: global, Workspace: sessionfs.LibraryFolders{Skills: []string{filepath.Join(jad, "skills")}, Commands: []string{filepath.Join(jad, "commands")}}}
	ctx := context.Background()

	_, _, err := ReadWorkspaceSkill(ctx, libraries, "both")
	var ambiguous *AmbiguousError
	var config *ConfigError
	if !errors.As(err, &ambiguous) || !errors.As(err, &config) || !strings.Contains(err.Error(), filepath.Join(global, ".agent", "skills", "both")) || !strings.Contains(err.Error(), filepath.Join(jad, "skills", "both")) {
		t.Fatalf("ReadWorkspaceSkill(both) = %v", err)
	}
	if skill, found, err := ReadWorkspaceSkill(ctx, libraries, "own"); err != nil || !found || skill.Directory != filepath.Join(jad, "skills", "own") {
		t.Fatalf("ReadWorkspaceSkill(own) = %+v %v %v", skill, found, err)
	}
	skills, err := ListWorkspaceSkills(ctx, libraries)
	if err != nil || len(skills) != 1 || skills[0].Name != "own" {
		t.Fatalf("ListWorkspaceSkills = %+v %v", skills, err)
	}
	library := New(Options{Workspaces: map[string]*sessionfs.Libraries{"w": libraries}, StartWorkspace: "w"})
	dirs, _ := scopedDirectoriesOf(libraries)
	for _, kind := range []Kind{Skill, Command} {
		files, err := filesForKind(ctx, kind, directoryList(dirs.workspace, kind))
		if err != nil || len(files) == 0 {
			t.Fatal(err)
		}
		var id string
		for _, file := range files {
			if strings.Contains(file.path, "both") || strings.HasSuffix(file.path, "review.md") {
				id = assetID(kind, file.path)
			}
		}
		if _, _, err := library.Read(ctx, kind, id, ""); !errors.As(err, &ambiguous) {
			t.Errorf("Read(%s) = %v, want AmbiguousError", kind, err)
		}
	}
	if _, err := library.List(ctx, "other"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Errorf("List(other) = %v", err)
	}
	if _, err := New(Options{WorkingDir: base}).List(ctx, "w"); !errors.Is(err, ErrUnknownWorkspace) {
		t.Errorf("a v1 library must refuse a workspace: %v", err)
	}
}
