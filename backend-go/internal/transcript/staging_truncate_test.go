//go:build linux

package transcript

import (
	"os"
	"strings"
	"testing"
)

// /dev/full accepts the open and fails every write; it also cannot be truncated, which is
// the case where a partial staging line would stay behind.
func TestAppendStagingLineReportsAFailedRollback(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full is not available")
	}
	_, err := appendStagingLine("/dev/full", []byte("{\"x\":1}\n"), false)
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if !strings.Contains(err.Error(), "truncate staging file") {
		t.Fatalf("error = %v, want it to say the staging file could not be truncated", err)
	}
}

func TestAppendStagingLineSuccessAndPlainWriteFailureKeepTheirErrors(t *testing.T) {
	path := t.TempDir() + "/items.jsonl"
	offset, err := appendStagingLine(path, []byte("one\n"), false)
	if err != nil || offset != 0 {
		t.Fatalf("first append = %d, %v", offset, err)
	}
	offset, err = appendStagingLine(path, []byte("two\n"), false)
	if err != nil || offset != 4 {
		t.Fatalf("second append = %d, %v, want offset 4", offset, err)
	}
}
