//go:build unix

package checkpoint

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func assertOwnerOnly(t *testing.T, root string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		count++
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, want no group/other access", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count < 10 {
		t.Fatalf("walked only %d entries under %s", count, root)
	}
}

func TestCheckpointDirectoriesAndObjectsAreOwnerOnlyUnderAPermissiveUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	f := newFixture(t, nil)
	f.seed(t)
	f.begin(t, "t1", 1)
	assertOwnerOnly(t, f.staging)
}

func TestAnExistingWorldReadableCheckpointTreeIsTightenedAtOpen(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	f := newFixture(t, nil)
	f.seed(t)
	f.begin(t, "t1", 1)
	err := filepath.WalkDir(f.staging, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			err = os.Chmod(path, 0o755)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "a.txt", "a1\n")
	f.begin(t, "t2", 2)
	for _, dir := range []string{f.staging, filepath.Join(f.staging, GitDirectoryName), filepath.Join(f.staging, GitDirectoryName, "info")} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s = %v, %v, want 0700", dir, info, err)
		}
	}
}
