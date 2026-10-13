package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const catalogMCPBody = `{"schema_version":1,"transport":{"type":"streamable_http","url":"http://127.0.0.1:1/mcp"}}`

func TestCatalogMCPListReadCreateAndReplace(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/mcp/ext/one"
	if reply := f.catalogCall(t, "PUT", path, catalogMCPBody, nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("create without a precondition = %d", reply.status)
	}
	created := f.catalogCall(t, "PUT", path, catalogMCPBody, map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated || created.etag == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	if list := f.catalogCall(t, "GET", "/api/catalog/mcp", "", nil); list.status != http.StatusOK || !strings.Contains(list.body, "ext/one") {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	read := f.catalogCall(t, "GET", path, "", nil)
	if read.status != http.StatusOK || read.body != catalogMCPBody || read.etag != created.etag {
		t.Fatalf("read = %d %q etag %q, created %q", read.status, read.body, read.etag, created.etag)
	}
	if again := f.catalogCall(t, "PUT", path, catalogMCPBody, map[string]string{"If-None-Match": "*"}); again.status != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing file = %d", again.status)
	}
	next := `{"transport":{"type":"stdio","command":"fixture","args":["--x"]},"timeout_ms":5000}`
	replaced := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": created.etag})
	if replaced.status != http.StatusOK || replaced.etag == created.etag {
		t.Fatalf("replace = %d %s", replaced.status, replaced.body)
	}
	if stale := f.catalogCall(t, "PUT", path, catalogMCPBody, map[string]string{"If-Match": created.etag}); stale.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with the old tag = %d", stale.status)
	}
	if weak := f.catalogCall(t, "PUT", path, catalogMCPBody, map[string]string{"If-Match": "W/" + replaced.etag}); weak.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with a weak tag = %d", weak.status)
	}
}

func TestCatalogMCPWriteIsCheckedByTheLoader(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/mcp/ext/one"
	created := f.catalogCall(t, "PUT", path, catalogMCPBody, map[string]string{"If-None-Match": "*"})
	target := filepath.Join(f.root, "mcp", "ext", "one.json")
	cases := map[string]struct{ body, want string }{
		"empty":                 {"", "expected a JSON object"},
		"not an object":         {`[]`, "expected a JSON object"},
		"schema version":        {`{"schema_version":2,"transport":{"type":"stdio","command":"x"}}`, "unsupported schema_version 2"},
		"no transport":          {`{}`, "transport must be an object"},
		"unknown transport":     {`{"transport":{"type":"pipe"}}`, "stdio or streamable_http"},
		"stdio without command": {`{"transport":{"type":"stdio"}}`, "transport.command"},
		"args of a wrong type":  {`{"transport":{"type":"stdio","command":"x","args":[1]}}`, "transport.args"},
		"bad env name":          {`{"transport":{"type":"stdio","command":"x","env":{"1bad":"v"}}}`, "transport.env"},
		"env set twice":         {`{"transport":{"type":"stdio","command":"x","env":{"A":"1"},"env_from":{"A":"B"}}}`, "must not set the same variable"},
		"url with credentials":  {`{"transport":{"type":"streamable_http","url":"http://u:p@host/mcp"}}`, "transport.url"},
		"url not http":          {`{"transport":{"type":"streamable_http","url":"ftp://host/mcp"}}`, "transport.url"},
		"bad header source":     {`{"transport":{"type":"streamable_http","url":"http://h/mcp","headers_from":{"X":"not valid"}}}`, "transport.headers_from"},
		"timeout out of range":  {`{"transport":{"type":"stdio","command":"x"},"timeout_ms":0}`, "timeout_ms"},
		"max bytes too small":   {`{"transport":{"type":"stdio","command":"x"},"max_result_bytes":10}`, "max_result_bytes"},
	}
	for name, c := range cases {
		reply := f.catalogCall(t, "PUT", path, c.body, map[string]string{"If-Match": created.etag})
		if reply.status != http.StatusBadRequest && reply.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d %s", name, reply.status, reply.body)
			continue
		}
		if !strings.Contains(reply.body, c.want) {
			t.Errorf("%s: body %q does not mention %q", name, reply.body, c.want)
		}
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != catalogMCPBody {
		t.Fatalf("a refused write changed the file: %q", onDisk)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

// A server file may name variables and a directory that only exist where a run happens.
func TestCatalogMCPWriteDoesNotCheckTheRunEnvironment(t *testing.T) {
	f := newLayoutFixture(t)
	body := `{"transport":{"type":"stdio","command":"x","cwd":"does-not-exist","env_from":{"A":"JAD_SURELY_UNSET_VARIABLE"}}}`
	reply := f.catalogCall(t, "PUT", "/api/catalog/mcp/ext/env", body, map[string]string{"If-None-Match": "*"})
	if reply.status != http.StatusCreated {
		t.Fatalf("create = %d %s", reply.status, reply.body)
	}
	body = `{"transport":{"type":"streamable_http","url":"http://h/mcp","headers_from":{"X-Key":"JAD_SURELY_UNSET_VARIABLE"}}}`
	if reply := f.catalogCall(t, "PUT", "/api/catalog/mcp/ext/header", body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
		t.Fatalf("create = %d %s", reply.status, reply.body)
	}
}

func TestCatalogMCPDeleteIsRefusedWhileAToolsetNeedsIt(t *testing.T) {
	f := newLayoutFixture(t)
	server := "/api/catalog/mcp/ext/one"
	created := f.catalogCall(t, "PUT", server, catalogMCPBody, map[string]string{"If-None-Match": "*"})
	toolset := "/api/catalog/toolsets/coding/uses-one"
	made := f.catalogCall(t, "PUT", toolset, `{"mcp":{"ext/one":{}}}`, map[string]string{"If-None-Match": "*"})
	if made.status != http.StatusCreated {
		t.Fatalf("toolset create = %d %s", made.status, made.body)
	}
	reply := f.catalogCall(t, "DELETE", server, "", map[string]string{"If-Match": created.etag})
	if reply.status != http.StatusConflict || !strings.Contains(reply.body, "coding/uses-one") {
		t.Fatalf("delete of a used server = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "mcp", "ext", "one.json")); err != nil {
		t.Fatalf("server removed although refused: %v", err)
	}
	if reply := f.catalogCall(t, "DELETE", toolset, "", map[string]string{"If-Match": made.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("toolset delete = %d %s", reply.status, reply.body)
	}
	if reply := f.catalogCall(t, "DELETE", server, "", nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", server, "", map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", server, "", map[string]string{"If-Match": created.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete of an unused server = %d %s", reply.status, reply.body)
	}
}
