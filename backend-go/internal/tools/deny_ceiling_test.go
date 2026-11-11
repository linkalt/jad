package tools

import (
	"slices"
	"testing"
)

func TestJoinDenyKeepsOrderAndEachEntryOnce(t *testing.T) {
	if got := JoinDeny(nil, []string{}); got != nil {
		t.Fatalf("JoinDeny of nothing = %v, want nil", got)
	}
	if got := JoinDeny([]string{"exec", "mcp:docs:*"}, []string{"read", "exec"}, []string{"mcp:docs:*"}); !slices.Equal(got, []string{"exec", "mcp:docs:*", "read"}) {
		t.Fatalf("JoinDeny = %v", got)
	}
}

// WithoutDenied drops the covered tools and keeps the receiver when nothing is covered; a catalog
// left with no tool is valid.
func TestWithoutDeniedNarrowsAndKeepsAnUncoveredCatalog(t *testing.T) {
	echo := testTool(t)
	mcp := testTool(t)
	mcp.Definition.ID, mcp.Definition.Source, mcp.Definition.Name = "mcp:docs:search", "mcp", "mcp_docs_search"
	catalog, err := NewCatalog([]Tool{echo, mcp})
	if err != nil {
		t.Fatal(err)
	}
	for _, deny := range [][]string{nil, {"read", "mcp:other:*", "mcp:doc:*"}} {
		if got, err := catalog.WithoutDenied(deny); err != nil || got != catalog {
			t.Fatalf("deny %v: got a new catalog (%v)", deny, err)
		}
	}
	narrowed, err := catalog.WithoutDenied([]string{"mcp:docs:*"})
	if err != nil || narrowed == catalog || !narrowed.Has("echo") || narrowed.Has("mcp_docs_search") || !catalog.Has("mcp_docs_search") {
		t.Fatalf("narrowed by mcp:docs:* = %v (%v)", narrowed.Definitions(), err)
	}
	empty, err := catalog.WithoutDenied([]string{"echo", "mcp:docs:search"})
	if err != nil || len(empty.Definitions()) != 0 {
		t.Fatalf("everything denied = %v (%v), want an empty catalog", empty, err)
	}
	if got, err := (*Catalog)(nil).WithoutDenied([]string{"echo"}); got != nil || err != nil {
		t.Fatalf("nil catalog = %v %v", got, err)
	}
}
