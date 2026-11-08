package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/webui"
)

func completedLine(turn, step int, text string) string {
	return fmt.Sprintf(`{"type":"response.completed","session_turn":%d,"session_step":%d,"response":{"id":"resp-%d","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]}}`+"\n", turn, step, step, text)
}

const childUserLine = `{"type":"message","role":"user","content":[{"type":"input_text","text":"the task"}]}` + "\n"

// childFolder writes a child's transcript under root as a run does: <call id>-<8 hex>.
func childFolder(t *testing.T, root, name, transcript string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func snapshotText(snapshot *webui.SubagentSnapshot) string {
	var texts []string
	for _, step := range snapshot.Steps {
		for _, part := range step.Parts {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "|")
}

func TestSubagentSnapshotIsFoundByCallIDAndGroupsCompletedSteps(t *testing.T) {
	root := t.TempDir()
	childFolder(t, root, "toolu_01-0a1b2c3d", childUserLine+completedLine(1, 1, "first")+completedLine(1, 2, "second"))
	// Another call's folder is never read for this one.
	childFolder(t, root, "toolu_02-0a1b2c3d", completedLine(1, 1, "other"))
	snapshot := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "toolu_01", Agent: "explore"}, defaultSubagentSnapshotLimits)
	if snapshot == nil || snapshot.Agent != "explore" || snapshotText(snapshot) != "first|second" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Steps[0].ID != "1.1" || snapshot.Steps[1].ID != "1.2" || snapshot.Steps[0].Status != "completed" {
		t.Fatalf("steps = %+v", snapshot.Steps)
	}
}

func TestSubagentSnapshotNeverClaimsAnUnfinishedStepCompleted(t *testing.T) {
	root := t.TempDir()
	// A torn last line (the child is writing it) and a response the provider did not
	// complete add no step; the complete line before them does.
	incomplete := `{"type":"response.completed","session_turn":1,"session_step":2,"response":{"id":"r2","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"cut off"}]}]}}` + "\n"
	torn := strings.TrimSuffix(completedLine(1, 3, "torn"), "\n")
	childFolder(t, root, "c1-0a1b2c3d", completedLine(1, 1, "done")+incomplete+torn)
	snapshot := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "c1"}, defaultSubagentSnapshotLimits)
	if snapshot == nil || snapshotText(snapshot) != "done" {
		t.Fatalf("snapshot = %+v, want only the complete step", snapshot)
	}
	// The response the provider did not complete is a newer one than the step shown, whose
	// text may be the child's; the torn line is not (it is not a record yet).
	if !snapshot.MayHaveNewer {
		t.Fatalf("snapshot = %+v, want MayHaveNewer for the refused response", snapshot)
	}
	childFolder(t, root, "c3-0a1b2c3d", completedLine(1, 1, "done")+torn)
	if got := readSnapshotOf(t, root, "c3", defaultSubagentSnapshotLimits); got == nil || got.MayHaveNewer {
		t.Fatalf("a torn last line alone is no newer response: %+v", got)
	}
	// A child that has written nothing complete yet has nothing to show.
	childFolder(t, root, "c2-0a1b2c3d", childUserLine+torn)
	if got := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "c2"}, defaultSubagentSnapshotLimits); got != nil {
		t.Fatalf("snapshot of a child with no completed step = %+v", got)
	}
}

func TestSubagentSnapshotRefusesPathsOutsideTheStorageDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsidePath := childFolder(t, outside, "evil-0a1b2c3d", completedLine(1, 1, "secret"))
	deeper := childFolder(t, filepath.Join(root, "nested"), "evil-0a1b2c3d", completedLine(1, 1, "deep"))
	runs := childFolder(t, root, ".runs", completedLine(1, 1, "manifest"))
	for name, call := range map[string]webui.SubagentCall{
		"absolute path elsewhere": {CallID: "x", Transcript: outsidePath},
		"traversal":               {CallID: "x", Transcript: filepath.Join(root, "..", filepath.Base(outside), "evil-0a1b2c3d", "messages.jsonl")},
		"two levels deep":         {CallID: "x", Transcript: deeper},
		"manifest folder":         {CallID: "x", Transcript: runs},
		"not messages.jsonl":      {CallID: "x", Transcript: filepath.Join(filepath.Dir(runs), "other.jsonl")},
		"relative":                {CallID: "x", Transcript: "evil-0a1b2c3d/messages.jsonl"},
	} {
		if got := readSubagentSnapshot(context.Background(), root, call, defaultSubagentSnapshotLimits); got != nil {
			t.Errorf("%s: snapshot = %+v, want none", name, got)
		}
	}
	// A folder that links out of the storage directory is not followed.
	if err := os.Symlink(filepath.Dir(outsidePath), filepath.Join(root, "link-0a1b2c3d")); err != nil {
		t.Skip("symlinks are not available")
	}
	if got := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "link"}, defaultSubagentSnapshotLimits); got != nil {
		t.Errorf("symlinked folder: snapshot = %+v, want none", got)
	}
	linkedFile := filepath.Join(root, "file-0a1b2c3d")
	if err := os.MkdirAll(linkedFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(linkedFile, "messages.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "file", Transcript: filepath.Join(linkedFile, "messages.jsonl")}, defaultSubagentSnapshotLimits); got != nil {
		t.Errorf("symlinked transcript: snapshot = %+v, want none", got)
	}
}

func TestSubagentSnapshotFallsBackToTheValidatedTranscriptPath(t *testing.T) {
	root := t.TempDir()
	// Two folders carry the call id, so the call id alone cannot choose one; the printed
	// path, which is a child folder of root, does.
	childFolder(t, root, "dup-0a1b2c3d", completedLine(1, 1, "one"))
	second := childFolder(t, root, "dup-1a1b2c3d", completedLine(1, 1, "two"))
	if got := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "dup"}, defaultSubagentSnapshotLimits); got != nil {
		t.Fatalf("an ambiguous call id gave %+v", got)
	}
	got := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "dup", Transcript: second}, defaultSubagentSnapshotLimits)
	if got == nil || snapshotText(got) != "two" {
		t.Fatalf("snapshot = %+v, want the printed folder's", got)
	}
}

func TestSubagentSnapshotCapsStepsPartsAndBytesAndSkipsHostileLines(t *testing.T) {
	root := t.TempDir()
	var transcript strings.Builder
	transcript.WriteString(strings.Repeat("x", 4096) + "\n") // not JSON: skipped
	for step := 1; step <= 10; step++ {
		transcript.WriteString(completedLine(1, step, strings.Repeat("é", 50)))
	}
	childFolder(t, root, "big-0a1b2c3d", transcript.String())
	limits := defaultSubagentSnapshotLimits
	limits.steps = webui.SubagentLimits{MaxSteps: 3, MaxPartsPerStep: 1, MaxPartBytes: 11, MaxTotalBytes: 1000}
	snapshot := readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "big"}, limits)
	if snapshot == nil || len(snapshot.Steps) != 3 || !snapshot.MayHaveNewer {
		t.Fatalf("snapshot = %+v, want 3 steps and MayHaveNewer (the child wrote 10)", snapshot)
	}
	part := snapshot.Steps[0].Parts[0]
	if part.Text != strings.Repeat("é", 5) || part.Truncated == nil || part.Truncated.Bytes != 100 {
		t.Fatalf("part = %+v, want text cut at a character boundary and marked", part)
	}

	// A line over the line limit is skipped, not parsed; the scan limit stops the file.
	childFolder(t, root, "long-0a1b2c3d", completedLine(1, 1, strings.Repeat("a", 500))+completedLine(1, 2, "kept"))
	limits = defaultSubagentSnapshotLimits
	limits.maxLine = 300
	snapshot = readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "long"}, limits)
	if snapshot == nil || snapshotText(snapshot) != "kept" || snapshot.MayHaveNewer {
		t.Fatalf("snapshot = %+v, want only the line within the limit, and a skipped line before the last step is no newer one", snapshot)
	}
	childFolder(t, root, "scan-0a1b2c3d", completedLine(1, 1, "head")+completedLine(1, 2, "beyond the scan limit"))
	limits = defaultSubagentSnapshotLimits
	limits.maxScan = int64(len(completedLine(1, 1, "head")))
	snapshot = readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: "scan"}, limits)
	if snapshot == nil || snapshotText(snapshot) != "head" || !snapshot.MayHaveNewer {
		t.Fatalf("snapshot = %+v, want only what the scan limit covers, and MayHaveNewer", snapshot)
	}
}

func readSnapshotOf(t *testing.T, root, callID string, limits subagentSnapshotLimits) *webui.SubagentSnapshot {
	t.Helper()
	snapshot, found, err := readSubagentSteps(context.Background(), root, webui.SubagentCall{CallID: callID}, limits)
	if !found || err != nil {
		t.Fatalf("read of %s: found %v, err %v", callID, found, err)
	}
	return snapshot
}

// TestSubagentSnapshotSaysWhatTheReaderCutAndTheBuilderCannotSee: the scan window and the
// line limit are the reader's cuts. A file over the window may hold a newer response past
// it (exact would mean reading it), so it says so; a skipped line says so unless a step
// follows it; a file that fits the window and has no skipped line does not.
func TestSubagentSnapshotSaysWhatTheReaderCutAndTheBuilderCannotSee(t *testing.T) {
	root := t.TempDir()
	first, second := completedLine(1, 1, "head"), completedLine(1, 2, "tail")
	childFolder(t, root, "fits-0a1b2c3d", first+second)
	childFolder(t, root, "window-0a1b2c3d", first+second)
	childFolder(t, root, "skiplast-0a1b2c3d", first+completedLine(1, 2, strings.Repeat("b", 500)))
	childFolder(t, root, "skipmid-0a1b2c3d", first+completedLine(1, 2, strings.Repeat("b", 500))+completedLine(1, 3, "c"))

	limits := defaultSubagentSnapshotLimits
	if got := readSnapshotOf(t, root, "fits", limits); got == nil || got.MayHaveNewer {
		t.Fatalf("a file inside the window: %+v", got)
	}
	// The window ends exactly after the first line: the second is out of it.
	limits.maxScan = int64(len(first))
	if got := readSnapshotOf(t, root, "window", limits); got == nil || snapshotText(got) != "head" || !got.MayHaveNewer {
		t.Fatalf("a file over the window: %+v", got)
	}
	// The window covers the file exactly: nothing is cut.
	limits.maxScan = int64(len(first + second))
	if got := readSnapshotOf(t, root, "fits", limits); got == nil || got.MayHaveNewer {
		t.Fatalf("a window that covers the file: %+v", got)
	}
	limits = defaultSubagentSnapshotLimits
	limits.maxLine = 300
	if got := readSnapshotOf(t, root, "skiplast", limits); got == nil || snapshotText(got) != "head" || !got.MayHaveNewer {
		t.Fatalf("a skipped last line: %+v", got)
	}
	if got := readSnapshotOf(t, root, "skipmid", limits); got == nil || snapshotText(got) != "head|c" || got.MayHaveNewer {
		t.Fatalf("a skipped line before the last step: %+v", got)
	}
}

func TestSubagentSnapshotIsCachedUntilTheTranscriptChanges(t *testing.T) {
	root := t.TempDir()
	path := childFolder(t, root, "c1-0a1b2c3d", completedLine(1, 1, "one"))
	call := webui.SubagentCall{CallID: "c1"}
	first := readSubagentSnapshot(context.Background(), root, call, defaultSubagentSnapshotLimits)
	// Same pointer: the second read neither opened nor parsed the transcript.
	if second := readSubagentSnapshot(context.Background(), root, call, defaultSubagentSnapshotLimits); second != first || first == nil {
		t.Fatalf("second read = %p, first = %p, want the cached snapshot", second, first)
	}
	// A transcript the child appended to is read again, and shows the new step.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(completedLine(1, 2, "two")); err != nil {
		t.Fatal(err)
	}
	file.Close()
	grown := readSubagentSnapshot(context.Background(), root, call, defaultSubagentSnapshotLimits)
	if grown == first || snapshotText(grown) != "one|two" {
		t.Fatalf("after append: %+v", grown)
	}
	// A call without a folder is remembered as absent while the storage directory is
	// unchanged, and looked for again once a folder appears.
	missing := webui.SubagentCall{CallID: "later"}
	if got := readSubagentSnapshot(context.Background(), root, missing, defaultSubagentSnapshotLimits); got != nil {
		t.Fatalf("missing child gave %+v", got)
	}
	if entry, ok := subagentSnapshots.get(root + "\x00later\x00\x00"); !ok || !entry.absent {
		t.Fatalf("absent call not cached: %+v, %v", entry, ok)
	}
	childFolder(t, root, "later-0a1b2c3d", completedLine(1, 1, "now"))
	if got := readSubagentSnapshot(context.Background(), root, missing, defaultSubagentSnapshotLimits); got == nil || snapshotText(got) != "now" {
		t.Fatalf("after the folder appeared: %+v", got)
	}
}

func TestSubagentSnapshotCacheKeepsAtMostItsCapacity(t *testing.T) {
	root := t.TempDir()
	limits := defaultSubagentSnapshotLimits
	limits.cacheEntries = 2
	for _, id := range []string{"a", "b", "c", "d"} {
		childFolder(t, root, id+"-0a1b2c3d", completedLine(1, 1, id))
		readSubagentSnapshot(context.Background(), root, webui.SubagentCall{CallID: id}, limits)
	}
	subagentSnapshots.mu.Lock()
	kept := 0
	for key := range subagentSnapshots.entries {
		if strings.HasPrefix(key, root) {
			kept++
		}
	}
	subagentSnapshots.mu.Unlock()
	if kept > 2 {
		t.Fatalf("cache kept %d entries of this root, want at most 2", kept)
	}
}

// TestHistoryBlockCarriesSubagentSnapshotsOnlyForSubagentCalls: the history route attaches
// the snapshot as the snapshot route does, and a tool that is not the subagent tool gets
// nothing even when its result names a child transcript.
func TestHistoryBlockCarriesSubagentSnapshotsOnlyForSubagentCalls(t *testing.T) {
	root := t.TempDir()
	childFolder(t, root, "sub-0a1b2c3d", completedLine(1, 1, "child said"))
	childFolder(t, root, "msg-0a1b2c3d", completedLine(1, 1, "other profile said"))
	output := func(callID string) string {
		return fmt.Sprintf("{\"id\":%q,\"status\":\"ok\",\"summary\":\"s\"}\n\n--- INFORMATION ---\nagent: explore\ntranscript: %s\n", callID, filepath.Join(root, callID+"-0a1b2c3d", "messages.jsonl"))
	}
	call := func(id, name string) string {
		return fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":%q,"arguments":"{}"}`, id, name)
	}
	result := func(id string) string {
		encoded, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": id, "output": output(id)})
		return string(encoded) + "\n"
	}
	backup := historyUserLine +
		`{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"r1","output":[` + call("sub", "subagent") + `,` + call("msg", "profile_message") + `]}}` + "\n" +
		result("sub") + result("msg")
	config := historyTestConfig(t, backup)
	config.NativeSubagent = &sessionfs.NativeSubagentSettings{StorageDir: root}
	response, err := (&Resolver{}).historyResponse(context.Background(), config, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshots, results := subagentSnapshotOf(t, response.Body)
	if results != 2 || len(snapshots) != 1 || snapshots[0]["agent"] != "explore" || snapshots[0]["steps"] != float64(1) {
		t.Fatalf("results = %d, snapshots = %v\n%s", results, snapshots, response.Body)
	}
	// The payload carries the count, not the child's steps.
	if strings.Contains(string(response.Body), "child said") || strings.Contains(string(response.Body), "other profile said") {
		t.Fatalf("profile_message result carries a child transcript:\n%s", response.Body)
	}
	// Without subagent storage there is nothing to read and nothing fails.
	config.NativeSubagent = nil
	response, err = (&Resolver{}).historyResponse(context.Background(), config, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshots, _ := subagentSnapshotOf(t, response.Body); len(snapshots) != 0 {
		t.Fatalf("snapshots without subagent storage: %v", snapshots)
	}
}

// TestReadSubagentStepsSaysWhetherTheChildWasFound: the subagent route needs to tell a child
// that has no completed step yet (found, 200 with no steps) from a call id with no child (404).
func TestReadSubagentStepsSaysWhetherTheChildWasFound(t *testing.T) {
	root := t.TempDir()
	childFolder(t, root, "young-0a1b2c3d", childUserLine)
	call := func(id string) webui.SubagentCall { return webui.SubagentCall{CallID: id} }
	if snapshot, found, _ := readSubagentSteps(context.Background(), root, call("young"), defaultSubagentSnapshotLimits); !found || snapshot != nil {
		t.Fatalf("child with no step: %+v, found %v", snapshot, found)
	}
	if snapshot, found, _ := readSubagentSteps(context.Background(), root, call("nobody"), defaultSubagentSnapshotLimits); found || snapshot != nil {
		t.Fatalf("unknown call: %+v, found %v", snapshot, found)
	}
	// Still running: two steps written, the third line torn. The steps so far are served,
	// and a read after the child wrote more sees them (the cache follows the file).
	path := childFolder(t, root, "busy-0a1b2c3d", childUserLine+completedLine(1, 1, "a")+completedLine(1, 2, "b")+`{"type":"response.compl`)
	snapshot, found, _ := readSubagentSteps(context.Background(), root, call("busy"), defaultSubagentSnapshotLimits)
	if !found || snapshot == nil || len(snapshot.Steps) != 2 || snapshot.Agent != "" {
		t.Fatalf("running child: %+v, found %v", snapshot, found)
	}
	if err := os.WriteFile(path, []byte(childUserLine+completedLine(1, 1, "a")+completedLine(1, 2, "b")+completedLine(1, 3, "c")), 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot, _, _ := readSubagentSteps(context.Background(), root, call("busy"), defaultSubagentSnapshotLimits); snapshot == nil || len(snapshot.Steps) != 3 {
		t.Fatalf("after the child wrote more: %+v", snapshot)
	}
}

// TestReadSubagentStepsTellsAnUnreadableChildFromAMissingOne: a child whose transcript is
// there but cannot be read is an error, not "no child" (the route must not answer 404),
// and the failure is not cached: once readable, the steps are served.
func TestReadSubagentStepsTellsAnUnreadableChildFromAMissingOne(t *testing.T) {
	root := t.TempDir()
	path := childFolder(t, root, "locked-0a1b2c3d", childUserLine+completedLine(1, 1, "a"))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("a mode 0 file is still readable here (running as root?)")
	}
	call := webui.SubagentCall{CallID: "locked"}
	if snapshot, found, err := readSubagentSteps(context.Background(), root, call, defaultSubagentSnapshotLimits); err == nil || !found || snapshot != nil {
		t.Fatalf("unreadable child: %+v, found %v, err %v; want an error", snapshot, found, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := readSubagentSteps(context.Background(), root, call, defaultSubagentSnapshotLimits)
	if err != nil || !found || snapshot == nil || snapshotText(snapshot) != "a" {
		t.Fatalf("readable again: %+v, found %v, err %v", snapshot, found, err)
	}
}

// TestChildFolderWithoutItsTranscriptYetIsNotRememberedAsAbsent: a running child creates its
// folder before its messages.jsonl, and creating the file does not touch the storage
// directory's modification time, so a miss in between must not be cached.
func TestChildFolderWithoutItsTranscriptYetIsNotRememberedAsAbsent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "early-0a1b2c3d"), 0o755); err != nil {
		t.Fatal(err)
	}
	call := webui.SubagentCall{CallID: "early"}
	if snapshot, found, _ := readSubagentSteps(context.Background(), root, call, defaultSubagentSnapshotLimits); !found || snapshot != nil {
		t.Fatalf("folder without transcript: %+v, found %v", snapshot, found)
	}
	childFolder(t, root, "early-0a1b2c3d", childUserLine+completedLine(1, 1, "now"))
	snapshot, found, _ := readSubagentSteps(context.Background(), root, call, defaultSubagentSnapshotLimits)
	if !found || snapshot == nil || snapshotText(snapshot) != "now" {
		t.Fatalf("after the transcript appeared: %+v, found %v", snapshot, found)
	}
}
