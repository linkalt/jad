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
	want := []string{"README.md", "src/main.go"}
	if !reflect.DeepEqual(listing.Files, want) || listing.Truncated {
		t.Fatalf("listing = %+v, want files %v and complete result", listing, want)
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
