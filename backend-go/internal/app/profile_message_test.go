package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

const profileMessageOwner = "local:demo\x00dev/coder"

func callerContext(t *testing.T, f *layoutFixture, parent context.Context) context.Context {
	t.Helper()
	return context.WithValue(parent, callerMessagesPathKey{}, boundRunnerOf(t, f, "dev/coder").config.MessagesPath)
}

func TestProfileMessageDeliversToTheTargetTranscriptAndSecondCallSeesTheFirst(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messenger := profileMessenger{resolver: f.service.resolver}

	answer, err := messenger.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "first question")
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

	if _, err := messenger.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "second question"); err != nil {
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

	_, err = profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
}

func TestProfileMessageWithoutTheCallerTranscriptPathFailsClosed(t *testing.T) {
	f := newLayoutFixture(t)
	_, err := profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if err == nil || errors.Is(err, native.ErrProfileCycle) || errors.Is(err, native.ErrProfileBusy) || errors.Is(err, native.ErrProfileSameTranscript) {
		t.Fatalf("error = %v, want a plain error", err)
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
			ctx = callerContext(t, f, ctx)
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

	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi"); err != nil {
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

	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "write it"); err != nil {
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

func TestSameTranscriptComparesTheMessagesPathNotTheRunMutex(t *testing.T) {
	f := newLayoutFixture(t)
	coder := boundRunnerOf(t, f, "dev/coder")
	alias := boundRunnerOf(t, f, "dev/coder")
	reviewer := boundRunnerOf(t, f, "review/reviewer")

	if !sameTranscript(coder.config.MessagesPath, alias) {
		t.Fatal("two runners over one transcript store were not reported as the same transcript")
	}
	reviewer.shared.runMu.Lock()
	defer reviewer.shared.runMu.Unlock()
	if sameTranscript(coder.config.MessagesPath, reviewer) {
		t.Fatal("a profile with its own transcript, whose run mutex is held, was reported as the same transcript")
	}
}

func TestSameTranscriptResolvesSymlinkedDirectoryAliases(t *testing.T) {
	f := newLayoutFixture(t)
	coder := boundRunnerOf(t, f, "dev/coder")
	realDir := filepath.Dir(coder.config.MessagesPath)
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasPath := filepath.Join(alias, filepath.Base(coder.config.MessagesPath))
	if !sameTranscript(aliasPath, coder) {
		t.Fatal("a symlinked directory alias of the messages path was not the same transcript")
	}
	if sameTranscript(aliasPath, boundRunnerOf(t, f, "review/reviewer")) {
		t.Fatal("an alias of another profile's transcript was reported as the same transcript")
	}
}

func TestSameTranscriptResolvesASymlinkedFileAndMissingParents(t *testing.T) {
	f := newLayoutFixture(t)
	coder := boundRunnerOf(t, f, "dev/coder")
	realPath := coder.config.MessagesPath
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	link := filepath.Join(t.TempDir(), "link.jsonl")
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !sameTranscript(link, coder) {
		t.Fatal("a symlink to the messages file was not the same transcript")
	}
	tempDir := t.TempDir()
	resolvedTempDir, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(tempDir, "no", "such", "dir", "messages.jsonl")
	if sameTranscript(missing, coder) {
		t.Fatal("a path under a missing directory was reported as the same transcript")
	}
	if got, want := canonicalPath(missing), filepath.Join(resolvedTempDir, "no", "such", "dir", "messages.jsonl"); got != want && got != filepath.Clean(missing) {
		t.Fatalf("canonicalPath(%q) = %q, want the path under the resolved temp dir", missing, got)
	}
	missingFile := filepath.Join(tempDir, "absent.jsonl")
	if got, want := canonicalPath(missingFile), filepath.Join(resolvedTempDir, "absent.jsonl"); got != want {
		t.Fatalf("canonicalPath(%q) = %q, want %q (resolved parent plus the missing file name)", missingFile, got, want)
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

	_, err = profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
}

func TestProfileMessageWithoutAnswerTextReturnsErrProfileNoAnswer(t *testing.T) {
	f := newLayoutFixture(t)
	f.script = func(string, string) string {
		return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"  "}]}`
	}

	_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileNoAnswer) {
		t.Fatalf("error = %v, want ErrProfileNoAnswer", err)
	}
}

func TestProfileMessageUsesTheLastTextWhenTheFinalResponseHasNone(t *testing.T) {
	f := newLayoutFixture(t)
	f.script = func(_, body string) string {
		if strings.Contains(body, "function_call_output") {
			return `{"type":"reasoning","summary":[]}`
		}
		return `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"earlier words"}]},` +
			`{"type":"function_call","call_id":"call-1","name":"nothing_here","arguments":"{}"}]`
	}

	answer, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "earlier words") {
		t.Fatalf("answer = %q, want the earlier response's text", answer)
	}
}

func TestProfileMessageWithOnlyAToolCallReturnsErrProfileNoAnswer(t *testing.T) {
	f := newLayoutFixture(t)
	f.script = func(_, body string) string {
		if strings.Contains(body, "function_call_output") {
			return `{"type":"reasoning","summary":[]}`
		}
		return `{"type":"function_call","call_id":"call-1","name":"nothing_here","arguments":"{}"}`
	}

	_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
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
	freeCtx, cancelFree := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFree()
	if err := bound.shared.runMu.LockCtx(freeCtx); err != nil {
		t.Fatalf("a cancelled run kept the run mutex: %v", err)
	}
	bound.shared.runMu.Unlock()
}

// shareCoderTranscript makes review/reviewer resolve to the transcript of dev/coder. The
// returned park puts that config in place again, because a parked entry expires.
func shareCoderTranscript(t *testing.T, f *layoutFixture) (boundRunner, func()) {
	t.Helper()
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
	park := func() {
		f.service.resolver.lockedConfigs.put(
			lockedConfigKey{file: location.File, profile: "review/reviewer"},
			lockedConfig{config: config, stamp: stamp, loaded: time.Now()},
		)
	}
	park()
	return coder, park
}

func TestProfileMessageToADifferentProfileOverOneTranscriptReportsSameTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	coder, _ := shareCoderTranscript(t, f)

	callerCtx := context.WithValue(context.Background(), callerMessagesPathKey{}, coder.config.MessagesPath)
	_, err := profileMessenger{resolver: f.service.resolver}.Message(callerCtx, profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileSameTranscript) {
		t.Fatalf("error = %v, want ErrProfileSameTranscript", err)
	}
}

func TestProfileMessageToolCallToAProfileOverTheCallersTranscriptReportsSameTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{"profiles":["review/reviewer"]}},"mcp":{}}`)
	f.captureBodies = true
	coder, park := shareCoderTranscript(t, f)
	call := scriptFunctionCall("profile_message", map[string]any{"profile": "review/reviewer", "message": "hi"})
	f.script = func(model, body string) string {
		park()
		return call(model, body)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := coder.Run(runCtx, json.RawMessage(`"go"`), session.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "function_call_output") || !strings.Contains(last, "profile_same_transcript") {
		t.Fatalf("the profile_message call did not report profile_same_transcript: %s", last)
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

// holdTheTargetRun scripts the caller's first request to call review/reviewer and holds the target's
// provider request (only that one) until its context ends.
func holdTheTargetRun(f *layoutFixture) {
	call := scriptFunctionCall("profile_message", map[string]any{"profile": "review/reviewer", "message": "hi"})
	f.reached, f.cancelled = make(chan struct{}, 4), make(chan struct{}, 4)
	f.script = func(model, body string) string {
		if model == "model-small" && !strings.Contains(body, "function_call_output") {
			f.mu.Lock()
			f.hold = true
			f.mu.Unlock()
		}
		return call(model, body)
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func targetRunErrorCache(f *layoutFixture) string {
	cache, _ := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "staging", "run-error-cache.json"))
	return string(cache)
}

func TestProfileMessageTimeoutCutsTheTargetRunEndToEnd(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{"profiles":["review/reviewer"],"timeout_ms":1000}},"mcp":{}}`)
	f.captureBodies = true
	holdTheTargetRun(f)
	coder := boundRunnerOf(t, f, "dev/coder")

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := coder.Run(runCtx, json.RawMessage(`"go"`), session.RunOptions{})
		done <- err
	}()
	awaitSignal(t, f.reached, "the target's provider request")
	f.mu.Lock()
	f.hold = false
	f.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("caller run: %v, want success after the tool timed out", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller run did not finish after the timeout")
	}
	awaitSignal(t, f.cancelled, "the held provider request to be cancelled")
	awaitTargetSessionFree(t, f)

	f.mu.Lock()
	last := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "function_call_output") || !strings.Contains(last, "profile_timeout") {
		t.Fatalf("the caller's next request did not carry profile_timeout: %s", last)
	}
	if cache := targetRunErrorCache(f); !strings.Contains(cache, profileRunFailureText) {
		t.Fatalf("target run error not recorded; cache: %s", cache)
	}
}

func TestProfileMessageCallerCancelReachesTheTargetProviderRequest(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{"profiles":["review/reviewer"],"timeout_ms":600000}},"mcp":{}}`)
	holdTheTargetRun(f)
	coder := boundRunnerOf(t, f, "dev/coder")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := coder.Run(runCtx, json.RawMessage(`"go"`), session.RunOptions{})
		done <- err
	}()
	awaitSignal(t, f.reached, "the target's provider request")
	cancel()
	awaitSignal(t, f.cancelled, "the held provider request to be cancelled")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller run did not return after cancel")
	}
	awaitTargetSessionFree(t, f)
	if cache := targetRunErrorCache(f); strings.Contains(cache, profileRunFailureText) {
		t.Fatalf("a cancelled target run was recorded as an error; cache: %s", cache)
	}
}

// awaitTargetSessionFree returns once the target's session lock is free, which is after Message
// has finished recording (or not recording) the target run's failure.
func awaitTargetSessionFree(t *testing.T, f *layoutFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		release, err := f.service.resolver.TryLockSession(context.Background(), "local:demo", "review/reviewer", "")
		if err == nil {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the target session lock stayed held: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeProfileRuns struct {
	err      error
	begun    atomic.Int32
	finished atomic.Int32
}

func (r *fakeProfileRuns) BeginProfileRun(string, string, string, string) (context.Context, func(string), error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	r.begun.Add(1)
	return context.Background(), func(string) { r.finished.Add(1) }, nil
}

type listedRun struct {
	Status string `json:"status"`
	Source string `json:"source"`
	Caller string `json:"caller"`
}

func targetRunInList(t *testing.T, f *layoutFixture) (listedRun, bool) {
	t.Helper()
	status, body := f.do(t, http.MethodGet, "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("session list: %d %s", status, body)
	}
	var list struct {
		Sessions []struct {
			ID            string               `json:"id"`
			RunsByProfile map[string]listedRun `json:"runs_by_profile"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	for _, listed := range list.Sessions {
		if listed.ID == "demo" {
			run, ok := listed.RunsByProfile["review/reviewer"]
			return run, ok
		}
	}
	t.Fatalf("session demo missing from the list: %s", body)
	return listedRun{}, false
}

// startHeldProfileMessage starts a Message to review/reviewer whose provider request is held.
func startHeldProfileMessage(t *testing.T, f *layoutFixture, ctx context.Context) chan error {
	t.Helper()
	f.mu.Lock()
	f.hold, f.reached = true, make(chan struct{}, 4)
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, ctx), profileMessageOwner, "review/reviewer", "hi")
		done <- err
	}()
	awaitSignal(t, f.reached, "the target's provider request")
	return done
}

func awaitMessageEnd(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Message did not return")
		return nil
	}
}

func TestProfileMessageListsTheTargetAsRunningWithItsCaller(t *testing.T) {
	f := newLayoutFixture(t)
	done := startHeldProfileMessage(t, f, context.Background())

	run, ok := targetRunInList(t, f)
	if !ok || run.Status != "running" || run.Source != "profile_message" || run.Caller != "dev/coder" {
		t.Fatalf("runs_by_profile[review/reviewer] = %+v (listed %v), want running from profile_message called by dev/coder", run, ok)
	}

	f.mu.Lock()
	f.hold = false
	f.mu.Unlock()
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("review/reviewer"), "")
	if status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	awaitMessageEnd(t, done)
}

func TestProfileMessageStopOfTheTargetCancelsItsRunAndReportsStopped(t *testing.T) {
	f := newLayoutFixture(t)
	f.cancelled = make(chan struct{}, 4)
	done := startHeldProfileMessage(t, f, context.Background())

	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("review/reviewer"), ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	awaitSignal(t, f.cancelled, "the held provider request to be cancelled")
	if err := awaitMessageEnd(t, done); !errors.Is(err, native.ErrProfileStopped) {
		t.Fatalf("error = %v, want ErrProfileStopped", err)
	}
	awaitTargetSessionFree(t, f)
	if _, ok := targetRunInList(t, f); ok {
		t.Fatal("the stopped target is still listed as a run")
	}
	if cache := targetRunErrorCache(f); strings.Contains(cache, profileRunFailureText) {
		t.Fatalf("a stopped target run was recorded as an error; cache: %s", cache)
	}
}

func TestProfileMessageTargetRunLeavesTheRegistryOnReturnErrorAndCancel(t *testing.T) {
	t.Run("return", func(t *testing.T) {
		f := newLayoutFixture(t)
		if _, err := (profileMessenger{resolver: f.service.resolver}).Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi"); err != nil {
			t.Fatal(err)
		}
		if run, ok := targetRunInList(t, f); ok && run.Source != "" {
			t.Fatalf("a finished profile_message run is still listed: %+v", run)
		}
	})
	t.Run("error", func(t *testing.T) {
		f := newLayoutFixture(t)
		f.script = func(string, string) string {
			return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"  "}]}`
		}
		_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
		if !errors.Is(err, native.ErrProfileNoAnswer) {
			t.Fatalf("error = %v, want ErrProfileNoAnswer", err)
		}
		if run, ok := targetRunInList(t, f); ok && run.Source != "" {
			t.Fatalf("a failed profile_message run is still listed: %+v", run)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newLayoutFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := startHeldProfileMessage(t, f, ctx)
		cancel()
		if err := awaitMessageEnd(t, done); err == nil || errors.Is(err, native.ErrProfileStopped) {
			t.Fatalf("error = %v, want the cancellation, not ErrProfileStopped", err)
		}
		awaitTargetSessionFree(t, f)
		if run, ok := targetRunInList(t, f); ok && run.Source != "" {
			t.Fatalf("a cancelled profile_message run is still listed: %+v", run)
		}
	})
}

func TestProfileMessageBeginRefusalReportsBusyAndReleasesTheTranscript(t *testing.T) {
	for name, code := range map[string]int{"mutation": http.StatusConflict, "cap": http.StatusTooManyRequests} {
		t.Run(name, func(t *testing.T) {
			f := newLayoutFixture(t)
			tracker := &fakeProfileRuns{err: &httpapi.StatusError{Code: code, Message: "refused"}}
			f.service.resolver.SetProfileRuns(tracker)
			_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
			if !errors.Is(err, native.ErrProfileBusy) {
				t.Fatalf("error = %v, want ErrProfileBusy", err)
			}
			awaitTargetSessionFree(t, f)
		})
	}
}

// mutationRacingRuns wraps the real tracker: it starts a compact of the target, which takes the
// mutation gate and then waits for the transcript the caller holds, and begins only once the
// real tracker refuses with the gate held.
type mutationRacingRuns struct {
	inner   httpapi.ProfileRunTracker
	compact func()
}

func (r *mutationRacingRuns) BeginProfileRun(reference, profile, turnID, caller string) (context.Context, func(string), error) {
	go r.compact()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runCtx, finish, err := r.inner.BeginProfileRun(reference, profile, turnID, caller)
		var statusErr *httpapi.StatusError
		if errors.As(err, &statusErr) && statusErr.Code == http.StatusConflict {
			return nil, nil, err
		}
		if err == nil {
			finish("aborted")
		}
		if time.Now().After(deadline) {
			return runCtx, nil, errors.New("the mutation gate was never observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProfileMessageRefusedByTheRealTrackerMutationGateReportsBusyAndReleasesTheTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	f.service.resolver.SetProfileRuns(&mutationRacingRuns{
		inner: f.service.api.ProfileRuns(),
		compact: func() {
			f.do(t, http.MethodPost, "/api/sessions/local:demo/compact?profile="+url.QueryEscape("review/reviewer"), "")
		},
	})
	_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
	awaitTargetSessionFree(t, f)
}

func TestProfileMessageOtherBeginErrorIsReturnedAndReleasesTheTranscript(t *testing.T) {
	f := newLayoutFixture(t)
	f.service.resolver.SetProfileRuns(&fakeProfileRuns{err: errors.New("run registry is closed")})
	_, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if err == nil || errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want a plain error", err)
	}
	awaitTargetSessionFree(t, f)
}

func TestProfileMessageFinishesTheTrackedRunOnce(t *testing.T) {
	f := newLayoutFixture(t)
	tracker := &fakeProfileRuns{}
	f.service.resolver.SetProfileRuns(tracker)
	if _, err := (profileMessenger{resolver: f.service.resolver}).Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi"); err != nil {
		t.Fatal(err)
	}
	if tracker.begun.Load() != 1 || tracker.finished.Load() != 1 {
		t.Fatalf("begun %d, finished %d, want 1 and 1", tracker.begun.Load(), tracker.finished.Load())
	}
}

func TestProfileMessageWithoutATrackerStillWorks(t *testing.T) {
	f := newLayoutFixture(t)
	f.service.resolver.SetProfileRuns(nil)
	answer, err := profileMessenger{resolver: f.service.resolver}.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi")
	if err != nil || !strings.Contains(answer, "ok") {
		t.Fatalf("answer %q, error %v", answer, err)
	}
}
