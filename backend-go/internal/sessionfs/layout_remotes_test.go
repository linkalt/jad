package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutRemotesFolder(t *testing.T) {
	setting := filepath.Join(t.TempDir(), "setting.json")
	layout, _, err := layoutFromSetting(map[string]json.RawMessage{}, setting)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(setting), ".jad/remotes"); layout.Remotes != want {
		t.Fatalf("default remotes folder %q, want %q", layout.Remotes, want)
	}
	layout, _, err = layoutFromSetting(map[string]json.RawMessage{"catalogs": json.RawMessage(`{"remotes":"boxes"}`)}, setting)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(setting), "boxes"); layout.Remotes != want {
		t.Fatalf("remotes folder %q, want %q", layout.Remotes, want)
	}
	_, _, err = layoutFromSetting(map[string]json.RawMessage{"catalogs": json.RawMessage(`{"remote":"x"}`)}, setting)
	if err == nil || !strings.Contains(err.Error(), "remotes, state") {
		t.Fatalf("unknown catalog key must list remotes: %v", err)
	}
}
