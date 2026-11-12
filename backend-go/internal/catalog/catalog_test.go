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
	if got, want := c.IDs(), []string{"cheap/mini", "coding/gpt-fast", "coding/gpt-slow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if got, want := c.Categories(), []string{"cheap", "coding"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("categories = %v, want %v", got, want)
	}
	entry, ok := c.Get("cheap/mini")
	if !ok || entry.Category != "cheap" || entry.Name != "mini" || entry.ID() != "cheap/mini" ||
		entry.Path != filepath.Join(dir, "cheap", "mini.json") {
		t.Fatalf("mini = %+v ok=%v", entry, ok)
	}
	if _, ok := c.Get("mini"); ok {
		t.Fatal("identity is category/name; a bare file name is not an ID")
	}
}

func TestScanMissingDirIsEmpty(t *testing.T) {
	c := mustScan(t, filepath.Join(t.TempDir(), "absent"))
	if c.Len() != 0 || len(c.Categories()) != 0 {
		t.Fatalf("want empty catalog, got %v", c.IDs())
	}
}

// One program, two environments: the same file name in two categories are two entries
// with two IDs, and nothing is ambiguous because every reference carries its category.
func TestTheSameFileNameInTwoCategoriesAreTwoEntries(t *testing.T) {
	c := mustScan(t, layout(t, "headed/betterwright.json", "headless/betterwright.json", "code/zvec.json"))
	if got, want := c.IDs(), []string{"code/zvec", "headed/betterwright", "headless/betterwright"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for id, want := range map[string]string{"headed/betterwright": "headed", "headless/betterwright": "headless"} {
		entry, err := c.Lookup(id)
		if err != nil || entry.Category != want || entry.Name != "betterwright" {
			t.Fatalf("%s: %+v %v", id, entry, err)
		}
	}
}

func TestLookupExplainsWhatWentWrong(t *testing.T) {
	c := mustScan(t, layout(t, "headed/betterwright.json", "code/zvec.json"))
	for _, tc := range []struct{ id, want string }{
		{"betterwright", "must be written <category>/<name>"},
		{"a/b/c", "must be written <category>/<name>"},
		{"code/betterwright", `"code/betterwright" is not available here (available: code/zvec, headed/betterwright)`},
	} {
		if _, err := c.Lookup(tc.id); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.id, err, tc.want)
		}
	}
}

func TestScanSkipsAFileOutsideACategory(t *testing.T) {
	dir := layout(t, "loose.json", "a/ok.json")
	c := mustScan(t, dir)
	if got := c.IDs(); !reflect.DeepEqual(got, []string{"a/ok"}) {
		t.Fatalf("ids = %v, want only a/ok", got)
	}
	skipped := c.Skipped()
	if len(skipped) != 1 || skipped[0].Path != filepath.Join(dir, "loose.json") || !strings.Contains(skipped[0].Reason, "must live in a category folder") {
		t.Fatalf("skipped = %v", skipped)
	}
}

// A name that is not an identifier leaves that one file (or category folder) out, with its path
// and the rule it broke; every other entry of the kind is still scanned.
func TestScanSkipsBadNamesAndKeepsTheRest(t *testing.T) {
	files := []string{"a/has space.json", "a/file (1).json", "a/colon:x.json", "a/star*.json", "bad cat/x.json", "a/-lead.json", "a/_lead.json", "a/good.json", "b/ünï.json", "ok/fine.json"}
	dir := layout(t, files...)
	c := mustScan(t, dir)
	if got, want := c.IDs(), []string{"a/good", "ok/fine"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	var paths []string
	for _, skipped := range c.Skipped() {
		paths = append(paths, strings.TrimPrefix(skipped.Path, dir+string(filepath.Separator)))
		if skipped.Reason == "" || !strings.Contains(skipped.String(), skipped.Path) {
			t.Errorf("skipped %+v has no usable reason", skipped)
		}
	}
	want := []string{"a/-lead.json", "a/_lead.json", "a/colon:x.json", "a/file (1).json", "a/has space.json", "a/star*.json", "b/ünï.json", "bad cat"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("skipped paths = %q, want %q", paths, want)
	}
}

// A skipped name cannot be named by any reference: it is not a valid identifier, so a lookup or
// a selector fails on the shape, and the valid sibling of a similar name is not mistaken for it.
func TestASkippedNameCannotBeReferenced(t *testing.T) {
	c := mustScan(t, layout(t, "a/file (1).json", "a/file.json"))
	if _, err := c.Lookup("a/file (1)"); err == nil || !strings.Contains(err.Error(), "must be written <category>/<name>") {
		t.Errorf("Lookup of a skipped name: err = %v", err)
	}
	if _, err := ParseSelector("a/file (1)"); err == nil {
		t.Error("a selector for a skipped name must be refused")
	}
	if _, ok := c.Get("a/file (1)"); ok {
		t.Error("a skipped name is in the catalog")
	}
	if got := c.IDs(); !reflect.DeepEqual(got, []string{"a/file"}) {
		t.Errorf("ids = %v", got)
	}
}

// What was skipped reappears once it is renamed: a scan is a fresh read of the folders.
func TestARenamedFileIsScannedAgain(t *testing.T) {
	dir := layout(t, "a/file (1).json")
	if c := mustScan(t, dir); c.Len() != 0 || len(c.Skipped()) != 1 {
		t.Fatalf("before the rename: ids = %v, skipped = %v", c.IDs(), c.Skipped())
	}
	if err := os.Rename(filepath.Join(dir, "a", "file (1).json"), filepath.Join(dir, "a", "file-1.json")); err != nil {
		t.Fatal(err)
	}
	if c := mustScan(t, dir); !reflect.DeepEqual(c.IDs(), []string{"a/file-1"}) || len(c.Skipped()) != 0 {
		t.Fatalf("after the rename: ids = %v, skipped = %v", c.IDs(), c.Skipped())
	}
}

// Files that are not configuration are ignored as before, with no report.
func TestScanIgnoresFilesThatAreNotConfiguration(t *testing.T) {
	dir := layout(t, "a/ok.json", ".hidden/x.json", "a/.dot.json", "a/notes.txt", "README.md")
	if err := os.MkdirAll(filepath.Join(dir, "a", "folder.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := mustScan(t, dir)
	if !reflect.DeepEqual(c.IDs(), []string{"a/ok"}) || len(c.Skipped()) != 0 {
		t.Fatalf("ids = %v, skipped = %v", c.IDs(), c.Skipped())
	}
}

// ScanObserved lists the category folders it reads, so a caller's cache sees a rename inside them.
func TestScanObservedStillSeesTheFoldersAroundASkippedName(t *testing.T) {
	dir := layout(t, "a/file (1).json", "bad cat/x.json")
	var seen []string
	if _, err := ScanObserved("model", dir, func(folder string) { seen = append(seen, filepath.Base(folder)) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{filepath.Base(dir), "a"}) {
		t.Fatalf("observed %v", seen)
	}
}

func TestScanIgnoresHiddenAndNonJSON(t *testing.T) {
	dir := layout(t, "a/ok.json", "a/.hidden.json", "a/notes.md", ".cache/x.json", "README.md")
	if err := os.MkdirAll(filepath.Join(dir, "a", "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := mustScan(t, dir)
	if got := c.IDs(); !reflect.DeepEqual(got, []string{"a/ok"}) {
		t.Fatalf("ids = %v", got)
	}
}

func TestParseSelector(t *testing.T) {
	for _, ok := range []string{"coding", "coding/gpt-fast", "*", "a.b_c-1", "headless/betterwright"} {
		s, err := ParseSelector(ok)
		if err != nil || s.String() != ok {
			t.Errorf("%q: %v %q", ok, err, s.String())
		}
	}
	for _, bad := range []string{"", " x", "x ", "category:coding", "mcp:srv", "a:b", "a/", "/b", "a/b/c", "a b/c", "a*", "**"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if _, err := ParseSelector("category:coding"); err == nil || !strings.Contains(err.Error(), "no prefix is used") {
		t.Errorf("the old prefix must be refused with an explanation: %v", err)
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

func TestNarrowByCategoryFileAndStar(t *testing.T) {
	c := mustScan(t, layout(t, "coding/a.json", "coding/b.json", "cheap/c.json", "other/d.json"))
	cases := []struct {
		sel  []string
		want []string
	}{
		{[]string{"coding"}, []string{"coding/a", "coding/b"}},
		{[]string{"coding", "cheap"}, []string{"cheap/c", "coding/a", "coding/b"}},
		{[]string{"other/d", "cheap"}, []string{"cheap/c", "other/d"}},
		{[]string{"*"}, []string{"cheap/c", "coding/a", "coding/b", "other/d"}},
		{[]string{"coding/a", "coding/a", "coding"}, []string{"coding/a", "coding/b"}},
	}
	for _, tc := range cases {
		got, err := c.Narrow(mustOf(t, tc.sel...))
		if err != nil {
			t.Fatalf("%v: %v", tc.sel, err)
		}
		if ids := got.IDs(); !reflect.DeepEqual(ids, tc.want) {
			t.Errorf("%v: got %v want %v", tc.sel, ids, tc.want)
		}
	}
}

func TestNarrowPicksOneEnvironmentOfASharedName(t *testing.T) {
	c := mustScan(t, layout(t, "headed/betterwright.json", "headless/betterwright.json"))
	got, err := c.Narrow(mustOf(t, "headless/betterwright"))
	if err != nil || !reflect.DeepEqual(got.IDs(), []string{"headless/betterwright"}) {
		t.Fatalf("got %v %v", got.IDs(), err)
	}
}

func TestNarrowOnlyNarrows(t *testing.T) {
	root := mustScan(t, layout(t, "coding/a.json", "coding/b.json", "cheap/c.json"))
	session, err := root.Narrow(mustOf(t, "coding"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := session.Narrow(mustOf(t, "coding/a"))
	if err != nil || !reflect.DeepEqual(profile.IDs(), []string{"coding/a"}) {
		t.Fatalf("profile = %v %v", profile.IDs(), err)
	}
	// cheap/c exists in the root but is outside the session scope: an explicit error,
	// for a file and for the whole category alike.
	for _, outside := range []string{"cheap/c", "cheap"} {
		_, err = session.Narrow(mustOf(t, outside))
		if err == nil || !strings.Contains(err.Error(), `"`+outside+`" is not available here`) ||
			!strings.Contains(err.Error(), "coding/a, coding/b") {
			t.Fatalf("%s: err = %v", outside, err)
		}
	}
	// Narrowing never mutates the level above.
	if root.Len() != 3 || session.Len() != 2 {
		t.Fatalf("levels changed: root=%d session=%d", root.Len(), session.Len())
	}
}

func TestNarrowReportsEveryMissingItem(t *testing.T) {
	c := mustScan(t, layout(t, "a/x.json"))
	_, err := c.Narrow(mustOf(t, "a", "p", "a/q"))
	if err == nil || !strings.Contains(err.Error(), `"p", "a/q" are not available here`) {
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
	if err := json.Unmarshal([]byte(`{"models":["coding","coding/gpt-fast","*"]}`), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(doc)
	if err != nil || string(out) != `{"models":["coding","coding/gpt-fast","*"]}` {
		t.Fatalf("round trip = %s %v", out, err)
	}
	absent, _ := json.Marshal(struct {
		Models Selection `json:"models,omitzero"`
	}{})
	if string(absent) != `{}` {
		t.Fatalf("absent must be omitted: %s", absent)
	}
	for _, bad := range []string{`{"models":null}`, `{"models":"gpt"}`, `{"models":[1]}`, `{"models":[" x"]}`, `{"models":["mcp:a"]}`, `{"models":["category:a"]}`} {
		var d struct {
			Models Selection `json:"models"`
		}
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestSelectorsReturnsACopy(t *testing.T) {
	s := mustOf(t, "a", "b/c")
	items := s.Selectors()
	items[0] = Selector{kind: selectAll}
	if s.Selectors()[0].String() != "a" {
		t.Fatal("Selectors must return a copy")
	}
	if All().Selectors() != nil {
		t.Fatal("absent selection has no items")
	}
}
