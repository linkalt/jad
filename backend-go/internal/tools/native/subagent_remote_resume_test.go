package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/tools"
)

// remoteCallOf runs one subagent call under owner with a remote caller on its context, as the
// remote API does after it authenticates a token.
func remoteCallOf(t *testing.T, catalog *tools.Catalog, owner, caller, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_r", "name": "subagent", "arguments": arguments})
	ctx := WithRemoteCaller(context.Background(), caller)
	raw, err := catalog.ExecuteFunctionCall(ctx, call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

// The success test: a run a remote token started can be resumed by that token, and the result
// names the source and a new run.
func TestRemoteCallerResumesARunItStarted(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5000}`)
	source := manager.list(testSessionKey)[0].id
	if caller := manager.byID(source).caller; caller != "remote:tok1" {
		t.Fatalf("the run does not record the remote caller that started it: %q", caller)
	}
	out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+source+`","block_ms":5000}`)
	if !strings.Contains(out, "resumed_from: "+source) {
		t.Fatalf("the remote resume does not name its source %s:\n%s", source, out)
	}
}

// Each refusal of the identity check gives unknown_run, as a local other-owner case does.
func TestRemoteResumeOfAnotherCallersRunIsUnknown(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5000}`)
	source := manager.list(testSessionKey)[0].id
	for name, caller := range map[string]string{"other token": "remote:tok2", "local call": ""} {
		t.Run(name, func(t *testing.T) {
			out := remoteCallOf(t, catalog, testSessionKey, caller, `{"task":"more","agent":"explore","resume":"`+source+`"}`)
			if !strings.Contains(out, "unknown_run") {
				t.Fatalf("the resume was not refused as unknown_run:\n%s", out)
			}
		})
	}
}

// A remote caller never resumes a local run: its caller is empty.
func TestRemoteCallerCannotResumeALocalRun(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	local := finishedSource(t, catalog, manager, testSessionKey)
	out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+local+`"}`)
	if !strings.Contains(out, "unknown_run") {
		t.Fatalf("a remote caller resumed a local run:\n%s", out)
	}
}

// A child of a remote-resumed run gets no remote caller: the child's launcher runs on the manager's
// context, which the call's carrier never reaches.
func TestRemoteResumedRunsChildHasNoRemoteCaller(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	var seen []string
	writer := transcribingLauncher(t, root)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		mu.Lock()
		seen = append(seen, remoteCallerOf(ctx))
		mu.Unlock()
		return writer.Run(ctx, request)
	}}
	catalog, manager := persistedCatalog(t, root, launcher, 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5000}`)
	source := manager.list(testSessionKey)[0].id
	if out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+source+`","block_ms":5000}`); !strings.Contains(out, "resumed_from") {
		t.Fatalf("the remote resume did not start:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("launcher ran %d times, want 2", len(seen))
	}
	// The carrier lives on the call's context; a child runs on the manager's context and never
	// sees it, so neither the started child nor the resumed one carries the remote caller.
	for index, caller := range seen {
		if caller != "" {
			t.Fatalf("child %d saw remote caller %q; a child must not carry the caller of its call", index, caller)
		}
	}
}

// A local call resumes only its own session's local runs: another session's run is unknown to it,
// even when the run is local.
func TestLocalCallCannotResumeAnotherSessionsRun(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	local := finishedSource(t, catalog, manager, testSessionKey)
	out := resumeCall(t, catalog, "some-other-session", `{"resume":"`+local+`","task":"more"}`)
	if !strings.Contains(out, "unknown_run") {
		t.Fatalf("a local call resumed another session's run:\n%s", out)
	}
}

// A remote resume gets the same refusals a local one does, each on the real manager and with the
// remote caller that started the source: an agent that is not the source's, an unanswered call, and
// a missing transcript. The source is left as it was in each case.
func TestRemoteResumeRefusalsMatchTheLocalOnes(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5000}`)
	source := manager.list(testSessionKey)[0].id
	dir := manifestOf(t, root, source).Dir

	if out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"other","resume":"`+source+`"}`); !strings.Contains(out, "resume_agent_mismatch") {
		t.Fatalf("a remote resume with another agent was not refused:\n%s", out)
	}
	open := `{"type":"function_call","call_id":"c_open","name":"read","arguments":"{}"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, subagentResumeFile), []byte(open), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+source+`"}`); !strings.Contains(out, "resume_unanswered_call") {
		t.Fatalf("a remote resume over an unanswered call was not refused:\n%s", out)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+source+`"}`); !strings.Contains(out, "is gone") {
		t.Fatalf("a remote resume with the transcript gone was not refused as gone:\n%s", out)
	}
	if state := manager.byID(source).view().state; state != subagentRunDone {
		t.Fatalf("the source changed to %q after refused resumes", state)
	}
}

// A remote resume is a top-level start: a child cannot make one, whatever caller its call carries.
// The child's call is refused as resume_in_child before any identity check.
func TestRemoteCallerChildCannotResume(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5000}`)
	source := manager.list(testSessionKey)[0].id
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_child", "name": "subagent", "arguments": `{"task":"more","agent":"explore","resume":"` + source + `"}`})
	ctx := WithRemoteCaller(context.Background(), "remote:tok1")
	raw, err := catalog.ExecuteFunctionCall(ctx, call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, AgentDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "resume_in_child") {
		t.Fatalf("a child's remote-caller resume was not refused as resume_in_child:\n%s", raw)
	}
}

// A remote resume starts its run through manager.start, the same path a local start takes: the new
// run is registered with its own id and names the source it continues. A top-level start is not
// capped (caps apply to nested starts), so this test checks registration, not a refusal.
func TestRemoteResumeGoesThroughTheManagersStart(t *testing.T) {
	root := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(writeTranscript(t, root, "call-resume-count", "more work"))
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "done", Rounds: 1}, nil
	}}
	catalog, manager := persistedCatalog(t, root, launcher, 10)
	remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"first","agent":"explore","block_ms":5}`)
	source := manager.list(testSessionKey)[0].id
	manager.mu.Lock()
	manager.runs[source].state = subagentRunDone
	manager.mu.Unlock()
	out := remoteCallOf(t, catalog, testSessionKey, "remote:tok1", `{"task":"more","agent":"explore","resume":"`+source+`","block_ms":5}`)
	if !strings.Contains(out, "resumed_from: "+source) {
		t.Fatalf("the remote resume did not start:\n%s", out)
	}
	runs := manager.list(testSessionKey)
	if len(runs) != 2 {
		t.Fatalf("the resumed run is not registered with the manager: %d runs, want 2", len(runs))
	}
	var resumedFrom string
	for _, run := range runs {
		if run.id != source {
			resumedFrom = run.resumedFrom
		}
	}
	if resumedFrom != source {
		t.Fatalf("the registered run does not continue the source %s: resumedFrom %q", source, resumedFrom)
	}
}

// A queued run is still going, so a resume of it is refused as resume_active. The queued run is a
// local one: a remote caller's call waits for its end, so it cannot queue behind a held slot here. The
// state check is shared by both callers, so this is the check that refuses a queued run for each.
func TestQueuedRunIsRefusedAsActiveWhenResumed(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	settings.MaxQueued = 1
	parentTools := withExecutors(t, readOnlyTool("read"))
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(settings, parentTools, nil, blockedLauncher(release, nil), nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parentTools, tool)
	// The first run holds the one slot and the second waits in the queue, as the queue test shows.
	subagentCallAs(t, catalog, testSessionKey, `{"task":"holds the slot","block_ms":0}`)
	subagentCallAs(t, catalog, testSessionKey, `{"task":"waits","block_ms":0}`)
	var queued string
	for _, run := range manager.list(testSessionKey) {
		if run.state == subagentRunQueued {
			queued = run.id
		}
	}
	if queued == "" {
		t.Fatal("no run is queued, so the state check is not exercised")
	}
	if state := manager.byID(queued).view().state; state != subagentRunQueued {
		t.Fatalf("the run is %q, want queued: the test would not reach the queued case", state)
	}
	out := resumeCall(t, catalog, testSessionKey, `{"task":"more","agent":"explore","resume":"`+queued+`"}`)
	if !strings.Contains(out, "resume_active") {
		t.Fatalf("a resume of a queued run was not refused as resume_active:\n%s", out)
	}
}
