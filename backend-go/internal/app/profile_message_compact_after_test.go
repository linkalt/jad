package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// compactUsageJSON is the usage every completed response of the fixture carries, so the
// target has a last completed response to read tokens from.
const compactUsageJSON = `"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6},`

// compactAfterTarget writes the target's transcript with the given last response input tokens
// (nil writes none, so the target has no completed response) and reports whether a message
// with the given gate settings compacts it.
func compactAfterTarget(t *testing.T, tokens *int, compact bool, compactAfter int64) bool {
	t.Helper()
	f := newLayoutFixture(t)
	if tokens != nil {
		messages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
		if err := os.MkdirAll(filepath.Dir(messages), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(messages, []byte(profileUsageTranscript(*tokens)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := countCompactions(f)
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", Compact: compact, CompactAfter: compactAfter}
	if _, err := (profileMessenger{resolver: f.service.resolver}).send(callerRunContext(t, f, 1), request); err != nil {
		t.Fatal(err)
	}
	return len(*calls) == 1
}

func TestProfileMessageCompactOffNeverCompactsWhateverCompactAfterIs(t *testing.T) {
	tokens := 10
	if compacted := compactAfterTarget(t, &tokens, false, 1); compacted {
		t.Fatal("compact off compacted the target with compact_after 1")
	}
}

func TestProfileMessageCompactOnWithDefaultCompactAfterCompactsOverZero(t *testing.T) {
	tokens := 5
	if !compactAfterTarget(t, &tokens, true, 0) {
		t.Fatal("compact on with compact_after 0 did not compact a target with 5 input tokens")
	}
}

func TestProfileMessageCompactAfterIsStrictlyGreater(t *testing.T) {
	tokens := 10
	if compactAfterTarget(t, &tokens, true, 10) {
		t.Fatal("compacted at tokens equal to compact_after; the threshold is strictly greater")
	}
	if !compactAfterTarget(t, &tokens, true, 9) {
		t.Fatal("did not compact at 10 tokens with compact_after 9")
	}
}

func TestProfileMessageCompactAfterRaisedInTheSameCallerRunStillCompacts(t *testing.T) {
	f := newLayoutFixture(t)
	calls := countCompactions(f)
	messenger := profileMessenger{resolver: f.service.resolver}
	// Seed the target with 5 input tokens in its own caller run.
	f.usage = compactUsageJSON
	if _, err := messenger.Message(callerRunContext(t, f, 2), profileMessageOwner, "review/reviewer", "seed", 0, false); err != nil {
		t.Fatal(err)
	}
	send := func() {
		t.Helper()
		request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", Compact: true, CompactAfter: 10}
		if _, err := messenger.send(callerRunContext(t, f, 1), request); err != nil {
			t.Fatal(err)
		}
	}
	// The first send reads 5 tokens, at or below compact_after; its own response raises them to 50.
	f.usage = `"usage":{"input_tokens":50,"output_tokens":1,"total_tokens":51},`
	send()
	if len(*calls) != 0 {
		t.Fatalf("first send compacted at 5 tokens with compact_after 10: %v", *calls)
	}
	send()
	if len(*calls) != 1 {
		t.Fatalf("second send in the same caller run compactions = %v, want 1", *calls)
	}
}

func TestProfileMessageQueuedCompactAfterRaisedInTheSameCallerRunStillCompacts(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.Compact = sessionfs.ProfileMessageCompactOn
		settings.CompactAfter = 10
	})
	calls := countCompactions(p.f)
	// servedAtCompaction records how many messages the target had served when each compaction ran.
	var servedAtCompaction []int
	compact := p.f.service.resolver.compactProfile
	p.f.service.resolver.compactProfile = func(ctx context.Context, session, profile string) error {
		servedAtCompaction = append(servedAtCompaction, len(p.order.served()))
		return compact(ctx, session, profile)
	}
	// beforeRun reads the usage once per queued message, when the pump starts it. The first
	// message reads 5 tokens, at or below compact_after; by the second the target has grown to 50.
	var reads atomic.Int64
	p.f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) {
		tokens := int64(5)
		if reads.Add(1) > 1 {
			tokens = 50
		}
		return &tokens, nil
	}
	// The held transcript makes the target busy, so both sends of the same caller run queue behind it.
	release := p.holdTranscript(t)
	queuedSend := func(text string) string {
		t.Helper()
		out := p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, text)
		if !strings.Contains(out, "state: queued") {
			t.Fatalf("a message to a busy target must queue:\n%s", out)
		}
		return p.handle(t, out)
	}
	first := queuedSend("alpha-1 please")
	second := queuedSend("alpha-2 please")
	release()
	for id, want := range map[string]string{first: "answer for alpha-1", second: "answer for alpha-2"} {
		if got := p.wait(t, id); !strings.Contains(got, want) {
			t.Fatalf("handle %s: want %q:\n%s", id, want, got)
		}
	}
	if len(*calls) != 1 || (*calls)[0] != "review/reviewer" {
		t.Errorf("compactions %v, want one of review/reviewer for the second queued message", *calls)
	}
	// The compaction must run after the first message is served and before the second is, so exactly one has been served.
	if !reflect.DeepEqual(servedAtCompaction, []int{1}) {
		t.Fatalf("served at compaction %v, want [1]: compaction must run after the first message is served and before the second", servedAtCompaction)
	}
}

func TestProfileMessageCompactOnWithoutUsageDoesNotCompact(t *testing.T) {
	if compactAfterTarget(t, nil, true, 0) {
		t.Fatal("compacted a target with no completed response")
	}
}

func TestProfileMessageMissingTargetTranscriptDoesNotCompactOrFailTheMessage(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	calls := countCompactions(f)
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", Compact: true}
	answer, err := profileMessenger{resolver: f.service.resolver}.send(callerRunContext(t, f, 1), request)
	if err != nil || !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q, error %v; want the message sent", answer, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(*calls) != 0 || len(f.bodies) != 1 {
		t.Fatalf("compactions %v, provider bodies %d", *calls, len(f.bodies))
	}
}

func TestProfileMessageUsageReadErrorSkipsCompactionAndSendsTheMessage(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	var logs lockedBuffer
	f.service.resolver.logger = slog.New(slog.NewTextHandler(&logs, nil))
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) {
		return nil, errors.New("usage unreadable")
	}
	calls := countCompactions(f)
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", Compact: true}
	answer, err := profileMessenger{resolver: f.service.resolver}.send(callerRunContext(t, f, 1), request)
	if err != nil || !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q, error %v; want the message sent", answer, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(*calls) != 0 || len(f.bodies) != 1 || !strings.Contains(f.bodies[0], "hi") {
		t.Fatalf("compactions %v, provider bodies %d", *calls, len(f.bodies))
	}
	if !strings.Contains(logs.String(), "profile_message usage read failed; not compacting") {
		t.Fatalf("read error not logged: %s", logs.String())
	}
}
