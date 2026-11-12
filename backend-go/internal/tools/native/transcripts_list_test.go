package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type fakeSessions []string

func (f fakeSessions) Sessions(context.Context, string) []string { return f }

// uniqueProfiles lists each profile once, as the real source does; the fake keys every
// profile twice so that an empty session name resolves.
type uniqueProfiles struct{ *fakeTranscriptProfiles }

// fakeProfilesMu guards the fake's record of lookups, which concurrent calls append to.
var fakeProfilesMu sync.Mutex

func (u uniqueProfiles) Profile(ctx context.Context, owner, session, profile string) (TranscriptTarget, error) {
	fakeProfilesMu.Lock()
	defer fakeProfilesMu.Unlock()
	return u.fakeTranscriptProfiles.Profile(ctx, owner, session, profile)
}

func (u uniqueProfiles) Candidates(ctx context.Context, owner, session string) ([]string, []string) {
	profiles, sessions := u.fakeTranscriptProfiles.Candidates(ctx, owner, session)
	seen := map[string]bool{}
	var unique []string
	for _, name := range profiles {
		if !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	return unique, sessions
}

// listFixture is a session "demo" whose profiles have transcripts of known age and size.
type listFixture struct {
	dir      string
	profiles *fakeTranscriptProfiles
	settings sessionfs.NativeTranscriptsSettings
}

const listCaller = "demo\x00dev/coder"

func (f *listFixture) addProfile(t *testing.T, session, profile, content string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(f.dir, session, profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	messages := filepath.Join(dir, "messages.jsonl")
	if content != "" {
		if err := os.WriteFile(messages, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(messages, when, when); err != nil {
			t.Fatal(err)
		}
	}
	f.profiles.targets[session+"|"+profile] = TranscriptTarget{Session: session, Profile: profile, Files: TranscriptFiles{Messages: messages, Backup: filepath.Join(dir, "messages.backup.jsonl")}}
	if session == "demo" {
		f.profiles.targets["|"+profile] = f.profiles.targets[session+"|"+profile]
	}
	return messages
}

func newListFixture(t *testing.T) *listFixture {
	t.Helper()
	f := &listFixture{dir: t.TempDir(), profiles: &fakeTranscriptProfiles{targets: map[string]TranscriptTarget{}}, settings: transcriptsTestSettings()}
	f.addProfile(t, "demo", "dev/coder", "{\"type\":\"message\"}\n", time.Minute)
	f.addProfile(t, "demo", "dev/reviewer", "{\"type\":\"message\"}\n{\"type\":\"message\"}\n", time.Hour)
	f.addProfile(t, "demo", "ops/never", "", 0)
	return f
}

func (f *listFixture) tool(t *testing.T, sessions TranscriptSessions) tools.Tool {
	t.Helper()
	tool, err := NewTranscriptsTool(f.settings, TranscriptFiles{}, uniqueProfiles{f.profiles}, sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func (f *listFixture) call(t *testing.T, sessions TranscriptSessions, arguments string) string {
	t.Helper()
	return callTranscripts(t, f.tool(t, sessions), arguments, tools.ExecutionContext{SessionKey: listCaller})
}

func lineOf(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestTranscriptsSessionsLevel(t *testing.T) {
	f := newListFixture(t)
	out := f.call(t, fakeSessions{"demo", "elsewhere", "demo2"}, `{"path":"sessions","filters":["demo*"]}`)
	if !strings.Contains(out, "2 sessions") || !strings.Contains(out, "path session:demo2") || strings.Contains(out, "elsewhere") {
		t.Fatalf("sessions:\n%s", out)
	}
	f.settings.AllowOtherSessions = false
	out = f.call(t, fakeSessions{"demo", "elsewhere"}, `{"path":"sessions"}`)
	if !strings.Contains(out, "1 sessions") || strings.Contains(out, "elsewhere") || !strings.Contains(out, "allow_other_sessions is false") {
		t.Fatalf("sessions switch off:\n%s", out)
	}
}
