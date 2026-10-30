package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

// persistedCatalog builds the tool over a manager that keeps manifests in dir, as the
// resolver does after a restart: a new manager over the same storage directory.
func persistedCatalog(t *testing.T, dir string, launcher SubagentLauncher, keep int) (*tools.Catalog, *SubagentManager) {
	t.Helper()
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: dir, Keep: keep})
	t.Cleanup(func() { _ = manager.Close() })
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir = dir
	settings.PersistRuns = true
	settings.KeepFinished = int64(keep)
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithManager(settings, parent, launcher, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, manager
}

// restoredOutput renders the run a restarted manager holds under id, the way a start call
// would, or reports it unknown to the owner.
func restoredOutput(t *testing.T, manager *SubagentManager, owner, id string) string {
	t.Helper()
	run := manager.get(id, owner)
	if run == nil {
		return "error_code: unknown_run"
	}
	run.loadText()
	out, err := (&subagentTool{}).renderRun("c", run.view())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// startedRunID runs one task to its end and returns the id of the run it made. A call
// that finishes inline does not print its id, so the id is read from the manager: the
// newest run of the owner.
func startedRunID(t *testing.T, catalog *tools.Catalog, manager *SubagentManager, owner, arguments string) string {
	t.Helper()
	subagentCallAs(t, catalog, owner, arguments)
	views := manager.list(owner)
	if len(views) == 0 {
		t.Fatal("the call made no run")
	}
	return views[0].id
}

// writeTranscript writes a child transcript with the given completed-response texts,
// one per response, and returns its directory.
func writeTranscript(t *testing.T, root, name string, texts ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	lines = append(lines, `{"type":"message","role":"user","content":[{"type":"input_text","text":"task"}]}`)
	for _, text := range texts {
		output := `[]`
		if text != "" {
			encoded, _ := json.Marshal(text)
			output = `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]`
		}
		lines = append(lines, `{"type":"response.completed","response":{"id":"r","status":"completed","output":`+output+`}}`)
		lines = append(lines, `{"type":"function_call_output","call_id":"c","output":"response.completed is only words here"}`)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func manifestOf(t *testing.T, dir, id string) subagentManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, subagentManifestDir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest subagentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestSubagentFinishedRunSurvivesARestartAndItsAnswerIsReadFromTheTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := writeTranscript(t, root, "call-1", "first thoughts", "the answer is 42")
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(transcript)
		return SubagentResult{Text: "the answer is 42", Rounds: 2, Tokens: 321}, nil
	}}
	first, manager := persistedCatalog(t, root, launcher, 10)
	id := startedRunID(t, first, manager, "", `{"task":"find it"}`)
	_ = manager.Close()

	// The server restarts: a new manager over the same directory, and no launcher call.
	_, secondManager := persistedCatalog(t, root, &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		t.Error("a restored run must not start a child")
		return SubagentResult{}, nil
	}}, 10)
	output := restoredOutput(t, secondManager, "", id)
	for _, want := range []string{"Agent explore finished", "the answer is 42", "rounds: 2", "tokens: 321"} {
		if !strings.Contains(output, want) {
			t.Errorf("restored run lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "first thoughts") {
		t.Errorf("a finished run must return its final answer, not an earlier round:\n%s", output)
	}
}

func TestSubagentRunThatWasGoingAtARestartIsLostWithItsPartialWork(t *testing.T) {
	root := t.TempDir()
	transcript := writeTranscript(t, root, "call-2", "found the writer in journal.go")
	release := make(chan struct{})
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
	parent := withExecutors(t, readOnlyTool("read"))
	firstTool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, firstManager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	first := catalogWithAsync(t, host, parent, firstTool)
	id := runIDOf(t, subagentCall(t, first, `{"task":"slow","block_ms":5}`))
	// The process dies here: nothing closes the first manager, so the manifest still
	// says running. The test reads it as the restart would.
	if got := manifestOf(t, root, id); got.State != subagentRunRunning || got.Dir != transcript {
		t.Fatalf("manifest before the restart = %+v", got)
	}
	defer close(release)

	_, restarted := persistedCatalog(t, root, &fakeLauncher{}, 10)
	output := restoredOutput(t, restarted, testSessionKey, id)
	for _, want := range []string{"error_code: subagent_lost", "run: " + id, "partial: true", "found the writer in journal.go", "transcript: " + filepath.Join(transcript, "messages.jsonl")} {
		if !strings.Contains(output, want) {
			t.Errorf("lost run lacks %q:\n%s", want, output)
		}
	}
	if got := manifestOf(t, root, id).State; got != subagentRunLost {
		t.Errorf("manifest state after recovery = %q, want lost", got)
	}
	if restarted.CancelOwner("") != 0 {
		t.Error("a lost run is not active, so a stop must not count it")
	}
}

func TestSubagentRestoredRunsKeepTheirOwner(t *testing.T) {
	root := t.TempDir()
	launcher := &fakeLauncher{}
	first, firstManager := persistedCatalog(t, root, launcher, 10)
	id := startedRunID(t, first, firstManager, "alpha", `{"task":"x"}`)

	_, second := persistedCatalog(t, root, launcher, 10)
	if out := restoredOutput(t, second, "beta", id); !strings.Contains(out, "unknown_run") {
		t.Errorf("another session read a restored run:\n%s", out)
	}
	if out := restoredOutput(t, second, "alpha", id); strings.Contains(out, "unknown_run") {
		t.Errorf("the owner lost its restored run:\n%s", out)
	}
}

func TestSubagentRestoredRunsAreListedAndCountedByTheStateContext(t *testing.T) {
	root := t.TempDir()
	first, firstManager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	id := startedRunID(t, first, firstManager, "", `{"task":"find it"}`)

	_, second := persistedCatalog(t, root, &fakeLauncher{}, 10)
	views := second.list("")
	if len(views) != 1 || views[0].id != id || views[0].state != subagentRunDone {
		t.Fatalf("restored views = %+v", views)
	}
}

func TestSubagentKeepFinishedAlsoPrunesTheManifests(t *testing.T) {
	root := t.TempDir()
	first, firstManager := persistedCatalog(t, root, &fakeLauncher{}, 2)
	var ids []string
	for range 4 {
		ids = append(ids, startedRunID(t, first, firstManager, "", `{"task":"x"}`))
		time.Sleep(2 * time.Millisecond)
	}
	entries, err := os.ReadDir(filepath.Join(root, subagentManifestDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("manifests on disk = %d, want 2 (keep_finished)", len(entries))
	}
	for _, id := range ids[:2] {
		if _, err := os.Stat(filepath.Join(root, subagentManifestDir, id+".json")); !os.IsNotExist(err) {
			t.Errorf("the manifest of the oldest run %s was kept", id)
		}
	}
}

func TestSubagentRecoveryDropsTheOldestRunsBeyondKeep(t *testing.T) {
	root := t.TempDir()
	first, firstManager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	var ids []string
	for range 5 {
		ids = append(ids, startedRunID(t, first, firstManager, "", `{"task":"x"}`))
		time.Sleep(2 * time.Millisecond)
	}
	_, second := persistedCatalog(t, root, &fakeLauncher{}, 2)
	listed := map[string]bool{}
	for _, view := range second.list("") {
		listed[view.id] = true
	}
	if len(listed) != 2 || !listed[ids[3]] || !listed[ids[4]] {
		t.Fatalf("kept after recovery = %v, want the newest two %v", listed, ids[3:])
	}
}

func TestSubagentBadManifestsAreSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	first, firstManager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	good := startedRunID(t, first, firstManager, "", `{"task":"x"}`)
	dir := filepath.Join(root, subagentManifestDir)
	for name, content := range map[string]string{
		"sub_00000001.json": `{not json`,
		"sub_00000002.json": `{"version":99,"id":"sub_00000002","state":"done","started_at":"2026-01-01T00:00:00Z"}`,
		"sub_00000003.json": `{"version":1,"id":"sub_00000099","state":"done","started_at":"2026-01-01T00:00:00Z"}`,
		"sub_00000004.json": `{"version":1,"id":"sub_00000004","state":"vanished","started_at":"2026-01-01T00:00:00Z"}`,
		"sub_00000005.json": `{"version":1,"id":"sub_00000005","state":"done"}`,
		"notes.txt":         `not a manifest`,
		"sub_zzzzzzzz.json": `{}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, second := persistedCatalog(t, root, &fakeLauncher{}, 10)
	views := second.list("")
	if len(views) != 1 || views[0].id != good {
		t.Fatalf("views after bad manifests = %+v, want only %s", views, good)
	}
}

func TestSubagentManifestCannotPointTheReaderOutsideTheStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := writeTranscript(t, t.TempDir(), "secret", "do not read me")
	manifest := subagentManifest{
		Version: 1, ID: "sub_0000abcd", Agent: "explore", Task: "x", State: subagentRunDone,
		StartedAt: time.Now(), EndedAt: time.Now(), Dir: outside,
	}
	data, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(root, subagentManifestDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, subagentManifestDir, "sub_0000abcd.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, manager := persistedCatalog(t, root, &fakeLauncher{}, 10)
	output := restoredOutput(t, manager, "", "sub_0000abcd")
	if strings.Contains(output, "do not read me") || strings.Contains(output, outside) {
		t.Errorf("a manifest led the reader outside the storage directory:\n%s", output)
	}
}

func TestSubagentRestoredRunWithAMissingTranscriptStillAnswers(t *testing.T) {
	root := t.TempDir()
	transcript := writeTranscript(t, root, "call-9", "answer")
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(transcript)
		return SubagentResult{Text: "answer", Rounds: 1, Tokens: 5}, nil
	}}
	first, firstManager := persistedCatalog(t, root, launcher, 10)
	id := startedRunID(t, first, firstManager, "", `{"task":"x"}`)
	if err := os.RemoveAll(transcript); err != nil {
		t.Fatal(err)
	}
	_, second := persistedCatalog(t, root, &fakeLauncher{}, 10)
	output := restoredOutput(t, second, "", id)
	if !strings.Contains(output, "finished without an answer") {
		t.Errorf("a run whose transcript is gone must say it has no answer:\n%s", output)
	}
}

func TestSubagentTranscriptReaderIgnoresWordsInsideToolResults(t *testing.T) {
	dir := writeTranscript(t, t.TempDir(), "c", "", "real answer")
	answers, err := readTranscriptAnswers(filepath.Join(dir, "messages.jsonl"))
	if err != nil || answers.last != "real answer" || answers.lastNonEmpty != "real answer" {
		t.Fatalf("answers = %+v, %v", answers, err)
	}
	dir = writeTranscript(t, t.TempDir(), "d", "earlier text", "")
	answers, err = readTranscriptAnswers(filepath.Join(dir, "messages.jsonl"))
	if err != nil || answers.last != "" || answers.lastNonEmpty != "earlier text" {
		t.Fatalf("answers of a child that ended on a tool call = %+v, %v", answers, err)
	}
}

func TestSubagentNonPersistentManagerWritesNothing(t *testing.T) {
	root := t.TempDir()
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir = root
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithManager(settings, parent, &fakeLauncher{}, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := tools.NewCatalog(append(parent, tool))
	subagentCall(t, catalog, `{"task":"x"}`)
	if _, err := os.Stat(filepath.Join(root, subagentManifestDir)); !os.IsNotExist(err) {
		t.Errorf("persist_runs off still created %s (err %v)", subagentManifestDir, err)
	}
}

func TestSubagentRestoredAnswerIsNotPinnedToTheFirstReadersLimit(t *testing.T) {
	root := t.TempDir()
	answer := strings.Repeat("a", 21000)
	transcript := writeTranscript(t, root, "call-7", answer)
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript(transcript)
		return SubagentResult{Text: answer, Rounds: 1, Tokens: 5}, nil
	}}
	first, firstManager := persistedCatalog(t, root, launcher, 10)
	id := startedRunID(t, first, firstManager, "", `{"task":"x"}`)
	_ = firstManager.Close()

	_, restarted := persistedCatalog(t, root, &fakeLauncher{}, 10)
	driver := &subagentDriver{host: hostWith(t, restarted)}
	cutBefore := restarted.byID(id).view().cut
	if got := driver.Detail(id, 4096); len(got) != 4096 {
		t.Fatalf("a status-sized read returned %d bytes, want 4096", len(got))
	}
	if got := driver.Detail(id, 32768); len(got) != len(answer) {
		t.Fatalf("a later, larger read returned %d bytes, want %d: the first reader's limit was kept", len(got), len(answer))
	}
	if cutAfter := restarted.byID(id).view().cut; cutAfter != cutBefore {
		t.Fatalf("a read changed the retained answer's cut flag from %v to %v", cutBefore, cutAfter)
	}
}
