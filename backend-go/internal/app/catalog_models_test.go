package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *layoutFixture) modelBody(name string) string {
	existing, err := os.ReadFile(filepath.Join(f.root, "models", "openai", "small.json"))
	if err != nil {
		panic(err)
	}
	var fields struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(existing, &fields); err != nil {
		panic(err)
	}
	return `{"provider":"openai","model":"` + name + `","api_key_env":"FIXTURE_KEY","base_url":"` + fields.BaseURL + `"}`
}

func TestCatalogModelsListReadAndCreate(t *testing.T) {
	f := newLayoutFixture(t)
	list := f.catalogCall(t, "GET", "/api/catalog/models", "", nil)
	if list.status != http.StatusOK || !strings.Contains(list.body, "openai/large") || !strings.Contains(list.body, "openai/small") {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	path := "/api/catalog/models/openai/extra"
	if reply := f.catalogCall(t, "PUT", path, f.modelBody("model-extra"), nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("create without a precondition = %d", reply.status)
	}
	created := f.catalogCall(t, "PUT", path, f.modelBody("model-extra"), map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated || created.etag == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	read := f.catalogCall(t, "GET", path, "", nil)
	if read.status != http.StatusOK || read.body != f.modelBody("model-extra") || read.etag != created.etag {
		t.Fatalf("read = %d %q etag %q", read.status, read.body, read.etag)
	}
	if again := f.catalogCall(t, "PUT", path, f.modelBody("x"), map[string]string{"If-None-Match": "*"}); again.status != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing model = %d", again.status)
	}
}

func TestCatalogModelWriteIsCheckedByTheLoader(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/models/openai/small"
	target := filepath.Join(f.root, "models", "openai", "small.json")
	before := f.catalogCall(t, "GET", path, "", nil)
	cases := map[string]struct{ body, want string }{
		"empty":            {``, "file is empty"},
		"not json":         {`{"model":`, "invalid JSON"},
		"not an object":    {`[1]`, "expected a JSON object"},
		"no model":         {`{"provider":"openai"}`, "model"},
		"bad version":      {`{"schema_version":7,"model":"m"}`, "schema_version"},
		"bad base_url":     {`{"model":"m","base_url":"ftp://x"}`, "base_url"},
		"bad auth mode":    {`{"model":"m","auth_mode":"basic"}`, "auth_mode"},
		"bad env name":     {`{"model":"m","api_key_env":"not valid"}`, "api_key_env"},
		"instructions":     {`{"model":"m","instructions":"x"}`, "instructions"},
		"reserved request": {`{"model":"m","request":{"input":1}}`, "request.input"},
		"oauth file":       {`{"model":"m","oauth_auth_file":"/x"}`, "oauth_auth_file"},
		"function tool":    {`{"model":"m","hosted_tools":[{"type":"function"}]}`, "hosted_tools"},
	}
	for name, tc := range cases {
		reply := f.catalogCall(t, "PUT", path, tc.body, map[string]string{"If-Match": before.etag})
		if reply.status != http.StatusUnprocessableEntity || !strings.Contains(reply.body, tc.want) {
			t.Errorf("%s: status %d body %s, want 422 naming %q", name, reply.status, reply.body, tc.want)
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
}

func TestCatalogModelReplaceTakesEffectOnTheNextRun(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/models/openai/small"
	before := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "PUT", path, f.modelBody("model-renamed"), map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with a stale tag = %d", reply.status)
	}
	replaced := f.catalogCall(t, "PUT", path, f.modelBody("model-renamed"), map[string]string{"If-Match": before.etag})
	if replaced.status != http.StatusOK || replaced.etag == before.etag {
		t.Fatalf("replace = %d %s", replaced.status, replaced.body)
	}
	status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"hi"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run after the edit = %d %s", status, body)
	}
	if got := f.lastModel(); got != "model-renamed" {
		t.Fatalf("the run used %q, want the edited model", got)
	}
}

func TestCatalogModelDeleteIsRefusedWhileAProfileNeedsIt(t *testing.T) {
	f := newLayoutFixture(t)
	create := func(path, body string) {
		t.Helper()
		if reply := f.catalogCall(t, "PUT", path, body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
			t.Fatalf("create %s = %d %s", path, reply.status, reply.body)
		}
	}
	remove := func(path string) catalogReply {
		t.Helper()
		current := f.catalogCall(t, "GET", path, "", nil)
		if current.status != http.StatusOK {
			t.Fatalf("read %s = %d", path, current.status)
		}
		return f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag})
	}

	// listed by name in two profiles
	if reply := remove("/api/catalog/models/openai/large"); reply.status != http.StatusConflict ||
		!strings.Contains(reply.body, "dev/coder") || !strings.Contains(reply.body, "review/reviewer") {
		t.Fatalf("delete of a listed model = %d %s, want 409 naming both profiles", reply.status, reply.body)
	}
	// the only model of a category that a profile selects by category
	create("/api/catalog/models/solo/only", f.modelBody("model-solo"))
	create("/api/catalog/profiles/dev/solo", `{"models":["solo"],"toolset":"coding/none"}`)
	if reply := remove("/api/catalog/models/solo/only"); reply.status != http.StatusConflict || !strings.Contains(reply.body, "dev/solo") {
		t.Fatalf("delete of the last model of a selected category = %d %s, want 409 naming dev/solo", reply.status, reply.body)
	}
	// one of several models of a category that a profile selects by category
	create("/api/catalog/models/openai/spare", f.modelBody("model-spare"))
	create("/api/catalog/profiles/dev/wide", `{"models":["openai"],"model_default":"openai/small","toolset":"coding/none"}`)
	if reply := remove("/api/catalog/models/openai/spare"); reply.status != http.StatusNoContent {
		t.Fatalf("delete of a model nothing needs = %d %s", reply.status, reply.body)
	}
	for _, id := range []string{"openai/large", "solo/only"} {
		if _, err := os.Stat(filepath.Join(f.root, "models", filepath.FromSlash(id)+".json")); err != nil {
			t.Fatalf("%s removed although refused: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "models", "openai", "spare.json")); err == nil {
		t.Fatal("spare model still there after delete")
	}
	if status, body := f.do(t, "GET", "/api/sessions/local:demo", ""); status != http.StatusOK {
		t.Fatalf("session no longer loads: %d %s", status, body)
	}
}

func TestCatalogModelDeleteNeedsIfMatch(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/models/openai/spare"
	if reply := f.catalogCall(t, "PUT", path, f.modelBody("m"), map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
		t.Fatalf("create = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", path, "", nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d", reply.status)
	}
}
