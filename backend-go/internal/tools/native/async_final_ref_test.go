package native

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// finalRefAnswer is a multibyte answer of about 5 KB: every cut inside it can split a rune.
func finalRefAnswer() string { return strings.Repeat("é日", 1000) + " END" }

// requireFinalRef checks the shape every kind gives a finished handle: ref is the result file
// holding want whole, bytes is its size, and the summary is a bounded head of it.
func requireFinalRef(t *testing.T, entry asyncmgr.Entry, folder, want string, summaryMax int, cut bool) {
	t.Helper()
	if entry.Ref == "" || filepath.Base(filepath.Dir(entry.Ref)) != folder || filepath.Ext(entry.Ref) != ".txt" {
		t.Fatalf("ref = %q, want a file in the %s folder", entry.Ref, folder)
	}
	data, err := os.ReadFile(entry.Ref)
	if err != nil || string(data) != want {
		t.Fatalf("ref file holds %d bytes (%v), want the whole %d", len(data), err, len(want))
	}
	if entry.Bytes != int64(len(want)) {
		t.Errorf("bytes = %d, want the size of the saved answer %d", entry.Bytes, len(want))
	}
	if entry.Cut != cut {
		t.Errorf("cut = %v, want %v (summary %d bytes of %d)", entry.Cut, cut, len(entry.Summary), len(want))
	}
	if len(entry.Summary) > summaryMax || !utf8.ValidString(entry.Summary) || !strings.HasPrefix(want, entry.Summary) {
		t.Errorf("summary is %d bytes, valid=%v, head=%v; want at most %d, valid UTF-8, a head of the answer", len(entry.Summary), utf8.ValidString(entry.Summary), strings.HasPrefix(want, entry.Summary), summaryMax)
	}
}

func TestProfileMessageFinalAnswerIsSavedWholeBehindRef(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	answer := finalRefAnswer()
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "\n" + answer + "\n"})
	waited, err := f.host.Manager().Wait(context.Background(), "s1:main", []string{id}, 5_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	// The delivered text is cut to result_max_bytes (1024) and says so.
	requireFinalRef(t, waited.Signals[0], "profile_message", answer, 1024, true)
	if want := "/state/demo/review/reviewer/messages.jsonl"; waited.Signals[0].RefTranscript != want {
		t.Errorf("ref_transcript = %q, want %q", waited.Signals[0].RefTranscript, want)
	}
	status, err := f.host.Manager().Status("s1:main", id)
	if err != nil || status != waited.Signals[0] {
		t.Fatalf("status = %+v, %v; want the entry wait delivered", status, err)
	}
	if raw, _ := json.Marshal(status); !strings.Contains(string(raw), `"ref_transcript":"/state/`) {
		t.Errorf("ref_transcript is not in the entry: %s", raw)
	}
	// The record stays small: the answer beyond the result limit is only in the file.
	info, err := os.Stat(filepath.Join(f.dir, ".async", id+".json"))
	if err != nil || info.Size() > 4096 {
		t.Fatalf("record: %v %v, want a small file", info, err)
	}
}

func TestProfileMessageShortFinalAnswerIsNotCut(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "looks fine"})
	waited, err := f.host.Manager().Wait(context.Background(), "s1:main", []string{id}, 5_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	requireFinalRef(t, waited.Signals[0], "profile_message", "looks fine", 1024, false)
	if waited.Signals[0].Summary != "looks fine" {
		t.Errorf("summary = %q", waited.Signals[0].Summary)
	}
}

// TestProfileMessageRefDoesNotDependOnTheResponseLimit: with a response budget too small for
// any result text, the entry carries a bounded cut head and the file still holds everything.
func TestProfileMessageRefDoesNotDependOnTheResponseLimit(t *testing.T) {
	f := newProfileMessageHandleFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.ResultMaxBytes = 1 << 20 }, func(c *asyncmgr.Config) {
		c.SummaryMaxBytes, c.StatusSummaryBytes, c.ResponseMaxBytes = 200, 200, 200+asyncmgr.EntryOverheadBytes+256
	})
	answer := finalRefAnswer()
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), "s1:main", []string{id}, 5_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	requireFinalRef(t, waited.Signals[0], "profile_message", answer, 200, true)
}

func TestProfileMessageFinalAnswerSurvivesAFailedResultFile(t *testing.T) {
	f := newProfileMessageHandleFixture(t, nil, nil)
	// A file where the results folder belongs: the answer cannot be saved.
	if err := os.WriteFile(filepath.Join(f.dir, ".async", "profile_message"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	answer := finalRefAnswer()
	id := handleIDPattern.FindString(f.send(t, sessionCaller, 0))
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), "s1:main", []string{id}, 5_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	entry := waited.Signals[0]
	if entry.State != asyncmgr.StateDone || !entry.OK || entry.Ref != "" || !entry.Cut || entry.Bytes != int64(len(answer)) || entry.Summary == "" {
		t.Fatalf("entry = %+v; want done, no ref, cut, the answer's size and a bounded summary", entry)
	}
	if entry.RefTranscript == "" {
		t.Errorf("the transcript ref was lost with the result file")
	}
}

func TestSubagentFinalAnswerIsSavedWholeBehindRef(t *testing.T) {
	transcriptDir := filepath.Join(t.TempDir(), "child")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	answer := finalRefAnswer()
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(transcriptDir)
		return SubagentResult{Text: answer + "\n"}, nil
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.ResultMaxBytes = 1024
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), settings, launcher)
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "look", "block_ms": int64(0)}))
	waitForState(t, f.host, "s1", id, asyncmgr.StateDone)
	status, err := f.host.Manager().Status("s1", id)
	if err != nil {
		t.Fatal(err)
	}
	requireFinalRef(t, status, "subagent", answer, 1024, true)
	if want := filepath.Join(transcriptDir, "messages.jsonl"); status.RefTranscript != want {
		t.Errorf("ref_transcript = %q, want %q", status.RefTranscript, want)
	}
	waited, err := f.host.Manager().Wait(context.Background(), "s1", []string{id}, 5_000_000_000)
	if err != nil || len(waited.Signals) != 1 || waited.Signals[0] != status {
		t.Fatalf("wait = %+v, %v; want the entry status gave", waited, err)
	}
}

func TestSubagentShortFinalAnswerIsNotCutAndHasNoTranscriptBeforeOne(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "found it"}, nil
	}}
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), subagentSettings(agentNamed("explore")), launcher)
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "look", "block_ms": int64(0)}))
	waitForState(t, f.host, "s1", id, asyncmgr.StateDone)
	status, err := f.host.Manager().Status("s1", id)
	if err != nil {
		t.Fatal(err)
	}
	requireFinalRef(t, status, "subagent", "found it", 1024, false)
	if status.RefTranscript != "" {
		t.Errorf("ref_transcript = %q, want none for a run with no transcript", status.RefTranscript)
	}
}

// remoteLongAnswer is a multibyte answer far over the remote's view limit; every part boundary
// of the result route can split a rune.
func remoteLongAnswer() string { return strings.Repeat("é日<&", 30_000) + " END" }

// requireRemoteFullRef checks a remote handle's entry against the whole answer the remote held.
func requireRemoteFullRef(t *testing.T, entry asyncmgr.Entry, folder, answer string, summaryMax int) {
	t.Helper()
	requireFinalRef(t, entry, folder, answer, summaryMax, true)
	if entry.RefTranscript != "" {
		t.Errorf("ref_transcript = %q, want none for a remote run", entry.RefTranscript)
	}
}

// The remote cuts its view at 4 KiB and the hub's response limit is 64 KiB: the answer is about
// 240 KB, and ref still holds all of it, read through the result route in parts.
func TestRemoteMessageFinalAnswerBeyondTheViewIsSavedWhole(t *testing.T) {
	p := startPeerWith(t, peerGrant, func(o *remoteapi.Options) { o.ResultMaxBytes = 4096 }, 64<<10)
	f := newRemoteMessageFixtureOn(t, p, nil)
	answer := remoteLongAnswer()
	id := handleIDPattern.FindString(f.send(t, remoteTarget, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	requireRemoteFullRef(t, waited.Signals[0], "profile_message", answer, 1024)
	status, err := f.host.Manager().Status(remoteCaller.SessionKey, id)
	if err != nil || status != waited.Signals[0] {
		t.Fatalf("status = %+v, %v; want the entry wait delivered", status, err)
	}
}

// The same answer with the remote's view limit above what the hub's response limit takes: the
// view is shortened to fit the hub's limit, so the poll does not fail as too large.
func TestRemoteViewIsShortenedToTheHubResponseLimit(t *testing.T) {
	p := startPeerWith(t, peerGrant, nil, 80<<10)
	f := newRemoteMessageFixtureOn(t, p, nil)
	// Escaped by JSON six times over ("<" is \u003c): a view of 80 KiB would not fit.
	answer := strings.Repeat("<", 200_000)
	id := handleIDPattern.FindString(f.send(t, remoteTarget, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	entry := waited.Signals[0]
	if entry.State != asyncmgr.StateDone || !entry.OK {
		t.Fatalf("entry = %+v, want done", entry)
	}
	requireRemoteFullRef(t, entry, "profile_message", answer, 1024)
}

func TestRemoteSubagentFinalAnswerBeyondTheViewIsSavedWhole(t *testing.T) {
	p := startPeerWith(t, peerGrant, func(o *remoteapi.Options) { o.ResultMaxBytes = 4096 }, 64<<10)
	f := newRemoteSubagentFixtureOn(t, p, nil)
	answer := remoteLongAnswer()
	id := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	requireRemoteFullRef(t, waited.Signals[0], "remote_subagent", answer, 4096)
}

// A remote answer within its view is saved from the view: no second request is made.
func TestRemoteFinalAnswerWithinTheViewIsNotCutAndNotFetched(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	var fetches atomic.Int32
	f.peer.handler.mu.Lock()
	next := f.peer.handler.next
	f.peer.handler.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			fetches.Add(1)
		}
		next.ServeHTTP(w, r)
	})
	f.peer.handler.mu.Unlock()
	id := remoteRunHandle.FindString(f.start(t, remoteAgent, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: "found it"})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	requireFinalRef(t, waited.Signals[0], "remote_subagent", "found it", 1024, false)
	if fetches.Load() != 0 {
		t.Errorf("%d result requests for an answer the view held whole", fetches.Load())
	}
}

// A remote without the result route (an older server) or one that fails it leaves no ref: the
// delivered text is cut and says so, and no partial file is left under the results folder.
func TestRemoteFinalAnswerThatCannotBeFetchedHasNoRefAndIsCut(t *testing.T) {
	p := startPeerWith(t, peerGrant, func(o *remoteapi.Options) { o.ResultMaxBytes = 4096 }, 0)
	f := newRemoteMessageFixtureOn(t, p, nil)
	p.handler.mu.Lock()
	next := p.handler.next
	p.handler.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
	p.handler.mu.Unlock()
	answer := remoteLongAnswer()
	id := handleIDPattern.FindString(f.send(t, remoteTarget, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: answer})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	entry := waited.Signals[0]
	if entry.State != asyncmgr.StateDone || !entry.OK || entry.Ref != "" || !entry.Cut || entry.Summary == "" {
		t.Fatalf("entry = %+v; want done, no ref, cut, and the delivered head", entry)
	}
	if left, _ := filepath.Glob(filepath.Join(f.host.dir, "profile_message", "*")); len(left) != 0 {
		t.Errorf("files left behind: %v", left)
	}
}

// A result that cannot be written locally leaves no ref either, and the remote answer is not
// presented as whole.
func TestRemoteFinalAnswerWithAFailedResultFileHasNoRef(t *testing.T) {
	p := startPeerWith(t, peerGrant, func(o *remoteapi.Options) { o.ResultMaxBytes = 4096 }, 0)
	f := newRemoteMessageFixtureOn(t, p, nil)
	if err := os.WriteFile(filepath.Join(f.host.dir, "profile_message"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := handleIDPattern.FindString(f.send(t, remoteTarget, 0, remoteCaller))
	f.peer.backend.run(t, 0).finish(remoteapi.RunResult{State: "done", Text: remoteLongAnswer()})
	waited, err := f.host.Manager().Wait(context.Background(), remoteCaller.SessionKey, []string{id}, 10_000_000_000)
	if err != nil || len(waited.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", waited, err)
	}
	if entry := waited.Signals[0]; entry.Ref != "" || !entry.Cut || !entry.OK {
		t.Fatalf("entry = %+v; want done, cut, no ref", entry)
	}
}

// A run the remote API serves keeps its whole answer for that API, while the local result of the
// same run is cut at result_max_bytes: the remote API bounds and parts the answer itself.
func TestExternalRunOutcomeIsTheWholeAnswerBeyondResultMaxBytes(t *testing.T) {
	answer := finalRefAnswer()
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: answer}, nil
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.ResultMaxBytes = 1024
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), settings, launcher)
	var run *ExternalRun
	ctx := WithExternalRun(context.Background(), func(r *ExternalRun) { run = r })
	arguments := map[string]any{"task": "look", "agent": "explore"}
	if f.tool.NormalizeArguments != nil {
		arguments = f.tool.NormalizeArguments(arguments)
	}
	out, err := f.tool.Execute(ctx, arguments, tools.ExecutionContext{CallID: "s1", WorkspaceRoot: f.dir, SessionDir: f.dir, SessionKey: "s1"})
	if err != nil || run == nil {
		t.Fatalf("execute: %v run=%v", err, run)
	}
	<-run.Done()
	if state, text, _ := run.Outcome(); state != "done" || text != answer {
		t.Fatalf("outcome: %s, %d bytes; want the whole %d-byte answer", state, len(text), len(answer))
	}
	if !strings.Contains(out, "truncated: true") {
		t.Errorf("the local result of the call is still cut:\n%.300s", out)
	}
	// The run holds the whole answer for the remote API.
	if f.manager.byID(run.ID()).full == "" {
		t.Fatal("the external run lost its whole answer")
	}
}

func TestLocalRunDoesNotKeepItsWholeAnswerInMemory(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: finalRefAnswer()}, nil
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.ResultMaxBytes = 1024
	f := newSubagentAsyncFixture(t, asyncmgr.DefaultConfig(), settings, launcher)
	id := runIDOf(t, f.start(t, "s1", map[string]any{"task": "look", "block_ms": int64(0)}))
	waitForState(t, f.host, "s1", id, asyncmgr.StateDone)
	if run := f.manager.byID(id); run == nil || run.full != "" {
		t.Fatalf("a local run holds its whole answer: %v", run)
	}
}
