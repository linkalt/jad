package app

import (
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// The two-name rule of a v1 server with several session roots (T-036b), on the profile reader of
// the remote API: the sub-agent runs of a profile (the token's agents globs) and the profile
// lists. A workspace-layout server limits a row to its workspace before this reader is asked, so
// the rule is exercised here with a token of the v1 kind directly (TestRemoteTranscriptsOfARow...).
func TestRemoteBareGlobTokenSeesSubagentsOfWhatItCanStart(t *testing.T) {
	w := startRowWorkspaces(t)
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
