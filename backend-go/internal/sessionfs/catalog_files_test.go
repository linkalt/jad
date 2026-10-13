package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateFileExclusiveRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b.json")
	if err := createFileExclusive(path, []byte(`{"first":1}`), false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := createFileExclusive(path, []byte(`{"second":2}`), false)
	var refused *CatalogError
	if !errors.As(err, &refused) || refused.Kind != CatalogPreconditionFailed {
		t.Fatalf("second create error = %v, want a failed precondition", err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"first":1}` {
		t.Fatalf("the existing file was replaced: %q", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary file left: %v", entries)
	}
}
