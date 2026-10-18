package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

const profileMessageOwner = "local:demo\x00dev/coder"

func TestProfileMessageDeliversToTheTargetTranscriptAndSecondCallSeesTheFirst(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messenger := profileMessenger{resolver: f.service.resolver}

	answer, err := messenger.Message(context.Background(), profileMessageOwner, "review/reviewer", "first question")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "ok") {
		t.Fatalf("answer = %q, want the provider's reply", answer)
	}
	reviewerMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
	data, err := os.ReadFile(reviewerMessages)
	if err != nil || !strings.Contains(string(data), "first question") || !strings.Contains(string(data), "ok") {
		t.Fatalf("transcript of review/reviewer: %v %s", err, data)
	}
	coderMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(coderMessages); err == nil && strings.Contains(string(data), "first question") {
		t.Fatalf("the caller's transcript received the message: %s", data)
	}

	if _, err := messenger.Message(context.Background(), profileMessageOwner, "review/reviewer", "second question"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "first question") || !strings.Contains(last, "second question") {
		t.Fatalf("second request does not carry the first exchange: %s", last)
	}
}

func TestProfileMessageToABusyTargetFailsFast(t *testing.T) {
	f := newLayoutFixture(t)
	release, err := f.service.resolver.LockSession(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
}

func TestProfileMessageRejectsAnOwnerKeyWithoutSeparator(t *testing.T) {
	f := newLayoutFixture(t)
	_, err := profileMessenger{resolver: f.service.resolver}.Message(context.Background(), "local:demo", "review/reviewer", "hi")
	if err == nil || errors.Is(err, native.ErrProfileCycle) || errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want a plain error", err)
	}
}

func TestRecordProfileRunFailureRecordsUnlessCancelled(t *testing.T) {
	for _, c := range []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		stop       func(cancel context.CancelFunc)
		recorded   bool
	}{
		{"timeout", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, func(context.CancelFunc) {}, true},
		{"cancel", func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, func(cancel context.CancelFunc) { cancel() }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLayoutFixture(t)
			f.mu.Lock()
			f.hold, f.reached = true, make(chan struct{}, 4)
			f.mu.Unlock()
			ctx, cancel := c.newContext()
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := profileMessenger{resolver: f.service.resolver}.Message(ctx, profileMessageOwner, "review/reviewer", "hi")
				done <- err
			}()
			select {
			case <-f.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("the run never reached the provider")
			}
			c.stop(cancel)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("run succeeded, want an error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end")
			}
			cache, _ := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "staging", "run-error-cache.json"))
			if got := strings.Contains(string(cache), profileRunFailureText); got != c.recorded {
				t.Fatalf("run error recorded = %v, want %v; cache: %s", got, c.recorded, cache)
			}
		})
	}
}

// scriptFunctionCall answers the first request of a run with a function call and every
// later one (a request that already carries a function_call_output) with the plain reply.
func scriptFunctionCall(name string, arguments map[string]any) func(model, body string) string {
	encoded, _ := json.Marshal(arguments)
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call-1", "name": name, "arguments": string(encoded)})
	return func(_, body string) string {
		if strings.Contains(body, "function_call_output") {
			return ""
		}
		return string(item)
	}
}

func TestProfileMessageRunCannotCallProfileMessageItself(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{"profiles":["dev/coder"]}},"mcp":{}}`)
	f.captureBodies = true
	f.script = scriptFunctionCall("profile_message", map[string]any{"profile": "dev/coder", "message": "again"})

	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(context.Background(), profileMessageOwner, "review/reviewer", "hi"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "function_call_output") || !strings.Contains(last, "profile_cycle") {
		t.Fatalf("the nested profile_message call did not report profile_cycle: %s", last)
	}
	reviewerMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
	if data, err := os.ReadFile(reviewerMessages); err != nil || !strings.Contains(string(data), "profile_cycle") {
		t.Fatalf("transcript of review/reviewer: %v %s", err, data)
	}
	coderMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(coderMessages); err == nil && strings.Contains(string(data), "again") {
		t.Fatalf("the nested call reached the caller's transcript: %s", data)
	}
}

func TestProfileMessageTargetToolWithApprovalAskRunsUnderAutoApprove(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"write":{"approval":"ask"}},"mcp":{}}`)
	f.captureBodies = true
	f.script = scriptFunctionCall("write", map[string]any{"path": "made.txt", "content": "by the target\n"})

	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(context.Background(), profileMessageOwner, "review/reviewer", "write it"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(f.cwd, "made.txt")); err != nil || string(data) != "by the target\n" {
		t.Fatalf("made.txt: %v %q, want the file written without a user approval", err, data)
	}
}

func boundRunnerOf(t *testing.T, f *layoutFixture, profile string) boundRunner {
	t.Helper()
	resolved, err := f.service.resolver.Resolve(context.Background(), "local:demo", profile, "")
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := resolved.Runner.(boundRunner)
	if !ok {
		t.Fatalf("runner is %T, want boundRunner", resolved.Runner)
	}
	return bound
}

func TestSameTranscriptComparesTheSharedStoreNotTheRunMutex(t *testing.T) {
	f := newLayoutFixture(t)
	coder := boundRunnerOf(t, f, "dev/coder")
	alias := boundRunnerOf(t, f, "dev/coder")
	reviewer := boundRunnerOf(t, f, "review/reviewer")

	if !sameTranscript(coder, alias) {
		t.Fatal("two runners over one transcript store were not reported as the same transcript")
	}
	reviewer.shared.runMu.Lock()
	defer reviewer.shared.runMu.Unlock()
	if sameTranscript(coder, reviewer) {
		t.Fatal("a profile with its own transcript, whose run mutex is held, was reported as the same transcript")
	}
}

func TestProfileMessageToARunningTargetReportsBusyNotSameTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	bound := boundRunnerOf(t, f, "review/reviewer")
	bound.shared.runMu.Lock()
	defer bound.shared.runMu.Unlock()
	release, err := f.service.resolver.LockSession(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
}

func TestProfileMessageWithoutAnswerTextReturnsErrProfileNoAnswer(t *testing.T) {
	f := newLayoutFixture(t)
	f.script = func(string, string) string {
		return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"  "}]}`
	}

	_, err := profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileNoAnswer) {
		t.Fatalf("error = %v, want ErrProfileNoAnswer", err)
	}
}

func TestBoundRunnerRunReturnsTheContextErrorWhileRunMutexIsHeld(t *testing.T) {
	f := newLayoutFixture(t)
	bound := boundRunnerOf(t, f, "review/reviewer")
	bound.shared.runMu.Lock()
	defer bound.shared.runMu.Unlock()
	countRecords := func() int {
		count := 0
		if err := bound.shared.store.ReadRecords(func(int64, []byte) error { count++; return nil }); err != nil {
			t.Fatal(err)
		}
		return count
	}
	recordsBefore := countRecords()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := bound.Run(ctx, json.RawMessage(`"hi"`), session.RunOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept waiting for the run mutex after its context was cancelled")
	}
	if got := countRecords(); got != recordsBefore {
		t.Fatalf("transcript has %d records, want %d: a run that never got the lock stored input", got, recordsBefore)
	}
}

// lateCancelContext reports no error on its first Err call and a closed Done channel from the start,
// like a context cancelled after LockCtx's own check but before its select.
type lateCancelContext struct {
	context.Context
	calls atomic.Int32
}

func (c *lateCancelContext) Done() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (c *lateCancelContext) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestBoundRunnerRunStoresNoInputWhenTheContextEndsAsTheFreeLockIsTaken(t *testing.T) {
	f := newLayoutFixture(t)
	bound := boundRunnerOf(t, f, "review/reviewer")
	countRecords := func() int {
		count := 0
		if err := bound.shared.store.ReadRecords(func(int64, []byte) error { count++; return nil }); err != nil {
			t.Fatal(err)
		}
		return count
	}
	recordsBefore := countRecords()
	for i := 0; i < 50; i++ {
		_, err := bound.Run(&lateCancelContext{Context: context.Background()}, json.RawMessage(`"hi"`), session.RunOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: error = %v, want context.Canceled", i, err)
		}
	}
	if got := countRecords(); got != recordsBefore {
		t.Fatalf("transcript has %d records, want %d: a cancelled run stored input", got, recordsBefore)
	}
	if !bound.shared.runMu.TryLock() {
		t.Fatal("a cancelled run kept the run mutex")
	}
	bound.shared.runMu.Unlock()
}

func TestProfileMessageToADifferentProfileOverOneTranscriptReportsSameTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	coder := boundRunnerOf(t, f, "dev/coder")
	backdate(t, f.root)
	location, err := sessionfs.Resolve(f.service.resolver.roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	config, stamp, err := sessionfs.LoadSessionSettingsStamped(location, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	config.StoragePaths, config.MessagesPath, config.StagingDir = coder.config.StoragePaths, coder.config.MessagesPath, coder.config.StagingDir
	f.service.resolver.lockedConfigs.put(
		lockedConfigKey{file: location.File, profile: "review/reviewer"},
		lockedConfig{config: config, stamp: stamp, loaded: time.Now()},
	)

	_, err = profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileSameTranscript) {
		t.Fatalf("error = %v, want ErrProfileSameTranscript", err)
	}
}

func TestProfileMessageRefusesNestedCallsAndTheCallerItself(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}

	nested := context.WithValue(context.Background(), profileChainKey{}, []string{"dev/coder", "review/reviewer"})
	if _, err := messenger.Message(nested, profileMessageOwner, "dev/coder", "hi"); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("nested call error = %v, want ErrProfileCycle", err)
	}
	if _, err := messenger.Message(context.Background(), profileMessageOwner, "dev/coder", "hi"); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("self call error = %v, want ErrProfileCycle", err)
	}
}
