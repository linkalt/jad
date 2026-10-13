package app

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func newKeyTestResolver(t *testing.T) *Resolver {
	t.Helper()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "primary", Label: "Primary", Directory: t.TempDir()}}, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	return resolver
}

// Two profiles of one session may use different environments of one MCP server, and
// LoadTools closes every connection a run does not list. They must not share a manager.
func TestMCPManagerIsPerSessionAndProfile(t *testing.T) {
	resolver := newKeyTestResolver(t)
	coder := sessionfs.SessionConfig{SettingPath: "/s/default/session.json", Profile: "dev/coder"}
	reviewer := sessionfs.SessionConfig{SettingPath: "/s/default/session.json", Profile: "review/reviewer"}
	other := sessionfs.SessionConfig{SettingPath: "/s/other/session.json", Profile: "dev/coder"}

	first, err := resolver.sharedMCPManager(coder)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := resolver.sharedMCPManager(coder)
	if first != again {
		t.Fatal("the same session and profile must reuse one manager")
	}
	if second, _ := resolver.sharedMCPManager(reviewer); second == first {
		t.Fatal("two profiles of one session share a manager, so one run would close the other's MCP connections")
	}
	if third, _ := resolver.sharedMCPManager(other); third == first {
		t.Fatal("two sessions share a manager")
	}
}
