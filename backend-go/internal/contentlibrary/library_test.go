package contentlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLibraryListsMarkdownAssetsAndReadsFullContent(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["library/prompts"],"template_dirs":["library/templates"],"skill_dirs":["library/skills"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "library/prompts/nested/prompt.md"), "---\ntitle: \"Prompt title\"\ndescription: >-\n  Folded\n  description\n---\n# Prompt heading\n\nPrompt body.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/prompts/another.md"), "# Another\n\nSecond paragraph.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/templates/starter.md"), "---\nname: Starter\n---\nTemplate body.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/skills/guide/SKILL.md"), "---\nname: guide\ndescription: Use this skill.\n---\n# Guide\n\nInstructions.\n")
	writeLibraryTestFile(t, filepath.Join(root, "library/skills/invalid/SKILL.md"), "# Not a valid skill\n")

	library := New(Options{WorkingDir: root, Environment: map[string]string{}})
	catalog, err := library.List(context.Background())
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

	asset, found, err := library.Read(context.Background(), Prompt, catalog.Prompts[1].ID)
	if err != nil || !found {
		t.Fatalf("Read(prompt) = found %v, error %v", found, err)
	}
	if asset.Content != "# Prompt heading\n\nPrompt body." || asset.Name != catalog.Prompts[1].Name {
		t.Errorf("full prompt asset = %+v", asset)
	}
	if _, found, err := library.Read(context.Background(), Prompt, "not-an-asset-id"); err != nil || found {
		t.Errorf("Read(invalid id) = found %v, error %v; want missing", found, err)
	}
}

func TestLibraryUsesNodeCompatibleAssetID(t *testing.T) {
	const want = "21eeedcef522acdddc36200f2e634de06f592746c59d89cf26e51068ba3a14b6"
	if got := assetID(Prompt, "/workspace/prompts/a.md"); got != want {
		t.Fatalf("asset ID = %s, want Node SHA-256 %s", got, want)
	}
}

func TestLibraryReloadsRootSettingsOnEachRequest(t *testing.T) {
	root := t.TempDir()
	setting := filepath.Join(root, "setting.json")
	writeLibraryTestFile(t, setting, `{"libraries":{"prompt_dirs":["first"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "first/one.md"), "First prompt.")
	writeLibraryTestFile(t, filepath.Join(root, "second/two.md"), "Second prompt.")
	library := New(Options{WorkingDir: root, Environment: map[string]string{}})

	first, err := library.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{first.Prompts[0].Name}; !reflect.DeepEqual(got, []string{"one"}) {
		t.Fatalf("first catalog = %v", got)
	}
	writeLibraryTestFile(t, setting, `{"libraries":{"prompt_dirs":["second"]}}`)
	second, err := library.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{second.Prompts[0].Name}; !reflect.DeepEqual(got, []string{"two"}) {
		t.Fatalf("catalog did not refresh from setting.json: %v", got)
	}
}

func TestLibrarySettingResolutionMatchesRootOptions(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["custom"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, ".agent/prompts/default.md"), "Default prompt.")
	writeLibraryTestFile(t, filepath.Join(root, "custom/custom.md"), "Custom prompt.")

	withoutSetting := New(Options{
		WorkingDir:  root,
		SessionsDir: "sessions",
		Environment: map[string]string{},
	})
	defaultCatalog, err := withoutSetting.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultCatalog.Prompts[0].Name; got != "default" {
		t.Fatalf("sessions-dir default root library = %q, want default", got)
	}

	withExplicitSetting := New(Options{
		WorkingDir:      root,
		SessionsDir:     "sessions",
		RootSettingPath: "setting.json",
		Environment:     map[string]string{},
	})
	customCatalog, err := withExplicitSetting.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := customCatalog.Prompts[0].Name; got != "custom" {
		t.Fatalf("explicit setting root library = %q, want custom", got)
	}
}

func TestLibraryRejectsInvalidSettingsAndMissingExplicitSetting(t *testing.T) {
	for _, test := range []struct {
		name    string
		setting string
	}{
		{name: "libraries must be object", setting: `{"libraries":null}`},
		{name: "directory list must be array", setting: `{"libraries":{"prompt_dirs":"bad"}}`},
		{name: "directory entry must be nonempty", setting: `{"libraries":{"prompt_dirs":[" "]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeLibraryTestFile(t, filepath.Join(root, "setting.json"), test.setting)
			library := New(Options{WorkingDir: root, Environment: map[string]string{}})
			_, err := library.List(context.Background())
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("List() error = %v, want ConfigError", err)
			}
		})
	}

	root := t.TempDir()
	library := New(Options{WorkingDir: root, RootSettingPath: "missing.json", Environment: map[string]string{}})
	_, err := library.List(context.Background())
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("missing explicit setting error = %v, want ConfigError", err)
	}
}

func TestLibrarySkipsAssetsOverTheSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["prompts"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "prompts/small.md"), "# Small\n\nFits.\n")
	oversized := "# Big\n\n" + strings.Repeat("x", 1<<20)
	writeLibraryTestFile(t, filepath.Join(root, "prompts/big.md"), oversized)

	library := New(Options{WorkingDir: root, Environment: map[string]string{}})
	catalog, err := library.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(catalog.Prompts), 1; got != want || catalog.Prompts[0].Name != "Small" {
		t.Fatalf("prompt count = %d (names %q), want only Small", len(catalog.Prompts), catalog.Prompts[0].Name)
	}
	bigID := assetID(Prompt, filepath.Join(root, "prompts/big.md"))
	if _, found, err := library.Read(context.Background(), Prompt, bigID); err != nil || found {
		t.Fatalf("Read(oversized) = found %v, error %v; want missing", found, err)
	}
}

func TestLibraryMaxAssetBytesDefaultsAndBounds(t *testing.T) {
	root := t.TempDir()
	limit, err := AssetByteLimit(root)
	if err != nil || limit != 1<<20 {
		t.Fatalf("default limit = %d, error %v; want %d", limit, err, 1<<20)
	}
	for _, test := range []struct {
		name    string
		setting string
		ok      bool
	}{
		{name: "minimum accepted", setting: `{"libraries":{"max_asset_bytes":1024}}`, ok: true},
		{name: "maximum accepted", setting: `{"libraries":{"max_asset_bytes":8388608}}`, ok: true},
		{name: "below minimum rejected", setting: `{"libraries":{"max_asset_bytes":1023}}`},
		{name: "above maximum rejected", setting: `{"libraries":{"max_asset_bytes":8388609}}`},
		{name: "non-integer rejected", setting: `{"libraries":{"max_asset_bytes":"big"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeLibraryTestFile(t, filepath.Join(root, "setting.json"), test.setting)
			_, err := AssetByteLimit(root)
			var configErr *ConfigError
			if test.ok && err != nil {
				t.Fatalf("AssetByteLimit() error = %v, want accepted", err)
			}
			if !test.ok && !errors.As(err, &configErr) {
				t.Fatalf("AssetByteLimit() error = %v, want ConfigError", err)
			}
		})
	}
}

func TestLibraryEmptyCatalogUsesArrays(t *testing.T) {
	root := t.TempDir()
	library := New(Options{WorkingDir: root, SessionsDir: "sessions", Environment: map[string]string{}})
	catalog, err := library.List(context.Background())
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
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["prompts","./prompts"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "prompts/real.md"), "Real prompt.")
	outside := filepath.Join(root, "outside.md")
	writeLibraryTestFile(t, outside, "Outside prompt.")
	if err := os.Symlink(outside, filepath.Join(root, "prompts/linked.md")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "prompts"), filepath.Join(root, "prompts/linked-dir")); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}

	catalog, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Prompts) != 1 || catalog.Prompts[0].Name != "real" {
		t.Fatalf("prompt catalog followed symlinks or duplicated a directory: %+v", catalog.Prompts)
	}
}

func TestLibraryErrorDoesNotNameTheAbsoluteDirectory(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["notes.txt"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, "notes.txt"), "not a directory")

	_, err := New(Options{WorkingDir: root, Environment: map[string]string{}}).List(context.Background())
	if err == nil {
		t.Fatal("a file named as a prompt directory was accepted")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error names the absolute directory: %q", err.Error())
	}
}

// The library reads the same root setting the session loader finds: .jad/setting.json of
// the working directory, then the one in $HOME/.jad.
func TestLibraryFindsTheRootSettingInTheJadFolderThenHome(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	writeLibraryTestFile(t, filepath.Join(cwd, "setting.json"), `{"libraries":{"prompt_dirs":["old"]}}`)
	writeLibraryTestFile(t, filepath.Join(cwd, "old/old.md"), "Old.")
	writeLibraryTestFile(t, filepath.Join(home, ".jad", "setting.json"), `{"libraries":{"prompt_dirs":["home"]}}`)
	writeLibraryTestFile(t, filepath.Join(home, ".jad", "home/home.md"), "Home.")
	writeLibraryTestFile(t, filepath.Join(cwd, ".jad", "setting.json"), `{"libraries":{"prompt_dirs":["project"]}}`)
	writeLibraryTestFile(t, filepath.Join(cwd, ".jad", "project/project.md"), "Project.")

	names := func(workingDir string) []string {
		t.Helper()
		catalog, err := New(Options{WorkingDir: workingDir, Environment: map[string]string{"HOME": home}}).List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, prompt := range catalog.Prompts {
			out = append(out, prompt.Name)
		}
		return out
	}
	if got := names(cwd); !reflect.DeepEqual(got, []string{"project"}) {
		t.Fatalf("project file must win: %v", got)
	}
	if err := os.Remove(filepath.Join(cwd, ".jad", "setting.json")); err != nil {
		t.Fatal(err)
	}
	if got := names(cwd); !reflect.DeepEqual(got, []string{"home"}) {
		t.Fatalf("home must win over the old place: %v", got)
	}
}
