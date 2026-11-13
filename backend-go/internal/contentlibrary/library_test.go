package contentlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestLibraryListsMarkdownAssetsAndReadsFullContent(t *testing.T) {
	root := t.TempDir()
	dirs := map[string][]string{"prompt_dirs": {"library/prompts"}, "template_dirs": {"library/templates"}, "skill_dirs": {"library/skills"}}
	writeLibraryTestFile(t, filepath.Join(root, "library/prompts/nested/prompt.md"), "---\ntitle: \"Prompt title\"\ndescription: >-\n  Folded\n  description\n---\n# Prompt heading\n\nPrompt body.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/prompts/another.md"), "# Another\n\nSecond paragraph.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/templates/starter.md"), "---\nname: Starter\n---\nTemplate body.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/skills/guide/SKILL.md"), "---\nname: guide\ndescription: Use this skill.\n---\n# Guide\n\nInstructions.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/skills/invalid/SKILL.md"), "# Not a valid skill\n")

	library := newTestLibrary(root, dirs)
	catalog, err := library.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(catalog.Prompts), 2; got != want {
		t.Fatalf("prompt count = %d, want %d: %+v", got, want, catalog.Prompts)
	}
	if got, want := len(catalog.Templates), 1; got != want {
		t.Fatalf("template count = %d, want %d", got, want)
	}
	if got, want := len(catalog.Skills), 1; got != want {
		t.Fatalf("skill count = %d, want %d", got, want)
	}
	if catalog.Prompts[0].Name != "Another" || catalog.Prompts[0].Description != "Second paragraph." {
		t.Errorf("first prompt metadata = %+v", catalog.Prompts[0])
	}
	if catalog.Prompts[1].Name != "Prompt title" || catalog.Prompts[1].Description != "Folded description" {
		t.Errorf("frontmatter prompt metadata = %+v", catalog.Prompts[1])
	}
	if catalog.Prompts[1].Source != "nested/prompt.md" || catalog.Prompts[1].Kind != Prompt {
		t.Errorf("prompt source/kind = %+v", catalog.Prompts[1])
	}
	if catalog.Skills[0].Name != "guide" || catalog.Skills[0].Description != "Use this skill." {
		t.Errorf("skill metadata = %+v", catalog.Skills[0])
	}

	asset, found, err := library.Read(context.Background(), Prompt, catalog.Prompts[1].ID, "")
	if err != nil || !found {
		t.Fatalf("Read(prompt) = found %v, error %v", found, err)
	}
	if asset.Content != "# Prompt heading\n\nPrompt body." || asset.Name != catalog.Prompts[1].Name {
		t.Errorf("full prompt asset = %+v", asset)
	}
	if _, found, err := library.Read(context.Background(), Prompt, "not-an-asset-id", ""); err != nil || found {
		t.Errorf("Read(invalid id) = found %v, error %v; want missing", found, err)
	}
}

func TestLibraryUsesNodeCompatibleAssetID(t *testing.T) {
	const want = "21eeedcef522acdddc36200f2e634de06f592746c59d89cf26e51068ba3a14b6"
	if got := assetID(Prompt, "/workspace/prompts/a.md"); got != want {
		t.Fatalf("asset ID = %s, want Node SHA-256 %s", got, want)
	}
}

func TestLibrarySkipsAssetsOverTheSizeLimit(t *testing.T) {
	root := t.TempDir()
	dirs := map[string][]string{"prompt_dirs": {"prompts"}}
	writeLibraryTestFile(t, filepath.Join(root, "prompts/small.md"), "# Small\n\nFits.\n")
	oversized := "# Big\n\n" + strings.Repeat("x", 1<<20)
	writeLibraryTestFile(t, filepath.Join(root, "prompts/big.md"), oversized)

	library := newTestLibrary(root, dirs)
	catalog, err := library.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(catalog.Prompts), 1; got != want || catalog.Prompts[0].Name != "Small" {
		t.Fatalf("prompt count = %d (names %q), want only Small", len(catalog.Prompts), catalog.Prompts[0].Name)
	}
	bigID := assetID(Prompt, filepath.Join(root, "prompts/big.md"))
	if _, found, err := library.Read(context.Background(), Prompt, bigID, ""); err != nil || found {
		t.Fatalf("Read(oversized) = found %v, error %v; want missing", found, err)
	}
}

func TestLibraryEmptyCatalogUsesArrays(t *testing.T) {
	root := t.TempDir()
	library := newTestLibrary(root, nil)
	catalog, err := library.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"prompts":[],"templates":[],"skills":[],"commands":[]}` {
		t.Fatalf("empty catalog JSON = %s", encoded)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent/prompts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty listing unexpectedly created a default directory: %v", err)
	}
}

// newTestLibrary is the library of one workspace "w" whose global folders are dirs (by
// libraries key, relative to root); a nil dirs leaves them to the built-in defaults under root.
func newTestLibrary(root string, dirs map[string][]string) *Library {
	libraries := &sessionfs.Libraries{GlobalDir: root}
	targets := map[string]*[]string{
		"prompt_dirs": &libraries.Global.Prompts, "template_dirs": &libraries.Global.Templates,
		"skill_dirs": &libraries.Global.Skills, "command_dirs": &libraries.Global.Commands,
	}
	for key, folders := range dirs {
		resolved := make([]string, 0, len(folders))
		for _, folder := range folders {
			path := resolveLibraryPath(root, folder)
			if !slices.Contains(resolved, path) {
				resolved = append(resolved, path)
			}
		}
		*targets[key] = resolved
	}
	return New(Options{Workspaces: map[string]*sessionfs.Libraries{"w": libraries}, StartWorkspace: "w"})
}

// max_asset_bytes of global.json is bounded: a value outside the range is a ConfigError, so a
// library never reads an unbounded file.
func TestLibraryMaxAssetBytesBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		bytes int64
		ok    bool
	}{
		{name: "unset uses the default", bytes: 0, ok: true},
		{name: "minimum accepted", bytes: 1024, ok: true},
		{name: "maximum accepted", bytes: 8388608, ok: true},
		{name: "below minimum rejected", bytes: 1023},
		{name: "above maximum rejected", bytes: 8388609},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := scopedDirectoriesOf(&sessionfs.Libraries{GlobalDir: t.TempDir(), MaxAssetBytes: test.bytes})
			var configErr *ConfigError
			if test.ok && err != nil {
				t.Fatalf("error = %v, want accepted", err)
			}
			if !test.ok && !errors.As(err, &configErr) {
				t.Fatalf("error = %v, want ConfigError", err)
			}
		})
	}
}

func writeLibraryTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLibrarySkipsSymlinkEntriesAndDeduplicatesDirectories(t *testing.T) {
	root := t.TempDir()
	dirs := map[string][]string{"prompt_dirs": {"prompts", "./prompts"}}
	writeLibraryTestFile(t, filepath.Join(root, "prompts/real.md"), "Real prompt.")
	outside := filepath.Join(root, "outside.md")
	writeLibraryTestFile(t, outside, "Outside prompt.")
	if err := os.Symlink(outside, filepath.Join(root, "prompts/linked.md")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "prompts"), filepath.Join(root, "prompts/linked-dir")); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}

	catalog, err := newTestLibrary(root, dirs).List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Prompts) != 1 || catalog.Prompts[0].Name != "real" {
		t.Fatalf("prompt catalog followed symlinks or duplicated a directory: %+v", catalog.Prompts)
	}
}

func TestLibraryErrorDoesNotNameTheAbsoluteDirectory(t *testing.T) {
	root := t.TempDir()
	dirs := map[string][]string{"prompt_dirs": {"notes.txt"}}
	writeLibraryTestFile(t, filepath.Join(root, "notes.txt"), "not a directory")

	_, err := newTestLibrary(root, dirs).List(context.Background(), "")
	if err == nil {
		t.Fatal("a file named as a prompt directory was accepted")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error names the absolute directory: %q", err.Error())
	}
}
