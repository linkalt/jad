package app

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const catalogSessionsPath = "/api/catalog/sessions"

// postRun starts a run on the demo session and returns the status and the stream. It never
// fails the test, so a goroutine may call it.
func (f *layoutFixture) postRun(input string) (int, string) {
	request, err := http.NewRequest("POST", f.api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"`+input+`"}`))
	if err != nil {
		return 0, err.Error()
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (f *layoutFixture) sessionBody(profile string) string {
	return `{"schema_version":1,"cwd":"` + f.cwd + `","active_profile":"` + profile + `","scope":{"profiles":["dev"]}}`
}

func TestCatalogSessionsListReadAndCreate(t *testing.T) {
	f := newLayoutFixture(t)
	list := f.catalogCall(t, "GET", catalogSessionsPath, "", nil)
	if list.status != http.StatusOK || !strings.Contains(list.body, `"id":"demo"`) {
		t.Fatalf("list = %d %s", list.status, list.body)
	}
	read := f.catalogCall(t, "GET", catalogSessionsPath+"/demo", "", nil)
	onDisk, _ := os.ReadFile(filepath.Join(f.root, "sessions", "demo.json"))
	if read.status != http.StatusOK || read.body != string(onDisk) || read.etag == "" {
		t.Fatalf("read = %d %q etag %q", read.status, read.body, read.etag)
	}
	if missing := f.catalogCall(t, "GET", catalogSessionsPath+"/nope", "", nil); missing.status != http.StatusNotFound {
		t.Fatalf("missing session = %d", missing.status)
	}
	if bad := f.catalogCall(t, "GET", catalogSessionsPath+"/..", "", nil); bad.status != http.StatusBadRequest {
		t.Fatalf("path escape = %d %s", bad.status, bad.body)
	}

	path := catalogSessionsPath + "/extra"
	if reply := f.catalogCall(t, "PUT", path, f.sessionBody("dev/coder"), nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("create without a precondition = %d", reply.status)
	}
	created := f.catalogCall(t, "PUT", path, f.sessionBody("dev/coder"), map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated || created.etag == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	if again := f.catalogCall(t, "PUT", path, f.sessionBody("dev/coder"), map[string]string{"If-None-Match": "*"}); again.status != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing session = %d", again.status)
	}
	if status, body := f.do(t, "GET", "/api/sessions/extra", ""); status != http.StatusOK {
		t.Fatalf("the new session does not load: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/sessions/extra/run", `{"input":"hi"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run in the new session = %d %s", status, body)
	}
}

func TestCatalogSessionWriteIsCheckedByTheLoader(t *testing.T) {
	f := newLayoutFixture(t)
	path := catalogSessionsPath + "/demo"
	target := filepath.Join(f.root, "sessions", "demo.json")
	before := f.catalogCall(t, "GET", path, "", nil)
	cases := map[string]struct{ body, want string }{
		"empty":            {``, "expected a JSON object"},
		"not json":         {`{"cwd":`, "demo.json"},
		"unknown key":      {`{"cwd":"` + f.cwd + `","active_profile":"dev/coder","nope":1}`, "nope"},
		"empty scope":      {`{"cwd":"` + f.cwd + `","scope":{"profiles":[]}}`, "matches no profile"},
		"unknown profile":  {`{"cwd":"` + f.cwd + `","active_profile":"dev/missing"}`, "dev/missing"},
		"outside scope":    {`{"cwd":"` + f.cwd + `","active_profile":"dev/coder","scope":{"profiles":["review"]}}`, "dev/coder"},
		"bad version":      {`{"schema_version":7,"cwd":"` + f.cwd + `","active_profile":"dev/coder"}`, "schema_version"},
		"cwd not a folder": {`{"cwd":"/no/such/folder","active_profile":"dev/coder"}`, "cwd"},
		"model_defaults":   {`{"cwd":"` + f.cwd + `","active_profile":"dev/coder","model_defaults":{}}`, "model_defaults"},
		"settings key":     {`{"cwd":"` + f.cwd + `","active_profile":"dev/coder","settings":{"model":"x"}}`, "settings may not carry"},
	}
	for name, tc := range cases {
		reply := f.catalogCall(t, "PUT", path, tc.body, map[string]string{"If-Match": before.etag})
		if reply.status != http.StatusUnprocessableEntity || !strings.Contains(reply.body, tc.want) {
			t.Errorf("%s: status %d body %s, want 422 naming %q", name, reply.status, reply.body, tc.want)
		}
		if strings.Contains(reply.body, "jad-session-check") {
			t.Errorf("%s: the message names the scratch folder: %s", name, reply.body)
		}
	}
	if onDisk, _ := os.ReadFile(target); string(onDisk) != before.body {
		t.Fatalf("a refused write changed the file: %q", onDisk)
	}
	if reply := f.catalogCall(t, "PUT", path, `{"cwd":"`+f.cwd+`"}`, map[string]string{"If-Match": before.etag}); reply.status != http.StatusOK {
		t.Fatalf("a session without active_profile = %d %s, want it accepted", reply.status, reply.body)
	}
	if reply := f.catalogCall(t, "PUT", catalogSessionsPath+"/fresh", `{"cwd":"/no/such/folder","active_profile":"dev/coder"}`, map[string]string{"If-None-Match": "*"}); reply.status != http.StatusUnprocessableEntity {
		t.Fatalf("refused create = %d", reply.status)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", "fresh.json")); err == nil {
		t.Fatal("a refused create left a session file behind")
	}
	scratch, _ := filepath.Glob(filepath.Join(os.TempDir(), "jad-session-check-*"))
	if len(scratch) != 0 {
		t.Fatalf("scratch folders left behind: %v", scratch)
	}
}

func TestCatalogSessionReplaceNeedsTheCurrentVersion(t *testing.T) {
	f := newLayoutFixture(t)
	path := catalogSessionsPath + "/demo"
	before := f.catalogCall(t, "GET", path, "", nil)
	next := `{"schema_version":1,"cwd":"` + f.cwd + `","active_profile":"dev/coder","category":"other","scope":{"profiles":["dev","review"]}}`
	if reply := f.catalogCall(t, "PUT", path, next, nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("replace without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("replace with a stale tag = %d", reply.status)
	}
	if reply := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": "W/" + before.etag}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("a weak tag must not satisfy If-Match, got %d", reply.status)
	}
	replaced := f.catalogCall(t, "PUT", path, next, map[string]string{"If-Match": before.etag})
	if replaced.status != http.StatusOK || replaced.etag == before.etag {
		t.Fatalf("replace = %d %s", replaced.status, replaced.body)
	}
	if onDisk, _ := os.ReadFile(filepath.Join(f.root, "sessions", "demo.json")); string(onDisk) != next {
		t.Fatalf("stored %q", onDisk)
	}
	if status, body := f.do(t, "GET", "/api/sessions/local:demo", ""); status != http.StatusOK || !strings.Contains(body, `"category":"other"`) {
		t.Fatalf("the edit is not served: %d %s", status, body)
	}
}

func TestCatalogSessionDeleteKeepsTheStateByDefault(t *testing.T) {
	f := newLayoutFixture(t)
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"one"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run = %d %s", status, body)
	}
	state := filepath.Join(f.cwd, ".jad", "state", "demo")
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("no state after a run: %v", err)
	}
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path, "", nil); reply.status != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": `"stale"`}); reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d", reply.status)
	}
	if reply := f.catalogCall(t, "DELETE", path+"?state=bogus", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusBadRequest {
		t.Fatalf("bad state value = %d", reply.status)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", "demo.json")); err != nil {
		t.Fatalf("a refused delete removed the session: %v", err)
	}
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", "demo.json")); err == nil {
		t.Fatal("the session file is still there")
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", ".demo.json.jad-lock")); err == nil {
		t.Fatal("the session lock file is still there")
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("the state folder was removed although it was to be kept: %v", err)
	}
	if status, body := f.do(t, "GET", "/api/sessions/local:demo", ""); status == http.StatusOK {
		t.Fatalf("a deleted session still answers: %d %s", status, body)
	}
	if reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNotFound {
		t.Fatalf("delete of a missing session = %d", reply.status)
	}
}

func TestCatalogSessionDeleteCanRemoveTheState(t *testing.T) {
	f := newLayoutFixture(t)
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"first run"}`); status != http.StatusOK {
		t.Fatalf("run = %d %s", status, body)
	}
	state := filepath.Join(f.cwd, ".jad", "state", "demo")
	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	if reply := f.catalogCall(t, "DELETE", path+"?state=delete", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", reply.status, reply.body)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("the state folder is still there")
	}

	created := f.catalogCall(t, "PUT", path, f.sessionBody("dev/coder"), map[string]string{"If-None-Match": "*"})
	if created.status != http.StatusCreated {
		t.Fatalf("create again = %d %s", created.status, created.body)
	}
	status, body := f.do(t, "GET", "/api/sessions/local:demo", "")
	if status != http.StatusOK || strings.Contains(body, "first run") {
		t.Fatalf("a session created again under the name shows the old transcript: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"second run"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run after create again = %d %s", status, body)
	}
	if status, body := f.do(t, "GET", "/api/sessions/local:demo", ""); strings.Contains(body, "first run") || !strings.Contains(body, "second run") {
		t.Fatalf("snapshot after the second run: %d %s", status, body)
	}
}

func TestCatalogSessionDeleteStopsAnActiveRun(t *testing.T) {
	f := newLayoutFixture(t)
	f.mu.Lock()
	f.hold, f.reached = true, make(chan struct{}, 4)
	f.mu.Unlock()

	runDone := make(chan string, 1)
	go func() {
		_, body := f.postRun("slow")
		runDone <- body
	}()
	select {
	case <-f.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the provider")
	}

	path := catalogSessionsPath + "/demo"
	current := f.catalogCall(t, "GET", path, "", nil)
	reply := f.catalogCall(t, "DELETE", path, "", map[string]string{"If-Match": current.etag})
	if reply.status != http.StatusNoContent {
		t.Fatalf("delete during a run = %d %s", reply.status, reply.body)
	}
	select {
	case body := <-runDone:
		if !strings.Contains(body, "event: error") && !strings.Contains(body, "aborted") {
			t.Fatalf("the stopped run did not end as stopped: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not stopped by the delete")
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", "demo.json")); err == nil {
		t.Fatal("the session file is still there")
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", ".demo.json.jad-lock")); err == nil {
		t.Fatal("the session lock file is still there")
	}
}

func TestCatalogSessionDeleteWithAStaleTagLeavesTheRunGoing(t *testing.T) {
	f := newLayoutFixture(t)
	f.mu.Lock()
	f.hold, f.reached = true, make(chan struct{}, 4)
	f.mu.Unlock()

	runDone := make(chan string, 1)
	go func() {
		_, body := f.postRun("slow")
		runDone <- body
	}()
	select {
	case <-f.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the provider")
	}
	reply := f.catalogCall(t, "DELETE", catalogSessionsPath+"/demo", "", map[string]string{"If-Match": `"stale"`})
	if reply.status != http.StatusPreconditionFailed {
		t.Fatalf("delete with a stale tag = %d %s", reply.status, reply.body)
	}
	select {
	case body := <-runDone:
		t.Fatalf("a refused delete ended the run: %s", body)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(f.root, "sessions", "demo.json")); err != nil {
		t.Fatalf("a refused delete removed the session: %v", err)
	}
	current := f.catalogCall(t, "GET", catalogSessionsPath+"/demo", "", nil)
	if reply := f.catalogCall(t, "DELETE", catalogSessionsPath+"/demo", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
		t.Fatalf("delete with the current tag = %d %s", reply.status, reply.body)
	}
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not stopped by the delete")
	}
}
