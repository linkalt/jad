package catalog

import (
	"reflect"
	"testing"
)

func TestWithoutDropsOneEntryAndKeepsTheOriginal(t *testing.T) {
	all := newCatalog("profile", []Entry{{Category: "a", Name: "x"}, {Category: "a", Name: "y"}, {Category: "b", Name: "x"}})
	left := all.Without("a/y")
	if got, want := left.IDs(), []string{"a/x", "b/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Without ids = %v, want %v", got, want)
	}
	if _, found := left.Get("a/y"); found {
		t.Fatal("removed entry still found")
	}
	if all.Len() != 3 {
		t.Fatalf("original changed: %v", all.IDs())
	}
	if got := all.Without("c/none").IDs(); len(got) != 3 {
		t.Fatalf("removing an unknown id changed the catalog: %v", got)
	}
}
