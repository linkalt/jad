package native

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkCodeContextCheckManyEntries checks 500 entries of one 4 MB file whose other
// lines changed, as a read of that file or a get of the task does.
func BenchmarkCodeContextCheckManyEntries(b *testing.B) {
	var builder strings.Builder
	for line := 1; line <= 100000; line++ {
		fmt.Fprintf(&builder, "line %06d of the benchmark file, padded out\n", line)
	}
	data := []byte(builder.String())
	var entries []codeContextEntry
	for id := int64(1); id <= 500; id++ {
		start := id*200 - 199
		content, _, _ := codeContextLines(data, start, start+1)
		entries = append(entries, codeContextEntry{ID: id, Path: "big.go", Start: start, End: start + 1, ContentSHA256: codeContextSHA256(content), FileSHA256: "an older file"})
	}
	reader := codeContextReader{settings: codeContextTestSettings(b.TempDir())}
	b.ResetTimer()
	for range b.N {
		checker := newCodeContextChecker(reader)
		checker.files["big.go"] = &codeContextFile{data: data, sha256: codeContextSHA256(data)}
		statuses := checker.checkAll(entries)
		if len(statuses) != 500 || statuses[500].stale {
			b.Fatalf("statuses = %d, #500 = %+v", len(statuses), statuses[500])
		}
	}
}

// The checker's lines answer what codeContextLines answers, for every range an entry can
// hold (readRange refuses start < 1 and end < start), of files with and without a final
// newline, empty lines and an empty file.
func TestCodeContextFileLinesMatchCodeContextLines(t *testing.T) {
	for _, data := range []string{"", "\n", "\n\n", "a", "a\n", "a\nb", "a\nb\n", "a\n\nb\n\n", "\na\n", "a\r\nb\r\n"} {
		file := &codeContextFile{data: []byte(data)}
		for start := int64(1); start <= 6; start++ {
			for end := start; end <= 6; end++ {
				want, _, wantOK := codeContextLines([]byte(data), start, end)
				got, ok := file.lines(start, end)
				if ok != wantOK || (ok && string(got) != string(want)) {
					t.Fatalf("%q lines %d-%d = %q %v, want %q %v", data, start, end, got, ok, want, wantOK)
				}
			}
		}
	}
}
