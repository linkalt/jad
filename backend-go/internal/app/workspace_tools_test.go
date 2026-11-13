package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func writeWorkspaceTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// twoWorkspaces loads workspaces a and b, each with session s1 and profile dev/coder; b also
// has session only-b.
func twoWorkspaces(t *testing.T) profileMessenger {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	global := filepath.Join(home, ".jad")
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	writeWorkspaceTestFile(t, filepath.Join(global, "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceTestFile(t, filepath.Join(global, "global.json"), `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	for _, dir := range []string{a, b} {
		writeWorkspaceTestFile(t, filepath.Join(dir, ".jad", "workspace.json"), `{}`)
		writeWorkspaceTestFile(t, filepath.Join(dir, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
		writeWorkspaceTestFile(t, filepath.Join(dir, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"]}`)
	}
	writeWorkspaceTestFile(t, filepath.Join(b, ".jad", "sessions", "only-b.json"), `{"schema_version":1}`)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	return profileMessenger{resolver: &Resolver{roots: loaded.Roots}}
}

// Design Q6: a tool of a session in workspace a reaches only a (profile_message too, unless
// the workspaces allow it both ways, §29). A session of b, named with or
// without its workspace, answers like a session that does not exist.
func TestWorkspaceToolsReachOnlyTheirOwnWorkspace(t *testing.T) {
	m := twoWorkspaces(t)
	ctx := context.Background()
	owner := "a:s1\x00dev/coder"
	if own, err := m.Profile(ctx, owner, "", ""); err != nil || own.Session != "a:s1" {
		t.Fatalf("own transcript: %+v, %v", own, err)
	}
	_, missing := m.Profile(ctx, owner, "a:nope", "dev/coder")
	for _, foreign := range []string{"b:s1", "b:only-b", "only-b"} {
		// transcript and transcripts (profile:<session>/... and subagent:<session>/...)
		_, err := m.Profile(ctx, owner, foreign, "dev/coder")
		if err == nil || missing == nil || err.Error() != missing.Error() {
			t.Errorf("transcript of %s: %v, want the error of a missing session %v", foreign, err, missing)
		}
		// transcripts session:<id> and profile list
		if profiles, sessions := m.Candidates(ctx, owner, foreign); profiles != nil || slices.ContainsFunc(sessions, func(s string) bool { return s[0] == 'b' }) {
			t.Errorf("candidates of %s: %q %q", foreign, profiles, sessions)
		}
		// profile_message to another session: nothing is accepted from another workspace unless
		// its workspace.json says so (§29, profile_message_workspace_test.go)
		if _, _, err := m.otherSession("a:s1", foreign, "dev/coder"); !errors.Is(err, native.ErrProfileUnknown) {
			t.Errorf("profile_message to %s: %v", foreign, err)
		}
		// profile info, compact and reset
		_, _, err = m.profileTarget(owner, foreign, "dev/coder")
		_, _, missingTarget := m.profileTarget(owner, "a:nope", "dev/coder")
		if err == nil || missingTarget == nil || err.Error() != missingTarget.Error() {
			t.Errorf("profile action on %s: %v, want %v", foreign, err, missingTarget)
		}
	}
	if sessions := m.Sessions(ctx, owner); !slices.Equal(sessions, []string{"a:s1"}) {
		t.Fatalf("sessions listed = %q", sessions)
	}
	// The other workspace sees its own, both of them.
	if sessions := m.Sessions(ctx, "b:s1\x00dev/coder"); !slices.Equal(sessions, []string{"b:only-b", "b:s1"}) {
		t.Fatalf("b's sessions = %q", sessions)
	}
	if reference, foreign, err := m.otherSession("b:s1", "only-b", "dev/coder"); err != nil || reference != "b:only-b" || foreign {
		t.Fatalf("b to its own session: %q, %v", reference, err)
	}
}
