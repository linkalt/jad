package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readConfigTextFileWithin(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := readConfigTextFile(path)
		done <- result{data, err}
	}()
	select {
	case got := <-done:
		return got.data, got.err
	case <-time.After(5 * time.Second):
		t.Fatalf("readConfigTextFile(%s) did not return", path)
		return nil, nil
	}
}

func TestReadConfigTextFileReadsASymlinkedConfigFile(t *testing.T) {
	// Symlinked config files are supported (P2B1-07): the read follows a final symlink to a
	// regular file, so a config kept outside the folder still loads. Characterisation test.
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.md")
	if err := os.WriteFile(target, []byte("shared prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "prompt.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := readConfigTextFileWithin(t, link)
	if err != nil || string(got) != "shared prompt" {
		t.Fatalf("readConfigTextFile(symlink) = %q, %v; want the target text", got, err)
	}
}

func TestReadConfigTextFileRefusesWhatIsNotABoundedRegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.env")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxConfigTextFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(dir, "exact.md")
	if err := os.WriteFile(exact, []byte(strings.Repeat("x", maxConfigTextFileBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	normal := filepath.Join(dir, "ok.md")
	if err := os.WriteFile(normal, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"fifo": fifo, "directory": dir, "oversize": big} {
		_, err := readConfigTextFileWithin(t, path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: want an error naming %s, got %v", name, path, err)
		}
	}
	if data, err := readConfigTextFileWithin(t, exact); err != nil || len(data) != maxConfigTextFileBytes {
		t.Errorf("file at the cap: %d bytes, %v", len(data), err)
	}
	if data, err := readConfigTextFileWithin(t, normal); err != nil || string(data) != "hello" {
		t.Errorf("normal file: %q, %v", data, err)
	}
}
