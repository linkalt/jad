package sessionfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCollectSessionFilesFiltersHiddenIgnoredSymlinkAndDeniedPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("README.md", "readme")
	writeFile(".hidden", "hidden")
	writeFile("src/main.go", "main")
	writeFile("private/secret.txt", "secret")
	writeFile("vendor/dependency.go", "vendor")
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "linked.txt")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	settings := defaultSessionFileListingSettings()
	settings.MaxDepth = 4
	settings.IgnoreDirectories = append(settings.IgnoreDirectories, "vendor")
	policy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(root, "private")}}}
	listing := CollectSessionFiles(root, settings, policy)
	want := []string{"README.md", "src/", "src/main.go"}
	if !reflect.DeepEqual(listing.Files, want) || listing.Truncated {
		t.Fatalf("listing = %+v, want files %v and complete result", listing, want)
	}

	settings.IncludeDirectories = false
	listing = CollectSessionFiles(root, settings, policy)
	want = []string{"README.md", "src/main.go"}
	if !reflect.DeepEqual(listing.Files, want) || listing.Truncated {
		t.Fatalf("include_directories off: listing = %+v, want files %v", listing, want)
	}
}

func TestCollectSessionFilesDenyRuleMatchesTheResolvedWorkspacePath(t *testing.T) {
	real := t.TempDir()
	base := t.TempDir()
	root := filepath.Join(base, "link")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(real, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "secrets", "key.txt"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "README.md"), []byte("readme"), 0o600); err != nil {
		t.Fatal(err)
	}

	settings := defaultSessionFileListingSettings()
	settings.MaxDepth = 4
	policy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(real, "secrets")}}}
	listing := CollectSessionFiles(root, settings, policy)
	for _, name := range listing.Files {
		if name == "secrets/key.txt" {
			t.Fatalf("listing = %+v, want secrets/key.txt hidden by the canonical deny rule", listing)
		}
	}
}

func TestCollectSessionFilesListsNestedFoldersAndCountsThemAgainstTheCap(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a/b/c.txt", "a/d.txt", "empty/.keep"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "void"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	settings := defaultSessionFileListingSettings()
	listing := CollectSessionFiles(root, settings, NativePathPolicy{})
	want := []string{"a/", "empty/", "void/", "a/b/", "a/d.txt", "a/b/c.txt"}
	if !reflect.DeepEqual(listing.Files, want) || listing.Truncated {
		t.Fatalf("listing = %+v, want %v (empty folders listed, the symlinked folder left out)", listing, want)
	}

	settings.MaxResults = 2
	listing = CollectSessionFiles(root, settings, NativePathPolicy{})
	if !listing.Truncated || len(listing.Files) != 2 {
		t.Fatalf("max_results 2 listing = %+v, want two entries and truncated", listing)
	}
}

func TestCollectSessionFilesHidesInternalLockFilesWhenHiddenEnabled(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"visible.txt", ".user-hidden", ".setting.json.jad-lock", ".messages.jsonl.jad-lock"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := defaultSessionFileListingSettings()
	settings.IncludeHidden = true
	listing := CollectSessionFiles(root, settings, NativePathPolicy{})
	want := []string{".user-hidden", "visible.txt"}
	if !reflect.DeepEqual(listing.Files, want) || listing.Truncated {
		t.Fatalf("listing = %+v, want only user files %v", listing, want)
	}
}

func TestCollectSessionFilesReportsConfiguredLimits(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := defaultSessionFileListingSettings()
	settings.MaxResults = 1
	listing := CollectSessionFiles(root, settings, NativePathPolicy{})
	if !listing.Truncated || len(listing.Files) != 1 || listing.Files[0] != "a.txt" {
		t.Fatalf("max_results listing = %+v", listing)
	}

	settings.MaxResults = 10
	settings.MaxEntries = 1
	listing = CollectSessionFiles(root, settings, NativePathPolicy{})
	if !listing.Truncated || len(listing.Files) != 1 {
		t.Fatalf("max_entries listing = %+v", listing)
	}

	settings.MaxEntries = 10
	settings.MaxDepth = 0
	listing = CollectSessionFiles(root, settings, NativePathPolicy{})
	if !listing.Truncated || len(listing.Files) != 0 {
		t.Fatalf("max_depth listing = %+v", listing)
	}
}

func TestCollectSessionFilesMissingWorkspaceIsEmpty(t *testing.T) {
	settings := defaultSessionFileListingSettings()
	listing := CollectSessionFiles(filepath.Join(t.TempDir(), "missing"), settings, NativePathPolicy{})
	if listing.Truncated || len(listing.Files) != 0 {
		t.Fatalf("missing workspace listing = %+v", listing)
	}
}

func TestPathWithinAcceptsRootAndDescendantsOnly(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "work", "space")
	cases := []struct {
		target string
		want   bool
	}{
		{root, true},
		{filepath.Join(root, "a", "b"), true},
		{filepath.Join(root, "..", "space", "a"), true},
		{filepath.Join(root, ".."), false},
		{filepath.Join(root, "..", "other"), false},
		{root + "-sibling", false},
		{filepath.Join(root, "..hidden"), true},
	}
	for _, tc := range cases {
		if got := PathWithin(root, tc.target); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", root, tc.target, got, tc.want)
		}
	}
}

func TestCollectSessionFilesDenyRuleThroughSymlinkedRuleHidesTheRealPath(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "secrets", "key.txt"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := defaultSessionFileListingSettings()
	settings.MaxDepth = 4
	policy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(link, "secrets")}}}
	listing := CollectSessionFiles(real, settings, policy)
	for _, name := range listing.Files {
		if name == "secrets/key.txt" {
			t.Fatalf("listing = %+v, want secrets/key.txt hidden by the symlinked deny rule", listing)
		}
	}
}

func TestCollectSessionFilesRelativeDenyRuleThroughSymlinkedWorkspace(t *testing.T) {
	real := t.TempDir()
	jadRoot := filepath.Join(t.TempDir(), "jad")
	if err := os.Symlink(real, jadRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "secrets", "key.txt"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := defaultSessionFileListingSettings()
	settings.MaxDepth = 4
	policy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: resolveNativeRulePath("secrets", jadRoot)}}}
	listing := CollectSessionFiles(real, settings, policy)
	for _, name := range listing.Files {
		if name == "secrets/key.txt" {
			t.Fatalf("listing = %+v, want secrets/key.txt hidden by the relative deny rule", listing)
		}
	}
}
