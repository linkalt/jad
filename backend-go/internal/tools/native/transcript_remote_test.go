package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/remote"
)

func readerWithEntry(t *testing.T, profiles string) remoteReader {
	t.Helper()
	dir := t.TempDir()
	extra := ""
	if profiles != "" {
		extra += `,"profiles":[` + profiles + `]`
	}
	body := `{"kind":"remote","remote":{"url":"http://example.invalid:8787","workspace":"proj"` + extra + `}}`
	if err := os.WriteFile(filepath.Join(dir, "peer.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := remote.NewHub(nil)
	t.Cleanup(hub.Close)
	return remoteReader{allow: true, names: []string{"peer"}, directory: hub.Directory(dir)}
}

func TestRemoteSelectorSwitchListAndEntryGlobs(t *testing.T) {
	r := readerWithEntry(t, `"review/*"`)
	if _, failure := r.selector("transcript", "remote:peer/profile:main/dev/coder"); failure == nil || failure.code != "remote_not_allowed" {
		t.Fatalf("the entry's profile globs apply: %+v", failure)
	}
	if _, failure := r.selector("transcript", "remote:peer/profile:main/review/a"); failure != nil {
		t.Fatalf("%+v", failure)
	}
	off := r
	off.allow = false
	if _, failure := off.selector("transcript", "remote:peer/profile:main/review/a"); failure == nil || failure.code != "remote_disabled" {
		t.Fatalf("%+v", failure)
	}
	other := r
	other.names = []string{"another"}
	if _, failure := other.selector("transcript", "remote:peer/profile:main/review/a"); failure == nil || failure.code != "unknown_remote" {
		t.Fatalf("%+v", failure)
	}
}

func TestRewriteRemoteOutputPrefixesPathsAndCursorsOnly(t *testing.T) {
	in := `{"id":"remote","status":"ok","summary":"2 profiles of session demo"}

--- INFORMATION ---
truncated: true
cursor: abc123

--- CONTENT token=t1 encoding=utf8 ---
each line: kind name
profile dev/coder | active x | 5 bytes | backup no | path profile:demo/dev/coder
run sub_0123abcd | done | agent a | path subagent:demo/dev/coder/sub_0123abcd
profile evil | path /etc/passwd
--- END CONTENT token=t1 ---
`
	out := rewriteRemoteOutput(in, "peer", "", 0)
	for _, want := range []string{"cursor: remote:peer/abc123", "path remote:peer/profile:demo/dev/coder", "path remote:peer/subagent:demo/dev/coder/sub_0123abcd", "path /etc/passwd\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "remote:peer//etc") {
		t.Error("a path that is not a known kind must be left alone")
	}
	cut := rewriteRemoteOutput(in, "peer", "", 40)
	if !strings.Contains(cut, "--- END CONTENT token=t1 ---") || len(cut) >= len(out) {
		t.Fatalf("content must be cut but stay well-formed:\n%s", cut)
	}
	if got := rewriteRemoteOutput("plain text", "peer", "", 5); got != "plain text" {
		t.Fatalf("%q", got)
	}
}

func TestSplitRemoteCursor(t *testing.T) {
	if name, inner, ok := splitRemoteCursor("remote:peer/abc_-123"); !ok || name != "peer" || inner != "abc_-123" {
		t.Fatalf("%q %q %v", name, inner, ok)
	}
	for _, bad := range []string{"", "abc", "remote:peer", "remote:peer/", "remote:peer/a/b", "remote:peer/a:b", "remote:/abc"} {
		if _, _, ok := splitRemoteCursor(bad); ok {
			t.Errorf("%q is not a wrapped cursor", bad)
		}
	}
}

// The remote:<binding> reads of the workspace layout: the session is always written bare, the
// request goes out as <workspace>:<session>, and the paths the remote prints come back without the
// workspace, so a path the model reads is a path it can write again.
func TestBindingReaderQualifiesTheSessionAndRewritesTheAnswer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "peer.json"), []byte(`{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := remote.NewHub(nil)
	defer hub.Close()
	reader := remoteReader{allow: true, names: []string{"peer"}, directory: hub.Directory(dir).Scoped(remote.Scope{})}
	for _, c := range []struct {
		tool, reference, want string
	}{
		{"transcript", "remote:peer/profile:main/review/security", "profile:proj:main/review/security"},
		{"transcript", "remote:peer/subagent:main/review/security/sub_0123abcd", "subagent:proj:main/review/security/sub_0123abcd"},
		{"transcripts", "remote:peer/session:main", "session:proj:main"},
		{"transcripts", "remote:peer/profile:main/review/security", "profile:proj:main/review/security"},
	} {
		got, failure := reader.selector(c.tool, c.reference)
		if failure != nil || got != c.want {
			t.Errorf("%s %s: %q %v, want %q", c.tool, c.reference, got, failure, c.want)
		}
	}
	for _, bad := range []string{"remote:peer", "remote:peer/profile:review/security", "remote:peer/profile:proj:main/review/security", "remote:peer/profile:main/review", "remote:peer/path:/etc/passwd", "remote:peer/profile:main/review/security/extra"} {
		if got, failure := reader.selector("transcript", bad); failure == nil {
			t.Errorf("%s was accepted as %q", bad, got)
		} else if failure.code != "bad_remote_reference" {
			t.Errorf("%s: %+v", bad, failure)
		}
	}
	in := `{"id":"remote","status":"ok","summary":"1 profile"}

--- INFORMATION ---
cursor: abc

--- CONTENT token=t1 encoding=utf8 ---
profile review/security | active x | 5 bytes | backup no | path profile:proj:main/review/security
run sub_0123abcd | done | agent a | path subagent:proj:main/review/security/sub_0123abcd
profile other | path profile:other:main/review/security
--- END CONTENT token=t1 ---
`
	out := rewriteRemoteOutput(in, "peer", "proj", 0)
	for _, want := range []string{"path remote:peer/profile:main/review/security", "path remote:peer/subagent:main/review/security/sub_0123abcd", "path remote:peer/profile:other:main/review/security"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
