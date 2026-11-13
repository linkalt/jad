package remote

import (
	"os"
	"path/filepath"
	"testing"
)

// The workspace-layout samples (sample/workspace-layout) are examples people copy: they must
// load as a binding and as a key file,
func TestWorkspaceLayoutSamplesLoad(t *testing.T) {
	root := filepath.Join("..", "..", "..", "sample")
	data, err := os.ReadFile(filepath.Join(root, "workspace-layout", "remotes", "build-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ParseBinding("build-box", "build-box.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Workspace != "main" || entry.WireSession("s") != "main:s" {
		t.Fatalf("%+v", entry)
	}
	keyData, err := os.ReadFile(filepath.Join(root, "workspace-layout", "auth", "keys", "build-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseKey("build-box", keyData)
	if err != nil || key.Binding != "build-box" || key.TokenEnv == "" || key.Token != "" {
		t.Fatalf("%+v %v", key, err)
	}
}
