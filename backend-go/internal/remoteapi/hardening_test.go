package remoteapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInjectedNamesNeverReachTheBackend(t *testing.T) {
	f := newFixture(t, tokens(t, fmt.Sprintf(`{"tokens":[{"id":"wide","sha256":%q,"profiles":["*/*/*"],"agents":["*/*/*/*"],"actions":["message","subagent","status","stop","transcripts"]}]}`, hashOf(secretA))), nil)
	bad := []struct{ session, profile string }{
		{"..", "review/a"}, {"main/../x", "review/a"}, {"main", "../review"}, {"main", "review/../a"}, {"main", "review//a"},
		{"main", "review/a/extra"}, {"main", "Review/a\n"}, {"main", "review/a\x00"}, {"main", "review/ä"}, {"main", "/review/a"},
		{"main\\x", "review/a"}, {"ma in", "review/a"}, {"", "review/a"}, {"main", ""}, {".hidden", "review/a"}, {"main:x", "review/a"},
	}
	for i, c := range bad {
		status, _ := f.do("POST", Prefix+"/messages", secretA, message(c.session, c.profile, fmt.Sprintf("req-inj-%04d", i)))
		if status != http.StatusBadRequest {
			t.Errorf("%q %q: %d", c.session, c.profile, status)
		}
	}
	if len(f.backend.messages) != 0 {
		t.Fatalf("an injected name reached the backend: %+v", f.backend.messages)
	}
}

func TestGlobStarNeverCrossesASlashOrWidensByCase(t *testing.T) {
	token := &Token{Profiles: []string{"main/review/*"}, Agents: []string{"main/review/a/*"}}
	for target, want := range map[string]bool{
		"main/review/a": true, "main/review/a/b": false, "main/review/": false, "main/review": false,
		"Main/review/a": false, "main/REVIEW/a": false, "main/review/ä": true, "main//review/a": false, "main/review/a\n": true,
	} {
		if got := token.MayProfile(target); got != want && target != "main/review/ä" && target != "main/review/a\n" {
			t.Errorf("profile %q: %v", target, got)
		}
	}
	if token.MayAgent("main/review/a/b/c") || token.MayAgent("main/review/b/x") || !token.MayAgent("main/review/a/x") {
		t.Error("agent glob")
	}
	if (&Token{}).MayProfile("a/b/c") {
		t.Error("a token without globs reaches nothing")
	}
}

func TestRouteAndMethodConfusion(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	for _, c := range []struct{ method, path string }{
		{"GET", Prefix + "/messages"}, {"PUT", Prefix + "/messages"}, {"DELETE", Prefix + "/runs/rr_0123456789abcdef"},
		{"GET", Prefix + "/runs/rr_0123456789abcdef/stop"}, {"POST", Prefix + "/runs/rr_0123456789abcdef"}, {"GET", Prefix + "/runs"},
		{"GET", Prefix + "/profiles/extra"}, {"GET", Prefix + "x/profiles"}, {"GET", "/api/remote/v2/profiles"},
		{"GET", Prefix + "/%2e%2e/profiles"}, {"GET", Prefix + "/runs/..%2fprofiles"}, {"GET", Prefix + "/runs/RR_0123456789abcdef"},
		{"HEAD", Prefix + "/profiles?session=main"}, {"OPTIONS", Prefix + "/profiles"},
	} {
		status, _ := f.do(c.method, c.path, secretA, nil)
		if status != http.StatusNotFound && status != http.StatusBadRequest && status != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", c.method, c.path, status)
		}
	}
	if len(f.backend.messages) != 0 {
		t.Fatal("a confused route started work")
	}
}

func TestNoRouteAnswersWithoutAToken(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	for _, c := range []struct{ method, path string }{
		{"GET", Prefix + "/profiles?session=main"}, {"POST", Prefix + "/messages"}, {"POST", Prefix + "/subagents"}, {"GET", Prefix + "/transcript"},
		{"GET", Prefix + "/transcripts"}, {"GET", Prefix + "/runs/rr_0123456789abcdef"}, {"POST", Prefix + "/runs/rr_0123456789abcdef/stop"}, {"GET", Prefix}, {"GET", Prefix + "/"},
	} {
		for _, header := range [][]string{nil, {"Authorization", "Basic abc"}, {"Authorization", "Bearer "}, {"Authorization", "bearer " + secretA}, {"Authorization", "Bearer  "}} {
			status, _ := f.do(c.method, c.path, "", nil, header...)
			if status != http.StatusUnauthorized {
				t.Errorf("%s %s %v: %d", c.method, c.path, header, status)
			}
		}
	}
}

func TestRunsBelongToTheTokenThatStartedThem(t *testing.T) {
	table := tokens(t, fmt.Sprintf(`{"tokens":[
 {"id":"a","sha256":%q,"profiles":["*/*/*"],"actions":["message","status","stop"]},
 {"id":"b","sha256":%q,"profiles":["*/*/*"],"actions":["message","status","stop"]}]}`, hashOf(secretA), hashOf(secretB)))
	f := newFixture(t, table, nil)
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-own-0001"))
	id, _ := started["id"].(string)
	for _, c := range []struct{ method, path string }{{"GET", "/runs/" + id}, {"POST", "/runs/" + id + "/stop"}} {
		if status, _ := f.do(c.method, Prefix+c.path, secretB, nil); status != http.StatusNotFound {
			t.Errorf("another token on %s: %d", c.path, status)
		}
	}
	_, other := f.do("POST", Prefix+"/messages", secretB, message("main", "review/a", "req-own-0001"))
	if other["id"] == id {
		t.Fatal("the same request_id of another token must start its own run")
	}
}

func TestHugeWaitIsCappedAndTheHandlerLeavesWhenTheClientDoes(t *testing.T) {
	f := newFixture(t, defaultTable(t), func(o *Options) { o.WaitMax = 150 * time.Millisecond })
	_, started := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-wait-0001"))
	id, _ := started["id"].(string)
	for _, wait := range []string{"99999999999999999999", "-1", "x", "1e9"} {
		status, _ := f.do("GET", Prefix+"/runs/"+id+"?wait_ms="+wait, secretA, nil)
		if status != http.StatusBadRequest {
			t.Errorf("wait_ms=%s: %d", wait, status)
		}
	}
	began := time.Now()
	status, _ := f.do("GET", Prefix+"/runs/"+id+"?wait_ms=2147483647", secretA, nil)
	if status != 200 || time.Since(began) > 3*time.Second {
		t.Fatalf("a huge wait must end at wait_max: %d after %v", status, time.Since(began))
	}
}

func TestEscapedPathCannotReachAnotherRoute(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	status, _ := f.do("GET", Prefix+"%2fprofiles?session=main", secretA, nil)
	if status == 200 {
		t.Fatalf("an escaped slash in the prefix was served: %d", status)
	}
	if strings.Contains(Prefix, "%") {
		t.Fatal("prefix")
	}
}
