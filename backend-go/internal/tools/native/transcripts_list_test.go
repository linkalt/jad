package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type fakeSessions []string

func (f fakeSessions) Sessions(context.Context) []string { return f }

// uniqueProfiles lists each profile once, as the real source does; the fake keys every
// profile twice so that an empty session name resolves.
type uniqueProfiles struct{ *fakeTranscriptProfiles }

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

func TestTranscriptsListsProfilesNewestFirstWithPathAndFile(t *testing.T) {
	f := newListFixture(t)
	out := f.call(t, nil, `{}`)
	coder, reviewer, never := strings.Index(out, "profile dev/coder"), strings.Index(out, "profile dev/reviewer"), strings.Index(out, "profile ops/never")
	if coder < 0 || !(coder < reviewer && reviewer < never) {
		t.Fatalf("order: %d %d %d\n%s", coder, reviewer, never, out)
	}
	line := lineOf(out, "profile dev/coder")
	messages := filepath.Join(f.dir, "demo", "dev/coder", "messages.jsonl")
	for _, want := range []string{"(yours)", "20", "19 bytes", "backup no", "path profile:demo/dev/coder", "file " + messages} {
		if !strings.Contains(line, want) {
			t.Errorf("line misses %q: %s", want, line)
		}
	}
	if !strings.Contains(lineOf(out, "ops/never"), "no transcript yet") {
		t.Errorf("never: %s", lineOf(out, "ops/never"))
	}
	if !strings.Contains(out, "3 profiles of session demo") || !strings.Contains(out, "path = what to give the transcript tool") {
		t.Errorf("header/legend missing:\n%s", out)
	}
}

func TestTranscriptsReportsTheBackupOfAProfile(t *testing.T) {
	f := newListFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "demo", "dev/reviewer", "messages.backup.jsonl"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.call(t, nil, `{}`)
	if !strings.Contains(lineOf(out, "dev/reviewer"), "backup yes") || !strings.Contains(lineOf(out, "dev/coder"), "backup no") {
		t.Fatalf("backup flags:\n%s", out)
	}
}

func TestTranscriptsFiltersGlobTheNameAndKeepTheRest(t *testing.T) {
	f := newListFixture(t)
	out := f.call(t, nil, `{"filters":["dev/*"]}`)
	if !strings.Contains(out, "2 profiles of session demo") || strings.Contains(out, "ops/never") {
		t.Fatalf("filter dev/*:\n%s", out)
	}
	out = f.call(t, nil, `{"filters":["REVIEW","dev*"]}`)
	if !strings.Contains(out, "1 profiles") && !strings.Contains(out, "1 profile") || strings.Contains(out, "dev/coder |") {
		t.Fatalf("two filters must both match:\n%s", out)
	}
	if out = f.call(t, nil, `{"filters":["zzz"]}`); !strings.Contains(out, "0 profiles of session demo") || strings.Contains(out, "each line") {
		t.Fatalf("no match:\n%s", out)
	}
}

func TestTranscriptsLimitAndCursorPageWithoutRepeatingOrLosing(t *testing.T) {
	f := newListFixture(t)
	for i := 0; i < 5; i++ {
		f.addProfile(t, "demo", "bulk/p"+string(rune('a'+i)), "x\n", time.Duration(i+2)*time.Hour)
	}
	var seen []string
	arguments := `{"limit":3}`
	for page := 0; page < 10; page++ {
		out := f.call(t, nil, arguments)
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "profile ") {
				seen = append(seen, strings.Fields(line)[1])
			}
		}
		cursor := ""
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "cursor: ") {
				cursor = strings.TrimPrefix(line, "cursor: ")
			}
		}
		if cursor == "" {
			break
		}
		if page == 0 {
			f.addProfile(t, "demo", "late/new", "x\n", 0) // appears between pages: must not repeat or lose others
		}
		arguments = `{"cursor":"` + cursor + `","limit":3}`
	}
	count := map[string]int{}
	for _, name := range seen {
		count[name]++
		if count[name] > 1 {
			t.Fatalf("%s listed twice: %v", name, seen)
		}
	}
	for _, want := range []string{"dev/coder", "dev/reviewer", "ops/never", "bulk/pa", "bulk/pe"} {
		if count[want] != 1 {
			t.Errorf("%s listed %d times: %v", want, count[want], seen)
		}
	}
}

func TestTranscriptsLimitIsCappedByTheConfiguration(t *testing.T) {
	f := newListFixture(t)
	f.settings.MaxEntries = 1
	out := f.call(t, nil, `{"limit":50}`)
	if !strings.Contains(out, "1 of 3 profiles") || !strings.Contains(out, "cursor") {
		t.Fatalf("cap:\n%s", out)
	}
}

func TestTranscriptsTooManyAsksForFilters(t *testing.T) {
	f := newListFixture(t)
	f.settings.ScanMaxEntries, f.settings.MaxEntries = 2, 2
	if out := f.call(t, nil, `{}`); !strings.Contains(out, "too_many") || !strings.Contains(out, "filters") {
		t.Fatalf("too many:\n%s", out)
	}
	if out := f.call(t, nil, `{"filters":["dev/*"]}`); strings.Contains(out, "too_many") {
		t.Fatalf("a filter that fits must list:\n%s", out)
	}
}

func TestTranscriptsMissingSessionAndDirectoriesListAsEmpty(t *testing.T) {
	f := newListFixture(t)
	f.profiles.targets["demo|dev/ghost"] = TranscriptTarget{Session: "demo", Profile: "dev/ghost", Files: TranscriptFiles{Messages: filepath.Join(f.dir, "no", "such", "dir", "messages.jsonl")}}
	out := f.call(t, nil, `{"filters":["ghost"]}`)
	if lineOf(out, "dev/ghost") != "" && !strings.Contains(lineOf(out, "dev/ghost"), "no transcript yet") {
		t.Fatalf("ghost:\n%s", out)
	}
	if out = f.call(t, nil, `{"path":"session:unknown"}`); !strings.Contains(out, "no_such_session") {
		t.Fatalf("unknown session:\n%s", out)
	}
	none := &listFixture{dir: t.TempDir(), profiles: &fakeTranscriptProfiles{targets: map[string]TranscriptTarget{}}, settings: transcriptsTestSettings()}
	tool, _ := NewTranscriptsTool(none.settings, TranscriptFiles{Messages: filepath.Join(none.dir, "x", "messages.jsonl")}, nil, nil, nil)
	out = callTranscripts(t, tool, `{}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, "no transcript yet") {
		t.Fatalf("without a profile source the caller's own profile lists:\n%s", out)
	}
}

func TestTranscriptsSwitchOffListsOnlyTheCallersProfile(t *testing.T) {
	f := newListFixture(t)
	f.settings.AllowOtherProfiles = false
	out := f.call(t, nil, `{}`)
	if !strings.Contains(out, "profile dev/coder") || strings.Contains(out, "dev/reviewer") || !strings.Contains(out, "allow_other_profiles is false") {
		t.Fatalf("only own profile:\n%s", out)
	}
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

func TestTranscriptsOtherSessionProfilesAndPathRuleDenial(t *testing.T) {
	f := newListFixture(t)
	secret := f.addProfile(t, "elsewhere", "dev/spy", "x\n", time.Minute)
	f.settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Dir(secret)}}}
	out := f.call(t, nil, `{"path":"session:elsewhere"}`)
	if strings.Contains(out, "dev/spy") || !strings.Contains(out, "1 denied by the read path rules") {
		t.Fatalf("denied profile must be hidden and counted:\n%s", out)
	}
	f.settings.Policy = sessionfs.NativePathPolicy{}
	if out = f.call(t, nil, `{"path":"session:elsewhere"}`); !strings.Contains(out, "path profile:elsewhere/dev/spy") {
		t.Fatalf("allowed:\n%s", out)
	}
}

func TestTranscriptsSymlinkedTranscriptOutsideItsFolderIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newListFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(outside, []byte(strings.Repeat("s", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	messages := filepath.Join(f.dir, "demo", "dev/reviewer", "messages.jsonl")
	_ = os.Remove(messages)
	if err := os.Symlink(outside, messages); err != nil {
		t.Fatal(err)
	}
	out := f.call(t, nil, `{}`)
	line := lineOf(out, "dev/reviewer")
	if !strings.Contains(line, "unreadable") || strings.Contains(line, "4096") {
		t.Fatalf("symlink out of the folder:\n%s", out)
	}
	_ = os.Remove(messages)
	inside := filepath.Join(filepath.Dir(messages), "real.jsonl")
	if err := os.WriteFile(inside, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, messages); err != nil {
		t.Fatal(err)
	}
	if line = lineOf(f.call(t, nil, `{}`), "dev/reviewer"); !strings.Contains(line, "4 bytes") {
		t.Fatalf("symlink inside the folder is followed: %s", line)
	}
}

func TestTranscriptsNeverWritesOrOpensATranscript(t *testing.T) {
	f := newListFixture(t)
	messages := f.addProfile(t, "demo", "dev/partial", "{\"type\":\"message\"}\n{\"type\":\"mess", time.Minute)
	before, _ := os.ReadFile(messages)
	info, _ := os.Stat(messages)
	f.call(t, nil, `{}`)
	after, _ := os.ReadFile(messages)
	later, _ := os.Stat(messages)
	if string(before) != string(after) || !info.ModTime().Equal(later.ModTime()) {
		t.Fatal("listing changed a transcript with a partial last line")
	}
}

func TestTranscriptsConcurrentCallsWhileFilesChangeLeaveNoGoroutines(t *testing.T) {
	f := newListFixture(t)
	tool := f.tool(t, fakeSessions{"demo"})
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			messages := filepath.Join(f.dir, "demo", "dev/coder", "messages.jsonl")
			_ = os.WriteFile(messages+".tmp", []byte("x\n"), 0o644)
			_ = os.Rename(messages+".tmp", messages)
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if out := callTranscripts(t, tool, `{}`, tools.ExecutionContext{SessionKey: listCaller}); !strings.Contains(out, `"status":"ok"`) {
					t.Errorf("call failed:\n%s", out)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestTranscriptsCancelledCallStops(t *testing.T) {
	f := newListFixture(t)
	tool := f.tool(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := tool.Execute(ctx, map[string]any{}, tools.ExecutionContext{CallID: "c", SessionKey: listCaller})
	if err != nil || !strings.Contains(out, "transcripts_cancelled") {
		t.Fatalf("out = %s, err = %v", out, err)
	}
}
