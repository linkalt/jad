package tools

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A tools.lazy entry counts as allowed, and load_tool too while the profile has lazy tools; deny wins
// over both, and a profile without lazy tools still needs load_tool in its allow list.
func TestProfileShowsCountsLazyEntriesAsAllowed(t *testing.T) {
	read := Definition{ID: "native.read", Source: "native", Name: "read"}
	bash := Definition{ID: "native.bash", Source: "native", Name: "bash"}
	loadTool := Definition{ID: "native.load_tool", Source: "native", Name: "load_tool"}
	lookup := Definition{ID: "mcp:docs:lookup", Source: "mcp", Name: "mcp__docs__lookup"}
	other := Definition{ID: "mcp:other:lookup", Source: "mcp", Name: "mcp__other__lookup"}
	extensionLoadTool := Definition{ID: "extension.load_tool", Source: "extension", Name: "load_tool"}

	lazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Lazy: []string{"read", "mcp:docs:*"}}
	noLazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}}
	emptyAllow := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{}, Lazy: []string{"read"}}
	denied := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Deny: []string{"read", "load_tool"}, Lazy: []string{"read"}}
	for _, tc := range []struct {
		name       string
		settings   sessionfs.ProfileToolSettings
		definition Definition
		want       bool
	}{
		{"allowed", lazy, bash, true},
		{"lazy, not in allow", lazy, read, true},
		{"lazy wildcard", lazy, lookup, true},
		{"other server", lazy, other, false},
		{"load_tool with lazy tools", lazy, loadTool, true},
		{"load_tool without lazy tools", noLazy, loadTool, false},
		{"read without lazy tools", noLazy, read, false},
		{"empty allow, lazy", emptyAllow, read, true},
		{"empty allow, not lazy", emptyAllow, bash, false},
		{"empty allow, load_tool", emptyAllow, loadTool, true},
		{"lazy but denied", denied, read, false},
		{"load_tool denied", denied, loadTool, false},
		{"not native load_tool", lazy, extensionLoadTool, false},
	} {
		if got := ProfileShows(tc.settings, tc.definition); got != tc.want {
			t.Errorf("%s: ProfileShows = %v, want %v", tc.name, got, tc.want)
		}
	}
}
