package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type catalogReply struct {
	status int
	body   string
	etag   string
}

func (f *layoutFixture) catalogCall(t *testing.T, method, path, body string, headers map[string]string) catalogReply {
	t.Helper()
	request, err := http.NewRequest(method, f.api.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headers["Authorization"] == "" {
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return catalogReply{status: response.StatusCode, body: string(data), etag: response.Header.Get("ETag")}
}

const catalogProfileBody = `{"models":["openai/small"],"model_default":"openai/small","toolset":"coding/none"}`

func TestCatalogProfilesListAndRead(t *testing.T) {
	f := newLayoutFixture(t)
	list := f.catalogCall(t, "GET", "/api/catalog/profiles", "", nil)
	if list.status != http.StatusOK {
		t.Fatalf("list status = %d %s", list.status, list.body)
	}
	var listing struct {
		Files []struct{ ID, ETag string }
	}
	if err := json.Unmarshal([]byte(list.body), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Files) != 2 || listing.Files[0].ID != "dev/coder" || listing.Files[1].ID != "review/reviewer" {
		t.Fatalf("listing = %+v", listing.Files)
	}
	if strings.Contains(list.body, `"name"`) {
		t.Fatalf("unnamed profiles must list without a name: %s", list.body)
	}
	read := f.catalogCall(t, "GET", "/api/catalog/profiles/dev/coder", "", nil)
	if read.status != http.StatusOK || read.etag == "" || read.etag != listing.Files[0].ETag {
		t.Fatalf("read = %d etag %q, listed %q", read.status, read.etag, listing.Files[0].ETag)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.root, "profiles", "dev", "coder.json"))
	if err != nil || read.body != string(onDisk) {
		t.Fatalf("read body is not the file bytes: %q vs %q (%v)", read.body, onDisk, err)
	}
	if missing := f.catalogCall(t, "GET", "/api/catalog/profiles/dev/nope", "", nil); missing.status != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.status)
	}
	if bad := f.catalogCall(t, "GET", "/api/catalog/profiles/dev/..%2Fcoder", "", nil); bad.status != http.StatusBadRequest {
		t.Fatalf("path escape status = %d %s", bad.status, bad.body)
	}
	if unknown := f.catalogCall(t, "GET", "/api/catalog/widgets", "", nil); unknown.status != http.StatusNotFound {
		t.Fatalf("unknown catalog status = %d", unknown.status)
	}
}

func TestCatalogRoutesNeedTheToken(t *testing.T) {
	f := newLayoutFixture(t)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		reply := f.catalogCall(t, method, "/api/catalog/profiles/dev/coder", "{}", map[string]string{"Authorization": "Bearer wrong"})
		if reply.status != http.StatusUnauthorized {
			t.Fatalf("%s without the token = %d", method, reply.status)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "profiles", "dev", "coder.json")); err != nil {
		t.Fatalf("profile file lost: %v", err)
	}
}

func TestCatalogProfileCreateNeedsIfNoneMatch(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/profiles/dev/extra"
	target := filepath.Join(f.root, "profiles", "dev", "extra.json")

	if reply := f.catalogCall(t, "PUT", path, catalogProfileBody, nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("create without a precondition = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file written without a precondition")
	}
	if reply := f.catalogCall(t, "PUT", path, catalogProfileBody, map[string]string{"If-Match": `"x"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("If-Match on a new file = %d", reply.status)
	}
	created := f.catalogCall(t, "PUT", path, catalogProfileBody, map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated || created.etag == "" {
		t.Fatalf("create = %d %s etag %q", created.status, created.body, created.etag)
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != catalogProfileBody {
		t.Fatalf("stored %q", onDisk)
	}
	if again := f.catalogCall(t, "PUT", path, catalogProfileBody, map[string]string{"If-None-Match": "*"}); again.status != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing file = %d", again.status)
	}
	status, body := f.do(t, "GET", "/api/sessions/local:demo", "")
	if status != http.StatusOK || !strings.Contains(body, "dev/extra") {
		t.Fatalf("the new profile is not offered to the session: %d %s", status, body)
	}
}

func TestCatalogProfileReplaceNeedsTheCurrentVersion(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/profiles/dev/coder"
	target := filepath.Join(f.root, "profiles", "dev", "coder.json")
	before := f.catalogCall(t, "GET", path, "", nil)
	next := `{"models":["openai/small","openai/large"],"model_default":"openai/large","toolset":"coding/none"}`

	if reply := f.catalogCall(t, "PUT", path, next, nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("replace without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with a stale tag = %d", reply.status)
	}
	if reply := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": "W/" + before.etag}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("a weak tag must not satisfy If-Match, got %d", reply.status)
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != before.body {
		t.Fatalf("a refused write changed the file: %q", onDisk)
	}
	replaced := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": before.etag})
	if replaced.status != http.StatusOK || replaced.etag == "" || replaced.etag == before.etag {
		t.Fatalf("replace = %d %s etag %q", replaced.status, replaced.body, replaced.etag)
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != next {
		t.Fatalf("stored %q", onDisk)
	}
	if stale := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": before.etag}); stale.status != http.StatusPreconditionFailed {
		t.Fatalf("the old tag still works after a write: %d", stale.status)
	}
	status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"hi"}`)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("run after the edit = %d %s", status, body)
	}
}

func TestCatalogProfileWriteIsCheckedByTheLoader(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/profiles/dev/coder"
	target := filepath.Join(f.root, "profiles", "dev", "coder.json")
	before := f.catalogCall(t, "GET", path, "", nil)
	cases := map[string]struct{ body, want string }{
		"unknown key":      {`{"models":["openai/small"],"nope":1}`, "nope"},
		"not json":         {`{"models":`, "coder.json"},
		"empty":            {``, "expected a JSON object"},
		"trailing data":    {`{} {}`, "unexpected data"},
		"unknown model":    {`{"models":["openai/missing"]}`, "openai/missing"},
		"default not kept": {`{"models":["openai/small"],"model_default":"openai/large"}`, "model_default"},
		"unknown toolset":  {`{"toolset":"coding/missing"}`, "coding/missing"},
		"bad version":      {`{"schema_version":7}`, "schema_version"},
		"empty prompt":     {`{"prompt_documents":[" "]}`, "prompt_documents"},
		"setting key":      {`{"settings":{"model":"x"}}`, "settings may not carry"},
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
	if bad := f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/a%20b", `{}`, map[string]string{"If-None-Match": "*"}); bad.status != http.StatusBadRequest {
		t.Fatalf("bad id = %d %s", bad.status, bad.body)
	}
}

func TestCatalogProfileWriteLeavesNoTemporaryFile(t *testing.T) {
	f := newLayoutFixture(t)
	path := "/api/catalog/profiles/dev/coder"
	before := f.catalogCall(t, "GET", path, "", nil)
	f.catalogCall(t, "PUT", path, `{"models":[`, map[string]string{"If-Match": before.etag})
	f.catalogCall(t, "PUT", path, catalogProfileBody, map[string]string{"If-Match": before.etag})
	f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/extra", catalogProfileBody, map[string]string{"If-None-Match": "*"})
	entries, err := os.ReadDir(filepath.Join(f.root, "profiles", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestCatalogProfileDelete(t *testing.T) {
	f := newLayoutFixture(t)
	extra := "/api/catalog/profiles/dev/extra"
	created := f.catalogCall(t, "PUT", extra, catalogProfileBody, map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	if reply := f.catalogCall(t, "DELETE", extra, "", nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", extra, "", map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d", reply.status)
	}
	target := filepath.Join(f.root, "profiles", "dev", "extra.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("a refused delete removed the file: %v", err)
	}
	if reply := f.catalogCall(t, "DELETE", extra, "", map[string]string{"If-Match": created.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("file still there after delete")
	}
	if reply := f.catalogCall(t, "DELETE", extra, "", map[string]string{"If-Match": created.etag}); reply.status != http.StatusNotFound {
		t.Fatalf("delete of a missing file = %d", reply.status)
	}
}

func TestCatalogProfileDeleteRefusedWhileASessionNeedsIt(t *testing.T) {
	f := newLayoutFixture(t)
	for _, id := range []string{"dev/coder", "review/reviewer"} {
		current := f.catalogCall(t, "GET", "/api/catalog/profiles/"+id, "", nil)
		reply := f.catalogCall(t, "DELETE", "/api/catalog/profiles/"+id, "", map[string]string{"If-Match": current.etag})
		if reply.status != http.StatusConflict || !strings.Contains(reply.body, "session demo") {
			t.Fatalf("delete of %s, the last profile of a scoped category = %d %s, want 409 naming the session", id, reply.status, reply.body)
		}
		if _, err := os.Stat(filepath.Join(f.root, "profiles", filepath.FromSlash(id)+".json")); err != nil {
			t.Fatalf("%s removed although refused: %v", id, err)
		}
	}
	status, body := f.do(t, "GET", "/api/sessions/local:demo", "")
	if status != http.StatusOK {
		t.Fatalf("session no longer loads: %d %s", status, body)
	}
}

func TestCatalogProfileConcurrentCreatesLetOneWin(t *testing.T) {
	f := newLayoutFixture(t)
	const writers = 8
	statuses := make(chan int, writers)
	for i := 0; i < writers; i++ {
		go func() {
			statuses <- f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/race", catalogProfileBody, map[string]string{"If-None-Match": "*"}).status
		}()
	}
	created, refused := 0, 0
	for i := 0; i < writers; i++ {
		switch status := <-statuses; status {
		case http.StatusCreated:
			created++
		case http.StatusPreconditionFailed:
			refused++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if created != 1 || refused != writers-1 {
		t.Fatalf("created %d, refused %d", created, refused)
	}
}

func TestCatalogProfileDeleteAllowedWhileTheCategoryKeepsAnother(t *testing.T) {
	f := newLayoutFixture(t)
	if reply := f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/extra", catalogProfileBody, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
		t.Fatalf("create = %d %s", reply.status, reply.body)
	}
	first := f.catalogCall(t, "GET", "/api/catalog/profiles/dev/coder", "", nil)
	reply := f.catalogCall(t, "DELETE", "/api/catalog/profiles/dev/coder", "", map[string]string{"If-Match": first.etag})
	if reply.status != http.StatusNoContent {
		t.Fatalf("delete of the session's default profile while its category has another = %d %s, want 204", reply.status, reply.body)
	}
	status, body := f.do(t, "GET", "/api/sessions", "")
	var list struct {
		Sessions []struct {
			SessionID      string `json:"session_id"`
			DefaultProfile string `json:"default_profile"`
		} `json:"sessions"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list.Sessions) != 1 {
		t.Fatalf("session list: %d %s", status, body)
	}
	if got := list.Sessions[0].DefaultProfile; got != "dev/extra" {
		t.Fatalf("default profile after the delete = %q, want dev/extra", got)
	}
}

func TestCatalogProfileListShowsTheNameTheFileSets(t *testing.T) {
	f := newLayoutFixture(t)
	named := `{"name":"Fast reviewer","models":["openai/small"],"model_default":"openai/small","toolset":"coding/none"}`
	if reply := f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/fast", named, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
		t.Fatalf("create a named profile = %d %s", reply.status, reply.body)
	}
	var listing struct {
		Files []struct{ ID, Name string }
	}
	list := f.catalogCall(t, "GET", "/api/catalog/profiles", "", nil)
	if err := json.Unmarshal([]byte(list.body), &listing); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range listing.Files {
		got[file.ID] = file.Name
	}
	if got["dev/fast"] != "Fast reviewer" || got["dev/coder"] != "" {
		t.Fatalf("names = %v", got)
	}
	padded := `{"name":"  Padded  ","models":["openai/small"],"toolset":"coding/none"}`
	blank := `{"name":"   ","models":["openai/small"],"toolset":"coding/none"}`
	for id, body := range map[string]string{"dev/padded": padded, "dev/blank": blank} {
		if reply := f.catalogCall(t, "PUT", "/api/catalog/profiles/"+id, body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
			t.Fatalf("create %s = %d %s", id, reply.status, reply.body)
		}
	}
	list = f.catalogCall(t, "GET", "/api/catalog/profiles", "", nil)
	listing.Files = nil
	if err := json.Unmarshal([]byte(list.body), &listing); err != nil {
		t.Fatal(err)
	}
	for _, file := range listing.Files {
		got[file.ID] = file.Name
	}
	if got["dev/padded"] != "Padded" || got["dev/blank"] != "" {
		t.Fatalf("a padded or blank name should be trimmed in the list: %v", got)
	}
	numeric := `{"name":7,"models":["openai/small"],"toolset":"coding/none"}`
	if reply := f.catalogCall(t, "PUT", "/api/catalog/profiles/dev/bad", numeric, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusUnprocessableEntity {
		t.Fatalf("a numeric name = %d %s, want 422", reply.status, reply.body)
	}
}

func TestCatalogListShowsTheNameEveryKindSets(t *testing.T) {
	f := newLayoutFixture(t)
	create := func(path, body string) {
		t.Helper()
		if reply := f.catalogCall(t, "PUT", path, body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
			t.Fatalf("create %s = %d %s", path, reply.status, reply.body)
		}
	}
	create("/api/catalog/models/openai/named", `{"name":" Named model ","model":"gpt-x"}`)
	create("/api/catalog/toolsets/coding/named", `{"name":"Named toolset"}`)
	create("/api/catalog/mcp/ext/named", `{"name":"Named server","transport":{"type":"streamable_http","url":"http://127.0.0.1:1/mcp"}}`)
	create("/api/catalog/sessions/grouped", `{"name":"Named session","category":" work ","cwd":"`+f.cwd+`","scope":{"profiles":["dev"]}}`)
	for _, c := range []struct{ kind, id, name, category string }{
		{"models", "openai/named", "Named model", ""},
		{"toolsets", "coding/named", "Named toolset", ""},
		{"mcp", "ext/named", "Named server", ""},
		{"sessions", "grouped", "Named session", "work"},
	} {
		var listing struct {
			Files []struct{ ID, Name, Category string }
		}
		list := f.catalogCall(t, "GET", "/api/catalog/"+c.kind, "", nil)
		if err := json.Unmarshal([]byte(list.body), &listing); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, file := range listing.Files {
			if file.ID == c.id {
				found = true
				if file.Name != c.name || file.Category != c.category {
					t.Errorf("%s %s = name %q category %q, want %q %q", c.kind, c.id, file.Name, file.Category, c.name, c.category)
				}
			}
		}
		if !found {
			t.Errorf("%s %s missing from %s", c.kind, c.id, list.body)
		}
	}
}

func TestCatalogRefusesANameThatIsNotAString(t *testing.T) {
	f := newLayoutFixture(t)
	for path, body := range map[string]string{
		"/api/catalog/models/openai/bad":   `{"name":7,"model":"gpt-x"}`,
		"/api/catalog/toolsets/coding/bad": `{"name":7}`,
		"/api/catalog/mcp/ext/bad":         `{"name":7,"transport":{"type":"streamable_http","url":"http://127.0.0.1:1/mcp"}}`,
	} {
		if reply := f.catalogCall(t, "PUT", path, body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s, want 422", path, reply.status, reply.body)
		}
	}
}

func TestCatalogProfileListNamesTheSessionsThatCanUseEachProfile(t *testing.T) {
	f := newLayoutFixture(t)
	create := func(path, body string) {
		t.Helper()
		if reply := f.catalogCall(t, "PUT", path, body, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusCreated {
			t.Fatalf("create %s = %d %s", path, reply.status, reply.body)
		}
	}
	create("/api/catalog/profiles/solo/only", `{"models":["openai/small"],"toolset":"coding/none"}`)
	create("/api/catalog/sessions/narrow", `{"name":"Narrow","category":"lab","cwd":"`+f.cwd+`","scope":{"profiles":["dev/coder"]}}`)
	create("/api/catalog/sessions/open", `{"cwd":"`+f.cwd+`"}`)

	var listing struct {
		Files []struct {
			ID       string
			Sessions []struct{ ID, Name, Category string }
		}
	}
	list := f.catalogCall(t, "GET", "/api/catalog/profiles", "", nil)
	if err := json.Unmarshal([]byte(list.body), &listing); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range listing.Files {
		var ids []string
		for _, session := range file.Sessions {
			ids = append(ids, session.ID)
		}
		got[file.ID] = strings.Join(ids, ",")
	}
	want := map[string]string{
		"dev/coder":       "demo,narrow,open",
		"review/reviewer": "demo,open",
		"solo/only":       "open",
	}
	for id, sessions := range want {
		if got[id] != sessions {
			t.Errorf("profile %s sessions = %q, want %q (all: %v)", id, got[id], sessions, got)
		}
	}
	for _, file := range listing.Files {
		if file.ID == "dev/coder" {
			for _, session := range file.Sessions {
				if session.ID == "narrow" && (session.Name != "Narrow" || session.Category != "lab") {
					t.Errorf("session ref = %+v, want its name and category", session)
				}
			}
		}
	}
}
