package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFileRangeHashesExactlyTheRequestedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(path, []byte("xxhelloyy"), 0o600); err != nil {
		t.Fatal(err)
	}
	const helloSHA256 = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	for _, test := range []struct {
		name       string
		start, end int64
		want       string
	}{
		{"section", 2, 7, helloSHA256},
		{"empty range", 4, 4, emptySHA256},
		{"empty range at end of file", 9, 9, emptySHA256},
	} {
		got, err := HashFileRange(path, test.start, test.end)
		if err != nil || got != test.want {
			t.Errorf("%s: HashFileRange(%d, %d) = %q, %v; want %q", test.name, test.start, test.end, got, err, test.want)
		}
	}
}

func TestHashFileRangeRejectsInvalidAndShortRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		start, end int64
	}{
		{"negative start", -1, 2},
		{"end before start", 2, 1},
	} {
		if _, err := HashFileRange(path, test.start, test.end); err == nil {
			t.Errorf("%s: HashFileRange(%d, %d) succeeded", test.name, test.start, test.end)
		}
	}
	if _, err := HashFileRange(path, 1, 4); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("range past end of file error = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := HashFileRange(filepath.Join(t.TempDir(), "missing"), 0, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file error = %v, want os.ErrNotExist", err)
	}
}

func TestWholeFileHashesAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	body := []byte(`{"type":"message"}` + "\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	whole, err := HashFileRange(path, 0, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fromTranscript, size, err := hashTranscriptFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile != whole || fromTranscript != whole || size != int64(len(body)) {
		t.Errorf("hashes disagree: range %q, hashFile %q, hashTranscriptFile %q (%d bytes)", whole, fromFile, fromTranscript, size)
	}
}
