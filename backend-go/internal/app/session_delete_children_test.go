package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func deleteDemoSession(t *testing.T, f *layoutFixture) {
	t.Helper()
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
}

func demoSettingPath(t *testing.T, f *layoutFixture) string {
	t.Helper()
	location, err := sessionfs.Resolve(f.service.resolver.roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return location.File
}

func TestSessionDeleteClosesItsMCPManagersAndKeepsOthers(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	settingPath := demoSettingPath(t, f)
	own, err := resolver.mcpManagerFor(settingPath + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	toolset, err := resolver.mcpManagerFor(settingPath + "\x00dev/coder\x00toolset\x00x")
	if err != nil {
		t.Fatal(err)
	}
	other, err := resolver.mcpManagerFor(filepath.Join(f.root, "sessions", "other.json") + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}

	deleteDemoSession(t, f)

	resolver.mu.Lock()
	remaining := len(resolver.mcpManagers)
	resolver.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("%d MCP managers left cached, want only the other session's", remaining)
	}
	if _, err := own.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the deleted session's MCP manager still accepts work")
	}
	if _, err := toolset.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the deleted session's toolset MCP manager still accepts work")
	}
	if _, err := other.LoadTools(t.Context(), nil, f.cwd); err != nil {
		t.Errorf("another session's MCP manager was closed: %v", err)
	}
}

func TestSessionDeleteWithStateAlsoClosesItsMCPManagers(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	manager, err := resolver.mcpManagerFor(demoSettingPath(t, f) + "\x00dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path+"?state=delete", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
	if _, err := manager.LoadTools(t.Context(), nil, f.cwd); err == nil {
		t.Error("the MCP manager of a session deleted with its state still accepts work")
	}
}

func TestSessionFileGoneReportsAMissingFileOnly(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "a.json")
	if err := os.WriteFile(present, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sessionFileGone(present) {
		t.Error("an existing file reported as gone")
	}
	if !sessionFileGone(filepath.Join(dir, "missing.json")) {
		t.Error("a missing file not reported as gone")
	}
}
