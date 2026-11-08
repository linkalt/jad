package tools

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A tools.lazy entry counts as allowed, and load_tool too while the profile has lazy tools; deny wins
// over both, and a profile without lazy tools still needs load_tool in its allow list.
func TestProfileShowsCountsLazyEntriesAsAllowed(t *testing.T) {
	read := Definition{ID: "native.read", Source: "native", Name: "read"}
	webSearch := Definition{ID: "extension.web_search", Source: "extension", Name: "web_search"}
	bash := Definition{ID: "native.bash", Source: "native", Name: "bash"}
	loadTool := Definition{ID: "native.load_tool", Source: "native", Name: "load_tool"}
	lookup := Definition{ID: "mcp:docs:lookup", Source: "mcp", Name: "mcp__docs__lookup"}
	other := Definition{ID: "mcp:other:lookup", Source: "mcp", Name: "mcp__other__lookup"}
	extensionLoadTool := Definition{ID: "extension.load_tool", Source: "extension", Name: "load_tool"}

	lazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Lazy: []string{"web_search", "mcp:docs:*"}}
	noLazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}}
	emptyAllow := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{}, Lazy: []string{"web_search"}}
	nativeLazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Lazy: []string{"read"}}
	search := Definition{ID: "mcp:docs:search", Source: "mcp", Name: "mcp__docs__search"}
	narrowed := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"mcp:docs:search"}, Lazy: []string{"mcp:docs:*"}}
	wholeServer := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"mcp:docs:*"}, Lazy: []string{"mcp:docs:*"}}
	prefixServer := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"mcp:docsx:search"}, Lazy: []string{"mcp:docs:*"}}
	exactLazy := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"mcp:docs:search"}, Lazy: []string{"mcp:docs:*", "mcp:docs:lookup"}}
	denied := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Deny: []string{"web_search", "load_tool"}, Lazy: []string{"web_search"}}
	for _, tc := range []struct {
		name       string
		settings   sessionfs.ProfileToolSettings
		definition Definition
		want       bool
	}{
		{"allowed", lazy, bash, true},
		{"lazy extension tool, not in allow", lazy, webSearch, true},
		// A native tool is never lazy: a lazy entry that names one allows nothing, and the loader refuses it.
		{"lazy entry naming a native tool does not allow it", nativeLazy, read, false},
		{"lazy entry naming a native tool, load_tool", nativeLazy, loadTool, true},
		{"lazy wildcard", lazy, lookup, true},
		{"other server", lazy, other, false},
		{"load_tool with lazy tools", lazy, loadTool, true},
		{"load_tool without lazy tools", noLazy, loadTool, false},
		{"read without lazy tools", noLazy, read, false},
		{"empty allow, lazy", emptyAllow, webSearch, true},
		{"empty allow, not lazy", emptyAllow, bash, false},
		{"empty allow, load_tool", emptyAllow, loadTool, true},
		{"lazy but denied", denied, webSearch, false},
		{"load_tool denied", denied, loadTool, false},
		{"not native load_tool", lazy, extensionLoadTool, false},
		// A lazy wildcard does not widen an allow list that names some of the server's tools.
		{"lazy wildcard, allow names another tool of the server", narrowed, lookup, false},
		{"lazy wildcard, allow names this tool", narrowed, search, true},
		{"lazy wildcard, allow names the whole server", wholeServer, lookup, true},
		{"lazy wildcard, allow names a server with a longer name", prefixServer, lookup, true},
		{"exact lazy entry beside a narrower allow", exactLazy, lookup, true},
	} {
		if got := ProfileShows(tc.settings, tc.definition); got != tc.want {
			t.Errorf("%s: ProfileShows = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A lazy entry names only a tool that can be lazy: an MCP or extension tool, never a native one.
func TestLazyEntryMatchesNeverNamesANativeTool(t *testing.T) {
	native := Definition{ID: "native.read_image", Source: "native", Name: "read_image"}
	extension := Definition{ID: "extension.web_search", Source: "extension", Name: "web_search"}
	mcp := Definition{ID: "mcp:docs:search", Source: "mcp", Name: "search"}
	for _, tc := range []struct {
		entry      string
		definition Definition
		want       bool
	}{
		{"read_image", native, false},
		{"web_search", extension, true},
		{"web_search", mcp, false},
		{"mcp:docs:search", mcp, true},
		{"mcp:docs:*", mcp, true},
		{"mcp:docs:*", native, false},
	} {
		if got := LazyEntryMatches(tc.entry, tc.definition); got != tc.want {
			t.Errorf("LazyEntryMatches(%q, %s) = %v, want %v", tc.entry, tc.definition.ID, got, tc.want)
		}
	}
	// allow and deny still name native tools.
	if !EntryMatches("read_image", native) || CanBeLazy(native) || !CanBeLazy(extension) || !CanBeLazy(mcp) {
		t.Error("EntryMatches must still match a native tool, and only MCP and extension tools can be lazy")
	}
}
