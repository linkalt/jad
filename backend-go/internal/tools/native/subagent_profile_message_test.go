package native

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// callerRunSentinel stands for a value the calling run's context carries (a cost meter or
// hook reporter, say): a sub-agent runs on a context its manager owns, so it must not reach the child.
type callerRunSentinel struct{}

type childMessenger struct {
	child    atomic.Bool
	sawValue atomic.Bool
	maxChain atomic.Int64
	started  chan struct{}
	ended    atomic.Int64
}

func (m *childMessenger) Prepare(ctx context.Context, _ ProfileMessageRequest) (ProfileMessagePlan, error) {
	m.child.Store(IsChildCall(ctx))
	m.sawValue.Store(ctx.Value(callerRunSentinel{}) != nil)
	m.maxChain.Store(int64(ProfileChainMax(ctx)))
	return &fakePlan{reply: m.reply, request: ProfileMessageRequest{}}, nil
}

func (m *childMessenger) reply(ctx context.Context) (string, error) {
	if m.started != nil {
		close(m.started)
		<-ctx.Done()
		m.ended.Add(1)
		return "", ctx.Err()
	}
	return "reviewed", nil
}

func profileMessageChild(t *testing.T, settings sessionfs.NativeSubagentSettings, messenger ProfileMessenger, run nestedBehaviour) *tools.Catalog {
	t.Helper()
	pm, err := NewProfileMessageTool(sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024, MaxChain: 3}, profileMessageTargets, messenger, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := append(withExecutors(t, readOnlyTool("read")), pm)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) { return run(ctx, request) }}
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestAChildListingProfileMessageGetsItAndOthersDoNot(t *testing.T) {
	messenger := &childMessenger{}
	var withTool, without []string
	catalog := profileMessageChild(t, subagentSettings(nestingAgent("asker", "read", "profile_message"), nestingAgent("plain", "read")), messenger,
		func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			names := toolNames(request.Tools)
			if request.Agent.Name == "asker" {
				withTool = names
				childCall(t, ctx, request, "profile_message", `{"profile":"reviewer","message":"hi"}`)
			} else {
				without = names
			}
			return SubagentResult{Text: "done"}, nil
		})
	subagentCall(t, catalog, `{"task":"x","agent":"asker"}`)
	subagentCall(t, catalog, `{"task":"x","agent":"plain"}`)
	if !strings.Contains(strings.Join(withTool, ","), "profile_message") {
		t.Errorf("an agent that lists profile_message did not get it: %v", withTool)
	}
	if strings.Contains(strings.Join(without, ","), "profile_message") {
		t.Errorf("an agent without it in its list got profile_message: %v", without)
	}
	if !messenger.child.Load() || messenger.maxChain.Load() != 3 {
		t.Errorf("the messenger saw child=%v max_chain=%d, want child=true max_chain=3", messenger.child.Load(), messenger.maxChain.Load())
	}
}

// profile_message is in a child's set by default, like a profile's own; a list that names only the old
// opt-in names restricts nothing.
func TestProfileMessageIsInAChildByDefault(t *testing.T) {
	settings := subagentSettings(nestingAgent("asker", "profile_message"))
	var got []string
	catalog := profileMessageChild(t, settings, &childMessenger{}, func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		got = toolNames(request.Tools)
		return SubagentResult{Text: "done"}, nil
	})
	if out := subagentCall(t, catalog, `{"task":"x"}`); !strings.Contains(out, "finished") {
		t.Fatalf("agent with only profile_message: %s", out)
	}
	if strings.Join(got, ",") != "read,profile_message" {
		t.Errorf("tools = %v, want read,profile_message", got)
	}
}

func TestListingProfileMessageWithoutTheSessionToolIsALoadError(t *testing.T) {
	parent := withExecutors(t, readOnlyTool("read"))
	_, err := newSubagentToolAsync(subagentSettings(nestingAgent("asker", "read", "profile_message")), parent, nil, &fakeLauncher{}, nil, NewSubagentManager(), nil)
	if err == nil || !strings.Contains(err.Error(), "profile_message") || !strings.Contains(err.Error(), "does not enable tools.native.profile_message") {
		t.Fatalf("error = %v, want one that names profile_message and says the session does not enable it", err)
	}
}

func TestStoppingAChildCancelsItsProfileMessageAndLeavesNoGoroutines(t *testing.T) {
	baseline := baselineCount()
	messenger := &childMessenger{started: make(chan struct{})}
	catalog := profileMessageChild(t, subagentSettings(nestingAgent("asker", "profile_message")), messenger,
		func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			childCall(t, ctx, request, "profile_message", `{"profile":"reviewer","message":"hi"}`)
			return SubagentResult{}, ctx.Err()
		})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- subagentCallWith(t, ctx, catalog, `{"task":"x"}`) }()
	select {
	case <-messenger.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the child never sent its message")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the subagent call did not return after cancel")
	}
	deadline := time.Now().Add(5 * time.Second)
	for messenger.ended.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if messenger.ended.Load() != 1 {
		t.Fatalf("the messenger saw its context end %d times, want 1", messenger.ended.Load())
	}
	requireNoGrowth(t, baseline, "a child that was waiting on profile_message")
}

// A profile_message made from a sub-agent runs on the sub-agent's own context, which carries
// none of the calling run's values. This pins that the child's context does not inherit the
// caller's, so cancellation and ownership stay the sub-agent manager's; the launcher copies only
// the values it names from SubagentRequest.CallerValues (the cost meter and hook reporter).
func TestAChildsProfileMessageCarriesNoCallerRunValues(t *testing.T) {
	messenger := &childMessenger{}
	catalog := profileMessageChild(t, subagentSettings(nestingAgent("asker", "read", "profile_message")), messenger,
		func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			childCall(t, ctx, request, "profile_message", `{"profile":"reviewer","message":"hi"}`)
			return SubagentResult{Text: "done"}, nil
		})
	ctx := context.WithValue(context.Background(), callerRunSentinel{}, uint64(7))
	subagentCallWith(t, ctx, catalog, `{"task":"x","agent":"asker"}`)
	if !messenger.child.Load() {
		t.Fatal("the messenger did not see a child call")
	}
	if messenger.sawValue.Load() {
		t.Fatal("the child's profile_message context carries the calling run's values: the child would then share the caller's cancellation and ownership, and the launcher's explicit copy of the id is redundant")
	}
}
