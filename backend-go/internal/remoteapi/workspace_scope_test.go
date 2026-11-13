package remoteapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWorkspaceTokenTableNeedsAWorkspaceAndBareGlobs(t *testing.T) {
	good := hashOf(secretA)
	row := func(extra string) string {
		return `{"tokens":[{"id":"a","sha256":"` + good + `"` + extra + `}]}`
	}
	for name, c := range map[string]struct{ body, want string }{
		"no workspace":    {row(`,"profiles":["s/c/*"]`), "workspace is required"},
		"bad workspace":   {row(`,"workspace":"a b"`), "workspace is required"},
		"colon workspace": {row(`,"workspace":"a:b"`), "workspace is required"},
		"colon glob":      {row(`,"workspace":"a","profiles":["a:*/c/*"]`), "has a colon"},
		"colon in agent":  {row(`,"workspace":"a","agents":["a:s/c/n/x"]`), "has a colon"},
	} {
		if _, err := ParseTokens([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q got %v", name, c.want, err)
		}
	}
	tokens, err := ParseTokens([]byte(row(`,"workspace":"proj","profiles":["main/review/*"]`)))
	if err != nil || tokens[0].Workspace != "proj" {
		t.Fatalf("%+v %v", tokens, err)
	}
}

// scopeBackend holds one profile list per canonical reference and records every session it is given.
type scopeBackend struct {
	*transcriptFake
	asked    []string
	infoFor  []ProfileRequest
	profiles map[string][]string
}

func (b *scopeBackend) Profiles(_ context.Context, session string) ([]string, error) {
	b.asked = append(b.asked, session)
	if names, ok := b.profiles[session]; ok {
		return names, nil
	}
	return nil, ErrNoSession
}

func (b *scopeBackend) ProfileInfo(_ context.Context, request ProfileRequest) (string, error) {
	b.infoFor = append(b.infoFor, request)
	return "profile " + request.Profile, nil
}

const subSecret = "tokenD_0123456789abcdef0123456789abcdef"

func scopeFixture(t *testing.T, workspaces ...string) (*fixture, *scopeBackend) {
	t.Helper()
	backend := &scopeBackend{
		transcriptFake: &transcriptFake{fakeBackend: &fakeBackend{profiles: map[string][]string{}}},
		profiles:       map[string][]string{"a:s1": {"dev/base"}, "b:s1": {"dev/base"}, "b:only-b": {"dev/base"}},
	}
	rows := fmt.Sprintf(`{"tokens":[
 {"id":"ra","sha256":%q,"workspace":"a","profiles":["s1/dev/*"],"actions":["message","status","transcripts","profile"]},
 {"id":"rb","sha256":%q,"workspace":"b","profiles":["*/dev/*"],"actions":["message","status","transcripts","profile"]},
 {"id":"rc","sha256":%q,"workspace":"c","profiles":["*/*/*"],"actions":["message","status","transcripts","profile"]},
 {"id":"rd","sha256":%q,"workspace":"a","profiles":["s1/dev/*"],"agents":["s1/dev/*/explore"],"actions":["subagent","status"]}]}`,
		hashOf(secretA), hashOf(secretB), hashOf("tokenC_0123456789abcdef0123456789abcdef"), hashOf(subSecret))
	parsed, err := ParseTokens([]byte(rows))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, StaticTokenTable(parsed, nil), func(o *Options) {
		o.Backend = backend
		o.Workspaces = workspaces
	})
	return f, backend
}

// A row's requests reach the backend as <workspace>:<session> of the row's workspace, whichever way
// the session is written; a session of another workspace, an unserved workspace and a row for a
// workspace the server does not serve are all a missing session, and the backend is not asked.
func TestWorkspaceRowQualifiesEverySessionOfItsRequests(t *testing.T) {
	f, backend := scopeFixture(t, "a", "b")
	for _, session := range []string{"s1", "a:s1"} {
		if status, body := f.do("POST", Prefix+"/messages", secretA, message(session, "dev/base", "req-0000"+strings.ReplaceAll(session, ":", "-"))); status != http.StatusAccepted {
			t.Fatalf("%s: %d %v", session, status, body)
		}
	}
	if got := []string{backend.messages[0].Session, backend.messages[1].Session}; got[0] != "a:s1" || got[1] != "a:s1" {
		t.Fatalf("the backend was given %q", got)
	}
	// The glob is over the bare session: s1/dev/* for a row of a does not reach another session.
	if status, _ := f.do("POST", Prefix+"/messages", secretA, message("s2", "dev/base", "req-glob-0001")); status != http.StatusForbidden {
		t.Fatalf("a bare glob that does not match: %d", status)
	}
	messages := len(backend.messages)
	for _, c := range []struct{ secret, session string }{
		{secretA, "b:s1"}, {secretA, "c:s1"}, {secretB, "a:s1"}, {"tokenC_0123456789abcdef0123456789abcdef", "s1"}, {"tokenC_0123456789abcdef0123456789abcdef", "c:s1"},
	} {
		status, body := f.do("POST", Prefix+"/messages", c.secret, message(c.session, "dev/base", "req-miss-"+fmt.Sprint(len(c.session))+c.secret[5:6]))
		if status != http.StatusNotFound || body["code"] != "no_session" {
			t.Errorf("%s with %s: %d %v", c.session, c.secret[:6], status, body)
		}
	}
	if len(backend.messages) != messages {
		t.Fatal("a request outside the row's workspace reached the backend")
	}

	// A sub-agent start is limited the same way, with the agents glob over the bare session.
	sub := func(secret, session, id string) int {
		status, _ := f.do("POST", Prefix+"/subagents", secret, map[string]any{"session": session, "profile": "dev/base", "agent": "explore", "task": "t", "request_id": id})
		return status
	}
	if status := sub(secretB, "s1", "req-sub-0001"); status != http.StatusForbidden {
		t.Fatalf("a row with no subagent action: %d", status)
	}
	if status := sub(subSecret, "b:s1", "req-sub-0002"); status != http.StatusNotFound || len(backend.subs) != 0 {
		t.Fatalf("a sub-agent start in another workspace: %d %v", status, backend.subs)
	}
	if status := sub(subSecret, "s1", "req-sub-0003"); status != http.StatusAccepted || len(backend.subs) != 1 || backend.subs[0].Session != "a:s1" {
		t.Fatalf("a sub-agent start: %d %+v", status, backend.subs)
	}

	// Profiles: the row of a lists a:s1, never asks for b's, and the backend sees a qualified name.
	backend.asked = nil
	for query, want := range map[string]int{"s1": 200, "a:s1": 200, "b:s1": 404} {
		if status, _ := f.do("GET", Prefix+"/profiles?session="+url.QueryEscape(query), secretA, nil); status != want {
			t.Errorf("profiles %s: %d, want %d", query, status, want)
		}
	}
	for _, asked := range backend.asked {
		if asked != "a:s1" {
			t.Errorf("the backend was asked for %q", asked)
		}
	}
	if status, _ := f.do("GET", Prefix+"/profile?session=b:s1&profile=dev/base", secretA, nil); status != http.StatusNotFound || len(backend.infoFor) != 0 {
		t.Fatalf("profile info of another workspace: %d %v", status, backend.infoFor)
	}
	if status, _ := f.do("GET", Prefix+"/profile?session=s1&profile=dev/base", secretA, nil); status != http.StatusOK || backend.infoFor[0].Session != "a:s1" {
		t.Fatalf("profile info: %d %+v", status, backend.infoFor)
	}
}

// The profile and agent checks the backend gets match canonical names inside the row's workspace
// only: the same session id in another workspace, and a bare name on a server of several
// workspaces, are not the row's.
func TestWorkspaceRowMayChecksUseCanonicalNamesInsideTheWorkspace(t *testing.T) {
	f, backend := scopeFixture(t, "a", "b")
	for _, path := range []string{"profile:s1/dev/base", "profile:a:s1/dev/base", "profile:b:s1/dev/base", "subagent:a:s1/dev/base/sub_1"} {
		backend.got = nil
		_, _ = f.do("GET", Prefix+"/transcript?path="+url.QueryEscape(path), secretA, nil)
		if strings.Contains(path, "b:s1") {
			if len(backend.got) != 0 {
				t.Errorf("%s reached the backend", path)
			}
			continue
		}
		if len(backend.got) != 1 {
			t.Fatalf("%s: backend calls %d", path, len(backend.got))
		}
		got := backend.got[0]
		if !strings.HasPrefix(got.Arguments["path"].(string), strings.SplitN(path, ":", 2)[0]+":a:s1/") {
			t.Errorf("%s was given as %v", path, got.Arguments["path"])
		}
		if !got.MayProfile("a:s1/dev/base") || got.MayProfile("a:s2/dev/base") || got.MayProfile("b:s1/dev/base") || got.MayProfile("s1/dev/base") {
			t.Errorf("%s: MayProfile a:s1 %v, a:s2 %v, b:s1 %v, bare s1 %v", path, got.MayProfile("a:s1/dev/base"), got.MayProfile("a:s2/dev/base"), got.MayProfile("b:s1/dev/base"), got.MayProfile("s1/dev/base"))
		}
	}
	// A cursor carries no path: the backend's check is then what limits it, and it admits no
	// other workspace's canonical name.
	backend.got = nil
	_, _ = f.do("GET", Prefix+"/transcript?cursor=abc", secretA, nil)
	if len(backend.got) != 1 || backend.got[0].MayProfile("b:s1/dev/base") || !backend.got[0].MayProfile("a:s1/dev/base") {
		t.Fatalf("cursor request: %+v", backend.got)
	}

	// On a server of the row's workspace alone the backend names a session bare, and the row owns it.
	single, singleBackend := scopeFixture(t, "a")
	_, _ = single.do("GET", Prefix+"/transcript?cursor=abc", secretA, nil)
	if len(singleBackend.got) != 1 || !singleBackend.got[0].MayProfile("s1/dev/base") || singleBackend.got[0].MayProfile("s2/dev/base") {
		t.Fatalf("single workspace server: %+v", singleBackend.got)
	}
}

// A v1 server keeps its table and its backend's names: a session is given as written.
func TestV1RowsGiveTheSessionAsWritten(t *testing.T) {
	f := newFixture(t, defaultTable(t), nil)
	if status, body := f.do("POST", Prefix+"/messages", secretA, message("main", "review/a", "req-v1-00001")); status != http.StatusAccepted {
		t.Fatalf("%d %v", status, body)
	}
	if f.backend.messages[0].Session != "main" {
		t.Fatalf("a v1 row's session is passed as written: %q", f.backend.messages[0].Session)
	}
}
