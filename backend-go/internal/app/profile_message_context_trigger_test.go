package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

func contextTrigger(action string, after int64) sessionfs.ProfileMessageContextTrigger {
	return sessionfs.ProfileMessageContextTrigger{Action: action, After: after}
}

// triggerLogs routes the resolver's log into a buffer at debug level.
func triggerLogs(f *layoutFixture) *lockedBuffer {
	var logs lockedBuffer
	f.service.resolver.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &logs
}

// countResets replaces the reset a profile_message does with a recorder.
func countResets(f *layoutFixture) *[]string {
	var mu sync.Mutex
	calls := &[]string{}
	f.service.resolver.resetProfile = func(_ context.Context, _, profile string) error {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, profile)
		return nil
	}
	return calls
}

// failOnUsageRead makes any read of the target's tokens fail the test: a zero threshold must
// not count tokens.
func failOnUsageRead(t *testing.T, f *layoutFixture) {
	t.Helper()
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) {
		t.Error("the target's tokens were read although the threshold is zero")
		return nil, errors.New("must not be called")
	}
}

func trySendWithTrigger(t *testing.T, f *layoutFixture, run uint64, text string, trigger sessionfs.ProfileMessageContextTrigger) (string, error) {
	t.Helper()
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: text, ContextTrigger: trigger}
	return (profileMessenger{resolver: f.service.resolver}).send(callerRunContext(t, f, run), request)
}

func sendWithTrigger(t *testing.T, f *layoutFixture, run uint64, text string, trigger sessionfs.ProfileMessageContextTrigger) string {
	t.Helper()
	answer, err := trySendWithTrigger(t, f, run, text, trigger)
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return answer
}

func targetConfig(t *testing.T, f *layoutFixture) sessionfs.SessionConfig {
	t.Helper()
	config, err := (profileMessenger{resolver: f.service.resolver}).profileConfig("local:demo", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func ctxCompletedLine(id string, inputTokens int) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}}`+"\n", id, inputTokens, inputTokens+1)
}

const userLine = `{"type":"message","role":"user","content":[{"type":"input_text","text":"old question"}]}` + "\n"

// writeUsageCacheFor publishes a usage-cache.json that validates against the target's current
// messages.jsonl and carries the given latest-response input tokens.
func writeUsageCacheFor(t *testing.T, f *layoutFixture, inputTokens int64) {
	t.Helper()
	config := targetConfig(t, f)
	shared, release, err := f.service.resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		t.Fatal(err)
	}
	summary := resetLatestResponseUsage(index.usage)
	summary.Current.InputTokens = &inputTokens
	if !shared.writeUsageCache(config, summary, index.size, index.lastResponseKey) {
		t.Fatal("usage cache was not written")
	}
	cache, valid := readUsageCacheState(config)
	if !valid || cache.Summary.Current == nil || *cache.Summary.Current.InputTokens != inputTokens {
		t.Fatalf("precondition: usage cache valid=%v summary=%+v", valid, cache.Summary)
	}
}

// The user's report: a target compacted earlier (or any target whose usage-cache.json says the
// latest response had 0 tokens) was never compacted again at the default threshold, and the
// cache could equally claim more tokens than the transcript has. The transcript decides.
func TestContextTriggerReadsTheTranscriptNotTheUsageCache(t *testing.T) {
	for name, tc := range map[string]struct {
		cacheTokens int64
		after       int64
		want        bool
	}{
		"zero cache default threshold":   {0, 0, true},
		"zero cache positive threshold":  {0, 3, true},
		"zero cache threshold at tokens": {0, 5, false},
		"stale high cache below":         {100, 10, false},
		"stale high cache default":       {100, 0, true},
		"stale low cache above":          {1, 4, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLayoutFixture(t)
			writeTargetTranscript(t, f, profileUsageTranscript(5))
			writeUsageCacheFor(t, f, tc.cacheTokens)
			compactions := countCompactions(f)
			sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextCompact, tc.after))
			if got := len(*compactions) == 1; got != tc.want {
				t.Fatalf("compacted = %v (calls %v), want %v", got, *compactions, tc.want)
			}
		})
	}
}

// Reading the tokens for the trigger neither reads nor rewrites usage-cache.json.
func TestContextTriggerTokenReadLeavesTheUsageCacheAlone(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, profileUsageTranscript(5))
	config := targetConfig(t, f)
	if _, err := os.Stat(config.UsageCachePath); err == nil {
		t.Fatal("precondition: a usage cache exists before any read")
	}
	tokens, err := (profileMessenger{resolver: f.service.resolver}).transcriptTokens(context.Background(), "local:demo", "review/reviewer")
	if err != nil || tokens == nil || *tokens != 5 {
		t.Fatalf("tokens = %v, %v; want 5", tokens, err)
	}
	if _, err := os.Stat(config.UsageCachePath); err == nil {
		t.Fatal("the trigger's token read wrote usage-cache.json")
	}
	writeUsageCacheFor(t, f, 77)
	before, _ := os.ReadFile(config.UsageCachePath)
	if tokens, err := (profileMessenger{resolver: f.service.resolver}).transcriptTokens(context.Background(), "local:demo", "review/reviewer"); err != nil || tokens == nil || *tokens != 5 {
		t.Fatalf("tokens with a cache saying 77 = %v, %v; want 5 from the transcript", tokens, err)
	}
	if after, _ := os.ReadFile(config.UsageCachePath); string(after) != string(before) {
		t.Fatal("the trigger's token read rewrote usage-cache.json")
	}
}

// The latest completed response decides, not the largest or the first; a transcript with no
// completed response, or with a response that carries no usage, has no tokens.
func TestContextTriggerTokensAreThoseOfTheLastCompletedResponse(t *testing.T) {
	noUsage := `{"type":"response.completed","response":{"id":"r-none","status":"completed","output":[]}}` + "\n"
	for name, tc := range map[string]struct {
		body string
		want *int64
	}{
		"last of several":       {userLine + ctxCompletedLine("r1", 50) + userLine + ctxCompletedLine("r2", 3), ptr64(3)},
		"duplicate id":          {userLine + ctxCompletedLine("r1", 9) + ctxCompletedLine("r1", 90), ptr64(9)},
		"no completed":          {userLine, nil},
		"last without usage":    {userLine + ctxCompletedLine("r1", 50) + noUsage, nil},
		"unrelated record last": {userLine + ctxCompletedLine("r1", 8) + userLine, ptr64(8)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLayoutFixture(t)
			writeTargetTranscript(t, f, tc.body)
			got, err := (profileMessenger{resolver: f.service.resolver}).transcriptTokens(context.Background(), "local:demo", "review/reviewer")
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("tokens = %v, want %v", deref(got), deref(tc.want))
			}
		})
	}
}

func ptr64(v int64) *int64 { return &v }

func deref(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// A transcript the token read cannot parse is an error, which the trigger logs and answers by
// not acting (the hook-free path of TestProfileMessageUsageReadErrorSkipsCompactionAndSendsTheMessage).
func TestContextTriggerTokenReadOfACorruptTranscriptFails(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine+"not json\n")
	if tokens, err := (profileMessenger{resolver: f.service.resolver}).transcriptTokens(context.Background(), "local:demo", "review/reviewer"); err == nil {
		t.Fatalf("tokens = %v, want an error for a corrupt transcript", deref(tokens))
	}
}

// With the default threshold a target that has content is acted on without counting tokens:
// a transcript with no completed response at all (no usage) still compacts, and the tokens are
// never read.
func TestContextTriggerZeroThresholdActsOnNonEmptyTranscriptWithoutUsage(t *testing.T) {
	for _, action := range []string{sessionfs.ProfileMessageContextCompact, sessionfs.ProfileMessageContextReset} {
		t.Run(action, func(t *testing.T) {
			f := newLayoutFixture(t)
			writeTargetTranscript(t, f, userLine)
			failOnUsageRead(t, f)
			compactions, resets := countCompactions(f), countResets(f)
			sendWithTrigger(t, f, 1, "hi", contextTrigger(action, 0))
			wantCompactions, wantResets := 0, 0
			if action == sessionfs.ProfileMessageContextCompact {
				wantCompactions = 1
			} else {
				wantResets = 1
			}
			if len(*compactions) != wantCompactions || len(*resets) != wantResets {
				t.Fatalf("compactions %v resets %v", *compactions, *resets)
			}
		})
	}
}

// A positive threshold on a transcript with no completed response has no tokens to compare, so
// nothing happens, as before.
func TestContextTriggerPositiveThresholdWithoutUsageDoesNothing(t *testing.T) {
	for _, action := range []string{sessionfs.ProfileMessageContextCompact, sessionfs.ProfileMessageContextReset} {
		f := newLayoutFixture(t)
		writeTargetTranscript(t, f, userLine)
		compactions, resets := countCompactions(f), countResets(f)
		sendWithTrigger(t, f, 1, "hi", contextTrigger(action, 1))
		if len(*compactions) != 0 || len(*resets) != 0 {
			t.Fatalf("%s: compactions %v resets %v", action, *compactions, *resets)
		}
	}
}

// A missing, empty or whitespace-only messages.jsonl is left alone: no maintenance, no token
// read, the message still goes through, and a compact says why it did nothing.
func TestContextTriggerEmptyMissingOrBlankTranscriptIsLeftAlone(t *testing.T) {
	for name, body := range map[string]*string{"missing": nil, "empty": ptrString(""), "blank": ptrString("\n \r\n\t\n")} {
		for _, after := range []int64{0, 5} {
			for _, action := range []string{sessionfs.ProfileMessageContextCompact, sessionfs.ProfileMessageContextReset} {
				t.Run(fmt.Sprintf("%s/%s/after %d", name, action, after), func(t *testing.T) {
					f := newLayoutFixture(t)
					logs := triggerLogs(f)
					f.captureBodies = true
					if body != nil {
						writeTargetTranscript(t, f, *body)
					}
					failOnUsageRead(t, f)
					compactions, resets := countCompactions(f), countResets(f)
					answer, err := trySendWithTrigger(t, f, 1, "hi", contextTrigger(action, after))
					// The run itself refuses a whitespace-only transcript; the others answer.
					if name != "blank" && (err != nil || !strings.Contains(answer, "ok")) {
						t.Fatalf("the message was not sent: %q, %v", answer, err)
					}
					if len(*compactions) != 0 || len(*resets) != 0 {
						t.Fatalf("maintenance on an empty transcript: compactions %v resets %v", *compactions, *resets)
					}
					warned := strings.Contains(logs.String(), "level=WARN msg=\"messages.jsonl is empty - nothing to compact\"")
					if warned != (action == sessionfs.ProfileMessageContextCompact) {
						t.Fatalf("empty-transcript warning = %v for %s:\n%s", warned, action, logs.String())
					}
					if strings.Contains(logs.String(), "level=WARN") && action == sessionfs.ProfileMessageContextReset {
						t.Fatalf("an empty target must not warn on reset:\n%s", logs.String())
					}
				})
			}
		}
	}
}

func ptrString(s string) *string { return &s }

// off never acts, whatever after is, and the zero trigger is off.
func TestContextTriggerOffNeverActs(t *testing.T) {
	for _, trigger := range []sessionfs.ProfileMessageContextTrigger{{}, contextTrigger(sessionfs.ProfileMessageContextOff, 0), contextTrigger(sessionfs.ProfileMessageContextOff, 1)} {
		f := newLayoutFixture(t)
		writeTargetTranscript(t, f, profileUsageTranscript(10))
		compactions, resets := countCompactions(f), countResets(f)
		sendWithTrigger(t, f, 1, "hi", trigger)
		if len(*compactions) != 0 || len(*resets) != 0 {
			t.Fatalf("%+v: compactions %v resets %v", trigger, *compactions, *resets)
		}
	}
}

// The reset the trigger does is the real one: the history, the compaction cache, the backups
// and the derived caches are gone before the target answers, and the answer it writes is the
// only history left.
func TestContextTriggerResetClearsTheTargetHistoryAndDerivedData(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	writeUsageCacheFor(t, f, 5)
	config := targetConfig(t, f)
	derived := []string{config.StoragePaths.MessagesBackup, config.StoragePaths.CompactionCache, config.StoragePaths.CompactionCache + ".backup", config.StoragePaths.StateContext, config.StoragePaths.CursorCache, config.StoragePaths.RunErrorCache}
	for _, path := range derived {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"old-secret":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := messages + ".deleted-history"
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}

	answer := sendWithTrigger(t, f, 1, "fresh question", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	if !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q", answer)
	}
	f.mu.Lock()
	body := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if strings.Contains(body, "old-secret") || !strings.Contains(body, "fresh question") {
		t.Fatalf("the target's request still carries the cleared history: %s", body)
	}
	transcriptNow, err := os.ReadFile(messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcriptNow), "old-secret") || strings.Count(string(transcriptNow), `"response.completed"`) != 1 || !strings.Contains(string(transcriptNow), "fresh question") {
		t.Fatalf("messages.jsonl after the reset:\n%s", transcriptNow)
	}
	for _, path := range derived {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "old-secret") {
			t.Fatalf("reset left the derived file %s", path)
		}
	}
	if _, err := os.Stat(archive); err == nil {
		t.Fatal("reset left the deleted-history archive")
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected warning:\n%s", logs.String())
	}
}

// Reset obeys the same trigger and claim as compact: once per caller run and target, again in
// the next run, and a message of the same run builds on the one before it.
func TestContextTriggerResetRunsOncePerCallerRunAndTarget(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	lastBody := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.bodies[len(f.bodies)-1]
	}
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	sendWithTrigger(t, f, 1, "first-msg", reset)
	if body := lastBody(); strings.Contains(body, "old question") || !strings.Contains(body, "first-msg") {
		t.Fatalf("first message of the run: %s", body)
	}
	sendWithTrigger(t, f, 1, "second-msg", reset)
	if body := lastBody(); !strings.Contains(body, "first-msg") || !strings.Contains(body, "second-msg") {
		t.Fatalf("a second message of the same run must keep the first: %s", body)
	}
	sendWithTrigger(t, f, 2, "third-msg", reset)
	if body := lastBody(); strings.Contains(body, "first-msg") || strings.Contains(body, "second-msg") || !strings.Contains(body, "third-msg") {
		t.Fatalf("the next caller run must reset again: %s", body)
	}
}

// A target that was empty at a run's first message is not wiped by the run's second message,
// which finds what the first one wrote: the claim is taken before the threshold-free check.
func TestContextTriggerResetDoesNotWipeWhatAnEarlierMessageOfTheRunWrote(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	sendWithTrigger(t, f, 1, "first-msg", reset)
	sendWithTrigger(t, f, 1, "second-msg", reset)
	f.mu.Lock()
	defer f.mu.Unlock()
	if body := f.bodies[len(f.bodies)-1]; !strings.Contains(body, "first-msg") {
		t.Fatalf("the second message wiped the first: %s", body)
	}
}

// With a threshold, a target under it is left alone, and the same run resets it once it has
// grown past it (the claim is only taken when the action runs).
func TestContextTriggerResetPositiveThresholdFollowsTheTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	f.usage = compactUsageJSON
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 5))
	if len(*resets) != 0 {
		t.Fatalf("reset at tokens equal to after: %v", *resets)
	}
	sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 4))
	if len(*resets) != 1 {
		t.Fatalf("no reset above after: %v", *resets)
	}
	sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 4))
	if len(*resets) != 1 {
		t.Fatalf("second reset in the same run: %v", *resets)
	}
}

// ResetProfile refuses at this point (the run is registered and holds the transcript lock), which
// is why the trigger has its own path: it resets while the run is held and leaves the gate and
// the lock as it found them.
func TestContextTriggerResetWorksUnderTheHeldRunWhereResetProfileRefuses(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	writeTargetTranscript(t, f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	resolver := f.service.resolver
	var public error
	var inHook sync.WaitGroup
	inHook.Add(1)
	resolver.resetProfile = func(ctx context.Context, reference, profile string) error {
		defer inHook.Done()
		done := make(chan error, 1)
		go func() { done <- resolver.ResetProfile(ctx, reference, profile) }()
		select {
		case public = <-done:
		case <-time.After(5 * time.Second):
			return errors.New("ResetProfile waited under the held run")
		}
		return resolver.resetTarget(ctx, reference, profile)
	}
	logs := triggerLogs(f)
	sendWithTrigger(t, f, 1, "fresh", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	inHook.Wait()
	if public != ErrRunActive {
		t.Fatalf("ResetProfile under the held run = %v, want ErrRunActive", public)
	}
	f.mu.Lock()
	body := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if strings.Contains(body, "old-secret") {
		t.Fatalf("history survived the reset: %s", body)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("warnings:\n%s", logs.String())
	}
	config := targetConfig(t, f)
	if resolver.gate.Held(sessionKey("local:demo", "review/reviewer")) || resolver.gate.Held(sessionKey(mustCanonical(t, f), "review/reviewer")) {
		t.Fatal("the mutation gate stayed held")
	}
	release, err := resolver.TryLockSession(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatalf("the transcript lock stayed held after the run: %v", err)
	}
	release()
	_ = config
}

func mustCanonical(t *testing.T, f *layoutFixture) string {
	t.Helper()
	reference, err := f.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

// The reset stops what the target started (its async handles) and leaves everyone else's
// handles, including the caller's own, going.
func TestContextTriggerResetStopsTheTargetsChildrenOnly(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}},"async":{}},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/exec"}`)
	f.captureBodies = true
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	target := boundRunnerOf(t, f, "review/reviewer")
	manager := f.service.resolver.sessionAsyncManager(target.config)
	if manager == nil {
		// The first run builds the session's async manager.
		sendWithTrigger(t, f, 9, "seed", contextTrigger(sessionfs.ProfileMessageContextOff, 0))
		manager = f.service.resolver.sessionAsyncManager(target.config)
	}
	if manager == nil {
		t.Fatal("the target has no async manager")
	}
	driver := &recordingDriver{}
	if err := manager.AddDriver(driver); err != nil {
		t.Fatal(err)
	}
	owner := sessionKey(target.reference, target.config.Profile)
	for id, ownerKey := range map[string]string{"target-child": owner, "other-profile": owner + "x", "caller-handle": profileMessageOwner} {
		if err := manager.Register(asyncmgr.Registration{ID: id, Owner: ownerKey, Kind: driver.Kind()}); err != nil {
			t.Fatal(err)
		}
	}
	sendWithTrigger(t, f, 1, "fresh", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	if got := driver.stopped(); strings.Join(got, ",") != "target-child:"+reasonSessionReset {
		t.Fatalf("stopped = %v, want only the target's own child with the reset reason", got)
	}
}

// A reset that is due and fails fails the message: nothing is sent, so the target cannot answer
// from the history the reset was meant to drop. The history is untouched, the failure is
// logged with its detail (the tool's text carries none), and the claim is given back: the
// caller's next message of the same run tries again, succeeds once the cause is gone, and
// then, as ever, the run's later messages do not reset again.
func TestContextTriggerResetFailureFailsTheMessageAndTheNextOneRetries(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	f.captureBodies = true
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	var attempts int
	failing := true
	f.service.resolver.resetProfile = func(ctx context.Context, reference, profile string) error {
		attempts++
		if failing {
			return errors.New("disk on fire")
		}
		return f.service.resolver.resetTarget(ctx, reference, profile)
	}
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	_, err := trySendWithTrigger(t, f, 1, "hi", reset)
	var triggerErr *native.ContextTriggerError
	if !errors.As(err, &triggerErr) || triggerErr.Action != sessionfs.ProfileMessageContextReset || strings.Contains(triggerErr.Reason, "disk") {
		t.Fatalf("send error = %v, want a ContextTriggerError for reset with a client-safe reason", err)
	}
	f.mu.Lock()
	sent := len(f.bodies)
	f.mu.Unlock()
	if sent != 0 {
		t.Fatalf("the failed reset still sent the message to the provider (%d requests)", sent)
	}
	if !strings.Contains(logs.String(), "profile_message reset failed; the reset is not done and the message is not sent") || !strings.Contains(logs.String(), "disk on fire") {
		t.Fatalf("failure not logged:\n%s", logs.String())
	}
	failing = false
	if answer := sendWithTrigger(t, f, 1, "retry-msg", reset); !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q", answer)
	}
	sendWithTrigger(t, f, 1, "later-msg", reset)
	if attempts != 2 {
		t.Fatalf("reset attempts in one run = %d, want 2 (the failure, then the retry only)", attempts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 2 || strings.Contains(f.bodies[0], "old question") || !strings.Contains(f.bodies[0], "retry-msg") || !strings.Contains(f.bodies[1], "retry-msg") {
		t.Fatalf("provider bodies after the retry: %v", f.bodies)
	}
}

// A compaction that fails keeps the old behaviour: logged, and the message is sent anyway.
func TestContextTriggerCompactFailureStillSendsTheMessage(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	f.service.resolver.compactProfile = func(context.Context, string, string) error { return errors.New("compactor down") }
	if answer := sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextCompact, 0)); !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q", answer)
	}
	if !strings.Contains(logs.String(), "profile_message compact failed; sending the message anyway") {
		t.Fatalf("failure not logged:\n%s", logs.String())
	}
}

// The real reset failing (not a hook): the compaction cache cannot be removed (a directory with
// content stands in its place), which Reset does before touching the transcript, so the history
// is intact, the message is not sent, and once the obstacle is gone the same run retries.
func TestContextTriggerRealResetFailureLeavesTheHistoryAndRetries(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	cache := targetConfig(t, f).StoragePaths.CompactionCache
	if err := os.MkdirAll(filepath.Join(cache, "obstacle"), 0o755); err != nil {
		t.Fatal(err)
	}
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	var triggerErr *native.ContextTriggerError
	if _, err := trySendWithTrigger(t, f, 1, "hi", reset); !errors.As(err, &triggerErr) {
		t.Fatalf("send error = %v, want a ContextTriggerError", err)
	}
	if data, _ := os.ReadFile(messages); !strings.Contains(string(data), "old question") {
		t.Fatalf("a failed reset changed the history: %q", data)
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	sendWithTrigger(t, f, 1, "retry-msg", reset)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 || strings.Contains(f.bodies[0], "old question") {
		t.Fatalf("the retry did not reset: %v", f.bodies)
	}
}

// A reset that fails midway no longer leaves an empty history with leftover caches: the empty
// transcript is published last (TurnStore.Reset), so a failure while removing the usage cache
// leaves the history in place, nothing is sent, the target's children are untouched (their
// history is still there), and the run's next message retries and, the cause gone, completes the
// whole reset: history, derived files and children. The strict rule is unaffected: a target
// whose transcript is genuinely empty is still left alone (TestContextTriggerEmptyMissingOrBlankTranscriptIsLeftAlone).
func TestContextTriggerFailedResetIsRetriedWithTheHistoryIntactAndFinishesTheCleanup(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}},"async":{}},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/exec"}`)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	config := targetConfig(t, f)
	stale := []string{config.StoragePaths.StateContext, config.StoragePaths.CursorCache, config.StoragePaths.RunErrorCache}
	for _, path := range stale {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"old-secret":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	usage := config.StoragePaths.UsageCache
	if err := os.MkdirAll(filepath.Join(usage, "obstacle"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := boundRunnerOf(t, f, "review/reviewer")
	sendWithTrigger(t, f, 9, "seed", contextTrigger(sessionfs.ProfileMessageContextOff, 0))
	manager := f.service.resolver.sessionAsyncManager(target.config)
	if manager == nil {
		t.Fatal("the target has no async manager")
	}
	driver := &recordingDriver{}
	if err := manager.AddDriver(driver); err != nil {
		t.Fatal(err)
	}
	owner := sessionKey(target.reference, target.config.Profile)
	if err := manager.Register(asyncmgr.Registration{ID: "target-child", Owner: owner, Kind: driver.Kind()}); err != nil {
		t.Fatal(err)
	}
	// The seed message appended to the history; keep what it left to see it cleared.
	before, _ := os.ReadFile(messages)
	f.mu.Lock()
	f.bodies = nil
	f.mu.Unlock()

	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	var triggerErr *native.ContextTriggerError
	if _, err := trySendWithTrigger(t, f, 1, "hi", reset); !errors.As(err, &triggerErr) {
		t.Fatalf("send error = %v, want a ContextTriggerError", err)
	}
	if data, _ := os.ReadFile(messages); string(data) != string(before) || !strings.Contains(string(data), "old-secret") {
		t.Fatalf("the failed reset changed the history:\n%s", data)
	}
	if got := driver.stopped(); len(got) != 0 {
		t.Fatalf("children stopped by a failed reset: %v", got)
	}
	f.mu.Lock()
	sent := len(f.bodies)
	f.mu.Unlock()
	if sent != 0 {
		t.Fatalf("the failed reset sent the message (%d requests)", sent)
	}

	if err := os.RemoveAll(usage); err != nil {
		t.Fatal(err)
	}
	if answer := sendWithTrigger(t, f, 1, "retry-msg", reset); !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q", answer)
	}
	f.mu.Lock()
	body := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if strings.Contains(body, "old-secret") || !strings.Contains(body, "retry-msg") {
		t.Fatalf("the retry did not clear the history: %s", body)
	}
	for _, path := range stale {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "old-secret") {
			t.Fatalf("the retry left the derived file %s", path)
		}
	}
	if got := driver.stopped(); strings.Join(got, ",") != "target-child:"+reasonSessionReset {
		t.Fatalf("stopped = %v, want the target's child with the reset reason once the reset completed", got)
	}
}

// A call with no running caller run (no id: a genuinely missing one) cannot tell a first message from a later one, so a due reset fails closed instead of
// resetting on every message; the history stays and nothing is sent.
func TestContextTriggerResetWithoutACallerRunFailsClosed(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	ctx := native.WithChildCall(context.Background())
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: contextTrigger(sessionfs.ProfileMessageContextReset, 0)}
	var triggerErr *native.ContextTriggerError
	for i := 0; i < 2; i++ {
		if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); !errors.As(err, &triggerErr) || !strings.Contains(triggerErr.Reason, "no running caller run") {
			t.Fatalf("send %d error = %v, want the no-caller-run refusal", i, err)
		}
	}
	if len(*resets) != 0 {
		t.Fatalf("resets = %v, want none", *resets)
	}
	if data, _ := os.ReadFile(messages); !strings.Contains(string(data), "old question") {
		t.Fatalf("history changed: %q", data)
	}
	f.mu.Lock()
	sent := len(f.bodies)
	f.mu.Unlock()
	if sent != 0 || !strings.Contains(logs.String(), "no running caller run") {
		t.Fatalf("provider requests %d, log:\n%s", sent, logs.String())
	}
	// Nothing due, nothing refused: an empty target, a positive threshold not passed, and a
	// compact trigger (which only costs a repeat) all go through without a caller run.
	request.Message = "again"
	request.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 1_000_000)
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) { return ptr64(5), nil }
	if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); err != nil {
		t.Fatalf("reset under a threshold that is not passed: %v", err)
	}
	f.service.resolver.profileUsage = nil
	compactions := countCompactions(f)
	request.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextCompact, 0)
	if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); err != nil || len(*compactions) != 1 {
		t.Fatalf("compact without a caller run: err %v, compactions %v", err, *compactions)
	}
}

// A reset that cannot read the target's transcript, or count its tokens for a threshold, fails
// closed too; a compaction in the same position only logs and sends.
func TestContextTriggerResetUnreadableStateFailsClosed(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) { return nil, errors.New("count failed") }
	var triggerErr *native.ContextTriggerError
	if _, err := trySendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 10)); !errors.As(err, &triggerErr) {
		t.Fatalf("token read failure: error = %v, want a ContextTriggerError", err)
	}
	f.service.resolver.profileUsage = nil
	// A transcript path that is a directory cannot be read.
	if err := os.Remove(messages); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(messages, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := trySendWithTrigger(t, f, 2, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 0)); !errors.As(err, &triggerErr) {
		t.Fatalf("unreadable transcript: error = %v, want a ContextTriggerError", err)
	}
	if len(*resets) != 0 {
		t.Fatalf("resets = %v, want none", *resets)
	}
}

// resetTarget itself reports a failure to its caller and leaves the history: a cancelled
// context stops it before any change, and an unknown profile is an error.
func TestResetTargetFailureLeavesTheHistory(t *testing.T) {
	f := newLayoutFixture(t)
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.service.resolver.resetTarget(cancelled, "local:demo", "review/reviewer"); err == nil {
		t.Fatal("reset with a cancelled context succeeded")
	}
	if err := f.service.resolver.resetTarget(context.Background(), "local:demo", "review/missing"); err == nil {
		t.Fatal("reset of an unknown profile succeeded")
	}
	if data, err := os.ReadFile(messages); err != nil || !strings.Contains(string(data), "old question") {
		t.Fatalf("history after failed resets = %q, %v", data, err)
	}
	if err := f.service.resolver.resetTarget(context.Background(), "local:demo", "review/reviewer"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(messages); err != nil || len(data) != 0 {
		t.Fatalf("history after a real reset = %q, %v", data, err)
	}
}

// Messages queued behind a busy target are the caller's input: a reset before the first of them
// must neither drop nor end the others. Both are answered, and only the first message of the
// caller's run resets, so the second still sees the first.
func TestContextTriggerResetKeepsQueuedMessages(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	})
	p.f.captureBodies = true
	messages := writeTargetTranscript(t, p.f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	release := p.holdTranscript(t)
	queuedSend := func(text string) string {
		t.Helper()
		out := p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, text)
		if !strings.Contains(out, "state: queued") {
			t.Fatalf("a message to a busy target must queue:\n%s", out)
		}
		return p.handle(t, out)
	}
	first, second := queuedSend("alpha-1 please"), queuedSend("alpha-2 please")
	data, _ := os.ReadFile(messages)
	if !strings.Contains(string(data), "old-secret") {
		t.Fatal("the reset ran while the target was still held by another run")
	}
	release()
	for id, want := range map[string]string{first: "answer for alpha-1", second: "answer for alpha-2"} {
		if got := p.wait(t, id); !strings.Contains(got, want) {
			t.Fatalf("handle %s: want %q:\n%s", id, want, got)
		}
	}
	data, err := os.ReadFile(messages)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "old-secret") || !strings.Contains(text, "alpha-1") || !strings.Contains(text, "alpha-2") {
		t.Fatalf("messages.jsonl after both queued messages:\n%s", text)
	}
	if served := p.order.served(); strings.Join(served, ",") != "alpha-1,alpha-2" {
		t.Fatalf("served %v", served)
	}
}

// An unqueued async call (a handle, no wait) resets on its own start too.
func TestContextTriggerResetOnAnAsyncHandleStart(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	})
	messages := writeTargetTranscript(t, p.f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	handle := p.handle(t, p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, "alpha-1 please"))
	if got := p.wait(t, handle); !strings.Contains(got, "answer for alpha-1") {
		t.Fatalf("answer:\n%s", got)
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "old-secret") || !strings.Contains(string(data), "alpha-1") {
		t.Fatalf("messages.jsonl:\n%s", data)
	}
}

// A busy target is still busy: the trigger runs once the message starts, never beside another
// run, so a held target neither resets nor fails the call.
func TestContextTriggerBusyTargetDoesNotResetAndTheCallQueues(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	})
	resets := countResets(p.f)
	writeTargetTranscript(t, p.f, userLine+ctxCompletedLine("old", 5))
	release := p.holdTranscript(t)
	out := p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, "alpha-1 please")
	if !strings.Contains(out, "state: queued") || len(*resets) != 0 {
		t.Fatalf("resets %v, out:\n%s", *resets, out)
	}
	release()
	p.wait(t, p.handle(t, out))
	if len(*resets) != 1 {
		t.Fatalf("resets after the message ran = %v, want 1", *resets)
	}
}

// A mutation of the target in progress (the HTTP reset or compact route holds the mutation
// gate) keeps the message from starting, so the trigger neither resets beside it nor sends: the
// message is busy. Once the mutation is over the same message starts and resets.
func TestContextTriggerWaitsOutAMutationOfTheTarget(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	key := sessionKey(mustCanonical(t, f), "review/reviewer")
	if !f.service.resolver.gate.Begin(key) {
		t.Fatal("the gate was already held")
	}
	_, err := trySendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	var busy *native.ProfileBusyError
	if !errors.As(err, &busy) {
		f.service.resolver.gate.End(key)
		t.Fatalf("send during a mutation: error = %v, want ProfileBusyError", err)
	}
	if len(*resets) != 0 {
		t.Fatalf("the trigger reset during a mutation: %v", *resets)
	}
	if data, _ := os.ReadFile(messages); !strings.Contains(string(data), "old question") {
		t.Fatalf("history changed during a mutation: %q", data)
	}
	f.service.resolver.gate.End(key)
	sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	if len(*resets) != 1 {
		t.Fatalf("resets after the mutation = %v, want 1", *resets)
	}
}

// Explicit resets (the HTTP route's ResetProfile) racing the trigger's resets and the messages
// they precede: every call ends (no deadlock), the only refusals are the documented busy ones,
// and the transcript is never left holding a half-written line. Meaningful under -race.
func TestContextTriggerResetRacesWithExplicitResets(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			err := f.service.resolver.ResetProfile(context.Background(), "local:demo", "review/reviewer")
			if err != nil && err != ErrRunActive && err != ErrGateHeld {
				t.Errorf("explicit reset: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for run := uint64(1); run <= 8; run++ {
		_, err := trySendWithTrigger(t, f, run, fmt.Sprintf("msg-%d", run), reset)
		var busy *native.ProfileBusyError
		if err != nil && !errors.As(err, &busy) {
			t.Errorf("message %d: %v", run, err)
		}
	}
	close(stop)
	wg.Wait()
	data, err := os.ReadFile(targetConfig(t, f).MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" && !json.Valid([]byte(line)) {
			t.Fatalf("a torn line in messages.jsonl: %q", line)
		}
	}
}

// A queued message whose reset fails when its turn comes ends as the context_trigger failure,
// without being sent and without wedging the line: the next queued message gets its turn, retries
// the reset (the same caller run), and is answered.
func TestContextTriggerResetFailureOfAQueuedMessageEndsItAndTheLineGoesOn(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	})
	messages := writeTargetTranscript(t, p.f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	var mu sync.Mutex
	attempts := 0
	p.f.service.resolver.resetProfile = func(ctx context.Context, reference, profile string) error {
		mu.Lock()
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			return errors.New("disk on fire")
		}
		return p.f.service.resolver.resetTarget(ctx, reference, profile)
	}
	release := p.holdTranscript(t)
	queuedSend := func(text string) string {
		t.Helper()
		out := p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, text)
		if !strings.Contains(out, "state: queued") {
			t.Fatalf("a message to a busy target must queue:\n%s", out)
		}
		return p.handle(t, out)
	}
	first, second := queuedSend("alpha-1 please"), queuedSend("alpha-2 please")
	release()
	if got := p.wait(t, first); !strings.Contains(got, "context_trigger_failed") || strings.Contains(got, "disk on fire") {
		t.Fatalf("first handle:\n%s", got)
	}
	if got := p.wait(t, second); !strings.Contains(got, "answer for alpha-2") {
		t.Fatalf("second handle:\n%s", got)
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "old-secret") || strings.Contains(string(data), "alpha-1") || !strings.Contains(string(data), "alpha-2") {
		t.Fatalf("messages.jsonl:\n%s", data)
	}
	if served := p.order.served(); strings.Join(served, ",") != "alpha-2" {
		t.Fatalf("served %v, want only alpha-2", served)
	}
}

// A caller run that has ended (a background sub-agent's call outliving the run it was started
// by) is no running caller run: a due reset fails closed, nothing is remembered for the dead
// run, and a compact acts like it does without an id.
func TestContextTriggerResetOfAnEndedCallerRunFailsClosed(t *testing.T) {
	f := newLayoutFixture(t)
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	ctx := callerRunContext(t, f, 1) // begins run 1001
	f.service.resolver.callerRuns.forget(1001)
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: contextTrigger(sessionfs.ProfileMessageContextReset, 0)}
	var triggerErr *native.ContextTriggerError
	if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); !errors.As(err, &triggerErr) || !strings.Contains(triggerErr.Reason, "no running caller run") {
		t.Fatalf("reset of an ended run: error = %v, want the no-running-caller-run refusal", err)
	}
	if len(*resets) != 0 || len(f.service.resolver.callerRuns.runs) != 0 {
		t.Fatalf("resets %v, remembered runs %v; want none", *resets, f.service.resolver.callerRuns.runs)
	}
	if data, _ := os.ReadFile(messages); !strings.Contains(string(data), "old question") {
		t.Fatalf("history changed: %q", data)
	}
}

// The launcher hands a child the calling run's id by value only: the child's context keeps its
// own cancellation, a child's own id is not replaced, and a caller without one adds nothing.
func TestWithCallerRunCopiesTheIdAndKeepsTheChildsOwnContext(t *testing.T) {
	callerValues := context.WithValue(context.Background(), callerRunKey{}, uint64(7))
	childParent, stopChild := context.WithCancel(context.Background())
	child := withCallerRun(childParent, callerValues)
	if got, _ := child.Value(callerRunKey{}).(uint64); got != 7 {
		t.Fatalf("child's caller run = %d, want 7", got)
	}
	if child.Err() != nil {
		t.Fatal("copying the id cancelled the child")
	}
	stopChild()
	if child.Err() == nil {
		t.Fatal("the child's own cancellation no longer reaches it")
	}
	// The calling run ending does not touch the child: only the value is taken.
	callerCtx, endCaller := context.WithCancel(callerValues)
	live := withCallerRun(context.Background(), callerCtx)
	endCaller()
	if live.Err() != nil {
		t.Fatal("the calling run's cancellation reached the child")
	}
	own := context.WithValue(context.Background(), callerRunKey{}, uint64(3))
	if got, _ := withCallerRun(own, callerValues).Value(callerRunKey{}).(uint64); got != 3 {
		t.Fatalf("an id already on the child's context was replaced by %d", got)
	}
	if withCallerRun(context.Background(), context.Background()).Value(callerRunKey{}) != nil {
		t.Fatal("an id appeared from a caller without one")
	}
}

// publishedSession is a maintenance session whose Reset reports what TurnStore.Reset reports
// when the empty transcript was published but its directory sync failed (or, with plain set, a
// failure that changed nothing).
type publishedSession struct {
	httpapi.SessionMutation
	err error
}

func (s publishedSession) Reset(context.Context) error { return s.err }

// targetChildren registers a live async handle owned by the reviewer, a handle of another owner
// and the caller's own, and returns the driver that records which are stopped.
func targetChildren(t *testing.T, f *layoutFixture) *recordingDriver {
	t.Helper()
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}},"async":{}},"mcp":{}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/exec"}`)
	target := boundRunnerOf(t, f, "review/reviewer")
	manager := f.service.resolver.sessionAsyncManager(target.config)
	if manager == nil {
		sendWithTrigger(t, f, 9, "seed", contextTrigger(sessionfs.ProfileMessageContextOff, 0))
		manager = f.service.resolver.sessionAsyncManager(target.config)
	}
	if manager == nil {
		t.Fatal("the target has no async manager")
	}
	driver := &recordingDriver{}
	if err := manager.AddDriver(driver); err != nil {
		t.Fatal(err)
	}
	owner := sessionKey(target.reference, target.config.Profile)
	for id, ownerKey := range map[string]string{"target-child": owner, "caller-handle": profileMessageOwner} {
		if err := manager.Register(asyncmgr.Registration{ID: id, Owner: ownerKey, Kind: driver.Kind()}); err != nil {
			t.Fatal(err)
		}
	}
	return driver
}

// A reset whose transcript was published but not confirmed durable cleared the history, so the
// target's children are stopped as for a clean reset and the error still reaches the caller; a
// failure that changed nothing stops nobody.
func TestResetResolvedStopsTheChildrenWhenTheResetWasPublishedButNotConfirmed(t *testing.T) {
	f := newLayoutFixture(t)
	driver := targetChildren(t, f)
	resolver := f.service.resolver
	resolved, err := resolver.ResolveMaintenance(context.Background(), "local:demo", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	plain := errors.New("could not remove a cache")
	if err := resolver.resetResolved(context.Background(), httpapi.ResolvedMutation{Reference: resolved.Reference, Profile: resolved.Profile, Session: publishedSession{resolved.Session, plain}}); !errors.Is(err, plain) {
		t.Fatalf("plain failure: error = %v", err)
	}
	if got := driver.stopped(); len(got) != 0 {
		t.Fatalf("a failed reset that changed nothing stopped %v", got)
	}
	unconfirmed := fmt.Errorf("reset applied but not confirmed durable: %w", &transcript.PublishedError{Err: errors.New("sync transcript directory: disk would not sync")})
	err = resolver.resetResolved(context.Background(), httpapi.ResolvedMutation{Reference: resolved.Reference, Profile: resolved.Profile, Session: publishedSession{resolved.Session, unconfirmed}})
	if err == nil || !strings.Contains(err.Error(), "not confirmed durable") {
		t.Fatalf("published failure: error = %v, want it reported", err)
	}
	if got := driver.stopped(); strings.Join(got, ",") != "target-child:"+reasonSessionReset {
		t.Fatalf("stopped = %v, want only the target's child, with the reset reason", got)
	}
}

// The trigger treats an unconfirmed publish as a reset that happened and may need attention:
// the message is not sent, the log says so, and the reason the model sees says it was applied
// but could not be confirmed and may need attention, not that nothing changed.
func TestContextTriggerResetPublishedButNotConfirmedFailsTheMessageAndSaysSo(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	f.captureBodies = true
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	f.service.resolver.resetProfile = func(context.Context, string, string) error {
		return fmt.Errorf("reset applied but not confirmed durable: %w", &transcript.PublishedError{Err: errors.New("disk would not sync")})
	}
	_, err := trySendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 0))
	var triggerErr *native.ContextTriggerError
	if !errors.As(err, &triggerErr) || !strings.Contains(triggerErr.Reason, "applied but could not be confirmed durable") || !strings.Contains(triggerErr.Reason, "may need attention") || strings.Contains(triggerErr.Reason, "disk") {
		t.Fatalf("send error = %v, want the applied-but-unconfirmed reason without the cause", err)
	}
	if !strings.Contains(logs.String(), "reset applied but not confirmed durable") || !strings.Contains(logs.String(), "disk would not sync") {
		t.Fatalf("not logged:\n%s", logs.String())
	}
	f.mu.Lock()
	sent := len(f.bodies)
	f.mu.Unlock()
	if sent != 0 {
		t.Fatalf("the message was sent (%d requests)", sent)
	}
}
