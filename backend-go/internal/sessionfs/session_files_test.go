package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sessionRoots(t *testing.T, count int) []Root {
	t.Helper()
	base := t.TempDir()
	roots := make([]Root, 0, count)
	for index := 0; index < count; index++ {
		id := string(rune('a' + index))
		directory := filepath.Join(base, id)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, Root{ID: id, Directory: directory, Layout: &Layout{State: filepath.Join(base, "state-"+id)}})
	}
	return roots
}

func catalogErrorKind(err error) (CatalogErrorKind, bool) {
	var refused *CatalogError
	if errors.As(err, &refused) {
		return refused.Kind, true
	}
	return 0, false
}

func TestNewLayoutSessionNeedsAnUnambiguousRoot(t *testing.T) {
	one := sessionRoots(t, 1)
	location, err := newLayoutSession(one, "demo")
	if err != nil || location.Reference != "demo" || location.File != filepath.Join(one[0].Directory, "demo.json") {
		t.Fatalf("one root: %+v %v", location, err)
	}
	if location, err = newLayoutSession(one, "a:demo"); err != nil || location.SessionID != "demo" {
		t.Fatalf("qualified in one root: %+v %v", location, err)
	}
	two := sessionRoots(t, 2)
	if _, err := newLayoutSession(two, "demo"); err == nil {
		t.Fatal("an unqualified name was accepted with two roots")
	}
	if location, err = newLayoutSession(two, "b:demo"); err != nil || location.Reference != "b:demo" || location.Root.ID != "b" {
		t.Fatalf("qualified in two roots: %+v %v", location, err)
	}
	for _, bad := range []string{"", "..", "a:..", "z:demo", "a/b", "a:b:c", "-x"} {
		if _, err := newLayoutSession(two, bad); err == nil {
			t.Errorf("newLayoutSession(%q) accepted a bad reference", bad)
		} else if kind, ok := catalogErrorKind(err); !ok || kind != CatalogBadID {
			t.Errorf("newLayoutSession(%q) error = %v, want a bad id", bad, err)
		}
	}
	plain := []Root{{ID: "p", Directory: t.TempDir()}}
	if _, err := newLayoutSession(plain, "demo"); err == nil {
		t.Fatal("a root without the catalog layout accepted a session")
	}
}

func TestListSessionFilesListsOnlyIDDotJSONFilesOfRootsWithALayout(t *testing.T) {
	roots := sessionRoots(t, 2)
	write := func(root Root, name, body string) {
		if err := os.MkdirAll(root.Directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root.Directory, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(roots[0], "one.json", `{"a":1}`)
	write(roots[0], "notes.txt", "x")
	write(roots[0], ".one.json.jad-lock", "")
	write(roots[0], "one.json.tmp-123", "{}")
	write(roots[0], "bad name.json", "{}")
	if err := os.Mkdir(filepath.Join(roots[0].Directory, "dir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(roots[1], "two.json", `{"b":2}`)
	roots = append(roots, Root{ID: "plain", Directory: t.TempDir()})
	write(roots[2], "legacy.json", `{"c":3}`)
	files, err := ListSessionFiles(roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].ID != "a:one" || files[1].ID != "b:two" {
		t.Fatalf("files = %+v", files)
	}
	if files[0].ETag != CatalogETag([]byte(`{"a":1}`)) {
		t.Fatalf("etag = %s", files[0].ETag)
	}
}

func TestListSessionFilesReportsTheNameAFileSets(t *testing.T) {
	roots := sessionRoots(t, 1)
	for id, body := range map[string]string{
		"named":    `{"name":"Main work","cwd":"/x"}`,
		"unnamed":  `{"cwd":"/x"}`,
		"notjson":  `{"name":`,
		"wrongtyp": `{"name":7}`,
	} {
		if err := os.WriteFile(filepath.Join(roots[0].Directory, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ListSessionFiles(roots)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range files {
		got[file.ID] = file.Name
	}
	want := map[string]string{"named": "Main work", "unnamed": "", "notjson": "", "wrongtyp": ""}
	if len(got) != len(want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("name of %s = %q, want %q", id, got[id], name)
		}
	}
}

func TestDeleteSessionFileKeepsOrRemovesState(t *testing.T) {
	for _, deleteState := range []bool{false, true} {
		roots := sessionRoots(t, 1)
		file := filepath.Join(roots[0].Directory, "demo.json")
		lock := filepath.Join(roots[0].Directory, ".demo.json.jad-lock")
		body := []byte(`{"x":1}`)
		for path, data := range map[string][]byte{file: body, lock: nil} {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		state := filepath.Join(roots[0].Layout.State, "demo", "dev", "coder")
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "messages.jsonl"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := DeleteSessionFile(roots, "demo", CatalogETag(body), deleteState)
		if err != nil {
			t.Fatalf("state=%v: %v", deleteState, err)
		}
		for _, path := range []string{file, lock} {
			if _, err := os.Stat(path); err == nil {
				t.Errorf("state=%v: %s is still there", deleteState, path)
			}
		}
		_, stateErr := os.Stat(state)
		if deleteState && (stateErr == nil || removed != filepath.Join(roots[0].Layout.State, "demo")) {
			t.Errorf("state=%v: state still there (%v) or wrong folder reported: %q", deleteState, stateErr, removed)
		}
		if !deleteState && (stateErr != nil || removed != "") {
			t.Errorf("state=%v: state lost (%v) or a folder was reported: %q", deleteState, stateErr, removed)
		}
	}
}

func TestSessionFilesWithDottedIDsAreListedReadAndDeleted(t *testing.T) {
	roots := sessionRoots(t, 1)
	for _, id := range []string{"v1.2", "a.json", "x-y_z"} {
		if err := os.WriteFile(filepath.Join(roots[0].Directory, id+".json"), []byte(`{"cwd":"/x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ListSessionFiles(roots)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, file := range files {
		listed[file.ID] = true
	}
	if len(listed) != 3 || !listed["v1.2"] || !listed["a.json"] || !listed["x-y_z"] {
		t.Fatalf("listed %v", listed)
	}
	if file, err := ReadSessionFile(roots, "a.json"); err != nil || string(file.Data) != `{"cwd":"/x"}` {
		t.Fatalf("ReadSessionFile(a.json) = %q, %v", file.Data, err)
	}
	if _, err := DeleteSessionFile(roots, "a.json", CatalogETag([]byte(`{"cwd":"/x"}`)), false); err != nil {
		t.Fatalf("DeleteSessionFile(a.json) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(roots[0].Directory, "a.json.json")); !os.IsNotExist(err) {
		t.Fatalf("a.json.json still exists after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(roots[0].Directory, "v1.2.json")); err != nil {
		t.Fatalf("deleting a.json removed another session: %v", err)
	}
	if _, err := newLayoutSession(roots, ".hidden"); err == nil {
		t.Fatal(".hidden was accepted as a session id")
	}
}

func TestTheFolderFormOfASessionIsNeitherListedNorRead(t *testing.T) {
	roots := sessionRoots(t, 1)
	folder := filepath.Join(roots[0].Directory, "old")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "session.json"), []byte(`{"cwd":"/x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := ListSessionFiles(roots)
	if err != nil || len(files) != 0 {
		t.Fatalf("ListSessionFiles = %+v, %v; want nothing", files, err)
	}
	if _, err := ReadSessionFile(roots, "old"); err == nil {
		t.Fatal("a session in the old folder form was read")
	} else if kind, ok := catalogErrorKind(err); !ok || kind != CatalogNotFound {
		t.Fatalf("ReadSessionFile(old) error = %v, want not found", err)
	}
}

func TestWriteSessionFileRefusesAnIDAnotherRootHolds(t *testing.T) {
	roots := sessionRoots(t, 2)
	original := filepath.Join(roots[0].Directory, "demo.json")
	if err := os.WriteFile(original, []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, created, err := WriteSessionFile(roots, "b:demo", []byte(`{"x":2}`), WriteCondition{IfNoneMatch: "*"}, false)
	kind, ok := catalogErrorKind(err)
	if err == nil || created || !ok || kind != CatalogInUse {
		t.Fatalf("created=%v err=%v kind=%v", created, err, kind)
	}
	for _, want := range []string{`"demo"`, `"a"`, `"b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	entries, readErr := os.ReadDir(roots[1].Directory)
	if readErr != nil || len(entries) != 0 {
		t.Errorf("root b was written to: %v %v", entries, readErr)
	}
	if data, _ := os.ReadFile(original); string(data) != `{"x":1}` {
		t.Errorf("original changed: %s", data)
	}
}
