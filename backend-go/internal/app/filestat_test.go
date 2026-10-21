package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatFileMatchesTheGenericStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	direct, err := statFile(path)
	if err != nil {
		t.Fatal(err)
	}
	generic, err := statFileGeneric(path)
	if err != nil {
		t.Fatal(err)
	}
	generic.BirthMS = direct.BirthMS
	if direct != generic {
		t.Fatalf("statFile = %+v, generic stat = %+v", direct, generic)
	}
	if direct.Size != 5 || !direct.HasIdentity || !direct.HasChange {
		t.Fatalf("statFile = %+v, want size 5 with identity and change time", direct)
	}
}

func TestStatFileReportsAMissingFile(t *testing.T) {
	_, err := statFile(filepath.Join(t.TempDir(), "missing"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
	if got := fileStamp(filepath.Join(t.TempDir(), "missing")); got != "-" {
		t.Fatalf("fileStamp of a missing file = %q, want -", got)
	}
}

func TestFileStampChangesOnEveryKindOfEdit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	original := fileStamp(path)
	if fileStamp(path) != original {
		t.Fatal("fileStamp changes without an edit")
	}
	changes := map[string]func(){
		"size":    func() { write("three") },
		"mtime":   func() { _ = os.Chtimes(path, time.Now(), time.Now().Add(-time.Hour)) },
		"removal": func() { _ = os.Remove(path) },
		"rename-over": func() {
			other := filepath.Join(directory, "other")
			_ = os.WriteFile(other, []byte("one"), 0o600)
			_ = os.Rename(other, path)
		},
	}
	for _, name := range []string{"size", "mtime", "rename-over", "removal"} {
		write("one")
		before := fileStamp(path)
		changes[name]()
		if fileStamp(path) == before {
			t.Errorf("fileStamp did not change on %s", name)
		}
	}
}

func TestFileStampChangesWhenAFileWithTheSameSizeAndTimeIsReplaced(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	moment := time.Unix(1_700_000_000, 0)
	if err := os.WriteFile(path, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, moment, moment)
	before := fileStamp(path)
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("diff"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(replacement, moment, moment)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if fileStamp(path) == before {
		t.Fatal("fileStamp ignores a replaced file with the same size and modification time")
	}
}
