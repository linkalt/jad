package native

import (
	"context"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// R6 follow-up: profile_message is checked on the caller config's max_depth (depthExceeds, the
// rule sub-agent starts use): the run a message starts is one level below the caller's RunDepth.
// The check comes after Prepare (so its refusals keep their precedence) and before Start (so an
// over-limit message is never queued, started or made a handle).

func TestProfileMessagePastMaxDepthIsRefusedBeforeAnythingStarts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		execution tools.ExecutionContext
		refused   bool
	}{
		{"user run", tools.ExecutionContext{SessionKey: "s1:main"}, false},
		{"sub-agent at depth 1", tools.ExecutionContext{SessionKey: "s1:main", AgentDepth: 1}, false},
		{"profile target at chain depth 1", tools.ExecutionContext{SessionKey: "s1:main", ChainDepth: 1}, false},
		{"sub-agent at depth 2", tools.ExecutionContext{SessionKey: "s1:main", AgentDepth: 2}, true},
		{"profile target at chain depth 2", tools.ExecutionContext{SessionKey: "s1:main", ChainDepth: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture's tool has the default max_depth 2 (no subagent section).
			f := newProfileMessageHandleFixture(t, nil, nil)
			out := f.send(t, tc.execution, 0)
			if refused := strings.Contains(out, "profile_depth_exceeded"); refused != tc.refused {
				t.Fatalf("refused = %v, want %v:\n%s", refused, tc.refused, out)
			}
			if f.messenger.prepared != 1 {
				t.Fatalf("prepared %d times, want 1", f.messenger.prepared)
			}
			if !tc.refused {
				f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "ok"})
				return
			}
			if !strings.Contains(out, "tools.native.subagent.max_depth") || strings.Contains(out, "pm_") {
				t.Fatalf("refusal text:\n%s", out)
			}
			if n := f.messenger.started(); n != 0 {
				t.Fatalf("%d runs started past max_depth", n)
			}
			if list := f.asyncCall(t, tc.execution, map[string]any{"action": "list", "state": "all"}); strings.Contains(list, "pm_") {
				t.Fatalf("a handle was registered past max_depth:\n%s", list)
			}
		})
	}
}

func TestProfileMessageDepthIsTheToolsConfiguredMaxDepth(t *testing.T) {
	messenger := &controlMessenger{}
	tool, err := newProfileMessageTool(profileMessageSettings(), &sessionfs.NativeSubagentSettings{MaxDepth: 4}, profileMessageTargets, messenger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for messenger.started() == 0 {
			time.Sleep(time.Millisecond)
		}
		messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "deep answer"})
	}()
	out, _ := tool.Execute(context.Background(), map[string]any{"profile": "reviewer", "message": "x"}, tools.ExecutionContext{CallID: "c", ChainDepth: 3})
	if !strings.Contains(out, "deep answer") {
		t.Fatalf("max_depth 4 must admit a message from depth 3:\n%s", out)
	}
	out, _ = tool.Execute(context.Background(), map[string]any{"profile": "reviewer", "message": "x"}, tools.ExecutionContext{CallID: "c", ChainDepth: 4})
	if !strings.Contains(out, "profile_depth_exceeded") || messenger.started() != 1 {
		t.Fatalf("max_depth 4 must refuse a message from depth 4 (started %d):\n%s", messenger.started(), out)
	}
	// A config with no subagent section has the default.
	if got := (*sessionfs.NativeSubagentSettings)(nil).RunMaxDepth(); got != 2 {
		t.Fatalf("RunMaxDepth without a section = %d, want the default 2", got)
	}
	if got := (&sessionfs.NativeSubagentSettings{MaxDepth: 5}).RunMaxDepth(); got != 5 {
		t.Fatalf("RunMaxDepth = %d, want the section's 5", got)
	}
}

// Every refusal of Prepare keeps its code when the depth is also past max_depth.
func TestProfileMessagePrepareRefusalsComeBeforeTheDepth(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{ErrProfileCycle, "profile_cycle"},
		{ErrProfileChainTooLong, "profile_chain_too_long"},
		{ErrProfileSameTranscript, "profile_same_transcript"},
	} {
		f := newProfileMessageHandleFixture(t, nil, nil)
		f.messenger.prepareErr = tc.err
		out := f.send(t, tools.ExecutionContext{SessionKey: "s1:main", ChainDepth: 5}, 0)
		if !strings.Contains(out, tc.code) || strings.Contains(out, "profile_depth_exceeded") {
			t.Fatalf("%v: want %s:\n%s", tc.err, tc.code, out)
		}
	}
	// An unknown profile is refused before Prepare, as before.
	f := newProfileMessageHandleFixture(t, nil, nil)
	out, _ := f.message.Execute(context.Background(), map[string]any{"profile": "nobody", "message": "x"}, tools.ExecutionContext{CallID: "c", SessionKey: "s1:main", ChainDepth: 5})
	if !strings.Contains(out, "unknown_profile") || f.messenger.prepared != 0 {
		t.Fatalf("unknown profile:\n%s", out)
	}
}

// A remote target is checked on the same local depth before anything is sent; the remote run
// starts at depth 0 on its own server, as before.
func TestRemoteProfileMessagePastMaxDepthIsRefusedBeforeItIsSent(t *testing.T) {
	f := newRemoteMessageFixture(t, peerGrant, nil)
	execution := remoteCaller
	execution.ChainDepth = 2
	out := f.send(t, remoteTarget, 5000, execution)
	if !strings.Contains(out, "profile_depth_exceeded") {
		t.Fatalf("depth:\n%s", out)
	}
	if len(f.peer.backend.messages) != 0 {
		t.Fatal("a message past max_depth reached the peer")
	}
	// A static refusal keeps its precedence.
	if out := f.send(t, "remote:nobody/main/review/reviewer", 5000, execution); !strings.Contains(out, "unknown_remote") {
		t.Fatalf("unknown remote:\n%s", out)
	}
	execution.ChainDepth = 1
	go func() { f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "ok"}) }()
	if out := f.send(t, remoteTarget, 5000, execution); !strings.Contains(out, "ok") || strings.Contains(out, "profile_depth_exceeded") {
		t.Fatalf("depth 1:\n%s", out)
	}
	if f.peer.backend.messages[0].Hops != 1 {
		t.Fatalf("hops = %d, want 1", f.peer.backend.messages[0].Hops)
	}
}
