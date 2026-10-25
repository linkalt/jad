package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type fakeTranscriptProfiles struct {
	targets map[string]TranscriptTarget
	asked   []string
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
	dir      string
	own      string
	other    string
	foreign  string
	subRoot  string
	manager  *SubagentManager
	profiles *fakeTranscriptProfiles
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
	f.profiles = &fakeTranscriptProfiles{targets: map[string]TranscriptTarget{
		"|review/reviewer":     {Session: "demo", Profile: "review/reviewer", Files: TranscriptFiles{Messages: f.other}},
		"demo|review/reviewer": {Session: "demo", Profile: "review/reviewer", Files: TranscriptFiles{Messages: f.other}},
		"elsewhere|dev/coder":  {Session: "elsewhere", Profile: "dev/coder", Files: TranscriptFiles{Messages: f.foreign}},
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

func TestTranscriptSelectorsAreMutuallyExclusiveAndNameBothKeys(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowPath = true
	for _, tc := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"profile": "review/reviewer", "path": f.other}, "profile and path"},
		{map[string]any{"profile": "review/reviewer", "subagent": "sub_00000001"}, "profile and subagent"},
		{map[string]any{"subagent": "sub_00000001", "path": f.other}, "subagent and path"},
		{map[string]any{"session": "demo"}, "session needs profile"},
	} {
		out := f.run(t, settings, tc.arguments, tools.ExecutionContext{})
		if !strings.Contains(out, "conflicting_selectors") || !strings.Contains(out, tc.want) {
			t.Errorf("%v: want %q, got %s", tc.arguments, tc.want, out)
		}
	}
}

func TestTranscriptReadsAnotherProfileAndSessionByDefault(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"profile": "review/reviewer"}, tools.ExecutionContext{})
	if !strings.Contains(out, "other answer") || !strings.Contains(out, "review/reviewer @ demo") {
		t.Fatalf("other profile: %s", out)
	}
	out = f.run(t, transcriptTestSettings(), map[string]any{"profile": "dev/coder", "session": "elsewhere"}, tools.ExecutionContext{})
	if !strings.Contains(out, "foreign answer") {
		t.Fatalf("other session: %s", out)
	}
}

func TestTranscriptAccessSwitchesRefuseOtherProfilesAndSessions(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowOtherProfiles = false
	if out := f.run(t, settings, map[string]any{"profile": "review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_profile_denied") || !strings.Contains(out, "allow_other_profiles") {
		t.Fatalf("other profiles off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"profile": "dev/coder"}, tools.ExecutionContext{}); !strings.Contains(out, "own answer") {
		t.Fatalf("the caller's own profile stays readable: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowOtherSessions = false
	if out := f.run(t, settings, map[string]any{"profile": "dev/coder", "session": "elsewhere"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_session_denied") || !strings.Contains(out, "allow_other_sessions") {
		t.Fatalf("other sessions off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"profile": "review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "other answer") {
		t.Fatalf("another profile of the same session stays readable: %s", out)
	}
}

func TestTranscriptOtherProfileFollowsTheReadPathRules(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: f.other}}}
	if out := f.run(t, settings, map[string]any{"profile": "review/reviewer"}, tools.ExecutionContext{}); !strings.Contains(out, "path_denied") {
		t.Fatalf("a denied transcript must not be read through the tool: %s", out)
	}
	settings.Policy = sessionfs.NativePathPolicy{}
	scoped := tools.ExecutionContext{PathScope: &tools.PathScope{Allow: []string{f.dir + "/nowhere"}}}
	if out := f.run(t, settings, map[string]any{"profile": "review/reviewer"}, scoped); !strings.Contains(out, "path_denied") {
		t.Fatalf("a call scope that does not reach the transcript must refuse it: %s", out)
	}
}

func TestTranscriptSubagentByRunIdOwnedOrAllowed(t *testing.T) {
	f := newSelectFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"subagent": "sub_00000001"}, tools.ExecutionContext{})
	if !strings.Contains(out, "child answer") || !strings.Contains(out, "subagent sub_00000001") {
		t.Fatalf("owned run: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"subagent": "sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "child answer") {
		t.Fatalf("a run of another session is readable by default: %s", out)
	}
	settings := transcriptTestSettings()
	settings.AllowOtherSessions = false
	if out := f.run(t, settings, map[string]any{"subagent": "sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_session_denied") {
		t.Fatalf("another session's run with allow_other_sessions off: %s", out)
	}
	settings = transcriptTestSettings()
	settings.AllowChildren = false
	if out := f.run(t, settings, map[string]any{"subagent": "sub_00000002"}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_subagent_denied") {
		t.Fatalf("a run the caller does not own with allow_children off: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"subagent": "sub_00000001"}, tools.ExecutionContext{}); !strings.Contains(out, "child answer") {
		t.Fatalf("the caller's own run stays readable: %s", out)
	}
}

func TestTranscriptSubagentErrors(t *testing.T) {
	f := newSelectFixture(t)
	if out := f.run(t, transcriptTestSettings(), map[string]any{"subagent": "sub_deadbeef"}, tools.ExecutionContext{}); !strings.Contains(out, "no_such_run") {
		t.Fatalf("unknown run: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"subagent": "sub_00000003"}, tools.ExecutionContext{}); !strings.Contains(out, "no_transcript_yet") {
		t.Fatalf("run without a transcript: %s", out)
	}
	if out := f.run(t, transcriptTestSettings(), map[string]any{"subagent": "sub_00000001", "source": "backup"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("backup of a sub-agent: %s", out)
	}
}

func TestTranscriptPathIsOffByDefault(t *testing.T) {
	f := newSelectFixture(t)
	if out := f.run(t, transcriptTestSettings(), map[string]any{"path": f.other}, tools.ExecutionContext{}); !strings.Contains(out, "transcript_path_disabled") || !strings.Contains(out, "allow_path") {
		t.Fatalf("path with allow_path off: %s", out)
	}
}

func TestTranscriptPathReadsATranscriptThatPassesTheReadRules(t *testing.T) {
	f := newSelectFixture(t)
	settings := transcriptTestSettings()
	settings.AllowPath = true
	if out := f.run(t, settings, map[string]any{"path": f.other}, tools.ExecutionContext{}); !strings.Contains(out, "other answer") {
		t.Fatalf("path: %s", out)
	}
	settings.Policy = sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: f.other}}}
	if out := f.run(t, settings, map[string]any{"path": f.other}, tools.ExecutionContext{}); !strings.Contains(out, "path_denied") {
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
	if out := f.run(t, settings, map[string]any{"path": notes}, tools.ExecutionContext{}); !strings.Contains(out, "not_a_transcript") {
		t.Fatalf("text file: %s", out)
	}
	outside := t.TempDir()
	writeTestTranscript(t, filepath.Join(outside, "secret.jsonl"), userLine("secret"))
	link := filepath.Join(f.dir, "link.jsonl")
	if err := os.Symlink(filepath.Join(outside, "secret.jsonl"), link); err != nil {
		t.Fatal(err)
	}
	if out := f.run(t, settings, map[string]any{"path": link}, tools.ExecutionContext{}); strings.Contains(out, "secret") {
		t.Fatalf("a symlink out of the workspace must follow the read rules: %s", out)
	}
	if out := f.run(t, settings, map[string]any{"path": notes, "source": "backup"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
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
	first := f.run(t, settings, map[string]any{"profile": "review/reviewer", "turns": "1"}, tools.ExecutionContext{})
	cursor := cursorOf(first)
	if cursor == "" {
		t.Fatalf("expected a cursor: %s", first)
	}
	next := f.run(t, settings, map[string]any{"cursor": cursor}, tools.ExecutionContext{})
	if !strings.Contains(next, "review/reviewer @ demo") || !strings.Contains(next, "page-") {
		t.Fatalf("the cursor must continue on the profile it was made for: %s", next)
	}
	if out := f.run(t, settings, map[string]any{"cursor": cursor, "profile": "dev/coder"}, tools.ExecutionContext{}); !strings.Contains(out, "conflicting_selectors") {
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
	for _, arguments := range []map[string]any{{"profile": "review/reviewer"}, {"subagent": "sub_00000001"}} {
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
		NativeTranscript:    &sessionfs.NativeTranscriptSettings{ResponseMaxBytes: 1 << 16, EntryMaxBytes: 4000, LineMaxBytes: 1 << 20, MaxTurnsPerCall: 50, ScanMaxBytes: 1 << 30, TimeoutMs: 30000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true},
	}, nil, nil, nil, nil, nil, nil, nil, f.manager, nil, messengerWithProfiles{f.profiles})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{"profile":"review/reviewer"}`, `{"subagent":"sub_00000001"}`} {
		call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "c", "name": "transcript", "arguments": arguments})
		raw, err := catalog.ExecuteFunctionCall(t.Context(), call, tools.ExecutionContext{SessionKey: selectCaller, WorkspaceRoot: f.dir})
		out := string(raw)
		if err != nil || strings.Contains(out, "unavailable") || !strings.Contains(out, "answer") {
			t.Errorf("%s: %q, err %v", arguments, out, err)
		}
	}
}
