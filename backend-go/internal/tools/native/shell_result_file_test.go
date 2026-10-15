package native

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeResultFile(t *testing.T, content string, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if size > int64(len(content)) {
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadShellResultFileCutsWithoutSplittingACharacter(t *testing.T) {
	for _, test := range []struct {
		name      string
		content   string
		maxBytes  int64
		want      string
		wantBytes int64
	}{
		{name: "fits", content: "answer\n", maxBytes: 100, want: "answer"},
		{name: "cut ascii", content: "abcdefghij", maxBytes: 4, want: "abcd", wantBytes: 6},
		{name: "cut inside a 3-byte character", content: "ab" + "€" + "cd", maxBytes: 3, want: "ab", wantBytes: 5},
		{name: "cut right after a character", content: "ab" + "€" + "cd", maxBytes: 5, want: "ab€", wantBytes: 2},
		{name: "cut inside a 4-byte character", content: "a😀b", maxBytes: 3, want: "a", wantBytes: 5},
		{name: "trailing whitespace is removed", content: "abc   \n\n", maxBytes: 100, want: "abc"},
		{name: "blank file", content: " \n\t", maxBytes: 100},
		{name: "empty file", content: "", maxBytes: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := readShellResultFile(writeResultFile(t, test.content, 0), test.maxBytes)
			if test.want == "" {
				if got != nil {
					t.Fatalf("readShellResultFile() = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.text != test.want || got.cutBytes != test.wantBytes {
				t.Fatalf("readShellResultFile() = %+v, want text %q cut %d", got, test.want, test.wantBytes)
			}
		})
	}
	if got := readShellResultFile(filepath.Join(t.TempDir(), "missing"), 10); got != nil {
		t.Fatalf("missing file = %+v, want nil", got)
	}
}

// A program can write a result file far larger than result_max_bytes; the cut size
// must still be the whole file's, and only the head may be read into memory.
func TestReadShellResultFileDoesNotLoadAHugeFile(t *testing.T) {
	const size = 200 << 20
	const maxBytes = 1000
	path := writeResultFile(t, "head of the answer "+strings.Repeat("x", 2000), size)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := readShellResultFile(path, maxBytes)
	runtime.ReadMemStats(&after)
	if got == nil || !strings.HasPrefix(got.text, "head of the answer ") || len(got.text) != maxBytes {
		t.Fatalf("readShellResultFile() = %d bytes of text, want %d", len(got.text), maxBytes)
	}
	if want := int64(size) - maxBytes; got.cutBytes != want {
		t.Fatalf("cutBytes = %d, want %d", got.cutBytes, want)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("read allocated %d bytes for a 200 MB file with a 1000 byte limit", allocated)
	}
}
