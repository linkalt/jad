package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateFileExclusiveRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b.json")
	if err := createFileExclusive(path, []byte(`{"first":1}`), false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := createFileExclusive(path, []byte(`{"second":2}`), false)
	var refused *CatalogError
	if !errors.As(err, &refused) || refused.Kind != CatalogPreconditionFailed {
		t.Fatalf("second create error = %v, want a failed precondition", err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"first":1}` {
		t.Fatalf("the existing file was replaced: %q", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary file left: %v", entries)
	}
}

func TestCheckWritePreconditionCases(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "a.json")
	if err := os.WriteFile(existing, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "b.json")
	tag := CatalogETag([]byte(`{"v":1}`))
	cases := []struct {
		name      string
		path      string
		condition WriteCondition
		exists    bool
		want      CatalogErrorKind
		ok        bool
	}{
		{"replace with the current tag", existing, WriteCondition{IfMatch: tag}, true, 0, true},
		{"replace with any (*)", existing, WriteCondition{IfMatch: "*"}, true, 0, true},
		{"replace with a stale tag", existing, WriteCondition{IfMatch: `"x"`}, false, CatalogPreconditionFailed, false},
		{"replace with a weak tag", existing, WriteCondition{IfMatch: "W/" + tag}, false, CatalogPreconditionFailed, false},
		{"replace with no header", existing, WriteCondition{}, false, CatalogPreconditionRequired, false},
		{"create over an existing file", existing, WriteCondition{IfNoneMatch: "*"}, false, CatalogPreconditionFailed, false},
		{"create a new file", missing, WriteCondition{IfNoneMatch: "*"}, false, 0, true},
		{"create without the header", missing, WriteCondition{}, false, CatalogPreconditionRequired, false},
		{"create with a wrong header value", missing, WriteCondition{IfNoneMatch: `"x"`}, false, CatalogPreconditionRequired, false},
		{"replace a missing file", missing, WriteCondition{IfMatch: tag}, false, CatalogPreconditionFailed, false},
	}
	for _, tc := range cases {
		exists, info, err := checkWritePrecondition(tc.path, "profile", "a/b", tc.condition)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
				continue
			}
			if exists != (tc.path == existing) || (exists && info == nil) {
				t.Errorf("%s: exists = %v info = %v", tc.name, exists, info)
			}
			continue
		}
		var refused *CatalogError
		if !errors.As(err, &refused) || refused.Kind != tc.want {
			t.Errorf("%s: error = %v, want kind %d", tc.name, err, tc.want)
		}
	}
}
