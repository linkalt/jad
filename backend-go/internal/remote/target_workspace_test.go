package remote

import (
	"fmt"
	"testing"
)

func defaultSessionEntry(session string) func(string) (Entry, error) {
	return func(name string) (Entry, error) { return Entry{Name: name, DefaultSession: session}, nil }
}

// A session is a session id or <workspace>:<session> (T-036): the reference parser takes both
// for a profile and a sub-agent, a default_session may be either, and the form keeps one colon.
func TestReferencesNameAWorkspaceQualifiedSession(t *testing.T) {
	for reference, want := range map[string]Target{
		"remote:box/s1/dev/coder":     {Remote: "box", Session: "s1", Profile: "dev/coder"},
		"remote:box/a:s1/dev/coder":   {Remote: "box", Session: "a:s1", Profile: "dev/coder"},
		"remote:box/ws-1.x:s_1/dev/c": {Remote: "box", Session: "ws-1.x:s_1", Profile: "dev/c"},
		"remote:box/dev/coder":        {Remote: "box", Session: "main", Profile: "dev/coder"},
	} {
		got, err := ParseProfile(reference, defaultSessionEntry("main"))
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v, want %+v", reference, got, err, want)
		}
		if got.Session != "main" && got.String() != reference {
			t.Errorf("String of %s = %s", reference, got.String())
		}
	}
	got, err := ParseSubagent("remote:box/a:s1/dev/coder/explore", defaultSessionEntry(""))
	if err != nil || got.Session != "a:s1" || got.Profile != "dev/coder" || got.Agent != "explore" {
		t.Fatalf("sub-agent: %+v, %v", got, err)
	}
	if got, err := ParseProfile("remote:box/dev/coder", defaultSessionEntry("b:s9")); err != nil || got.Session != "b:s9" {
		t.Fatalf("a qualified default_session: %+v, %v", got, err)
	}
	for _, bad := range []string{
		"remote:box/a:s1:x/dev/coder", "remote:box/:s1/dev/coder", "remote:box/a:/dev/coder", "remote:box/.a:s1/dev/coder",
		"remote:box/a:.s1/dev/coder", "remote:box/a:../dev/coder", "remote:box/a b:s1/dev/coder", "remote:box/a:s1/dev/coder/extra",
	} {
		if got, err := ParseProfile(bad, defaultSessionEntry("")); err == nil {
			t.Errorf("%s was accepted: %+v", bad, got)
		}
	}
}

func TestDefaultSessionMayBeWorkspaceQualified(t *testing.T) {
	for session, ok := range map[string]bool{"main": true, "a:main": true, "a:b:c": false, "a:": false, ":a": false, "a/b": false} {
		body := fmt.Sprintf(`{"kind":"remote","remote":{"url":"http://h","token_env":"JAD_REMOTE_T","default_session":%q}}`, session)
		if _, err := parse(t, body); (err == nil) != ok {
			t.Errorf("default_session %q: %v, want ok=%v", session, err, ok)
		}
	}
}
