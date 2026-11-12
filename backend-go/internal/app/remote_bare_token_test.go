package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// A token whose globs are written for bare session ids can start a run on a session of a server
// with several workspaces. It reads that run's transcript too, with the bare id or the
// <workspace>:<session> a cursor names, but only while the bare id still resolves to exactly
// that session: a bare id two workspaces hold, a session of another workspace and a bare glob
// that does not match give nothing (T-036b).
func TestRemoteBareGlobTokenReadsWhatItCanStart(t *testing.T) {
	const (
		bare   = "bare_0123456789abcdef0123456789abcdefab"
		onlyB  = "onlyb_0123456789abcdef0123456789abcdef"
		wsOnly = "wsonly_0123456789abcdef0123456789abcdef"
	)
	grant := func(glob string) string {
		return `"profiles":["` + glob + `"],"agents":["` + glob + `/*"],"actions":["message","subagent","status","stop","transcripts","profile"]`
	}
	w := startRemoteWorkspaces(t, tokenTable(
		tokenRow("bare", bare, grant("s1/dev/*")),
		tokenRow("onlyb", onlyB, grant("only-b/dev/*")),
		tokenRow("wsonly", wsOnly, grant("a:*/dev/*")),
		tokenRow("any", remoteTestSecret, fullGrant)))
	var lines strings.Builder
	for i := 0; i < 70; i++ {
		lines.WriteString(remoteUser(fmt.Sprintf("question %d", i)))
		lines.WriteString(remoteAnswer(i+1, fmt.Sprintf("answer %d", i)))
	}
	for workspace, texts := range map[string]map[string]string{
		w.a: {"s1": "the words of a:s1"},
		w.b: {"s1": "the words of b:s1", "only-b": lines.String()},
	} {
		for session, text := range texts {
			content := text
			if !strings.Contains(text, "answer 0") {
				content = remoteUser("question") + remoteAnswer(1, text)
			}
			file := filepath.Join(workspace, ".jad", "state", session, "dev", "base", "messages.jsonl")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(secret, path string) (bool, string) {
		t.Helper()
		status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?path="+url.QueryEscape(path), secret, nil)
		out, _ := body["output"].(string)
		return status == 200 && !strings.Contains(out, "error_code"), out
	}
	expect := func(secret, path string, want bool, words string) {
		t.Helper()
		got, out := read(secret, path)
		if got != want || (want && !strings.Contains(out, words)) || (!want && strings.Contains(out, "the words of")) {
			t.Errorf("%s reading %s: readable %v, want %v:\n%s", secret[:5], path, got, want, out)
		}
	}

	// s1 is held by both workspaces: the bare id is ambiguous for a bare token, so it reads
	// nothing, as written or as a:s1; the token that reaches every session still reads a:s1.
	expect(bare, "profile:s1/dev/base", false, "")
	expect(bare, "profile:a:s1/dev/base", false, "")
	expect(bare, "profile:b:s1/dev/base", false, "")
	expect(remoteTestSecret, "profile:a:s1/dev/base", true, "the words of a:s1")
	expect(remoteTestSecret, "profile:b:s1/dev/base", true, "the words of b:s1")

	// With b's s1 gone, the bare id means a:s1 alone and the bare token reads it, written either
	// way, but never b's.
	if err := os.Rename(filepath.Join(w.b, ".jad", "sessions", "s1.json"), filepath.Join(w.b, ".jad", "sessions", "s1.hidden")); err != nil {
		t.Fatal(err)
	}
	expect(bare, "profile:s1/dev/base", true, "the words of a:s1")
	expect(bare, "profile:a:s1/dev/base", true, "the words of a:s1")
	expect(bare, "profile:b:s1/dev/base", false, "")
	expect(bare, "profile:s1/dev/other", false, "")
	expect(wsOnly, "profile:s1/dev/base", false, "")
	expect(wsOnly, "profile:a:s1/dev/base", true, "the words of a:s1")
	// ... and a second workspace that later holds the id takes the access away again.
	if err := os.Rename(filepath.Join(w.b, ".jad", "sessions", "s1.hidden"), filepath.Join(w.b, ".jad", "sessions", "s1.json")); err != nil {
		t.Fatal(err)
	}
	expect(bare, "profile:s1/dev/base", false, "")
	expect(bare, "profile:a:s1/dev/base", false, "")

	// A session only b holds: its bare token reads it in b and never as a session of a.
	expect(onlyB, "profile:only-b/dev/base", true, "answer 69")
	expect(onlyB, "profile:b:only-b/dev/base", true, "answer 69")
	expect(onlyB, "profile:a:only-b/dev/base", false, "")
	expect(onlyB, "profile:s1/dev/base", false, "")

	// The cursor of that read names the canonical session and continues for the same token.
	_, first := read(onlyB, "profile:only-b/dev/base")
	match := remoteCursorLine.FindStringSubmatch(first)
	if match == nil {
		t.Fatalf("140 steps exceed one page, so a cursor is expected:\n%s", first)
	}
	status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?cursor="+url.QueryEscape(match[1]), onlyB, nil)
	if next, _ := body["output"].(string); status != 200 || strings.Contains(next, "error_code") || !strings.Contains(next, "1.0 ") {
		t.Fatalf("the cursor did not continue: %d %v", status, body)
	}
	if status, body := w.call(t, "GET", remoteapi.Prefix+"/transcript?cursor="+url.QueryEscape(match[1]), bare, nil); status == 200 && !strings.Contains(fmt.Sprint(body["output"]), "error_code") {
		t.Fatalf("another token read with a cursor of a session it does not reach: %v", body)
	}
}

// The same decision for the sub-agent runs of a profile (the token's agents globs) and for the
// profile and session lists, on the profile reader of the remote API.
func TestRemoteBareGlobTokenSeesSubagentsOfWhatItCanStart(t *testing.T) {
	w := startRemoteWorkspaces(t, tokenTable(tokenRow("any", remoteTestSecret, fullGrant)))
	token := remoteapi.Token{Profiles: []string{"only-b/dev/*"}, Agents: []string{"only-b/dev/*/explore"}}
	profiles := tokenProfiles{inner: profileMessenger{resolver: w.service.resolver}, may: token.MayProfile, mayAgent: token.MayAgent}
	target, err := profiles.Profile(t.Context(), remoteTranscriptCaller, "only-b", "dev/base")
	if err != nil || target.Session != "b:only-b" {
		t.Fatalf("a bare token reads the bare id of a session only b holds: %+v %v", target, err)
	}
	if !target.MayAgent("explore") || target.MayAgent("other") {
		t.Fatalf("agents: explore %v, other %v", target.MayAgent("explore"), target.MayAgent("other"))
	}
	// The cursor form works the same, and the other workspace does not.
	if _, err := profiles.Profile(t.Context(), remoteTranscriptCaller, "b:only-b", "dev/base"); err != nil {
		t.Fatalf("canonical form: %v", err)
	}
	if _, err := profiles.Profile(t.Context(), remoteTranscriptCaller, "a:only-b", "dev/base"); err == nil {
		t.Fatal("a session of another workspace was read")
	}
	if names, _ := profiles.Candidates(t.Context(), remoteTranscriptCaller, "only-b"); len(names) != 1 || names[0] != "dev/base" {
		t.Fatalf("candidates for the bare token: %q", names)
	}
	if names, _ := profiles.Candidates(t.Context(), remoteTranscriptCaller, "b:only-b"); len(names) != 1 {
		t.Fatalf("candidates for the canonical form: %q", names)
	}
	// The bare id counts for a qualified name only when it resolves to that very session, so a
	// name that is not where the bare id resolves (here a:only-b, held by b) gets no second name.
	if names := profiles.sessionNames("a:only-b"); len(names) != 1 {
		t.Fatalf("names of a:only-b: %q", names)
	}
	if names := profiles.sessionNames("b:only-b"); len(names) != 2 || names[1] != "only-b" {
		t.Fatalf("names of b:only-b: %q", names)
	}
	if names := profiles.sessionNames("a:s1"); len(names) != 1 {
		t.Fatalf("names of a:s1, an id of both workspaces: %q", names)
	}
	// An id both workspaces hold gives the bare token nothing, as a profile or an agent.
	both := remoteapi.Token{Profiles: []string{"s1/dev/*"}, Agents: []string{"s1/dev/*/*"}}
	profiles = tokenProfiles{inner: profileMessenger{resolver: w.service.resolver}, may: both.MayProfile, mayAgent: both.MayAgent}
	for _, session := range []string{"s1", "a:s1", "b:s1"} {
		if _, err := profiles.Profile(t.Context(), remoteTranscriptCaller, session, "dev/base"); err == nil {
			t.Errorf("%s: an ambiguous bare id was read", session)
		}
	}
	if names, _ := profiles.Candidates(t.Context(), remoteTranscriptCaller, "s1"); len(names) != 0 {
		t.Fatalf("candidates of an ambiguous id: %q", names)
	}
}

// v1 with several session roots: the same rule, with the root id as the qualifier.
func TestRemoteBareGlobTokenInV1WithSeveralRoots(t *testing.T) {
	var second string
	f := newLayoutFixtureWithBudget(t, nil, nil, func(config *Config) {
		second = filepath.Join(t.TempDir(), "sessions2")
		layout := config.Roots[0]
		layout.ID, layout.Directory = "second", second
		config.Roots = append(config.Roots, layout)
	})
	demo, err := os.ReadFile(filepath.Join(f.root, "sessions", "demo.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"demo.json", "solo.json"} {
		if err := os.MkdirAll(second, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(second, name), demo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "sessions", "mine.json"), demo, 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(token remoteapi.Token, session string) (string, error) {
		profiles := tokenProfiles{inner: profileMessenger{resolver: f.service.resolver}, may: token.MayProfile, mayAgent: token.MayAgent}
		target, err := profiles.Profile(t.Context(), remoteTranscriptCaller, session, "dev/coder")
		return target.Session, err
	}
	bare := func(session string) remoteapi.Token {
		return remoteapi.Token{Profiles: []string{session + "/dev/*"}}
	}
	for _, c := range []struct {
		token   remoteapi.Token
		session string
		want    string // the canonical session, "" for refused
	}{
		{bare("solo"), "solo", "second:solo"},
		{bare("solo"), "second:solo", "second:solo"},
		{bare("solo"), "local:solo", ""},
		{bare("mine"), "mine", "local:mine"},
		{bare("demo"), "demo", ""},
		{bare("demo"), "local:demo", ""},
		{bare("demo"), "second:demo", ""},
		{remoteapi.Token{Profiles: []string{"*/dev/*"}}, "second:demo", "second:demo"},
		{remoteapi.Token{Profiles: []string{"second:*/dev/*"}}, "second:solo", "second:solo"},
		{remoteapi.Token{Profiles: []string{"second:*/dev/*"}}, "solo", ""},
	} {
		got, err := read(c.token, c.session)
		if (c.want == "") != (err != nil) || got != c.want {
			t.Errorf("globs %v reading %s: %q, %v; want %q", c.token.Profiles, c.session, got, err, c.want)
		}
	}
}
