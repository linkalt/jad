package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const catalogToolsetBody = `{"schema_version":1,"native":{"read":{}},"mcp":{}}`

func TestCatalogToolsetsListReadAndCreate(t *testing.T) {
	f := newLayoutFixture(t)
	list := f.catalogCall(t, "GET", "/api/catalog/toolsets", "", nil)
	if list.status != http.StatusOK || !strings.Contains(list.body, "coding/none") {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	path := "/api/catalog/toolsets/coding/spare"
	if reply := f.catalogCall(t, "PUT", path, catalogToolsetBody, nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("create without a precondition = %d", reply.status)
	}
	created := f.catalogCall(t, "PUT", path, catalogToolsetBody, map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated || created.etag == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	read := f.catalogCall(t, "GET", path, "", nil)
	if read.status != http.StatusOK || read.body != catalogToolsetBody || read.etag != created.etag {
		t.Fatalf("read = %d %q etag %q, created %q", read.status, read.body, read.etag, created.etag)
	}
	if again := f.catalogCall(t, "PUT", path, catalogToolsetBody, map[string]string{"If-None-Match": "*"}); again.status != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing file = %d", again.status)
	}
	replaced := f.catalogCall(t, "PUT", path, `{"native":{"read":{},"grep":{}}}`, map[string]string{"If-Match": created.etag})
	if replaced.status != http.StatusOK || replaced.etag == created.etag {
		t.Fatalf("replace = %d %s", replaced.status, replaced.body)
	}
	if stale := f.catalogCall(t, "PUT", path, catalogToolsetBody, map[string]string{"If-Match": created.etag}); stale.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with the old tag = %d", stale.status)
	}
}

func TestCatalogToolsetWriteIsCheckedByTheLoader(t *testing.T) {
	f := newLayoutFixture(t)
	serverFile := filepath.Join(f.root, "mcp", "ext", "one.json")
	if err := os.MkdirAll(filepath.Dir(serverFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverFile, []byte(`{"transport":{"type":"streamable_http","url":"http://127.0.0.1:1/mcp"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.root, "mcp", "more", "one.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"transport":{"type":"streamable_http","url":"http://127.0.0.1:2/mcp"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := "/api/catalog/toolsets/coding/none"
	target := filepath.Join(f.root, "tools", "coding", "none.json")
	before := f.catalogCall(t, "GET", path, "", nil)
	cases := map[string]struct{ body, want string }{
		"empty":                  {"", "expected a JSON object"},
		"not an object":          {`[]`, "cannot unmarshal"},
		"unknown key":            {`{"bogus":1}`, "unknown field"},
		"schema version":         {`{"schema_version":2}`, "unsupported schema_version 2"},
		"unknown native tool":    {`{"native":{"nope":{}}}`, "nope"},
		"unknown native key":     {`{"native":{"read":{"bogus":1}}}`, "bogus"},
		"hooks of a wrong type":  {`{"hooks":5}`, "hooks"},
		"unknown mcp server":     {`{"mcp":{"ext/missing":{}}}`, "ext/missing"},
		"unknown mcp use key":    {`{"mcp":{"ext/one":{"bogus":true}}}`, "bogus"},
		"removed mcp approval":   {`{"mcp":{"ext/one":{"approval":"ask"}}}`, "approval was removed"},
		"same server file name":  {`{"mcp":{"ext/one":{},"more/one":{}}}`, "same file name"},
		"mcp that is not object": {`{"mcp":[]}`, "mcp must be an object"},
	}
	for name, c := range cases {
		reply := f.catalogCall(t, "PUT", path, c.body, map[string]string{"If-Match": before.etag})
		if reply.status != http.StatusBadRequest && reply.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d %s", name, reply.status, reply.body)
			continue
		}
		if !strings.Contains(reply.body, c.want) {
			t.Errorf("%s: body %q does not mention %q", name, reply.body, c.want)
		}
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != before.body {
		t.Fatalf("a refused write changed the file: %q", onDisk)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	good := `{"native":{"read":{}},"mcp":{"ext/one":{"deny_tools":["x"]}}}`
	if reply := f.catalogCall(t, "PUT", path, good, map[string]string{"If-Match": before.etag}); reply.status != http.StatusOK {
		t.Fatalf("a valid toolset naming an MCP server = %d %s", reply.status, reply.body)
	}
}

func TestCatalogToolsetDeleteIsRefusedWhileAProfileNeedsIt(t *testing.T) {
	f := newLayoutFixture(t)
	used := f.catalogCall(t, "GET", "/api/catalog/toolsets/coding/none", "", nil)
	reply := f.catalogCall(t, "DELETE", "/api/catalog/toolsets/coding/none", "", map[string]string{"If-Match": used.etag})
	if reply.status != http.StatusConflict || !strings.Contains(reply.body, "dev/coder") || !strings.Contains(reply.body, "review/reviewer") {
		t.Fatalf("delete of a used toolset = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "tools", "coding", "none.json")); err != nil {
		t.Fatalf("toolset removed although refused: %v", err)
	}

	spare := "/api/catalog/toolsets/coding/spare"
	created := f.catalogCall(t, "PUT", spare, catalogToolsetBody, map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	if reply := f.catalogCall(t, "DELETE", spare, "", nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", spare, "", map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", spare, "", map[string]string{"If-Match": created.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete of an unused toolset = %d %s", reply.status, reply.body)
	}
	if status, body := f.do(t, "GET", "/api/sessions/local:demo", ""); status != http.StatusOK {
		t.Fatalf("session no longer loads: %d %s", status, body)
	}
}
