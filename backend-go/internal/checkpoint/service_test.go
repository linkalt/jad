package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func defaultSettings() sessionfs.CheckpointSettings {
	return sessionfs.CheckpointSettings{
		Enabled: true, GitBinary: "git", MaxCheckpoints: 20, MaxFiles: 20_000, MaxTotalBytes: 512 * 1024 * 1024,
		TimeoutMS: 120_000, IncludeMCPTools: true, SnapshotOn: "first_mutation", SafeRestore: true,
	}
}

type fixture struct {
	workspace string
	staging   string
	service   *Service
}

func newFixture(t *testing.T, change func(*sessionfs.CheckpointSettings)) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	staging := filepath.Join(root, "staging")
	for _, dir := range []string{workspace, staging} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settings := defaultSettings()
	if change != nil {
		change(&settings)
	}
	service := NewService(Options{
		Settings: settings, Workspace: workspace, StagingDir: staging,
		HarnessPaths: []string{filepath.Join(workspace, ".tasks")}, Durability: "process",
	})
	return &fixture{workspace: workspace, staging: staging, service: service}
}

func (f *fixture) path(name string) string {
	return filepath.Join(f.workspace, filepath.FromSlash(name))
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(f.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) exists(name string) bool {
	_, err := os.Lstat(f.path(name))
	return err == nil
}

func (f *fixture) seed(t *testing.T) {
	t.Helper()
	f.write(t, "a.txt", "a0\n")
	f.write(t, "d/b.txt", "b0\n")
	f.write(t, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(f.path("run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", f.path("link")); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) begin(t *testing.T, turn string, number int64) string {
	t.Helper()
	id := f.service.Begin(context.Background(), turn, number)
	if id == "" {
		t.Fatalf("Begin(%s) took no checkpoint: %+v", turn, f.service.List())
	}
	return id
}

func wantError(t *testing.T, err error, code string) *Error {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("error = %v, want a %s refusal", err, code)
	}
	return refusal
}

func TestSafeRestoreRevertsEditsDeletionsNewFilesModesAndSymlinks(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "a1\n")
	_ = os.Remove(f.path("d/b.txt"))
	f.write(t, "new.txt", "new\n")
	f.write(t, "made/deep/x.txt", "x\n")
	_ = os.Chmod(f.path("run.sh"), 0o644)
	_ = os.Remove(f.path("link"))
	_ = os.Symlink("d", f.path("link"))
	f.service.Finish(id)

	result, err := f.service.Restore(context.Background(), id, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "safe" || len(result.Conflicts) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if f.read(t, "a.txt") != "a0\n" || f.read(t, "d/b.txt") != "b0\n" {
		t.Fatal("edited or deleted files were not restored")
	}
	if f.exists("new.txt") || f.exists("made/deep/x.txt") {
		t.Fatal("files the run created were not removed")
	}
	if info, _ := os.Stat(f.path("run.sh")); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
	if target, _ := os.Readlink(f.path("link")); target != "a.txt" {
		t.Fatalf("symlink target = %q, want a.txt", target)
	}
	if result.UndoCheckpointID == "" {
		t.Fatal("no undo checkpoint")
	}
}

func TestDryRunListsPathsChangesNothingAndWritesNoCheckpoint(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "a1\n")
	f.write(t, "new.txt", "new\n")
	f.service.Finish(id)
	listed := len(f.service.List())

	result, err := f.service.Restore(context.Background(), id, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Restored, []string{"a.txt"}) || !reflect.DeepEqual(result.Deleted, []string{"new.txt"}) || !result.DryRun {
		t.Fatalf("result = %+v", result)
	}
	if result.UndoCheckpointID != "" || f.read(t, "a.txt") != "a1\n" || !f.exists("new.txt") || len(f.service.List()) != listed {
		t.Fatal("a dry run changed something")
	}
}

func TestSafeRestoreLeavesAFileTheUserChangedAfterTheRun(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "agent\n")
	f.write(t, "d/b.txt", "agent-b\n")
	f.service.Finish(id)
	f.write(t, "a.txt", "user edit after the run\n")

	result, err := f.service.Restore(context.Background(), id, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Conflicts, []string{"a.txt"}) || !reflect.DeepEqual(result.Restored, []string{"d/b.txt"}) {
		t.Fatalf("result = %+v", result)
	}
	if f.read(t, "a.txt") != "user edit after the run\n" || f.read(t, "d/b.txt") != "b0\n" {
		t.Fatal("safe restore touched the user's edit or missed the run's")
	}
}

func TestFullRestoreMakesTheWorkspaceEqualToBeforeTheRun(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "agent\n")
	f.service.Finish(id)
	f.write(t, "a.txt", "user\n")
	f.write(t, "later.txt", "later\n")

	result, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "full" || f.read(t, "a.txt") != "a0\n" || f.exists("later.txt") {
		t.Fatalf("full restore left the workspace different: %+v", result)
	}
}

func TestARestoreIsUndoableThroughTheCheckpointItRecords(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "agent\n")
	f.write(t, "new.txt", "new\n")
	f.service.Finish(id)

	restored, err := f.service.Restore(context.Background(), id, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.read(t, "a.txt") != "a0\n" || restored.UndoCheckpointID == "" {
		t.Fatalf("restore = %+v", restored)
	}
	var undo *Record
	for _, record := range f.service.List() {
		if record.ID == restored.UndoCheckpointID {
			undo = &record
		}
	}
	if undo == nil || undo.Kind != KindPreRestore || undo.RestoredFrom != id {
		t.Fatalf("undo record = %+v", undo)
	}
	if _, err := f.service.Restore(context.Background(), restored.UndoCheckpointID, RestoreOptions{Mode: ModeFull}); err != nil {
		t.Fatal(err)
	}
	if f.read(t, "a.txt") != "agent\n" || f.read(t, "new.txt") != "new\n" {
		t.Fatal("undoing the restore did not bring the run's changes back")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func TestTheUsersOwnGitRepositoryIsUntouched(t *testing.T) {
	f := newFixture(t, nil)
	gitIn(t, f.workspace, "init", "-q")
	f.seed(t)
	gitIn(t, f.workspace, "add", "-A")
	gitIn(t, f.workspace, "-c", "user.email=u@e", "-c", "user.name=u", "commit", "-qm", "init")
	f.write(t, "a.txt", "dirty\n")
	f.write(t, "untracked.txt", "u\n")
	fingerprint := func() string {
		return strings.Join([]string{
			gitIn(t, f.workspace, "status", "--short"), gitIn(t, f.workspace, "stash", "list"),
			gitIn(t, f.workspace, "write-tree"), gitIn(t, f.workspace, "rev-parse", "HEAD"), gitIn(t, f.workspace, "for-each-ref"),
		}, "|")
	}
	before := fingerprint()

	id := f.begin(t, "turn-1", 1)
	f.write(t, "b2.txt", "n\n")
	f.service.Finish(id)
	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.exists("b2.txt") {
		t.Fatal("the run's new file was not removed")
	}
	if after := fingerprint(); after != before {
		t.Fatalf("the user's repository changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestIgnoredFilesHarnessDirectoriesAndBackupsAreNeitherSnapshottedNorRestored(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	f.write(t, ".gitignore", "build/\n")
	f.write(t, "build/out.js", "o0\n")
	f.write(t, ".tasks/t.json", "t0\n")
	id := f.begin(t, "turn-1", 1)
	f.write(t, "build/out.js", "o1\n")
	f.write(t, ".tasks/t.json", "t1\n")
	f.write(t, "a.txt.bak-1791259056390-8d53b3fdbbc761e8", "old\n")
	f.write(t, "a.txt", "a1\n")
	f.service.Finish(id)

	result, err := f.service.Restore(context.Background(), id, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Restored, []string{"a.txt"}) || len(result.Deleted) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if f.read(t, "build/out.js") != "o1\n" || f.read(t, ".tasks/t.json") != "t1\n" || !f.exists("a.txt.bak-1791259056390-8d53b3fdbbc761e8") {
		t.Fatal("an excluded file was rolled back")
	}
}

func TestExclusionPatternsKeepOnlyHarnessPathsInsideTheWorkspaceAndEscapeGlobs(t *testing.T) {
	patterns := ExclusionPatterns("/w/ws", []string{"/w/ws/.tasks", "/w/ws/a[1]/b", "/elsewhere/x", "/w/ws"})
	if !slices.Contains(patterns, "/.tasks") || !slices.Contains(patterns, `/a\[1\]/b`) {
		t.Fatalf("patterns = %v", patterns)
	}
	for _, pattern := range patterns {
		if strings.Contains(pattern, "elsewhere") || pattern == "/" {
			t.Fatalf("pattern %q should not be there: %v", pattern, patterns)
		}
	}
}

func TestAWorkspaceOverMaxFilesIsSkippedWithAWarning(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxFiles = 2 })
	f.seed(t)
	if id := f.service.Begin(context.Background(), "turn-1", 1); id != "" {
		t.Fatalf("Begin returned %q for a skipped snapshot", id)
	}
	records := f.service.List()
	if len(records) != 1 || records[0].State != StateSkipped || !strings.Contains(records[0].Warning, "max_files") {
		t.Fatalf("records = %+v", records)
	}
	_, err := f.service.Restore(context.Background(), records[0].ID, RestoreOptions{DryRun: true})
	wantError(t, err, CodeConflict)
}

func TestAMissingGitBinaryIsRecordedAsFailedAndNeverBlocks(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) { s.GitBinary = "/nonexistent/git" })
	f.seed(t)
	if id := f.service.Begin(context.Background(), "turn-1", 1); id != "" {
		t.Fatalf("Begin returned %q without git", id)
	}
	records := f.service.List()
	if len(records) != 1 || records[0].State != StateFailed || !strings.Contains(records[0].Warning, "cannot run") {
		t.Fatalf("records = %+v", records)
	}
}

func TestAFilesystemRootOrTheHomeDirectoryIsRefusedAsAWorkspace(t *testing.T) {
	for _, workspace := range []string{"/", os.Getenv("HOME")} {
		if workspace == "" {
			continue
		}
		staging := t.TempDir()
		service := NewService(Options{Settings: defaultSettings(), Workspace: workspace, StagingDir: staging, Durability: "process"})
		if id := service.Begin(context.Background(), "turn-1", 1); id != "" {
			t.Fatalf("Begin(%s) returned %q", workspace, id)
		}
		records := service.List()
		if len(records) != 1 || records[0].State != StateSkipped || !strings.Contains(records[0].Warning, "filesystem root or the home directory") {
			t.Fatalf("workspace %s: records = %+v", workspace, records)
		}
	}
}

func TestOnlyMaxCheckpointsRecordsAreKeptOldestPrunedFirst(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) { s.MaxCheckpoints = 3 })
	f.seed(t)
	var ids []string
	for turn := int64(1); turn <= 5; turn++ {
		f.write(t, "a.txt", "a"+string(rune('0'+turn))+"\n")
		id := f.begin(t, "turn", turn)
		f.service.Finish(id)
		ids = append(ids, id)
	}
	var listed []string
	for _, record := range f.service.List() {
		listed = append(listed, record.ID)
	}
	if !reflect.DeepEqual(listed, []string{ids[4], ids[3], ids[2]}) {
		t.Fatalf("listed = %v, ids = %v", listed, ids)
	}
	_, err := f.service.Restore(context.Background(), ids[0], RestoreOptions{DryRun: true})
	wantError(t, err, CodeNotFound)
}

func TestSafeRestoreNeedsTheRunEndSnapshotButFullRestoreDoesNot(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "agent\n")
	// The run never finished (for example the server stopped): there is no after_tree.
	refusal := wantErrorFrom(t, f.service, id)
	if !strings.Contains(refusal.Message, "run-end snapshot") {
		t.Fatalf("message = %q", refusal.Message)
	}
	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull}); err != nil {
		t.Fatal(err)
	}
	if f.read(t, "a.txt") != "a0\n" {
		t.Fatal("full restore did not restore the file")
	}
}

func wantErrorFrom(t *testing.T, service *Service, id string) *Error {
	t.Helper()
	_, err := service.Restore(context.Background(), id, RestoreOptions{})
	return wantError(t, err, CodeConflict)
}

func TestRestoringACheckpointThatDoesNotExistIsNotFound(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.service.Restore(context.Background(), "ckpt-nope", RestoreOptions{DryRun: true})
	wantError(t, err, CodeNotFound)
}

func TestADamagedCheckpointsFileReadsAsAnEmptyList(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.WriteFile(filepath.Join(f.staging, MetadataFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if records := f.service.List(); len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}
}

func TestCallChangesWorkspace(t *testing.T) {
	call := func(name string, args any) json.RawMessage {
		encoded, _ := json.Marshal(args)
		raw, _ := json.Marshal(map[string]string{"name": name, "arguments": string(encoded)})
		return raw
	}
	cases := []struct {
		name   string
		call   json.RawMessage
		source string
		mcp    bool
		want   bool
	}{
		{"write", call("write", map[string]any{}), "native", true, true},
		{"edit", call("edit", map[string]any{}), "native", true, true},
		{"bash", call("bash", map[string]any{}), "native", true, true},
		{"exec runs a program that can change files", call("exec", map[string]any{"lang": "bash", "code": "x"}), "native", true, true},
		{"process start", call("process", map[string]any{"command": "x"}), "native", true, true},
		{"process without a command", call("process", map[string]any{"block_ms": 1}), "native", true, false},
		{"process with a blank command starts nothing", call("process", map[string]any{"command": "  "}), "native", true, false},
		{"read", call("read", map[string]any{}), "native", true, false},
		{"grep", call("grep", map[string]any{}), "native", true, false},
		{"notebook edit", call("notebook", map[string]any{"action": "edit", "path": "a.ipynb"}), "native", true, true},
		{"notebook insert", call("notebook", map[string]any{"action": "insert", "path": "a.ipynb"}), "native", true, true},
		{"notebook delete", call("notebook", map[string]any{"action": "delete", "path": "a.ipynb"}), "native", true, true},
		{"notebook read", call("notebook", map[string]any{"action": "read", "path": "a.ipynb"}), "native", true, false},
		{"notebook with bad arguments", json.RawMessage(`{"name":"notebook","arguments":"{bad"}`), "native", true, false},
		{"mcp counted", call("srv_tool", map[string]any{}), "mcp", true, true},
		{"mcp not counted", call("srv_tool", map[string]any{}), "mcp", false, false},
		{"process with bad arguments", json.RawMessage(`{"name":"process","arguments":"{bad"}`), "native", true, false},
		{"not JSON", json.RawMessage(`nope`), "native", true, false},
	}
	for _, tc := range cases {
		if got := CallChangesWorkspace(tc.call, tc.source, tc.mcp); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestARunTakesOneCheckpointAtItsFirstChangingCallNoneForAReadOnlyRun(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	readOnly := NewRun(f.service, "turn-1", 1)
	readOnly.Start(context.Background())
	readOnly.BeforeCalls(context.Background(), false)
	readOnly.Finish()
	if n := len(f.service.List()); n != 0 {
		t.Fatalf("a read-only run recorded %d checkpoints", n)
	}

	changing := NewRun(f.service, "turn-2", 2)
	changing.Start(context.Background())
	changing.BeforeCalls(context.Background(), false)
	if n := len(f.service.List()); n != 0 {
		t.Fatalf("a run recorded %d checkpoints before any changing call", n)
	}
	changing.BeforeCalls(context.Background(), true)
	changing.BeforeCalls(context.Background(), true)
	changing.Finish()
	records := f.service.List()
	if len(records) != 1 || records[0].State != StateComplete {
		t.Fatalf("records = %+v", records)
	}
}

func TestRunStartTakesTheCheckpointBeforeAnyCallEvenARead(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) { s.SnapshotOn = "run_start" })
	f.seed(t)
	run := NewRun(f.service, "turn-1", 1)
	run.Start(context.Background())
	if n := len(f.service.List()); n != 1 {
		t.Fatalf("records after Start = %d, want 1", n)
	}
	run.BeforeCalls(context.Background(), true)
	run.Finish()
	records := f.service.List()
	if len(records) != 1 || records[0].State != StateComplete {
		t.Fatalf("records = %+v", records)
	}
}

func TestNewRunIsNilWhenCheckpointsAreOffAndItsMethodsAreSafe(t *testing.T) {
	f := newFixture(t, func(s *sessionfs.CheckpointSettings) { s.Enabled = false })
	f.seed(t)
	run := NewRun(f.service, "turn-1", 1)
	if run != nil {
		t.Fatal("NewRun returned a run while checkpoints are off")
	}
	run.Start(context.Background())
	run.BeforeCalls(context.Background(), true)
	run.Finish()
	if run.IncludeMCPTools() {
		t.Fatal("a nil run reports MCP tools as changes")
	}
	if entries, _ := os.ReadDir(f.staging); len(entries) != 0 {
		t.Fatalf("an off checkpoint created %d files", len(entries))
	}
}

func TestReportShowsARecordLeftOpenAsInterruptedOnlyWhileNoRunIsLive(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "a1\n")
	stored, err := os.ReadFile(filepath.Join(f.staging, MetadataFile))
	if err != nil {
		t.Fatal(err)
	}

	live := f.service.Report(true)
	if len(live) != 1 || live[0].State != StateOpen || live[0].Warning != "" {
		t.Fatalf("while a run is live = %+v, want the open record untouched", live)
	}
	gone := f.service.Report(false)
	if len(gone) != 1 || gone[0].State != StateInterrupted || gone[0].Warning != InterruptedWarning || gone[0].BeforeTree == "" {
		t.Fatalf("with no run live = %+v, want interrupted with its saved state kept", gone)
	}
	if f.service.List()[0].State != StateOpen {
		t.Fatal("List must report the stored state")
	}
	after, err := os.ReadFile(filepath.Join(f.staging, MetadataFile))
	if err != nil || string(after) != string(stored) {
		t.Fatalf("reporting wrote the record back: err=%v", err)
	}

	f.service.Finish(id)
	if finished := f.service.Report(false); finished[0].State != StateComplete || finished[0].Warning != "" {
		t.Fatalf("a finished record = %+v, want it untouched", finished[0])
	}
}

// swapDirForSymlink makes a restore fail after its checkout: the run replaces the
// directory d with a symlink, so the restore writes d/b.txt back, and adds a file
// in a read-only directory, which the restore then cannot remove.
func swapDirForSymlink(t *testing.T, f *fixture) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	if err := os.RemoveAll(f.path("d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", f.path("d")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "locked/new.txt", "n\n")
	if err := os.Chmod(f.path("locked"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.path("locked"), 0o755) })
	f.service.Finish(id)
	return id
}

func undoRecords(f *fixture) []Record {
	var undos []Record
	for _, record := range f.service.List() {
		if record.Kind == KindPreRestore {
			undos = append(undos, record)
		}
	}
	return undos
}

func TestARestoreThatFailsMidWayLeavesAnUndoRecordThatRestoresThePreRestoreState(t *testing.T) {
	for _, mode := range []string{ModeSafe, ModeFull} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			id := swapDirForSymlink(t, f)

			if _, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: mode}); err == nil {
				t.Fatal("the restore succeeded, want the removal in the read-only directory to fail")
			}
			if err := os.Chmod(f.path("locked"), 0o755); err != nil {
				t.Fatal(err)
			}
			undos := undoRecords(f)
			if len(undos) != 1 || undos[0].State != StateFailed || undos[0].BeforeTree == "" || undos[0].RestoredFrom != id || undos[0].Warning == "" {
				t.Fatalf("undo records = %+v, want one failed pre_restore record with its state", undos)
			}
			if !f.exists("d/b.txt") {
				t.Fatal("the restore did not write anything before failing; the test no longer exercises a half-restored tree")
			}

			if _, err := f.service.Restore(context.Background(), undos[0].ID, RestoreOptions{Mode: ModeFull}); err != nil {
				t.Fatal(err)
			}
			if target, err := os.Readlink(f.path("d")); err != nil || target != "a.txt" {
				t.Fatalf("d after the undo = %q, %v; want the symlink the workspace had before the restore", target, err)
			}
		})
	}
}

func TestTheUndoRecordIsStoredOpenBeforeTheFirstWorkspaceWrite(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	snapshot := filepath.Join(t.TempDir(), "metadata-at-first-write.json")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper.sh")
	f := newFixture(t, func(settings *sessionfs.CheckpointSettings) { settings.GitBinary = wrapper })
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = checkout-index ]; then cp '" + filepath.Join(f.staging, MetadataFile) + "' '" + snapshot + "'; fi\ndone\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.seed(t)
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "a1\n")
	f.service.Finish(id)

	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("checkout-index never ran: %v", err)
	}
	var stored struct{ Checkpoints []Record }
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	var undo *Record
	for position := range stored.Checkpoints {
		if stored.Checkpoints[position].Kind == KindPreRestore {
			undo = &stored.Checkpoints[position]
		}
	}
	if undo == nil || undo.State != StateOpen || undo.BeforeTree == "" || undo.RestoredFrom != id {
		t.Fatalf("pre_restore record at the first write = %+v, want it stored open with its state", undo)
	}
}

func TestARestoreWhoseCheckoutOrSecondCaptureFailsLeavesAFailedUndoRecord(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for name, script := range map[string]string{
		"checkout":       `case " $* " in *" checkout-index "*) echo boom >&2; exit 1;; esac`,
		"second capture": `case " $* " in *" checkout-index "*) touch "$MARK";; *" add "*) [ -e "$MARK" ] && { echo boom >&2; exit 1; };; esac`,
	} {
		t.Run(name, func(t *testing.T) {
			wrapper := filepath.Join(t.TempDir(), "git-wrapper.sh")
			t.Setenv("MARK", filepath.Join(t.TempDir(), "mark"))
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"+script+"\nexec "+realGit+" \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			f := newFixture(t, func(settings *sessionfs.CheckpointSettings) { settings.GitBinary = wrapper })
			f.seed(t)
			id := f.begin(t, "turn-1", 1)
			f.write(t, "a.txt", "a1\n")
			f.service.Finish(id)

			if _, err := f.service.Restore(context.Background(), id, RestoreOptions{}); err == nil {
				t.Fatal("the restore succeeded, want the git failure")
			}
			undos := undoRecords(f)
			if len(undos) != 1 || undos[0].State != StateFailed || undos[0].BeforeTree == "" || undos[0].Warning == "" {
				t.Fatalf("undo records = %+v, want one failed pre_restore record", undos)
			}
		})
	}
}

func TestRestoreIsByteExactDespiteWorkspaceGitAttributes(t *testing.T) {
	f := newFixture(t, nil)
	files := map[string]string{
		".gitattributes": "* text eol=crlf\n*.id ident\n*.flt filter=upper\n*.bin -text\n",
		"lf.txt":         "line1\nline2\n",
		"crlf.txt":       "line1\r\nline2\r\n",
		"ident.id":       "$Id: keep-me $\n$Id$\n",
		"f.flt":          "lower\n",
		"d.bin":          "a\r\nb\n",
	}
	for name, content := range files {
		f.write(t, name, content)
	}
	id := f.begin(t, "turn-1", 1)
	for name := range files {
		if name != ".gitattributes" {
			f.write(t, name, "changed\n")
		}
	}
	f.service.Finish(id)
	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		if got := f.read(t, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
