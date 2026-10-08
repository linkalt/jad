package app

import (
	"path/filepath"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestResolverSharesOneSubagentManagerPerStorageDirAndClosesIt(t *testing.T) {
	workspace := t.TempDir()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)

	configFor := func(dir string) sessionfs.SessionConfig {
		return sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: filepath.Join(workspace, dir)}}
	}
	first := resolver.sharedSubagentManager(configFor("kids"))
	if first == nil || first != resolver.sharedSubagentManager(configFor("kids/")) {
		t.Fatal("the resolver did not reuse the manager for the same storage directory")
	}
	if other := resolver.sharedSubagentManager(configFor("other")); other == nil || other == first {
		t.Fatal("a different storage directory shared the manager")
	}
	if resolver.sharedSubagentManager(sessionfs.SessionConfig{}) != nil {
		t.Fatal("a session without the subagent tool got a manager")
	}

	resolver.Close()
	if !first.Closed() {
		t.Fatal("closing the resolver left the sub-agent manager open")
	}
	if resolver.sharedSubagentManager(configFor("late")) != nil {
		t.Fatal("a closed resolver created a manager")
	}
}
