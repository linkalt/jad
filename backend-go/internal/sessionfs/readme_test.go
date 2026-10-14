package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readmeSettings(max int) SessionFileListingSettings {
	settings := defaultSessionFileListingSettings()
	settings.ReadmeMaxBytes = max
	return settings
}

func TestReadSessionReadmeReturnsTheFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Title\n\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSessionReadme(root, readmeSettings(1024), NativePathPolicy{})
	if err != nil || got.Name != "README.md" || got.Content != "# Title\n\nbody" || got.Truncated {
		t.Fatalf("readme = %+v, err = %v", got, err)
	}
}

func TestReadSessionReadmeCutsAtTheLimitOnACharacterBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("ab\u00e9cd"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "ab" plus the first byte of the two-byte é.
	got, err := ReadSessionReadme(root, readmeSettings(3), NativePathPolicy{})
	if err != nil || got.Content != "ab" || !got.Truncated {
		t.Fatalf("readme = %+v, err = %v, want \"ab\" truncated", got, err)
	}
	if got, err = ReadSessionReadme(root, readmeSettings(4), NativePathPolicy{}); err != nil || got.Content != "ab\u00e9" || !got.Truncated {
		t.Fatalf("readme = %+v, err = %v, want the whole é kept", got, err)
	}
	if got, err = ReadSessionReadme(root, readmeSettings(6), NativePathPolicy{}); err != nil || got.Truncated || got.Content != "ab\u00e9cd" {
		t.Fatalf("readme = %+v, err = %v, want the file whole at exactly its size", got, err)
	}
}

func TestReadSessionReadmeRefusesWhatTheListingWouldHide(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "README.md")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := ReadSessionReadme(root, readmeSettings(1024), NativePathPolicy{}); !errors.Is(err, ErrReadmeMissing) {
		t.Fatalf("a link out of the folder: err = %v, want ErrReadmeMissing", err)
	}

	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("denied"), 0o600); err != nil {
		t.Fatal(err)
	}
	deny := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: filepath.Join(root, "README.md")}}}
	if _, err := ReadSessionReadme(root, readmeSettings(1024), deny); !errors.Is(err, ErrReadmeMissing) {
		t.Fatalf("a denied path: err = %v, want ErrReadmeMissing", err)
	}
}

func TestReadSessionReadmeMissingAndBinary(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadSessionReadme(root, readmeSettings(1024), NativePathPolicy{}); !errors.Is(err, ErrReadmeMissing) {
		t.Fatalf("no file: err = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "README.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionReadme(root, readmeSettings(1024), NativePathPolicy{}); !errors.Is(err, ErrReadmeMissing) {
		t.Fatalf("a folder: err = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte{0xff, 0xfe, 'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionReadme(root, readmeSettings(1024), NativePathPolicy{}); !errors.Is(err, ErrReadmeNotText) {
		t.Fatalf("binary: err = %v", err)
	}
}

func TestSessionFilesReadmeSettings(t *testing.T) {
	got, err := normalizeSessionFileListing([]byte(`{"readme_file":"START.md","readme_max_bytes":10}`), "p")
	if err != nil || got.ReadmeFile != "START.md" || got.ReadmeMaxBytes != 10 {
		t.Fatalf("settings = %+v, err = %v", got, err)
	}
	def, _ := normalizeSessionFileListing(nil, "p")
	if def.ReadmeFile != "README.md" || def.ReadmeMaxBytes != 256<<10 {
		t.Fatalf("defaults = %+v", def)
	}
	for _, bad := range []string{`{"readme_file":"docs/x.md"}`, `{"readme_file":".."}`, `{"readme_file":""}`, `{"readme_file":3}`, `{"readme_max_bytes":-1}`, `{"readme_max_bytes":99999999999}`} {
		if _, err := normalizeSessionFileListing([]byte(bad), "p"); err == nil || !strings.Contains(err.Error(), "session_files.readme") {
			t.Fatalf("%s: err = %v, want a readme error", bad, err)
		}
	}
}
