package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/tools"
)

// finishedSource runs one task to its end under owner and returns the id of the finished run.
func finishedSource(t *testing.T, catalog *tools.Catalog, manager *SubagentManager, owner string) string {
	t.Helper()
	return startedRunID(t, catalog, manager, owner, `{"task":"first"}`)
}

// resumeCall runs one subagent call under owner and returns the rendered output.
func resumeCall(t *testing.T, catalog *tools.Catalog, owner, arguments string) string {
	t.Helper()
	return subagentCallAs(t, catalog, owner, arguments)
}

func TestResumeOfAnUnknownOrOtherOwnersRunIsUnknown(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	for name, args := range map[string]string{
		"unknown id":       `{"resume":"sub_ffffffff","task":"more"}`,
		"other owner's id": `{"resume":"` + id + `","task":"more"}`,
	} {
		t.Run(name, func(t *testing.T) {
			owner := testSessionKey
			if name == "other owner's id" {
				owner = "someone-else"
			}
			out := resumeCall(t, catalog, owner, args)
			if !strings.Contains(out, "error_code: unknown_run") {
				t.Fatalf("output = %s, want unknown_run", out)
			}
		})
	}
}

// transcribingLauncher is a child that writes a transcript and answers. Its folder is unique to
// the run, as the real launcher's is, so two runs of one call never share a folder.
func transcribingLauncher(t *testing.T, root string) *fakeLauncher {
	var mu sync.Mutex
	runs := 0
	return &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		mu.Lock()
		runs++
		name := fmt.Sprintf("call-%s-%d", request.CallID, runs)
		mu.Unlock()
		dir := writeTranscript(t, root, name, "the answer")
		request.OnTranscript(dir)
		return SubagentResult{Text: "the answer", Rounds: 1}, nil
	}}
}

func TestResumeLeavesTheSourceRunUnchanged(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	before, err := os.ReadFile(filepath.Join(manifestDirOf(root), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	out := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(out, "resumed_from: "+id) {
		t.Fatalf("result does not name the source run:\n%s", out)
	}
	after, err := os.ReadFile(filepath.Join(manifestDirOf(root), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("the source manifest changed:\n before %s\n after  %s", before, after)
	}
}

func manifestDirOf(root string) string { return filepath.Join(root, subagentManifestDir) }

func TestResumeRefusesARunThatIsNotFinished(t *testing.T) {
	root := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(writeTranscript(t, root, "call-running", "still going"))
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "done", Rounds: 1}, nil
	}}
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir = root
	settings.PersistRuns = true
	settings.KeepFinished = 10
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	out := subagentCallAs(t, catalog, testSessionKey, `{"task":"slow","block_ms":5}`)
	id := runIDOf(t, out)
	if state := manager.byID(id).view().state; state != subagentRunRunning {
		t.Fatalf("source state = %q, want running", state)
	}
	got := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(got, "resume_active") || !strings.Contains(got, "still running") {
		t.Fatalf("a running source was not refused:\n%s", got)
	}
}

func TestResumeRefusesAnAgentThatIsNotTheSourcesAndInheritsWhenOmitted(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	mismatch := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more","agent":"other"}`)
	if !strings.Contains(mismatch, "resume_agent_mismatch") {
		t.Fatalf("a different agent was not refused:\n%s", mismatch)
	}
	inherited := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(inherited, "resumed_from: "+id) {
		t.Fatalf("omitting agent did not inherit the source's agent:\n%s", inherited)
	}
}

func TestResumeRefusesASourceWithAnUnansweredToolCall(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	dir := manifestOf(t, root, id).Dir
	open := `{"type":"function_call","call_id":"c_open","name":"read","arguments":"{}"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, subagentResumeFile), []byte(open), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(got, "resume_unanswered_call") {
		t.Fatalf("an unanswered call was not refused:\n%s", got)
	}
}

func TestResumeRefusesAMissingTranscriptAndSaysItIsGone(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	if err := os.RemoveAll(manifestOf(t, root, id).Dir); err != nil {
		t.Fatal(err)
	}
	got := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(got, "unknown_run") || !strings.Contains(got, "is gone") {
		t.Fatalf("a missing transcript was not refused as gone:\n%s", got)
	}
}

func TestResumeRefusesALostRun(t *testing.T) {
	root := t.TempDir()
	transcript := writeTranscript(t, root, "call-lost", "partial work")
	release := make(chan struct{})
	defer close(release)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(transcript)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{}, ctx.Err()
	}}
	firstManager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = firstManager.Close() })
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir = root
	settings.PersistRuns = true
	parent := withExecutors(t, readOnlyTool("read"))
	firstTool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, firstManager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	first := catalogWithAsync(t, host, parent, firstTool)
	id := runIDOf(t, subagentCallAs(t, first, testSessionKey, `{"task":"slow","block_ms":5}`))
	// The server dies here: the manifest still says running. The restart reads it as lost.
	catalog, _ := persistedCatalog(t, root, &fakeLauncher{}, 10)
	got := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(got, "resume_lost") || !strings.Contains(got, "cannot be resumed") {
		t.Fatalf("a lost run was not refused:\n%s", got)
	}
}

func TestResumeIsRefusedForARemoteAgent(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	id := finishedSource(t, catalog, manager, testSessionKey)
	got := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more","agent":"remote:other/explore"}`)
	if !strings.Contains(got, "resume_remote") {
		t.Fatalf("a remote agent with resume was not refused:\n%s", got)
	}
}

func TestResumedTranscriptSurvivesPruningTheSource(t *testing.T) {
	root := t.TempDir()
	// keep is 2: the resumed run and the newest other run stay; the source is the oldest and is pruned.
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 2)
	// Pruning deletes a dropped run's folder only when delete_transcripts is on.
	manager.DeleteTranscriptsUnder(root)
	source := finishedSource(t, catalog, manager, testSessionKey)
	sourceDir := manifestOf(t, root, source).Dir
	resumed := startedRunID(t, catalog, manager, testSessionKey, `{"resume":"`+source+`","task":"more"}`)
	finishedSource(t, catalog, manager, testSessionKey)
	if _, err := os.Stat(sourceDir); err == nil {
		t.Fatal("the source folder was not pruned, so the test proves nothing")
	}
	dir := manifestOf(t, root, resumed).Dir
	if _, err := os.Stat(filepath.Join(dir, subagentResumeFile)); err != nil {
		t.Fatalf("the resumed transcript is gone after the source was pruned: %v", err)
	}
}

func TestResumedRunIsAnAsyncHandleAndWaitStatusStopWorkOnIt(t *testing.T) {
	root := t.TempDir()
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir = root
	settings.PersistRuns = true
	settings.KeepFinished = 10
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, transcribingLauncher(t, root), nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	source := finishedSource(t, catalog, manager, testSessionKey)
	out := resumeCall(t, catalog, testSessionKey, `{"resume":"`+source+`","task":"more","block_ms":0}`)
	resumed := runIDOf(t, out)
	if !strings.Contains(out, "resumed_from: "+source) {
		t.Fatalf("result lacks resumed_from:\n%s", out)
	}
	if got := manager.byID(resumed); got == nil || got.resumedFrom != source {
		t.Fatalf("the resumed run does not name its source: %+v", got)
	}
}

func TestAChildCannotResumeARun(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	source := finishedSource(t, catalog, manager, testSessionKey)
	// A child with depth 1 names a run of its session: the refusal comes before any lookup.
	launcher := &fakeLauncher{}
	_, tool := buildSubagentCatalogFrom(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	out, err := tool.Execute(context.Background(), map[string]any{"task": "more", "resume": source}, tools.ExecutionContext{CallID: "c", SessionKey: testSessionKey, AgentDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "resume_in_child") {
		t.Fatalf("a child's resume was not refused:\n%s", out)
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("a refused child resume still started a child: %d", len(launcher.requests))
	}
}

func TestResumedRunsTranscriptIsItsOwnFolderAndHoldsTheSeededHistory(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	source := finishedSource(t, catalog, manager, testSessionKey)
	resumed := startedRunID(t, catalog, manager, testSessionKey, `{"resume":"`+source+`","task":"more"}`)
	dir, owner, found := manager.RunTranscript(resumed)
	if !found || owner != testSessionKey {
		t.Fatalf("RunTranscript(%s) = %q, %q, %v", resumed, dir, owner, found)
	}
	if dir == manifestOf(t, root, source).Dir {
		t.Fatal("the resumed run's transcript is the source's folder")
	}
	data, err := os.ReadFile(filepath.Join(dir, subagentResumeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "task") {
		t.Fatalf("the resumed transcript does not hold the seeded history:\n%s", data)
	}
}

func TestConfigNoteSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	source := finishedSource(t, catalog, manager, testSessionKey)
	// The source ran with an older definition: its fingerprint differs from the agent's now.
	manager.byID(source).config = "fingerprint-of-an-older-definition"
	resumed := startedRunID(t, catalog, manager, testSessionKey, `{"resume":"`+source+`","task":"more"}`)
	if got := manager.byID(resumed).view().configNote; got == "" {
		t.Fatal("the resume made no config note")
	}
	// The server restarts: the note is rebuilt from the manifest and must still be there.
	restarted := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = restarted.Close() })
	if got := restarted.byID(resumed).view().configNote; got == "" {
		t.Fatal("the config note of a resumed run is lost on a restart")
	}
}

func TestResumeWithMatchingOrMissingFingerprintMakesNoConfigNote(t *testing.T) {
	root := t.TempDir()
	catalog, manager := persistedCatalog(t, root, transcribingLauncher(t, root), 10)
	source := finishedSource(t, catalog, manager, testSessionKey)
	// The source's fingerprint is the agent's own: a resume with an unchanged definition has no note.
	matching := startedRunID(t, catalog, manager, testSessionKey, `{"resume":"`+source+`","task":"more"}`)
	if got := manager.byID(matching).view().configNote; got != "" {
		t.Fatalf("an unchanged definition made a config note: %q", got)
	}
	// A source written before the field existed has no fingerprint, so there is nothing to compare.
	manager.byID(source).config = ""
	missing := startedRunID(t, catalog, manager, testSessionKey, `{"resume":"`+source+`","task":"more"}`)
	if got := manager.byID(missing).view().configNote; got != "" {
		t.Fatalf("a source without a fingerprint made a config note: %q", got)
	}
}
