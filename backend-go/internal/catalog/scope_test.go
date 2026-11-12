package catalog

import (
	"slices"
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
		t.Fatal("profile: must find nothing in a catalog without the profile scope")
	}
	if _, err := merged.Lookup("other:openai/mini"); err == nil {
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

// A profile's own models are a third scope: an ID also in a shared scope is ambiguous without
// a prefix, and profile: picks the own entry (phase 3c.1).
func TestProfileScopePicksTheOwnEntry(t *testing.T) {
	global := newCatalog("model", []Entry{{Category: "openai", Name: "default", Path: "/g/openai/default.json"}})
	workspace := newCatalog("model", []Entry{{Category: "local", Name: "llama", Path: "/w/local/llama.json"}})
	own := newCatalog("model", []Entry{{Category: "openai", Name: "default", Path: "/p/openai/default.json"}, {Category: "openai", Name: "fast", Path: "/p/openai/fast.json"}})
	merged := MergeScopes("model", Scope{Name: ScopeGlobal, Catalog: global}, Scope{Name: ScopeWorkspace, Catalog: workspace}, Scope{Name: ScopeProfile, Catalog: own})
	if _, err := merged.Lookup("openai/default"); err == nil || !strings.Contains(err.Error(), "global:openai/default or profile:openai/default") {
		t.Fatalf("bare clash = %v", err)
	}
	for reference, path := range map[string]string{"profile:openai/default": "/p/openai/default.json", "global:openai/default": "/g/openai/default.json", "openai/fast": "/p/openai/fast.json", "profile:openai/fast": "/p/openai/fast.json"} {
		if entry, err := merged.Lookup(reference); err != nil || entry.Path != path {
			t.Fatalf("%s = %+v, %v", reference, entry, err)
		}
	}
	if _, err := merged.Lookup("workspace:openai/fast"); err == nil {
		t.Fatal("workspace: reaches an own entry")
	}
	selection, _ := Of("profile:openai")
	narrowed, err := merged.Narrow(selection)
	if err != nil || !slices.Equal(narrowed.IDs(), []string{"openai/default", "openai/fast"}) {
		t.Fatalf("profile:openai = %v, %v", narrowed, err)
	}
	if entry, _ := narrowed.Get("openai/default"); entry.Path != "/p/openai/default.json" {
		t.Fatalf("narrowed default = %+v", entry)
	}
	// Without the own scope, profile: finds nothing.
	shared := MergeScopes("model", Scope{Name: ScopeGlobal, Catalog: global}, Scope{Name: ScopeWorkspace, Catalog: workspace})
	if _, err := shared.Lookup("profile:openai/fast"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("shared profile: lookup = %v", err)
	}
}
