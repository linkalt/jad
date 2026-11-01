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

func profileTestSettings() sessionfs.NativeProfileSettings {
	return sessionfs.NativeProfileSettings{MaxEntries: 50, ScanMaxEntries: 2000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
}

// profileFixture is the transcripts list fixture (real profile transcripts of known size and
// age) behind native.profile.
type profileFixture struct {
	list *listFixture
	tool tools.Tool
}

func newProfileFixture(t *testing.T, settings sessionfs.NativeProfileSettings) *profileFixture {
	t.Helper()
	list := newListFixture(t)
	return &profileFixture{list: list, tool: newProfileTool(t, settings, list.profiles)}
}

func newProfileTool(t *testing.T, settings sessionfs.NativeProfileSettings, profiles *fakeTranscriptProfiles) tools.Tool {
	t.Helper()
	tool, err := NewProfileTool(settings, TranscriptFiles{}, uniqueProfiles{profiles}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func profileSettingsWithPolicy(policy sessionfs.NativePathPolicy) sessionfs.NativeProfileSettings {
	settings := profileTestSettings()
	settings.Policy = policy
	return settings
}

func (f *profileFixture) call(t *testing.T, arguments string, execution tools.ExecutionContext) string {
	t.Helper()
	if execution.SessionKey == "" {
		execution.SessionKey = listCaller
	}
	return callTranscripts(t, f.tool, arguments, execution)
}

func TestProfileListShowsTheProfilesNewestFirst(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	coder, reviewer := strings.Index(out, "profile dev/coder"), strings.Index(out, "profile dev/reviewer")
	if coder < 0 || reviewer < 0 || coder > reviewer {
		t.Fatalf("list order wrong:\n%s", out)
	}
	if !strings.Contains(out, "dev/coder (yours)") || !strings.Contains(out, "path profile:demo/dev/coder") {
		t.Fatalf("own profile not marked or no path:\n%s", out)
	}
	if !strings.Contains(out, "no transcript yet") {
		t.Fatalf("profile with no transcript not reported:\n%s", out)
	}
}

func TestProfileListHonoursAllowOtherProfiles(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowOtherProfiles = false
	f := newProfileFixture(t, settings)
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	if strings.Contains(out, "dev/reviewer") || !strings.Contains(out, "dev/coder") {
		t.Fatalf("only the caller's profile may be listed:\n%s", out)
	}
}

func TestProfileListPagesWithLimitAndCursor(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	first := f.call(t, `{"action":"list","limit":1}`, tools.ExecutionContext{})
	cursor := ""
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "cursor: ") {
			cursor = strings.TrimPrefix(line, "cursor: ")
		}
	}
	if cursor == "" || !strings.Contains(first, "truncated") {
		t.Fatalf("first page has no cursor:\n%s", first)
	}
	second := f.call(t, `{"action":"list","cursor":"`+cursor+`"}`, tools.ExecutionContext{})
	if strings.Contains(second, "profile dev/coder") || !strings.Contains(second, "profile dev/reviewer") {
		t.Fatalf("second page is wrong:\n%s", second)
	}
}

func TestProfileListRefusesAProfilePath(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	out := f.call(t, `{"action":"list","path":"profile:dev/coder"}`, tools.ExecutionContext{})
	if !strings.Contains(out, "invalid_path") {
		t.Fatalf("list with a profile path = %s", out)
	}
}

// The line of a profile carries its file path, backup flag and the no-transcript case: the
// assertions transcripts' profile listing had, now on profile's list.
func TestProfileListLineCarriesFileBackupAndNoTranscript(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	line := lineOf(out, "profile dev/coder")
	messages := filepath.Join(f.list.dir, "demo", "dev/coder", "messages.jsonl")
	for _, want := range []string{"(yours)", "19 bytes", "backup no", "path profile:demo/dev/coder", "file " + messages} {
		if !strings.Contains(line, want) {
			t.Errorf("line misses %q: %s", want, line)
		}
	}
	if !strings.Contains(lineOf(out, "ops/never"), "no transcript yet") {
		t.Errorf("never: %s", lineOf(out, "ops/never"))
	}
	if !strings.Contains(out, "3 profiles of session demo") {
		t.Errorf("header missing:\n%s", out)
	}
}

func TestProfileListReportsTheBackupOfAProfile(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	if err := os.WriteFile(filepath.Join(f.list.dir, "demo", "dev/reviewer", "messages.backup.jsonl"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	if !strings.Contains(lineOf(out, "dev/reviewer"), "backup yes") || !strings.Contains(lineOf(out, "dev/coder"), "backup no") {
		t.Fatalf("backup flags:\n%s", out)
	}
}

func TestProfileListFiltersGlobTheName(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	out := f.call(t, `{"action":"list","filters":["dev/*"]}`, tools.ExecutionContext{})
	if !strings.Contains(out, "2 profiles of session demo") || strings.Contains(out, "ops/never") {
		t.Fatalf("filter dev/*:\n%s", out)
	}
	out = f.call(t, `{"action":"list","filters":["REVIEW","dev*"]}`, tools.ExecutionContext{})
	if !strings.Contains(out, "1 profiles of session demo") || !strings.Contains(out, "dev/reviewer") || strings.Contains(out, "dev/coder") {
		t.Fatalf("filters REVIEW and dev* together:\n%s", out)
	}
}

func TestProfileListTooManyAsksForFilters(t *testing.T) {
	settings := profileTestSettings()
	settings.ScanMaxEntries = 1
	f := newProfileFixture(t, settings)
	if out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{}); !strings.Contains(out, "too_many") || !strings.Contains(out, "filters") {
		t.Fatalf("too many:\n%s", out)
	}
}

func TestProfileListHidesPathsForTheRemoteAPI(t *testing.T) {
	settings := profileTestSettings()
	settings.HidePaths = true
	f := newProfileFixture(t, settings)
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	if strings.Contains(out, " | file ") || strings.Contains(out, f.list.dir) {
		t.Fatalf("a hidden-path list shows a file path:\n%s", out)
	}
}

func TestProfileListUnknownSessionIsNotFound(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	out := f.call(t, `{"action":"list","path":"session:unknown"}`, tools.ExecutionContext{})
	if !strings.Contains(out, "no_such_session") {
		t.Fatalf("unknown session:\n%s", out)
	}
}

// A profile whose transcript the read path rules deny is left out and counted, as transcripts did.
func TestProfileListHidesADeniedTranscript(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	secret := f.list.addProfile(t, "demo", "dev/spy", "x\n", time.Minute)
	f.tool = newProfileTool(t, profileSettingsWithPolicy(sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Dir(secret)}}}), f.list.profiles)
	out := f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	if strings.Contains(out, "dev/spy") || !strings.Contains(out, "1 not readable by this caller") {
		t.Fatalf("denied profile must be hidden and counted:\n%s", out)
	}
}

// A transcript that is a symlink out of its folder is reported unreadable, not followed; one
// inside the folder is followed.
func TestProfileListSymlinkedTranscriptIsNotFollowedOutsideItsFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newProfileFixture(t, profileTestSettings())
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(outside, []byte(strings.Repeat("s", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	messages := filepath.Join(f.list.dir, "demo", "dev/reviewer", "messages.jsonl")
	_ = os.Remove(messages)
	if err := os.Symlink(outside, messages); err != nil {
		t.Fatal(err)
	}
	line := lineOf(f.call(t, `{"action":"list"}`, tools.ExecutionContext{}), "dev/reviewer")
	if !strings.Contains(line, "unreadable") || strings.Contains(line, "4096") {
		t.Fatalf("symlink out of the folder:\n%s", line)
	}
	_ = os.Remove(messages)
	inside := filepath.Join(filepath.Dir(messages), "real.jsonl")
	if err := os.WriteFile(inside, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, messages); err != nil {
		t.Fatal(err)
	}
	if line = lineOf(f.call(t, `{"action":"list"}`, tools.ExecutionContext{}), "dev/reviewer"); !strings.Contains(line, "4 bytes") {
		t.Fatalf("symlink inside the folder is followed: %s", line)
	}
}

// A listing never writes or opens a transcript for writing, even one with a partial last line.
func TestProfileListNeverWritesATranscript(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	messages := f.list.addProfile(t, "demo", "dev/partial", "{\"type\":\"message\"}\n{\"type\":\"mess", time.Minute)
	before, _ := os.ReadFile(messages)
	info, _ := os.Stat(messages)
	f.call(t, `{"action":"list"}`, tools.ExecutionContext{})
	after, _ := os.ReadFile(messages)
	later, _ := os.Stat(messages)
	if string(before) != string(after) || !info.ModTime().Equal(later.ModTime()) {
		t.Fatal("listing changed a transcript with a partial last line")
	}
}

// Concurrent listings while the transcripts change stay correct and leave no goroutines.
func TestProfileListConcurrentCallsWhileFilesChangeLeaveNoGoroutines(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			messages := filepath.Join(f.list.dir, "demo", "dev/coder", "messages.jsonl")
			_ = os.WriteFile(messages+".tmp", []byte("x\n"), 0o644)
			_ = os.Rename(messages+".tmp", messages)
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if out := callTranscripts(t, f.tool, `{"action":"list"}`, tools.ExecutionContext{SessionKey: listCaller}); !strings.Contains(out, `"status":"ok"`) {
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

// A cancelled listing reports profile_cancelled and stops.
func TestProfileListCancelledCallStops(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := f.tool.Execute(ctx, map[string]any{"action": "list"}, tools.ExecutionContext{CallID: "c", SessionKey: listCaller})
	if err != nil || !strings.Contains(out, "profile_cancelled") {
		t.Fatalf("out = %s, err = %v", out, err)
	}
}

// The configured cap bounds one list response even when the call asks for more.
func TestProfileListLimitIsCappedByTheConfiguration(t *testing.T) {
	settings := profileTestSettings()
	settings.MaxEntries = 1
	f := newProfileFixture(t, settings)
	out := f.call(t, `{"action":"list","limit":50}`, tools.ExecutionContext{})
	if !strings.Contains(out, "1 profiles of session demo") || !strings.Contains(out, "cursor") {
		t.Fatalf("cap:\n%s", out)
	}
}
