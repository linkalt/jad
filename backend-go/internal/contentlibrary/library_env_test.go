package contentlibrary

import (
	"context"
	"path/filepath"
	"testing"
)

// JAD_SESSIONS_DIR is no longer read anywhere: the root loader refuses it, and the library
// must not treat it as a reason to skip the root setting either.
func TestLibraryIgnoresTheRemovedSessionsDirVariable(t *testing.T) {
	root := t.TempDir()
	writeLibraryTestFile(t, filepath.Join(root, "setting.json"), `{"libraries":{"prompt_dirs":["custom"]}}`)
	writeLibraryTestFile(t, filepath.Join(root, ".agent/prompts/default.md"), "Default prompt.")
	writeLibraryTestFile(t, filepath.Join(root, "custom/custom.md"), "Custom prompt.")

	library := New(Options{
		WorkingDir:  root,
		Environment: map[string]string{"JAD_SESSIONS_DIR": "sessions"},
	})
	catalog, err := library.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Prompts[0].Name; got != "custom" {
		t.Fatalf("library with JAD_SESSIONS_DIR set = %q, want custom: the variable must not skip the root setting", got)
	}
}
