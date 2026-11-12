package native

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fakeProfileActions records what reached the port, so a refusal can be shown to stop before it.
type fakeProfileActions struct {
	tokens    *int64
	calls     []string
	documents []sessionfs.ProfileDocumentChange
	failErr   error
}

func (f *fakeProfileActions) ProfileInfo(_ context.Context, _, session, profile string) (*int64, error) {
	f.calls = append(f.calls, "info "+session+" "+profile)
	return f.tokens, nil
}

func (f *fakeProfileActions) CompactProfile(_ context.Context, _, session, profile string) error {
	f.calls = append(f.calls, "compact "+session+" "+profile)
	return f.failErr
}

func (f *fakeProfileActions) ResetProfile(_ context.Context, _, session, profile string) error {
	f.calls = append(f.calls, "reset "+session+" "+profile)
	return f.failErr
}

func (f *fakeProfileActions) ProfileConfig(_ context.Context, ownerKey string, _ sessionfs.ProfileRun) (sessionfs.ProfileConfig, error) {
	f.calls = append(f.calls, "config "+strings.ReplaceAll(ownerKey, "\x00", " "))
	return sessionfs.ProfileConfig{Profile: "own"}, f.failErr
}

func (f *fakeProfileActions) EditProfile(_ context.Context, ownerKey string, request sessionfs.ProfileEditRequest) (sessionfs.ProfileEditResult, error) {
	f.calls = append(f.calls, request.Action+" "+strings.ReplaceAll(ownerKey, "\x00", " ")+" "+request.IfMatch+" "+request.HistoryID+" "+string(request.Contents))
	f.documents = append(f.documents, request.Documents...)
	return sessionfs.ProfileEditResult{Profile: "own", Action: request.Action}, f.failErr
}

func (f *fakeProfileActions) ProfileHistory(_ context.Context, ownerKey string, limit int) (sessionfs.ProfileHistory, error) {
	f.calls = append(f.calls, "history "+strings.ReplaceAll(ownerKey, "\x00", " ")+" "+strconv.Itoa(limit))
	return sessionfs.ProfileHistory{Profile: "own"}, f.failErr
}

func newProfileActionsTool(t *testing.T, settings sessionfs.NativeProfileSettings, actions ProfileActions) tools.Tool {
	t.Helper()
	tool, err := NewProfileTool(settings, TranscriptFiles{}, nil, actions)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestProfileChildrenMayNotCompactOrReset(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	for _, action := range []string{"compact", "reset"} {
		out := callTranscripts(t, tool, `{"action":"`+action+`","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
		if !strings.Contains(out, "profile_children_denied") {
			t.Fatalf("%s from a sub-agent = %s", action, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a refused child action reached the port: %v", actions.calls)
	}
}

func TestProfileChildrenMayStillInfo(t *testing.T) {
	tokens := int64(7)
	actions := &fakeProfileActions{tokens: &tokens}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	out := callTranscripts(t, tool, `{"action":"info","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "context tokens: 7") {
		t.Fatalf("info from a sub-agent = %s", out)
	}
}

func TestProfileSelfTargetIsRefusedAndTellsTheCaller(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	for _, action := range []string{"compact", "reset"} {
		for _, path := range []string{"", "profile:dev/coder", "profile:demo/dev/coder"} {
			arguments := `{"action":"` + action + `"`
			if path != "" {
				arguments += `,"path":"` + path + `"`
			}
			out := callTranscripts(t, tool, arguments+`}`, tools.ExecutionContext{SessionKey: listCaller})
			if !strings.Contains(out, "profile_self") || !strings.Contains(out, "another profile") {
				t.Fatalf("%s of self (%q) = %s", action, path, out)
			}
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a self-target action reached the port: %v", actions.calls)
	}
}

// self is the caller's own profile, read from the session key: the same profile as no path.
func TestProfileSelfInfoReadsTheCallerProfile(t *testing.T) {
	tokens := int64(7)
	actions := &fakeProfileActions{tokens: &tokens}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	out := callTranscripts(t, tool, `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "context tokens: 7") || strings.Join(actions.calls, " | ") != "info demo dev/coder" {
		t.Fatalf("info of self = %s, calls %v", out, actions.calls)
	}
}

func TestProfileSelfCompactAndResetAreProfileSelf(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	for _, action := range []string{"compact", "reset"} {
		out := callTranscripts(t, tool, `{"action":"`+action+`","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller})
		if !strings.Contains(out, "profile_self") {
			t.Fatalf("%s of self = %s", action, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a self action reached the port: %v", actions.calls)
	}
}

func TestProfileSubAgentSelfFollowsAllowChildren(t *testing.T) {
	tokens := int64(7)
	actions := &fakeProfileActions{tokens: &tokens}
	out := callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), actions), `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "context tokens: 7") || strings.Join(actions.calls, " | ") != "info demo dev/coder" {
		t.Fatalf("sub-agent info of self with allow_children on = %s, calls %v", out, actions.calls)
	}
	out = callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), actions), `{"action":"compact","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "profile_children_denied") || len(actions.calls) != 1 {
		t.Fatalf("sub-agent compact of self = %s, calls %v", out, actions.calls)
	}
	settings := profileTestSettings()
	settings.AllowChildren = false
	actions = &fakeProfileActions{tokens: &tokens}
	out = callTranscripts(t, newProfileActionsTool(t, settings, actions), `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "profile_children_denied") || len(actions.calls) != 0 {
		t.Fatalf("sub-agent info of self with allow_children off = %s, calls %v", out, actions.calls)
	}
}

func TestProfileSelfNeedsNoOtherSessionOrProfileSwitch(t *testing.T) {
	tokens := int64(7)
	actions := &fakeProfileActions{tokens: &tokens}
	settings := profileTestSettings()
	settings.AllowOtherSessions = false
	settings.AllowOtherProfiles = false
	out := callTranscripts(t, newProfileActionsTool(t, settings, actions), `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "context tokens: 7") || strings.Join(actions.calls, " | ") != "info demo dev/coder" {
		t.Fatalf("info of self with the other switches off = %s, calls %v", out, actions.calls)
	}
}

// Without a caller there is no own profile to act on, so self is refused before the port.
func TestProfileSelfWithoutACallerIsRefusedBeforeThePort(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	for _, key := range []string{"", "demo"} {
		for _, arguments := range []string{`{"action":"info","path":"self"}`, `{"action":"compact","path":"self"}`, `{"action":"reset","path":"self"}`} {
			out := callTranscripts(t, tool, arguments, tools.ExecutionContext{SessionKey: key})
			if !strings.Contains(out, "profile_caller_unknown") {
				t.Fatalf("self with session key %q: %s = %s", key, arguments, out)
			}
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a self action without a caller reached the port: %v", actions.calls)
	}
}

// A session key with a NUL but no profile part names no profile, so self is refused the same way.
func TestProfileSelfWithoutAProfileIsRefusedBeforeThePort(t *testing.T) {
	actions := &fakeProfileActions{}
	out := callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), actions), `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: "demo\x00"})
	if !strings.Contains(out, "profile_caller_unknown") {
		t.Fatalf("self with session key demo NUL = %s, calls %v", out, actions.calls)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("self with an empty profile part reached the port: %v", actions.calls)
	}
}

// A session key with a NUL but no session part names no session, so self is refused the same way.
func TestProfileSelfWithoutASessionIsRefusedBeforeThePort(t *testing.T) {
	actions := &fakeProfileActions{}
	out := callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), actions), `{"action":"info","path":"self"}`, tools.ExecutionContext{SessionKey: "\x00demo"})
	if !strings.Contains(out, "profile_caller_unknown") {
		t.Fatalf("self with session key NUL demo = %s, calls %v", out, actions.calls)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("self with an empty session part reached the port: %v", actions.calls)
	}
}

func TestProfileListOfSelfIsInvalidPath(t *testing.T) {
	out := callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), &fakeProfileActions{}), `{"action":"list","path":"self"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "invalid_path") || strings.Contains(out, "profiles of session") {
		t.Fatalf("list of self = %s", out)
	}
}

func TestProfileResetWhileActiveIsReportedNotHidden(t *testing.T) {
	actions := &fakeProfileActions{failErr: errors.New("cannot reset a profile while a run is active")}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	out := callTranscripts(t, tool, `{"action":"reset","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "profile_reset_failed") || !strings.Contains(out, "while a run is active") {
		t.Fatalf("reset while a run is active = %s", out)
	}
}

func TestProfileInfoWithNoUsageSaysSoAndIsNotZero(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	out := callTranscripts(t, tool, `{"action":"info","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "no usage yet") || strings.Contains(out, "context tokens: 0") {
		t.Fatalf("info with no usage = %s", out)
	}
}

func TestProfileCompactAndResetReachThePortForAnotherProfile(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	callTranscripts(t, tool, `{"action":"compact","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller})
	callTranscripts(t, tool, `{"action":"reset","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller})
	if strings.Join(actions.calls, " | ") != "compact demo dev/reviewer | reset demo dev/reviewer" {
		t.Fatalf("calls = %v", actions.calls)
	}
}

// The switches apply to the tool's own actions, not only to the listing: a sub-agent is refused
// every action when allow_children is off, and the other session and other profile switches
// refuse info, compact and reset of them before the port is reached.
func TestProfileChildrenSwitchRefusesEveryActionWhenOff(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowChildren = false
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, settings, actions)
	for _, arguments := range []string{`{}`, `{"action":"list"}`, `{"action":"info","path":"profile:dev/reviewer"}`} {
		out := callTranscripts(t, tool, arguments, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
		if !strings.Contains(out, "profile_children_denied") || !strings.Contains(out, "allow_children is false") {
			t.Fatalf("%s from a sub-agent with allow_children off = %s", arguments, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a refused child call reached the port: %v", actions.calls)
	}
}

func TestProfileChildrenSwitchOnKeepsSubAgentInfo(t *testing.T) {
	tokens := int64(3)
	actions := &fakeProfileActions{tokens: &tokens}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	out := callTranscripts(t, tool, `{"action":"info","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "context tokens: 3") {
		t.Fatalf("info from a sub-agent with allow_children on = %s", out)
	}
}

func TestProfileOtherSessionSwitchRefusesResetAndInfo(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowOtherSessions = false
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, settings, actions)
	for _, arguments := range []string{
		`{"action":"reset","path":"profile:other/dev/reviewer"}`,
		`{"action":"compact","path":"profile:other/dev/reviewer"}`,
		`{"action":"info","path":"profile:other/dev/reviewer"}`,
		`{"action":"list","path":"session:other"}`,
	} {
		out := callTranscripts(t, tool, arguments, tools.ExecutionContext{SessionKey: listCaller})
		if !strings.Contains(out, "profile_session_denied") {
			t.Fatalf("%s with allow_other_sessions off = %s", arguments, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a cross-session call reached the port: %v", actions.calls)
	}
}

func TestProfileOtherProfileSwitchRefusesInfoCompactAndReset(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowOtherProfiles = false
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, settings, actions)
	for _, action := range []string{"info", "compact", "reset"} {
		out := callTranscripts(t, tool, `{"action":"`+action+`","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller})
		if !strings.Contains(out, "profile_profile_denied") || !strings.Contains(out, "allow_other_profiles is false") {
			t.Fatalf("%s of another profile with allow_other_profiles off = %s", action, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a cross-profile call reached the port: %v", actions.calls)
	}
}

// filters reach the listing through the Validator, as a catalog call does, not only when the
// arguments are passed to the tool directly.
func TestProfileFiltersPassTheValidator(t *testing.T) {
	f := newProfileFixture(t, profileTestSettings())
	validator, err := tools.CompileSchema(profileInputSchema)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := validator.Validate([]byte(`{"action":"list","filters":["*reviewer"]}`))
	if err != nil || len(issues) != 0 {
		t.Fatalf("list with filters was refused by the schema: %v %v", issues, err)
	}
	out := f.call(t, `{"action":"list","filters":["*reviewer"]}`, tools.ExecutionContext{})
	if strings.Contains(out, "dev/coder") || !strings.Contains(out, "dev/reviewer") {
		t.Fatalf("filters did not narrow the list:\n%s", out)
	}
}

func TestProfileSubAgentListOfAProfilePathIsRefused(t *testing.T) {
	out := callTranscripts(t, newProfileActionsTool(t, profileTestSettings(), &fakeProfileActions{}), `{"action":"list","path":"sessions"}`, tools.ExecutionContext{SessionKey: listCaller})
	if !strings.Contains(out, "invalid_path") || strings.Contains(out, "profiles of session") {
		t.Fatalf("list of sessions = %s", out)
	}
}

// A sub-agent with allow_children off is refused before the remote branch, so it reads no remote
// profile either: the refusal is the children one, not the remote one.
func TestProfileChildrenSwitchRefusesARemoteInfoWhenOff(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowChildren = false
	settings.AllowRemote = true
	out := callTranscripts(t, newProfileActionsTool(t, settings, &fakeProfileActions{}), `{"action":"info","path":"remote:peer/profile:main/review/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1})
	if !strings.Contains(out, "profile_children_denied") {
		t.Fatalf("remote info from a sub-agent with allow_children off = %s", out)
	}
}

// config reads only the caller's own profile: by no path, self or its own name, also in plan
// mode; another profile and a sub-agent are refused before the port, whatever the gates say.
func TestProfileConfigIsTheCallersOwnOnly(t *testing.T) {
	settings := profileTestSettings()
	settings.AllowOtherProfiles, settings.AllowOtherSessions, settings.AllowChildren = true, true, true
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, settings, actions)
	for _, path := range []string{"", "self", "profile:dev/coder", "profile:demo/dev/coder"} {
		arguments := `{"action":"config"`
		if path != "" {
			arguments += `,"path":"` + path + `"`
		}
		for _, plan := range []bool{false, true} {
			out := callTranscripts(t, tool, arguments+`}`, tools.ExecutionContext{SessionKey: listCaller, PlanMode: plan})
			if !strings.Contains(out, `"profile": "own"`) {
				t.Fatalf("config of %q (plan %v) = %s", path, plan, out)
			}
		}
	}
	if want := "config demo dev/coder"; len(actions.calls) != 8 || actions.calls[0] != want {
		t.Fatalf("calls = %v", actions.calls)
	}
	actions.calls = nil
	for path, code := range map[string]string{"profile:dev/other": "profile_config_self_only", "profile:other/dev/coder": "profile_config_self_only", "session:demo": "invalid_path"} {
		out := callTranscripts(t, tool, `{"action":"config","path":"`+path+`"}`, tools.ExecutionContext{SessionKey: listCaller})
		if !strings.Contains(out, code) {
			t.Fatalf("config of %s = %s", path, out)
		}
	}
	if out := callTranscripts(t, tool, `{"action":"config"}`, tools.ExecutionContext{SessionKey: listCaller, AgentDepth: 1}); !strings.Contains(out, "profile_children_denied") {
		t.Fatalf("config from a sub-agent = %s", out)
	}
	if out := callTranscripts(t, tool, `{"action":"config"}`, tools.ExecutionContext{}); !strings.Contains(out, "profile_caller_unknown") {
		t.Fatalf("config without a caller = %s", out)
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a refused config reached the port: %v", actions.calls)
	}
}
