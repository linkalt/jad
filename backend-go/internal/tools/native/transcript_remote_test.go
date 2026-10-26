package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/remote"
)

func readerWithEntry(t *testing.T, defaultSession string, profiles string) remoteReader {
	t.Helper()
	dir := t.TempDir()
	extra := ""
	if defaultSession != "" {
		extra += `,"default_session":"` + defaultSession + `"`
	}
	if profiles != "" {
		extra += `,"profiles":[` + profiles + `]`
	}
	body := `{"kind":"remote","remote":{"url":"http://example.invalid:8787","token_env":"JAD_REMOTE_X"` + extra + `}}`
	if err := os.WriteFile(filepath.Join(dir, "peer.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := remote.NewHub(nil)
	t.Cleanup(hub.Close)
	return remoteReader{allow: true, names: []string{"peer"}, directory: hub.Directory(dir)}
}

func TestRemoteSelectorGrammar(t *testing.T) {
	r := readerWithEntry(t, "demo", "")
	good := map[[2]string]string{
		{"transcript", "remote:peer/profile:main/dev/coder"}:               "profile:main/dev/coder",
		{"transcript", "remote:peer/profile:dev/coder"}:                    "profile:demo/dev/coder",
		{"transcript", "remote:peer/subagent:main/dev/coder/sub_0123abcd"}: "subagent:main/dev/coder/sub_0123abcd",
		{"transcript", "remote:peer/subagent:dev/coder/sub_0123abcd"}:      "subagent:demo/dev/coder/sub_0123abcd",
		{"transcripts", "remote:peer"}:                                     "session:demo",
		{"transcripts", "remote:peer/session:main"}:                        "session:main",
		{"transcripts", "remote:peer/profile:main/dev/coder"}:              "profile:main/dev/coder",
		{"transcripts", "remote:peer/profile:dev/coder"}:                   "profile:demo/dev/coder",
	}
	for in, want := range good {
		if got, failure := r.selector(in[0], in[1]); failure != nil || got != want {
			t.Errorf("%v: got %q, %v want %q", in, got, failure, want)
		}
	}
	bad := [][2]string{
		{"transcript", "remote:peer"}, {"transcript", "remote:peer/path:/etc/passwd"}, {"transcript", "remote:peer/profile:main/../coder"},
		{"transcript", "remote:peer/profile:a/b/c/d"}, {"transcript", "remote:peer/subagent:main/dev/coder/notarun"},
		{"transcript", "remote:peer/subagent:sub_0123abcd"}, {"transcript", "remote:peer/profile:main/dev/co:der"}, {"transcript", "remote:peer/session:main"},
		{"transcripts", "remote:peer/subagent:main/dev/coder/sub_0123abcd"}, {"transcript", "remote:/profile:a/b/c"}, {"transcript", "remote:peer/profile:main//coder"},
	}
	for _, in := range bad {
		if got, failure := r.selector(in[0], in[1]); failure == nil {
			t.Errorf("%v must be refused, got %q", in, got)
		}
	}
	noDefault := readerWithEntry(t, "", "")
	if _, failure := noDefault.selector("transcripts", "remote:peer"); failure == nil {
		t.Error("remote:<name> alone needs a default_session")
	}
	if _, failure := noDefault.selector("transcript", "remote:peer/profile:dev/coder"); failure == nil {
		t.Error("a short path needs a default_session")
	}
}

func TestRemoteSelectorSwitchListAndEntryGlobs(t *testing.T) {
	r := readerWithEntry(t, "demo", `"review/*"`)
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
	out := rewriteRemoteOutput(in, "peer", 0)
	for _, want := range []string{"cursor: remote:peer/abc123", "path remote:peer/profile:demo/dev/coder", "path remote:peer/subagent:demo/dev/coder/sub_0123abcd", "path /etc/passwd\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "remote:peer//etc") {
		t.Error("a path that is not a known kind must be left alone")
	}
	cut := rewriteRemoteOutput(in, "peer", 40)
	if !strings.Contains(cut, "--- END CONTENT token=t1 ---") || len(cut) >= len(out) {
		t.Fatalf("content must be cut but stay well-formed:\n%s", cut)
	}
	if got := rewriteRemoteOutput("plain text", "peer", 5); got != "plain text" {
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
