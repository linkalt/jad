package native

import (
	"path/filepath"
	"reflect"
	"testing"

	"jad/backend-go/internal/tools"
)

func TestNarrowScopeNeverReachesMoreThanTheParent(t *testing.T) {
	root := scopeWorkspace(t)
	abs := func(p string) string { return scopeRoot(root, p) }
	cases := []struct {
		name        string
		parent, own *tools.PathScope
		allow, deny []string
		nothing     bool
	}{
		{name: "neither", parent: nil, own: nil},
		{name: "parent only", parent: &tools.PathScope{Allow: []string{"src"}}, own: nil, allow: []string{abs("src")}},
		{name: "own only", parent: nil, own: &tools.PathScope{Allow: []string{abs("docs")}}, allow: []string{abs("docs")}},
		{name: "child inside parent", parent: &tools.PathScope{Allow: []string{"src"}}, own: &tools.PathScope{Allow: []string{"src/secret"}}, allow: []string{abs("src/secret")}},
		{name: "child wider than parent", parent: &tools.PathScope{Allow: []string{"src/secret"}}, own: &tools.PathScope{Allow: []string{"src"}}, allow: []string{abs("src/secret")}},
		{name: "disjoint", parent: &tools.PathScope{Allow: []string{"src"}}, own: &tools.PathScope{Allow: []string{"docs"}}, nothing: true},
		{name: "denies add up", parent: &tools.PathScope{Deny: []string{"src"}}, own: &tools.PathScope{Deny: []string{"docs"}}, deny: []string{abs("src"), abs("docs")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := narrowScope(root, c.parent, c.own)
			if c.parent == nil && c.own == nil {
				if got != nil {
					t.Fatalf("scope = %+v, want nil", got)
				}
				return
			}
			if c.nothing {
				if len(got.Allow) != 0 || !reflect.DeepEqual(got.Deny, []string{string(filepath.Separator)}) {
					t.Fatalf("scope = %+v, want nothing reachable", got)
				}
				return
			}
			if c.allow != nil {
				want := make([]string, len(c.allow))
				for i, a := range c.allow {
					want[i] = abs(a)
				}
				if !reflect.DeepEqual(got.Allow, want) {
					t.Errorf("allow = %v, want %v", got.Allow, want)
				}
			}
			if c.deny != nil && !reflect.DeepEqual(got.Deny, c.deny) {
				t.Errorf("deny = %v, want %v", got.Deny, c.deny)
			}
		})
	}
}
