package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// layout writes empty .json files at the given <category>/<file> paths.
func layout(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustScan(t *testing.T, dir string) *Catalog {
	t.Helper()
	c, err := Scan("model", dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustOf(t *testing.T, texts ...string) Selection {
	t.Helper()
	s, err := Of(texts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScanReadsCategoryFolders(t *testing.T) {
	dir := layout(t, "coding/gpt-fast.json", "coding/gpt-slow.json", "cheap/mini.json")
	c := mustScan(t, dir)
	if got, want := c.Names(), []string{"gpt-fast", "gpt-slow", "mini"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if got, want := c.Categories(), []string{"cheap", "coding"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("categories = %v, want %v", got, want)
	}
	entry, ok := c.Get("mini")
	if !ok || entry.Category != "cheap" || entry.Path != filepath.Join(dir, "cheap", "mini.json") {
		t.Fatalf("mini = %+v ok=%v", entry, ok)
	}
	if _, ok := c.Get("cheap/mini"); ok {
		t.Fatal("identity is the plain file name, never a path")
	}
}

func TestScanMissingDirIsEmpty(t *testing.T) {
	c := mustScan(t, filepath.Join(t.TempDir(), "absent"))
	if c.Len() != 0 || len(c.Categories()) != 0 {
		t.Fatalf("want empty catalog, got %v", c.Names())
	}
}

func TestScanAllowsTheSameNameInTwoCategories(t *testing.T) {
	dir := layout(t, "headed/betterwright.json", "headless/betterwright.json", "code/zvec.json")
	c := mustScan(t, dir)
	if got, want := c.Names(), []string{"betterwright", "zvec"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	found := c.Find("betterwright")
	if len(found) != 2 || found[0].Category != "headed" || found[1].Category != "headless" {
		t.Fatalf("find = %+v", found)
	}
	if _, ok := c.Get("betterwright"); ok {
		t.Fatal("Get must refuse an ambiguous name")
	}
	if _, ok := c.Get("zvec"); !ok {
		t.Fatal("Get must return a unique name")
	}
	err := c.Unique()
	if err == nil {
		t.Fatal("want ambiguity error")
	}
	for _, part := range []string{`"betterwright" is in 2 categories`, filepath.Join("headed", "betterwright.json"), filepath.Join("headless", "betterwright.json"), "<category>/<name>"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q lacks %q", err, part)
		}
	}
	if strings.Contains(err.Error(), "zvec") {
		t.Fatalf("a unique name must not be reported: %v", err)
	}
}

func TestNarrowingResolvesAnAmbiguousName(t *testing.T) {
	c := mustScan(t, layout(t, "headed/betterwright.json", "headless/betterwright.json", "code/zvec.json"))
	for _, tc := range []struct {
		sel      []string
		wantCats []string
	}{
		{[]string{"category:headless", "zvec"}, []string{"headless"}},
		{[]string{"headed/betterwright"}, []string{"headed"}},
		{[]string{"betterwright"}, []string{"headed", "headless"}},
	} {
		got, err := c.Narrow(mustOf(t, tc.sel...))
		if err != nil {
			t.Fatalf("%v: %v", tc.sel, err)
		}
		var cats []string
		for _, e := range got.Find("betterwright") {
			cats = append(cats, e.Category)
		}
		if !reflect.DeepEqual(cats, tc.wantCats) {
			t.Errorf("%v: betterwright categories = %v, want %v", tc.sel, cats, tc.wantCats)
		}
		unique := got.Unique() == nil
		if unique != (len(tc.wantCats) == 1) {
			t.Errorf("%v: unique = %v", tc.sel, unique)
		}
	}
	// A qualified name that does not exist is an error naming it.
	if _, err := c.Narrow(mustOf(t, "code/betterwright")); err == nil || !strings.Contains(err.Error(), `"code/betterwright" is not available here`) {
		t.Fatalf("err = %v", err)
	}
}

func TestScanRejectsAFileOutsideACategory(t *testing.T) {
	dir := layout(t, "loose.json")
	_, err := Scan("tools", dir)
	if err == nil || !strings.Contains(err.Error(), "must live in a category folder") {
		t.Fatalf("err = %v", err)
	}
}

func TestScanRejectsBadNames(t *testing.T) {
	for _, file := range []string{"a/has space.json", "a/colon:x.json", "a/star*.json", "bad cat/x.json", "a/-lead.json"} {
		if _, err := Scan("model", layout(t, file)); err == nil {
			t.Errorf("%q: want error", file)
		}
	}
}

func TestScanIgnoresHiddenAndNonJSON(t *testing.T) {
	dir := layout(t, "a/ok.json", "a/.hidden.json", "a/notes.md", ".cache/x.json", "README.md")
	if err := os.MkdirAll(filepath.Join(dir, "a", "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := mustScan(t, dir)
	if got := c.Names(); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestParseSelector(t *testing.T) {
	for _, ok := range []string{"gpt-fast", "category:coding", "*", "a.b_c-1", "headless/betterwright"} {
		s, err := ParseSelector(ok)
		if err != nil || s.String() != ok {
			t.Errorf("%q: %v %q", ok, err, s.String())
		}
	}
	for _, bad := range []string{"", " x", "x ", "category:", "category: x", "mcp:srv", "a/", "/b", "a/b/c", "a b/c", "a*", "**", "cat:coding"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestNarrowAbsentIsEverythingAndEmptyIsNothing(t *testing.T) {
	c := mustScan(t, layout(t, "a/x.json", "b/y.json"))
	all, err := c.Narrow(All())
	if err != nil || all.Len() != 2 {
		t.Fatalf("absent: %v len=%d", err, all.Len())
	}
	none, err := c.Narrow(mustOf(t))
	if err != nil || none.Len() != 0 {
		t.Fatalf("empty: %v len=%d", err, none.Len())
	}
}

func TestNarrowByNameCategoryAndStar(t *testing.T) {
	c := mustScan(t, layout(t, "coding/a.json", "coding/b.json", "cheap/c.json", "other/d.json"))
	cases := []struct {
		sel  []string
		want []string
	}{
		{[]string{"category:coding"}, []string{"a", "b"}},
		{[]string{"category:coding", "category:cheap"}, []string{"a", "b", "c"}},
		{[]string{"d", "category:cheap"}, []string{"c", "d"}},
		{[]string{"*"}, []string{"a", "b", "c", "d"}},
		{[]string{"category:nothing"}, nil},
		{[]string{"a", "a", "category:coding"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		got, err := c.Narrow(mustOf(t, tc.sel...))
		if err != nil {
			t.Fatalf("%v: %v", tc.sel, err)
		}
		if names := got.Names(); !reflect.DeepEqual(names, tc.want) && !(len(names) == 0 && len(tc.want) == 0) {
			t.Errorf("%v: got %v want %v", tc.sel, names, tc.want)
		}
	}
}

func TestNarrowOnlyNarrows(t *testing.T) {
	root := mustScan(t, layout(t, "coding/a.json", "coding/b.json", "cheap/c.json"))
	session, err := root.Narrow(mustOf(t, "category:coding"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := session.Narrow(mustOf(t, "a"))
	if err != nil || !reflect.DeepEqual(profile.Names(), []string{"a"}) {
		t.Fatalf("profile = %v %v", profile.Names(), err)
	}
	// "c" exists in the root but is outside the session scope: an explicit error.
	_, err = session.Narrow(mustOf(t, "c"))
	if err == nil || !strings.Contains(err.Error(), `"c" is not available here`) || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("err = %v", err)
	}
	// A category outside the scope simply intersects to nothing.
	none, err := session.Narrow(mustOf(t, "category:cheap"))
	if err != nil || none.Len() != 0 {
		t.Fatalf("category outside scope: %v len=%d", err, none.Len())
	}
	// Narrowing never mutates the level above.
	if root.Len() != 3 || session.Len() != 2 {
		t.Fatalf("levels changed: root=%d session=%d", root.Len(), session.Len())
	}
}

func TestNarrowReportsEveryMissingName(t *testing.T) {
	c := mustScan(t, layout(t, "a/x.json"))
	_, err := c.Narrow(mustOf(t, "x", "p", "q"))
	if err == nil || !strings.Contains(err.Error(), `"p", "q" are not available here`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSelectionJSON(t *testing.T) {
	var doc struct {
		Models Selection `json:"models,omitzero"`
	}
	if err := json.Unmarshal([]byte(`{}`), &doc); err != nil || !doc.Models.IsAll() {
		t.Fatalf("missing key must be absent: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"models":[]}`), &doc); err != nil || doc.Models.IsAll() {
		t.Fatalf("[] must be present and empty: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"models":["category:coding","gpt-fast","*"]}`), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(doc)
	if err != nil || string(out) != `{"models":["category:coding","gpt-fast","*"]}` {
		t.Fatalf("round trip = %s %v", out, err)
	}
	absent, _ := json.Marshal(struct {
		Models Selection `json:"models,omitzero"`
	}{})
	if string(absent) != `{}` {
		t.Fatalf("absent must be omitted: %s", absent)
	}
	for _, bad := range []string{`{"models":null}`, `{"models":"gpt"}`, `{"models":[1]}`, `{"models":[" x"]}`, `{"models":["mcp:a"]}`} {
		var d struct {
			Models Selection `json:"models"`
		}
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestSelectorsReturnsACopy(t *testing.T) {
	s := mustOf(t, "a", "category:b")
	items := s.Selectors()
	items[0] = Selector{kind: selectAll}
	if s.Selectors()[0].String() != "a" {
		t.Fatal("Selectors must return a copy")
	}
	if All().Selectors() != nil {
		t.Fatal("absent selection has no items")
	}
}
