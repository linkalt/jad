package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// hookCalls records what the resolver's reset and compact hooks were asked, in place of
// the real ones.
type hookCalls struct {
	mu             sync.Mutex
	resets, compts []string
}

func (w *messageWorkspaces) hookTriggers() *hookCalls {
	calls := &hookCalls{}
	w.service.resolver.resetProfile = func(_ context.Context, reference, profile string) error {
		calls.mu.Lock()
		defer calls.mu.Unlock()
		calls.resets = append(calls.resets, reference+"/"+profile)
		return nil
	}
	w.service.resolver.compactProfile = func(_ context.Context, reference, profile string) error {
		calls.mu.Lock()
		defer calls.mu.Unlock()
		calls.compts = append(calls.compts, reference+"/"+profile)
		return nil
	}
	return calls
}

func (c *hookCalls) counts() (resets, compacts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.resets...), append([]string(nil), c.compts...)
}

// isDenied reports whether err is the refusal of the policy by who for action.
func isDenied(err error, by, action string) bool {
	var denied *native.ProfileDeniedError
	return errors.As(err, &denied) && denied.By == by && denied.Action == action
}

func withTrigger(action string, after int64) func(*native.ProfileMessageRequest) {
	return func(request *native.ProfileMessageRequest) {
		request.ContextTrigger = sessionfs.ProfileMessageContextTrigger{Action: action, After: after}
	}
}

// A reset context_trigger of the caller is refused for a target of another workspace unless the
// target workspace's allow_reset says otherwise, before anything of the target is touched: the
// real reset is never run (b's transcript is the same bytes, with and without the usage
// threshold), nothing is queued, and the refusal comes after the target is found, so a
// workspace that is not listed still reads as unknown.
func TestProfileMessageForeignResetIsRefusedBeforeTouchingTheTarget(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	first, err := w.prepare(t, ownerA, "b:s1", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	w.send(t, first)
	transcript := w.transcript(w.b, "s1", "review/reviewer")
	if !strings.Contains(transcript, "hello") {
		t.Fatalf("b's transcript: %q", transcript)
	}
	calls := w.hookTriggers()
	for name, after := range map[string]int64{"after zero": 0, "after set": 1} {
		plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", withTrigger(sessionfs.ProfileMessageContextReset, after))
		if !isDenied(err, "workspace:b", "reset") || plan != nil {
			t.Fatalf("%s: plan %v, err %v", name, plan, err)
		}
	}
	if got := w.transcript(w.b, "s1", "review/reviewer"); got != transcript {
		t.Fatalf("b's transcript changed:\n%s", got)
	}
	if resets, compacts := calls.counts(); len(resets)+len(compacts) != 0 {
		t.Fatalf("hooks ran: resets %v compacts %v", resets, compacts)
	}
	// The real reset (no hook) leaves the file alone as well.
	w.service.resolver.resetProfile, w.service.resolver.compactProfile = nil, nil
	if _, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", withTrigger(sessionfs.ProfileMessageContextReset, 0)); !isDenied(err, "workspace:b", "reset") {
		t.Fatal(err)
	}
	if got := w.transcript(w.b, "s1", "review/reviewer"); got != transcript {
		t.Fatalf("b's transcript changed by the real reset:\n%s", got)
	}
	// The same call without the trigger is sent, so the refusal is the trigger's and not the target's.
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if outcome := w.send(t, plan); outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}

	// A workspace that is not listed is an unknown profile, trigger or not.
	closed := startMessageWorkspaces(t, messageWorkspaceOptions{unlistedB: true})
	if _, err := closed.prepare(t, ownerA, "b:s1", "review/reviewer", withTrigger(sessionfs.ProfileMessageContextReset, 0)); !errors.Is(err, native.ErrProfileUnknown) {
		t.Fatalf("not listed: %v", err)
	}
}

// A compact context_trigger is not refused across workspaces: it deletes nothing, keeps its
// backup and runs with the target's own settings. The hook is asked about the target's session
// of b (the caller's workspace is never the one compacted), and the message is sent.
func TestProfileMessageForeignCompactStillRuns(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	first, err := w.prepare(t, ownerA, "b:s1", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	w.send(t, first)
	calls := w.hookTriggers()
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer", withTrigger(sessionfs.ProfileMessageContextCompact, 0))
	if err != nil {
		t.Fatal(err)
	}
	if outcome := w.send(t, plan); outcome.Err != nil || !strings.Contains(outcome.Text, "ok") {
		t.Fatalf("outcome = %+v", outcome)
	}
	resets, compacts := calls.counts()
	if len(resets) != 0 || len(compacts) != 1 || compacts[0] != "b:s1/review/reviewer" {
		t.Fatalf("resets %v compacts %v", resets, compacts)
	}
	if got := w.transcript(w.b, "s1", "review/reviewer"); strings.Count(got, "hello") != 2 {
		t.Fatalf("both messages are in b's transcript: %q", got)
	}
	// The real compaction is resolved in b (the target's own session and settings).
	w.service.resolver.compactProfile = nil
	if err := w.service.resolver.compactTarget(context.Background(), "b:s1", "review/reviewer"); err != nil {
		t.Fatalf("real compaction of the target: %v", err)
	}
}

// A reset inside one workspace is unchanged: another profile of the caller's own session, and
// a session of the caller's workspace written with its workspace, are reset before the message
// as before (the real reset: the first message is gone from the target's next request).
func TestProfileMessageSameWorkspaceResetStillRuns(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	for name, session := range map[string]string{"own session": "", "own workspace, qualified": "b:s1"} {
		first, err := w.prepare(t, ownerB, session, "review/reviewer", func(request *native.ProfileMessageRequest) { request.Message = "first-" + name })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w.send(t, first)
		second, err := w.prepare(t, ownerB, session, "review/reviewer", withTrigger(sessionfs.ProfileMessageContextReset, 0), func(request *native.ProfileMessageRequest) { request.Message = "second-" + name })
		if err != nil {
			t.Fatalf("%s: a reset in one workspace was refused: %v", name, err)
		}
		if outcome := w.send(t, second); outcome.Err != nil {
			t.Fatalf("%s: outcome = %+v", name, outcome)
		}
		got := w.transcript(w.b, "s1", "review/reviewer")
		if strings.Contains(got, "first-"+name) || !strings.Contains(got, "second-"+name) {
			t.Fatalf("%s: the target was not reset before the message:\n%s", name, got)
		}
	}
}

// The transcript of a target in another workspace is shown to the caller by its name; the
// plan keeps the file for the host's own reads. A target of the caller's own workspace is
// shown as before, by its file.
func TestProfileMessageTranscriptOfAnotherWorkspaceIsLabelled(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{})
	plan, err := w.prepare(t, ownerA, "b:s1", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	file, caller := plan.(native.ProfileMessageTranscripts).Transcripts()
	label, callerLabel := plan.(native.ProfileTranscriptLabels).TranscriptLabels()
	if file != filepath.Join(w.b, ".jad", "state", "s1", "review", "reviewer", "messages.jsonl") || label != "local:b/s1/review/reviewer" || callerLabel != caller || caller == "" {
		t.Fatalf("file %q label %q caller %q callerLabel %q", file, label, caller, callerLabel)
	}
	own, err := w.prepare(t, ownerB, "", "review/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	ownFile, _ := own.(native.ProfileMessageTranscripts).Transcripts()
	if ownLabel, _ := own.(native.ProfileTranscriptLabels).TranscriptLabels(); ownLabel != ownFile || ownFile == "" {
		t.Fatalf("own workspace: file %q label %q", ownFile, ownLabel)
	}
}

// A workspace named remote is an ordinary local workspace: local:remote/<session>/... reaches
// it, and remote: itself still means a remote server and is never read as this workspace. The
// tool end to end, then the transcript the model sees names the target by its canonical label,
// not by the file of b.
func TestProfileMessageReachesAWorkspaceNamedRemote(t *testing.T) {
	w := startMessageWorkspaces(t, messageWorkspaceOptions{bID: "remote", aToolset: "coding/talk"})
	if plan, err := w.prepare(t, ownerA, "remote:s1", "review/reviewer"); err != nil || plan.(*profileMessagePlan).reference != "remote:s1" {
		t.Fatalf("the resolver reaches the workspace named remote: %v", err)
	}
	call := scriptFunctionCall("profile_message", map[string]any{"profile": "local:remote/s1/review/reviewer", "message": "question-for-b"})
	w.script = func(model, body string) string {
		switch {
		case strings.Contains(body, "question-for-b") && !strings.Contains(body, "ask-b"):
			return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer-from-b"}]}`
		case strings.Contains(body, "ask-b"):
			return call(model, body)
		}
		return ""
	}
	request, err := http.NewRequest(http.MethodPost, w.api.URL+"/api/sessions/"+url.PathEscape("a:s1")+"/run?profile="+url.QueryEscape("dev/base"), strings.NewReader(`{"input":"ask-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("run: %d %s", response.StatusCode, body)
	}
	callerTranscript := w.transcript(w.a, "s1", "dev/base")
	if !strings.Contains(callerTranscript, "answer-from-b") || !strings.Contains(w.transcript(w.b, "s1", "review/reviewer"), "question-for-b") {
		t.Fatalf("a's transcript:\n%s\nb's: %q", callerTranscript, w.transcript(w.b, "s1", "review/reviewer"))
	}
	// The tool result shows the target by its qualified name and never by a file of b.
	if !strings.Contains(callerTranscript, "transcript: local:remote/s1/review/reviewer") || strings.Contains(callerTranscript, w.b) {
		t.Fatalf("the caller's transcript shows b's file or lacks the label:\n%s", callerTranscript)
	}
}
