package fserr

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestUnwrapPathErrorReturnsTheOperationErrorThatErrorsUnwrapReturns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err := os.ReadFile(missing)
	if err == nil {
		t.Fatal("ReadFile(missing) error = nil")
	}
	// The same text the callers printed before the helper existed.
	if got, want := UnwrapPathError(err).Error(), errors.Unwrap(err).Error(); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if !errors.Is(UnwrapPathError(err), fs.ErrNotExist) {
		t.Fatalf("UnwrapPathError(%v) lost the not-exist cause", err)
	}
}

// A file that cannot be read is the "cannot be read:" branch the callers print.
func TestUnwrapPathErrorMessageForAPermissionDeniedFileMatchesTheOldText(t *testing.T) {
	locked := filepath.Join(t.TempDir(), "locked.json")
	if err := os.WriteFile(locked, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := os.ReadFile(locked)
	if !errors.Is(err, fs.ErrPermission) {
		t.Skipf("ReadFile on a 0000 file did not give permission denied (uid may be root): %v", err)
	}
	if got, want := "cannot be read: "+UnwrapPathError(err).Error(), "cannot be read: "+errors.Unwrap(err).Error(); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestUnwrapPathErrorLeavesAnErrorWithoutAPathUnchanged(t *testing.T) {
	plain := errors.New("plain")
	if got := UnwrapPathError(plain); got != plain {
		t.Fatalf("UnwrapPathError(plain) = %v, want the same error", got)
	}
}
