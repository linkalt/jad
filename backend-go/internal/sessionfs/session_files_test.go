package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
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
	if err != nil || location.Reference != "demo" || location.Directory != filepath.Join(one[0].Directory, "demo") {
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

func TestListSessionFilesSkipsRootsAndFoldersWithoutALayoutSession(t *testing.T) {
	roots := sessionRoots(t, 2)
	write := func(root Root, name, body string) {
		dir := filepath.Join(root.Directory, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(roots[0], "one", `{"a":1}`)
	write(roots[0], "empty", "")
	write(roots[1], "two", `{"b":2}`)
	roots = append(roots, Root{ID: "plain", Directory: t.TempDir()})
	write(roots[2], "legacy", `{"c":3}`)
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

func TestDeleteSessionFileKeepsOrRemovesState(t *testing.T) {
	for _, deleteState := range []bool{false, true} {
		roots := sessionRoots(t, 1)
		dir := filepath.Join(roots[0].Directory, "demo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"x":1}`)
		for name, data := range map[string][]byte{"session.json": body, sessionLockFileName: nil} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
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
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("state=%v: the session folder is still there", deleteState)
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
