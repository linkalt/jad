package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two files that define the same tool are an error naming both (4c: the workspace's list is read
// after the global one, and there is no override).
func TestDuplicateToolDefinitionNamesBothFiles(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "async.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(dir, "global.json"), filepath.Join(dir, "workspace.json")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loadDefinitions([]string{first}); err != nil {
		t.Fatal(err)
	}
	_, err = loadDefinitions([]string{first, second})
	if err == nil || !strings.Contains(err.Error(), second+": duplicate tool definition") || !strings.Contains(err.Error(), "also defined in "+first) {
		t.Fatalf("err = %v", err)
	}
	// The same file listed twice is the loader's list-level dedupe, not this function's.
	if _, err := loadDefinitions([]string{first, first}); err == nil {
		t.Fatal("the same file twice is a duplicate here")
	}
}
