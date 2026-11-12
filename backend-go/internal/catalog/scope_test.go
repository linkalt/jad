package catalog

import (
	"strings"
	"testing"
)

func scopedFixture() *Catalog {
	global := newCatalog("model", []Entry{{Category: "openai", Name: "default", Path: "/g/openai/default.json"}, {Category: "openai", Name: "mini", Path: "/g/openai/mini.json"}})
	workspace := newCatalog("model", []Entry{{Category: "openai", Name: "default", Path: "/w/openai/default.json"}, {Category: "local", Name: "llama", Path: "/w/local/llama.json"}})
	return MergeScopes("model", Scope{Name: "global", Catalog: global}, Scope{Name: "workspace", Catalog: workspace})
}

// A scope prefix picks the entry of that scope, also one Merge left out as ambiguous; without
// one the ambiguous ID is still an error naming the prefixes (design §4.4).
func TestScopePrefixPicksAnEntry(t *testing.T) {
	merged := scopedFixture()
	if _, err := merged.Lookup("openai/default"); err == nil || !strings.Contains(err.Error(), "global:openai/default or workspace:openai/default") {
		t.Fatalf("bare ambiguous lookup = %v", err)
	}
	for prefix, path := range map[string]string{"global": "/g/openai/default.json", "workspace": "/w/openai/default.json"} {
		entry, err := merged.Lookup(prefix + ":openai/default")
		if err != nil || entry.Path != path || entry.Scope != prefix {
			t.Fatalf("%s: %+v %v", prefix, entry, err)
		}
	}
	if _, err := merged.Lookup("workspace:openai/mini"); err == nil {
		t.Fatal("a prefix must not reach an entry of the other scope")
	}
	if _, err := merged.Lookup("profile:openai/mini"); err == nil {
		t.Fatal("an unknown prefix must be refused")
	}
	selection, err := Of("workspace:openai/default", "global:openai")
	if err == nil {
		_, err = merged.Narrow(selection)
	}
	if err == nil || !strings.Contains(err.Error(), "selected from two scopes") {
		t.Fatalf("one ID from both scopes = %v", err)
	}
	selection, _ = Of("workspace:openai/default", "openai/mini")
	narrowed, err := merged.Narrow(selection)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := narrowed.Lookup("openai/default")
	if err != nil || entry.Path != "/w/openai/default.json" || narrowed.Len() != 2 {
		t.Fatalf("narrowed = %v, lookup %+v %v", narrowed.IDs(), entry, err)
	}
	if text := selection.Selectors()[0].String(); text != "workspace:openai/default" {
		t.Fatalf("selector string = %q", text)
	}
	if _, err := newCatalog("model", []Entry{{Category: "openai", Name: "default"}}).Narrow(selection); err == nil {
		t.Fatal("a prefix matches nothing in a catalog of one folder (v1)")
	}
}
