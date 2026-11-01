package native

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// runsFixture is the profile demo|dev/coder with a sub-agent storage directory of manifests.
type runsFixture struct {
	*listFixture
	storage string
	target  TranscriptTarget
}

const (
	runOwn   = "demo\x00dev/coder"
	runOther = "other\x00dev/x"
)

func newRunsFixture(t *testing.T) *runsFixture {
	t.Helper()
	f := &runsFixture{listFixture: newListFixture(t)}
	f.storage = filepath.Join(f.dir, "storage", ".subagents")
	f.target = f.profiles.targets["demo|dev/coder"]
	f.target.SubagentDir, f.target.PersistRuns = f.storage, true
	f.profiles.targets["demo|dev/coder"] = f.target
	f.profiles.targets["|dev/coder"] = f.target
	return f
}

// addRun writes a manifest and, when content is not empty, the run's transcript folder.
func (f *runsFixture) addRun(t *testing.T, id, owner, state, agent, call, content string, age time.Duration) string {
	t.Helper()
	dir := ""
	if call != "" {
		dir = filepath.Join(f.storage, call+"-0a1b2c3d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			file := filepath.Join(dir, "messages.jsonl")
			if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			when := time.Now().Add(-age)
			_ = os.Chtimes(file, when, when)
		}
	}
	started := time.Now().Add(-age - time.Minute).UTC().Truncate(time.Second)
	manifest := subagentManifest{Version: subagentManifestVersion, ID: id, Owner: owner, Agent: agent, State: state, StartedAt: started, Dir: dir}
	if state != subagentRunRunning && state != subagentRunQueued {
		manifest.EndedAt = started.Add(time.Minute)
	}
	if err := newSubagentStore(f.storage, nil).write(manifest); err != nil {
		t.Fatal(err)
	}
	return dir
}

// set stores the target under both keys the fake resolves it by.
func (f *runsFixture) set() {
	f.profiles.targets["demo|dev/coder"] = f.target
	f.profiles.targets["|dev/coder"] = f.target
}

func (f *runsFixture) calls(t *testing.T, manager *SubagentManager, arguments string) string {
	t.Helper()
	tool, err := NewTranscriptsTool(f.settings, TranscriptFiles{}, uniqueProfiles{f.profiles}, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	return callTranscripts(t, tool, arguments, tools.ExecutionContext{SessionKey: runOwn})
}

func TestTranscriptsListsRunsWithPathFileStatusAndParentCall(t *testing.T) {
	f := newRunsFixture(t)
	dir := f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "researcher", "call_00_abc", "{\"type\":\"message\"}\n", time.Hour)
	f.addRun(t, "sub_bbbbbbbb", runOwn, subagentRunRunning, "planner", "", "", time.Minute)
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	line := lineOf(out, "sub_aaaaaaaa")
	for _, want := range []string{"done", "agent researcher", "call call_00_abc", "19 bytes", "path subagent:sub_aaaaaaaa", "file " + filepath.Join(dir, "messages.jsonl"), "ended"} {
		if !strings.Contains(line, want) {
			t.Errorf("run line misses %q: %s", want, line)
		}
	}
	if !strings.Contains(lineOf(out, "sub_bbbbbbbb"), "no transcript yet") || strings.Index(out, "sub_bbbbbbbb") > strings.Index(out, "sub_aaaaaaaa") {
		t.Errorf("newest first, queued run has no transcript yet:\n%s", out)
	}
	if !strings.Contains(out, "2 sub-agent runs of profile:demo/dev/coder") || strings.Contains(out, "persist_runs is off") {
		t.Errorf("header:\n%s", out)
	}
}

func TestTranscriptsStatusAndFiltersNarrowRuns(t *testing.T) {
	f := newRunsFixture(t)
	f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "researcher", "call_1", "x\n", time.Hour)
	f.addRun(t, "sub_bbbbbbbb", runOwn, subagentRunFailed, "planner", "call_2", "x\n", 2*time.Hour)
	f.addRun(t, "sub_cccccccc", runOwn, subagentRunDone, "planner", "call_3", "x\n", 3*time.Hour)
	if out := f.calls(t, nil, `{"path":"profile:dev/coder","status":"done"}`); !strings.Contains(out, "2 sub-agent runs") || strings.Contains(out, "sub_bbbbbbbb") {
		t.Errorf("status:\n%s", out)
	}
	if out := f.calls(t, nil, `{"path":"profile:dev/coder","filters":["plan*"]}`); !strings.Contains(out, "sub_bbbbbbbb") || strings.Contains(out, "sub_aaaaaaaa") {
		t.Errorf("filter on the agent name:\n%s", out)
	}
	if out := f.calls(t, nil, `{"path":"profile:dev/coder","filters":["call_3"]}`); !strings.Contains(out, "1 sub-agent runs") || !strings.Contains(out, "sub_cccccccc") {
		t.Errorf("filter on the parent call:\n%s", out)
	}
}

func TestTranscriptsRunsPageWithCursor(t *testing.T) {
	f := newRunsFixture(t)
	for i, id := range []string{"sub_00000001", "sub_00000002", "sub_00000003", "sub_00000004", "sub_00000005"} {
		f.addRun(t, id, runOwn, subagentRunDone, "a", "call_"+id, "x\n", time.Duration(i+1)*time.Hour)
	}
	seen := map[string]int{}
	arguments := `{"path":"profile:dev/coder","limit":2}`
	for page := 0; page < 6; page++ {
		out := f.calls(t, nil, arguments)
		cursor := ""
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "run ") {
				seen[strings.Fields(line)[1]]++
			}
			if strings.HasPrefix(line, "cursor: ") {
				cursor = strings.TrimPrefix(line, "cursor: ")
			}
		}
		if cursor == "" {
			break
		}
		arguments = `{"cursor":"` + cursor + `","limit":2}`
	}
	if len(seen) != 5 {
		t.Fatalf("seen = %v", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s listed %d times", id, n)
		}
	}
}

func TestTranscriptsNotesPersistRunsOffAndNoStorage(t *testing.T) {
	f := newRunsFixture(t)
	f.target.PersistRuns = false
	f.set()
	if out := f.calls(t, nil, `{"path":"profile:dev/coder"}`); !strings.Contains(out, "persist_runs is off") || !strings.Contains(out, "0 sub-agent runs") {
		t.Errorf("persist off:\n%s", out)
	}
	f.target.SubagentDir = ""
	f.set()
	if out := f.calls(t, nil, `{"path":"profile:dev/coder"}`); !strings.Contains(out, "no sub-agent storage") || !strings.Contains(out, `"status":"ok"`) {
		t.Errorf("no storage:\n%s", out)
	}
}

func TestTranscriptsMissingStorageDirectoryListsAsEmpty(t *testing.T) {
	f := newRunsFixture(t)
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	if !strings.Contains(out, `"status":"ok"`) || !strings.Contains(out, "0 sub-agent runs") || strings.Contains(out, "each line") {
		t.Fatalf("missing storage dir:\n%s", out)
	}
}

func TestTranscriptsMergesLiveRunsWithManifestsAndLiveWins(t *testing.T) {
	f := newRunsFixture(t)
	f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunRunning, "researcher", "call_1", "x\n", time.Hour)
	manager := NewSubagentManager()
	manager.runs["sub_aaaaaaaa"] = &subagentRun{id: "sub_aaaaaaaa", owner: runOwn, agent: "researcher", state: subagentRunDone, startedAt: time.Now().Add(-time.Hour), endedAt: time.Now(), dir: filepath.Join(f.storage, "call_1-0a1b2c3d")}
	manager.runs["sub_dddddddd"] = &subagentRun{id: "sub_dddddddd", owner: runOwn, agent: "live", state: subagentRunRunning, startedAt: time.Now()}
	f.target.Runs = manager
	f.set()
	out := f.calls(t, manager, `{"path":"profile:dev/coder"}`)
	if !strings.Contains(out, "2 sub-agent runs") || !strings.Contains(lineOf(out, "sub_aaaaaaaa"), "done") || !strings.Contains(out, "sub_dddddddd") {
		t.Fatalf("live and manifest runs:\n%s", out)
	}
}

func TestTranscriptsRunOwnerRules(t *testing.T) {
	f := newRunsFixture(t)
	f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "a", "call_1", "x\n", time.Hour)
	f.addRun(t, "sub_bbbbbbbb", runOther, subagentRunDone, "b", "call_2", "x\n", 2*time.Hour)
	f.addRun(t, "sub_cccccccc", "demo\x00ops/never", subagentRunDone, "c", "call_3", "x\n", 3*time.Hour)
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	if !strings.Contains(out, "3 sub-agent runs") {
		t.Fatalf("defaults list every run of the storage:\n%s", out)
	}
	f.settings.AllowChildren = false
	out = f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	if !strings.Contains(out, "sub_aaaaaaaa") || strings.Contains(out, "sub_bbbbbbbb") || strings.Contains(out, "sub_cccccccc") || !strings.Contains(out, "not readable by this caller") {
		t.Fatalf("allow_children off: only the caller's own runs:\n%s", out)
	}
	f.settings.AllowChildren, f.settings.AllowOtherSessions = true, false
	out = f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	if strings.Contains(out, "sub_bbbbbbbb") || !strings.Contains(out, "sub_cccccccc") {
		t.Fatalf("allow_other_sessions off hides the other session's run only:\n%s", out)
	}
}

func TestTranscriptsOtherProfileRunsUseTheOtherProfilesStorage(t *testing.T) {
	f := newRunsFixture(t)
	other := f.profiles.targets["demo|dev/reviewer"]
	other.SubagentDir = filepath.Join(f.dir, "storage-reviewer", ".subagents")
	f.profiles.targets["demo|dev/reviewer"] = other
	f.profiles.targets["|dev/reviewer"] = other
	store := newSubagentStore(other.SubagentDir, nil)
	if err := store.write(subagentManifest{Version: subagentManifestVersion, ID: "sub_eeeeeeee", Owner: "demo\x00dev/reviewer", Agent: "r", State: subagentRunDone, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	out := f.calls(t, nil, `{"path":"profile:dev/reviewer"}`)
	if !strings.Contains(out, "path subagent:demo/dev/reviewer/sub_eeeeeeee") {
		t.Fatalf("another profile's run path:\n%s", out)
	}
	if out = f.calls(t, nil, `{"path":"profile:dev/nobody"}`); !strings.Contains(out, "no_such_profile") {
		t.Fatalf("unknown profile:\n%s", out)
	}
	f.settings.AllowOtherProfiles = false
	if out = f.calls(t, nil, `{"path":"profile:dev/reviewer"}`); !strings.Contains(out, "transcripts_profile_denied") {
		t.Fatalf("switch off:\n%s", out)
	}
}

func TestTranscriptsRunTranscriptOutsideTheStorageIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newRunsFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "messages.jsonl"), []byte(strings.Repeat("s", 777)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.storage, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.storage, "call_x-0a1b2c3d")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := newSubagentStore(f.storage, nil).write(subagentManifest{Version: subagentManifestVersion, ID: "sub_ffffffff", Owner: runOwn, Agent: "a", State: subagentRunDone, StartedAt: time.Now().UTC(), Dir: link}); err != nil {
		t.Fatal(err)
	}
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	line := lineOf(out, "sub_ffffffff")
	if !strings.Contains(line, "unreadable") || strings.Contains(line, "777") || strings.Contains(line, outside) {
		t.Fatalf("symlinked run folder:\n%s", out)
	}
}

func TestTranscriptsRunDeniedByPathRulesIsHiddenAndCounted(t *testing.T) {
	f := newRunsFixture(t)
	dir := f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "a", "call_1", "x\n", time.Hour)
	f.settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: dir}}}
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	if strings.Contains(out, "sub_aaaaaaaa") || !strings.Contains(out, "1 not readable by this caller") {
		t.Fatalf("denied run:\n%s", out)
	}
}

func TestTranscriptsRunListingNeverWritesAnything(t *testing.T) {
	f := newRunsFixture(t)
	dir := f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "a", "call_1", "{\"type\":\"mess", time.Hour)
	snapshot := func() map[string]string {
		seen := map[string]string{}
		_ = filepath.Walk(f.dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				data, _ := os.ReadFile(path)
				seen[path] = info.ModTime().String() + string(data)
			}
			return nil
		})
		return seen
	}
	before := snapshot()
	f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	f.calls(t, nil, `{}`)
	after := snapshot()
	if len(before) != len(after) {
		t.Fatalf("files before %d, after %d (dir %s)", len(before), len(after), dir)
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("%s changed", path)
		}
	}
}

func TestTranscriptsListedPathsResolveInTheTranscriptTool(t *testing.T) {
	f := newRunsFixture(t)
	f.addRun(t, "sub_aaaaaaaa", runOwn, subagentRunDone, "a", "call_1", userLine("hello")+"\n", time.Hour)
	out := f.calls(t, nil, `{"path":"profile:dev/coder"}`)
	pathOf := func(out, kind string) string {
		line := lineOf(out, kind)
		_, rest, _ := strings.Cut(line, "| path ")
		path, _, _ := strings.Cut(rest, " ")
		return path
	}
	profileTool, err := NewProfileTool(profileTestSettings(), TranscriptFiles{}, uniqueProfiles{f.profiles}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runPath, profilePath := pathOf(out, "sub_aaaaaaaa"), pathOf(callTranscripts(t, profileTool, `{"action":"list"}`, tools.ExecutionContext{SessionKey: runOwn}), "profile dev/reviewer")
	if runPath != "subagent:sub_aaaaaaaa" || profilePath != "profile:demo/dev/reviewer" {
		t.Fatalf("paths = %q, %q", runPath, profilePath)
	}
	reader, err := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{}, uniqueProfiles{f.profiles}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSubagentManager()
	manager.runs["sub_aaaaaaaa"] = &subagentRun{id: "sub_aaaaaaaa", owner: runOwn, dir: filepath.Join(f.storage, "call_1-0a1b2c3d"), done: make(chan struct{})}
	reader, err = NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{}, uniqueProfiles{f.profiles}, manager)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{runPath, profilePath} {
		result, err := reader.Execute(context.Background(), map[string]any{"path": path}, tools.ExecutionContext{CallID: "c", SessionKey: runOwn})
		if err != nil || strings.Contains(result, "invalid_path") || strings.Contains(result, "no_such") {
			t.Errorf("transcript refused %q: %s (%v)", path, result, err)
		}
	}
}
