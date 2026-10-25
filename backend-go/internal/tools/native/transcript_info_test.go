package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The same key, transcript, names the full path of the relevant messages.jsonl in the
// results of subagent, profile_message and transcript.

func wantTranscriptKey(t *testing.T, label, output, path string) {
	t.Helper()
	if !strings.Contains(output, "transcript: "+path+"\n") {
		t.Errorf("%s: want the line %q in:\n%s", label, "transcript: "+path, output)
	}
}

func transcriptReportingLauncher(dir string, run func(ctx context.Context) (SubagentResult, error)) *fakeLauncher {
	return &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(dir)
		return run(ctx)
	}}
}

func TestSubagentResultNamesTheChildTranscriptInEveryOutcome(t *testing.T) {
	const want = "/kids/call-1/messages.jsonl"
	finished := transcriptReportingLauncher("/kids/call-1", func(context.Context) (SubagentResult, error) {
		return SubagentResult{Text: "the answer", Rounds: 1}, nil
	})
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), finished, readOnlyTool("read"))
	wantTranscriptKey(t, "finished", subagentCall(t, catalog, `{"task":"x","block_ms":5000}`), want)

	empty := transcriptReportingLauncher("/kids/call-1", func(context.Context) (SubagentResult, error) { return SubagentResult{}, nil })
	catalog, _ = buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), empty, readOnlyTool("read"))
	wantTranscriptKey(t, "empty", subagentCall(t, catalog, `{"task":"x","block_ms":5000}`), want)

	failing := transcriptReportingLauncher("/kids/call-1", func(context.Context) (SubagentResult, error) {
		return SubagentResult{}, errors.New("provider exploded")
	})
	catalog, _ = buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), failing, readOnlyTool("read"))
	wantTranscriptKey(t, "failed", subagentCall(t, catalog, `{"task":"x","block_ms":5000}`), want)

	release := make(chan struct{})
	running := transcriptReportingLauncher("/kids/call-1", func(ctx context.Context) (SubagentResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "late"}, nil
	})
	catalog, _ = buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), running, readOnlyTool("read"))
	out := subagentCall(t, catalog, `{"task":"x","block_ms":50}`)
	close(release)
	wantTranscriptKey(t, "still running", out, want)
	if !strings.Contains(asyncWait(t, catalog), want) {
		t.Error("the async signal of the finished run must keep the transcript as its ref")
	}
}

func TestProfileMessageResultNamesTheTargetAndTheCallerTranscript(t *testing.T) {
	const target, caller = "/state/demo/review/reviewer/messages.jsonl", "/state/demo/dev/coder/messages.jsonl"
	f := newProfileMessageHandleFixture(t, nil, nil)

	out := f.send(t, sessionCaller, 0)
	wantTranscriptKey(t, "running handle", out, target)
	if !strings.Contains(out, "caller_transcript: "+caller) {
		t.Errorf("the caller's own file must be labelled caller_transcript:\n%s", out)
	}
	f.messenger.run(t, 0).finish(ProfileMessageOutcome{Text: "the answer"})
	wait := f.asyncCall(t, sessionCaller, map[string]any{"action": "wait", "wait_ms": json.Number("5000")})
	if !strings.Contains(wait, target) {
		t.Errorf("the async signal must carry the target transcript as ref:\n%s", wait)
	}

	done := make(chan string, 1)
	go func() { done <- f.send(t, sessionCaller, 5000) }()
	for {
		f.messenger.mu.Lock()
		n := len(f.messenger.runs)
		f.messenger.mu.Unlock()
		if n > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.messenger.run(t, 1).finish(ProfileMessageOutcome{Text: "inline answer"})
	answered := <-done
	wantTranscriptKey(t, "answered", answered, target)
	if !strings.Contains(answered, "caller_transcript: "+caller) {
		t.Errorf("answered result lacks caller_transcript:\n%s", answered)
	}

	go func() { done <- f.send(t, sessionCaller, 5000) }()
	for {
		f.messenger.mu.Lock()
		n := len(f.messenger.runs)
		f.messenger.mu.Unlock()
		if n > 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.messenger.run(t, 2).finish(ProfileMessageOutcome{Err: ErrProfileNoAnswer})
	wantTranscriptKey(t, "failed", <-done, target)
}
