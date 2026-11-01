package native

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fakeProfileActions records what reached the port, so a refusal can be shown to stop before it.
type fakeProfileActions struct {
	tokens  *int64
	calls   []string
	failErr error
}

func (f *fakeProfileActions) ProfileInfo(_ context.Context, session, profile string) (*int64, error) {
	f.calls = append(f.calls, "info "+session+" "+profile)
	return f.tokens, nil
}

func (f *fakeProfileActions) CompactProfile(_ context.Context, session, profile string) error {
	f.calls = append(f.calls, "compact "+session+" "+profile)
	return f.failErr
}

func (f *fakeProfileActions) ResetProfile(_ context.Context, session, profile string) error {
	f.calls = append(f.calls, "reset "+session+" "+profile)
	return f.failErr
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
