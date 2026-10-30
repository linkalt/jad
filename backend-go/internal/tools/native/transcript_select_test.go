package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type fakeTranscriptProfiles struct {
	targets map[string]TranscriptTarget
	asked   []string
}

func (f *fakeTranscriptProfiles) Candidates(_ context.Context, _, session string) ([]string, []string) {
	if session == "" || session == "demo" || session == "elsewhere" {
		var profiles []string
		for key := range f.targets {
			if _, profile, _ := strings.Cut(key, "|"); profile != "" {
				profiles = append(profiles, profile)
			}
		}
		return profiles, nil
	}
	return nil, []string{"demo", "elsewhere"}
}

func (f *fakeTranscriptProfiles) Profile(_ context.Context, _, session, profile string) (TranscriptTarget, error) {
	f.asked = append(f.asked, session+"|"+profile)
	target, ok := f.targets[session+"|"+profile]
	if !ok {
		return TranscriptTarget{}, errors.New("no such profile")
	}
	return target, nil
}

type selectFixture struct {
	dir        string
	own        string
	other      string
	foreign    string
	subRoot    string
	foreignDir string
	manager    *SubagentManager
	profiles   *fakeTranscriptProfiles
}

const selectCaller = "demo\x00dev/coder"

func newSelectFixture(t *testing.T) *selectFixture {
	t.Helper()
	dir := t.TempDir()
	f := &selectFixture{dir: dir, own: filepath.Join(dir, "own.jsonl"), other: filepath.Join(dir, "other.jsonl"), foreign: filepath.Join(dir, "foreign.jsonl"), subRoot: filepath.Join(dir, "sub-1")}
	writeTestTranscript(t, f.own, userLine("own question"), answerLine(1, 1, textItem("own answer")))
	writeTestTranscript(t, f.other, userLine("other question"), answerLine(1, 1, textItem("other answer")))
	writeTestTranscript(t, f.foreign, userLine("foreign question"), answerLine(1, 1, textItem("foreign answer")))
	if err := os.MkdirAll(f.subRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestTranscript(t, filepath.Join(f.subRoot, "messages.jsonl"), userLine("child task"), answerLine(1, 1, textItem("child answer")))
	f.manager = NewSubagentManager()
	f.manager.runs["sub_00000001"] = &subagentRun{id: "sub_00000001", owner: selectCaller, dir: f.subRoot}
	f.manager.runs["sub_00000002"] = &subagentRun{id: "sub_00000002", owner: "elsewhere\x00dev/coder", dir: f.subRoot}
	f.manager.runs["sub_00000003"] = &subagentRun{id: "sub_00000003", owner: selectCaller}
	reviewerRuns := NewSubagentManager()
	reviewerRuns.runs["sub_00000004"] = &subagentRun{id: "sub_00000004", owner: "demo\x00review/reviewer", dir: f.subRoot}
	f.foreignDir = filepath.Join(dir, "foreign-subagents")
	foreignRun := filepath.Join(f.foreignDir, "call-1")
	if err := os.MkdirAll(foreignRun, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestTranscript(t, filepath.Join(foreignRun, "messages.jsonl"), userLine("foreign child task"), answerLine(1, 1, textItem("foreign child answer")))
	foreignStore := newSubagentStore(f.foreignDir, nil)
	if err := foreignStore.write(subagentManifest{Version: subagentManifestVersion, ID: "sub_00000005", Owner: "elsewhere\x00dev/coder", State: subagentRunDone, StartedAt: time.Now(), Dir: foreignRun}); err != nil {
		t.Fatal(err)
	}
	f.profiles = &fakeTranscriptProfiles{targets: map[string]TranscriptTarget{
		"|review/reviewer":     {Session: "demo", Profile: "review/reviewer", Files: TranscriptFiles{Messages: f.other}, Runs: reviewerRuns},
		"demo|review/reviewer": {Session: "demo", Profile: "review/reviewer", Files: TranscriptFiles{Messages: f.other}, Runs: reviewerRuns},
		"elsewhere|dev/coder":  {Session: "elsewhere", Profile: "dev/coder", Files: TranscriptFiles{Messages: f.foreign}, SubagentDir: f.foreignDir},
		"|dev/coder":           {Session: "demo", Profile: "dev/coder", Files: TranscriptFiles{Messages: f.own}},
	}}
	return f
}

func (f *selectFixture) run(t *testing.T, settings sessionfs.NativeTranscriptSettings, arguments map[string]any, execution tools.ExecutionContext) string {
	t.Helper()
	tool, err := NewTranscriptTool(settings, TranscriptFiles{Messages: f.own}, f.profiles, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	execution.CallID = "c"
	if execution.SessionKey == "" {
		execution.SessionKey = selectCaller
	}
	if execution.WorkspaceRoot == "" {
		execution.WorkspaceRoot = f.dir
	}
	out, err := tool.Execute(t.Context(), arguments, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTranscriptOldSelectorKeysAreRejectedNamingPath(t *testing.T) {
	f := newSelectFixture(t)
	tool, _ := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{Messages: f.own}, f.profiles, f.manager)
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"profile": `"review/reviewer"`, "session": `"demo"`, "subagent": `"sub_00000001"`, "list": `true`} {
		call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c", "name": "transcript", "arguments": "{\"" + key + "\":" + value + "}"})
		raw, err := catalog.ExecuteFunctionCall(t.Context(), call, tools.ExecutionContext{SessionKey: selectCaller})
		out := string(raw)
		if err != nil || !strings.Contains(out, "transcript has no "+key) || !strings.Contains(out, "path") {
			t.Errorf("%s: %q, err %v", key, out, err)
		}
	}
}

func TestTranscriptPathRefusesBadNamesAndListsThePrefixes(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowPath = true
	for _, bad := range []string{"nowhere:x", "profile:", "profile:review", "profile:a/b/c/d", "profile:review//reviewer", "profile:../review/reviewer", "profile:review/./reviewer", "profile:review/rev\\iewer", "profile:/review/reviewer", "subagent:sub_zz", "subagent:demo/sub_00000001", "subagent:a/b/c/d/sub_00000001", "subagent:review/reviewer/sub_zz", "subagent:../sub_00000001", "subagent:", "plain"} {
		out := f.run(t, settings, map[string]any{"path": bad}, tools.ExecutionContext{})
		if !strings.Contains(out, "invalid_path") || !strings.Contains(out, "subagent:<run id>") || !strings.Contains(out, "subagent:<category>/<name>/<run id>") || !strings.Contains(out, "profile:<category>/<name>") {
			t.Errorf("%q: %s", bad, out)
		}
	}
	if len(f.profiles.asked) != 0 {
		t.Fatalf("a refused name must not reach the lookup: %v", f.profiles.asked)
	}
}

func TestTranscriptHeaderPrintsThePathToReuse(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:review/reviewer"}, tools.ExecutionContext{})
	if !strings.Contains(out, "profile:demo/review/reviewer: ") {
		t.Fatalf("profile header: %s", out)
	}
	out = f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_00000001"}, tools.ExecutionContext{})
	if !strings.Contains(out, "subagent:sub_00000001: ") {
		t.Fatalf("subagent header: %s", out)
	}
}

func TestTranscriptNotFoundNamesTheNearestCandidatesUpToTheCap(t *testing.T) {
	f := newSelectFixture(t)
	for i := 0; i < 30; i++ {
		f.profiles.targets["|cat/p"+strings.Repeat("x", i)] = TranscriptTarget{}
	}
	settings := transcriptTestSettings()
	settings.SuggestMax = 20
	out := f.run(t, settings, map[string]any{"path": "profile:review/revewer"}, tools.ExecutionContext{})
	if !strings.Contains(out, "no_such_profile") || !strings.Contains(out, "available") || !strings.Contains(out, "review/reviewer") {
		t.Fatalf("not found: %s", out)
	}
	if got := strings.Count(out, "cat/p"); got > 20 {
		t.Fatalf("%d suggestions, want at most 20: %s", got, out)
	}
	out = f.run(t, settings, map[string]any{"path": "profile:nosuch/review/reviewer"}, tools.ExecutionContext{})
	if !strings.Contains(out, "elsewhere") || !strings.Contains(out, "demo") {
		t.Fatalf("an unknown session must list the sessions: %s", out)
	}
	out = f.run(t, settings, map[string]any{"path": "subagent:sub_deadbeef"}, tools.ExecutionContext{})
	if !strings.Contains(out, "no_such_run") || !strings.Contains(out, "sub_00000001") || strings.Contains(out, "sub_00000002") {
		t.Fatalf("the caller's own runs are suggested: %s", out)
	}
}

func TestTranscriptNotFoundHidesOtherSessionsWhenTheyAreOff(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowOtherSessions = false
	for _, path := range []string{"profile:nosuch/review/reviewer", "profile:elsewhere/cat/nosuch"} {
		if out := f.run(t, settings, map[string]any{"path": path}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_profile") || strings.Contains(out, "available") || strings.Contains(out, "review/reviewer") {
			t.Fatalf("%s names other sessions: %s", path, out)
		}
	}
	if out := f.run(t, settings, map[string]any{"path": "profile:review/revewer"}, tools.ExecutionContext{}); !strings.Contains(out, "review/reviewer") {
		t.Fatalf("the caller's own profiles stay suggested: %s", out)
	}
}

func TestTranscriptSubagentFollowsTheProfileLayout(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:review/reviewer/sub_00000004"}, tools.ExecutionContext{})
	if !strings.Contains(out, "child answer") || !strings.Contains(out, "subagent:demo/review/reviewer/sub_00000004: ") {
		t.Fatalf("a run of another profile in this session: %s", out)
	}
	out = f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:elsewhere/dev/coder/sub_00000005"}, tools.ExecutionContext{})
	if !strings.Contains(out, "foreign child answer") || !strings.Contains(out, "subagent:elsewhere/dev/coder/sub_00000005: ") {
		t.Fatalf("a run of another session read from its manifest: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:dev/coder/sub_00000001"}, tools.ExecutionContext{}); !strings.Contains(out, "child answer") || !strings.Contains(out, "subagent:sub_00000001: ") {
		t.Fatalf("the caller's own profile spelled out reads the same run under the short path: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:review/reviewer/sub_00000001"}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_run") {
		t.Fatalf("a run that belongs to another profile's storage: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:elsewhere/dev/coder/sub_00000004"}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_run") || !strings.Contains(out, "sub_00000005") {
		t.Fatalf("an unknown run lists the runs of that profile: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:nosuch/review/reviewer/sub_00000004"}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_profile") {
		t.Fatalf("an unknown session: %s", out)
	}
}

func TestTranscriptSubagentOfAnotherProfileKeepsTheAccessSwitches(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowOtherProfiles = false
	if out := f.run(t, settings, map[string]any{"path": "subagent:review/reviewer/sub_00000004"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_profile_denied") {
		t.Fatalf("other profiles off: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowOtherSessions = false
	if out := f.run(t, settings, map[string]any{"path": "subagent:elsewhere/dev/coder/sub_00000005"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_session_denied") {
		t.Fatalf("other sessions off: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowChildren = false
	if out := f.run(t, settings, map[string]any{"path": "subagent:review/reviewer/sub_00000004"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_subagent_denied") {
		t.Fatalf("a run the caller did not start with allow_children off: %s", out)
	}
	settings = transcriptTestSettings()
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: f.subRoot}}}
	if out := f.run(t, settings, map[string]any{"path": "subagent:review/reviewer/sub_00000004"}, tools.ExecutionContext{}); !strings.Contains(out, "path_denied") {
		t.Fatalf("a denied run folder: %s", out)
	}
}

func TestTranscriptCursorKeepsTheProfileQualifiedRun(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 25
	out := f.run(t, settings, map[string]any{"path": "subagent:review/reviewer/sub_00000004", "turns": "1-"}, tools.ExecutionContext{})
	cursor := cursorOf(out)
	if cursor == "" {
		t.Fatalf("response was not cut: %s", out)
	}
	if next := f.run(t, settings, map[string]any{"cursor": cursor}, tools.ExecutionContext{}); !strings.Contains(next, "subagent:demo/review/reviewer/sub_00000004: ") {
		t.Fatalf("the cursor must carry the canonical path: %s", next)
	}
}

func TestTranscriptResultNamesTheFileItRead(t *testing.T) {
	f := newSelectFixture(t)
	cases := map[string]string{
		"":                                      f.own,
		"profile:review/reviewer":               f.other,
		"subagent:sub_00000001":                 filepath.Join(f.subRoot, "messages.jsonl"),
		"subagent:review/reviewer/sub_00000004": filepath.Join(f.subRoot, "messages.jsonl"),
	}
	for path, want := range cases {
		arguments := map[string]any{}
		if path != "" {
			arguments["path"] = path
		}
		if out := f.run(t, transcriptTestSettings(), arguments, tools.ExecutionContext{}); !strings.Contains(out, "transcript: "+want+"\n") {
			t.Errorf("%q: the result must name %s: %s", path, want, out)
		}
	}
}

func TestTranscriptDefaultIsTheSubagentsOwnRunWhenCalledByAChild(t *testing.T) {
	f := newSelectFixture(t)
	child := tools.ExecutionContext{AgentDepth: 1, ProcessOwner: "sub_00000001"}
	if out := f.run(t, transcriptTestSettings(), map[string]any{}, child); !strings.Contains(out, "child answer") || strings.Contains(out, "own answer") {
		t.Fatalf("a sub-agent without path must read its own run: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{}, tools.ExecutionContext{}); !strings.Contains(out, "own answer") {
		t.Fatalf("the session's own run reads the session transcript: %s", out)
	}
}

func TestTranscriptReadsAnotherProfileAndSessionByDefault(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:review/reviewer"}, tools.ExecutionContext{})
	if !strings.Contains(out, "other answer") || !strings.Contains(out, "profile:demo/review/reviewer") {
		t.Fatalf("other profile: %s", out)
	}
	out = f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:elsewhere/dev/coder"}, tools.ExecutionContext{})
	if !strings.Contains(out, "foreign answer") {
		t.Fatalf("other session: %s", out)
	}
}

func TestTranscriptAccessSwitchesRefuseOtherProfilesAndSessions(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowOtherProfiles = false
	if out := f.run(t, settings, map[string]any{"path": "profile:review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_profile_denied") || !strings.Contains(out, "allow_other_profiles") {
		t.Fatalf("other profiles off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"path": "profile:dev/coder"}, tools.ExecutionContext{}); !strings.Contains(out, "own answer") {
		t.Fatalf("the caller's own profile stays readable: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowOtherSessions = false
	if out := f.run(t, settings, map[string]any{"path": "profile:elsewhere/dev/coder"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_session_denied") || !strings.Contains(out, "allow_other_sessions") {
		t.Fatalf("other sessions off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"path": "profile:review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "other answer") {
		t.Fatalf("another profile of the same session stays readable: %s", out)
	}
}

func TestTranscriptOtherProfileFollowsTheReadPathRules(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: f.other}}}
	if out := f.run(t, settings, map[string]any{"path": "profile:review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "path_denied") {
		t.Fatalf("a denied transcript must not be read through the tool: %s", out)
	}
	settings.Policy = sessionfs.NativePathPolicy{}
	scoped := tools.ExecutionContext{PathScope: &tools.PathScope{Allow: []string{f.dir + "/nowhere"}}}
	if out := f.run(t, settings, map[string]any{"path": "profile:review/reviewer"}, scoped); !strings.Contains(out, "path_denied") {
		t.Fatalf("a call scope that does not reach the transcript must refuse it: %s", out)
	}
}

func TestTranscriptSubagentByRunIdOwnedOrAllowed(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_00000001"}, tools.ExecutionContext{})
	if !strings.Contains(out, "child answer") || !strings.Contains(out, "subagent:sub_00000001") {
		t.Fatalf("owned run: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "child answer") {
		t.Fatalf("a run of another session is readable by default: %s", out)
	}
	settings := transcriptTestSettings()
	settings.AllowOtherSessions = false
	if out := f.run(t, settings, map[string]any{"path": "subagent:sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_session_denied") {
		t.Fatalf("another session's run with allow_other_sessions off: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowChildren = false
	if out := f.run(t, settings, map[string]any{"path": "subagent:sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_subagent_denied") {
		t.Fatalf("a run the caller does not own with allow_children off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"path": "subagent:sub_00000001"}, tools.ExecutionContext{}); !strings.Contains(out, "child answer") {
		t.Fatalf("the caller's own run stays readable: %s", out)
	}
}

func TestTranscriptSubagentErrors(t *testing.T) {
	f := newSelectFixture(t)
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_deadbeef"}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_run") {
		t.Fatalf("unknown run: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_00000003"}, tools.ExecutionContext{}); !strings.Contains(out, "no_transcript_yet") {
		t.Fatalf("run without a transcript: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "subagent:sub_00000001", "source": "backup"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("backup of a sub-agent: %s", out)
	}
}

func TestTranscriptPathIsOffByDefault(t *testing.T) {
	f := newSelectFixture(t)
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": "path:" + f.other}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_path_disabled") || !strings.Contains(out, "allow_path") {
		t.Fatalf("path with allow_path off: %s", out)
	}
}

func TestTranscriptPathReadsATranscriptThatPassesTheReadRules(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowPath = true
	if out := f.run(t, settings, map[string]any{"path": "path:" + f.other}, tools.ExecutionContext{}); !strings.Contains(out, "other answer") {
		t.Fatalf("path: %s", out)
	}
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: f.other}}}
	if out := f.run(t, settings, map[string]any{"path": "path:" + f.other}, tools.ExecutionContext{}); !strings.Contains(out, "path_denied") {
		t.Fatalf("a denied path: %s", out)
	}
}

func TestTranscriptPathRefusesAFileThatIsNotATranscriptAndAnEscapingSymlink(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowPath = true
	notes := filepath.Join(f.dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("just some text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := f.run(t, settings, map[string]any{"path": "path:" + notes}, tools.ExecutionContext{}); !strings.Contains(out, "not_a_transcript") {
		t.Fatalf("text file: %s", out)
	}
	outside := t.TempDir()
	writeTestTranscript(t, filepath.Join(outside, "secret.jsonl"), userLine("secret"))
	link := filepath.Join(f.dir, "link.jsonl")
	if err := os.Symlink(filepath.Join(outside, "secret.jsonl"), link); err != nil {
		t.Fatal(err)
	}
	if out := f.run(t, settings, map[string]any{"path": "path:" + link}, tools.ExecutionContext{}); strings.Contains(out, "secret") {
		t.Fatalf("a symlink out of the workspace must follow the read rules: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"path": "path:" + notes, "source": "backup"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("backup of a path: %s", out)
	}
}

func TestTranscriptCursorKeepsItsSelectorAndRefusesANewOne(t *testing.T) {
	f := newSelectFixture(t)
	lines := []string{userLine("start")}
	for step := 1; step <= 12; step++ {
		lines = append(lines, answerLine(1, step, textItem("page-"+strings.Repeat("x", 120))))
	}
	writeTestTranscript(t, f.other, lines...)
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 400
	first := f.run(t, settings, map[string]any{"path": "profile:review/reviewer", "turns": "1"}, tools.ExecutionContext{})
	cursor := cursorOf(first)
	if cursor == "" {
		t.Fatalf("expected a cursor: %s", first)
	}
	next := f.run(t, settings, map[string]any{"cursor": cursor}, tools.ExecutionContext{})
	if !strings.Contains(next, "profile:demo/review/reviewer") || !strings.Contains(next, "page-") {
		t.Fatalf("the cursor must continue on the profile it was made for: %s", next)
	}
	if out := f.run(t, settings, map[string]any{"cursor": cursor, "path": "profile:dev/coder"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("a cursor with a selector: %s", out)
	}
	settings.AllowOtherProfiles = false
	if out := f.run(t, settings, map[string]any{"cursor": cursor}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_profile_denied") {
		t.Fatalf("a cursor must not outlive a switch turned off: %s", out)
	}
}

func TestTranscriptWithoutASourceReadsOnlyTheOwnTranscript(t *testing.T) {
	f := newSelectFixture(t)
	tool, _ := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{Messages: f.own}, nil, nil)
	for _, arguments := range []map[string]any{{"path": "profile:review/reviewer"}, {"path": "subagent:sub_00000001"}} {
		out, _ := tool.Execute(t.Context(), arguments, tools.ExecutionContext{CallID: "c", SessionKey: selectCaller})
		if !strings.Contains(out, "unavailable") {
			t.Errorf("%v: %s", arguments, out)
		}
	}
}

type messengerWithProfiles struct{ *fakeTranscriptProfiles }

func (messengerWithProfiles) Prepare(context.Context, ProfileMessageRequest) (ProfileMessagePlan, error) {
	return nil, errors.New("not used")
}

func TestBuildCatalogGivesTheTranscriptToolTheMessengerProfilesAndTheRunManager(t *testing.T) {
	f := newSelectFixture(t)
	catalog, err := BuildCatalogWithMessenger(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(f.dir, "setting.json"),
		ToolDefinitionPaths: []string{sampleTranscriptManifest},
		StoragePaths:        sessionfs.SessionStoragePaths{Messages: f.own},
		NativeTranscript:    &sessionfs.NativeTranscriptSettings{ResponseMaxBytes: 1 << 16, PreviewBytes: 200, ValueBytes: 80, MaxListEntries: 100, MaxDetailBytes: 16384, LineMaxBytes: 1 << 20, ScanMaxBytes: 1 << 30, TimeoutMs: 30000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true},
	}, nil, nil, nil, nil, nil, nil, nil, f.manager, nil, messengerWithProfiles{f.profiles})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{"path":"profile:review/reviewer"}`, `{"path":"subagent:sub_00000001"}`} {
		call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c", "name": "transcript", "arguments": arguments})
		raw, err := catalog.ExecuteFunctionCall(t.Context(), call, tools.ExecutionContext{SessionKey: selectCaller, WorkspaceRoot: f.dir})
		out := string(raw)
		if err != nil || strings.Contains(out, "unavailable") || !strings.Contains(out, "answer") {
			t.Errorf("%s: %q, err %v", arguments, out, err)
		}
	}
}
