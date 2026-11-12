package sessionfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionlock"
)

// Every test here works in a temporary base (migrateFixture): W, G and $HOME are folders of
// t.TempDir(), and the environment is explicit, so nothing outside it is read or written.

func (f *migrateFixture) options(root string) MigrateOptions {
	return MigrateOptions{WorkspaceDir: f.workspace, Root: root, Environment: map[string]string{"HOME": f.home}, LibraryDefaults: map[string][]string{"skill_dirs": {".agent/skills"}}}
}

func (f *migrateFixture) apply(t *testing.T) (MigrationApplyResult, error) {
	t.Helper()
	result, err := ApplyWorkspaceMigration(context.Background(), f.options(""))
	return result, err
}

// planner runs the planner the way the applier does, for tests that change something between
// the plan and the writes.
func (f *migrateFixture) planner(t *testing.T) *migrationPlanner {
	t.Helper()
	p, err := newMigrationPlanner(f.options(""))
	if err != nil {
		t.Fatal(err)
	}
	p.run()
	if p.plan.Blocked() {
		t.Fatalf("blocked: %+v", p.plan.Blockers)
	}
	return p
}

// hashes maps every regular file below dir (lock files left out) to its hash.
func hashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && !strings.HasSuffix(path, ".jad-lock") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[path] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unchanged fails when a file of before is missing from after or differs.
func unchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("a source file changed or was removed: %s", path)
		}
	}
}

func lastJournalOps(t *testing.T, path string) []string {
	t.Helper()
	entries, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	ops := make([]string, 0, len(entries))
	for _, entry := range entries {
		ops = append(ops, entry.Op)
	}
	return ops
}

func TestApplyMigratesTheRepositoryShapedSettingAndLoadsAsV2(t *testing.T) {
	f := newMigrateFixture(t)
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	after := hashes(t, f.base)
	unchanged(t, before, after)

	// The additions to G are byte for byte the sources.
	want, _ := os.ReadFile(filepath.Join(f.jad, "mcp/web/search.json"))
	got, err := os.ReadFile(filepath.Join(f.global, "mcp/web/search.json"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("G copy: %v", err)
	}
	// The settings files are exactly the planned bytes, private.
	for _, name := range []string{filepath.Join(f.jad, "workspace.json"), filepath.Join(f.global, "global.json")} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, err, info)
		}
	}
	plan := result.Plan
	for _, setting := range plan.Settings {
		data, _ := os.ReadFile(setting.Path)
		if !bytes.Equal(data, setting.Content) {
			t.Fatalf("%s is not the planned content", setting.Path)
		}
	}
	// What was created is exactly workspace.json, global.json and the MCP file, in that order
	// of being needed: workspace.json is last.
	var created []string
	for _, item := range result.Created {
		created = append(created, item.Path)
	}
	if len(created) != 3 || created[len(created)-1] != filepath.Join(f.jad, "workspace.json") {
		t.Fatalf("created: %v", created)
	}
	// It starts in the workspace layout now, and the sessions were compared.
	config, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: map[string]string{"HOME": f.home}})
	if err != nil || len(config.Workspaces) == 0 {
		t.Fatalf("v2 load: %v %+v", err, config.Workspaces)
	}
	if result.Compared < 1 {
		t.Fatalf("compared %d", result.Compared)
	}
	ops := lastJournalOps(t, result.Journal)
	if ops[0] != journalStart || ops[len(ops)-1] != journalComplete {
		t.Fatalf("journal: %v", ops)
	}
	// The v1 setting is still there, untouched.
	if _, err := os.Stat(filepath.Join(f.jad, "setting.json")); err != nil {
		t.Fatal(err)
	}
}

func TestApplyOfABlockedPlanWritesNothing(t *testing.T) {
	f := newMigrateFixture(t)
	f.write(t, filepath.Join(f.global, "mcp/web/search.json"), `{"schema_version":1}`) // other content in G
	before := snapshotTree(t, f.base)
	result, err := f.apply(t)
	var blocked *MigrationBlockedError
	if !errors.As(err, &blocked) || blocked.Blockers == 0 || result.Journal != "" {
		t.Fatalf("err %v result %+v", err, result)
	}
	after := snapshotTree(t, f.base)
	if len(after) != len(before) {
		t.Fatalf("the tree changed: %d entries before, %d after", len(before), len(after))
	}
	for path, record := range before {
		if after[path] != record {
			t.Fatalf("%s changed", path)
		}
	}
}

// stateCopyFixture is a setting with its state outside W/.jad, which the applier copies.
func stateCopyFixture(t *testing.T) (*migrateFixture, string) {
	f := newMigrateFixture(t)
	v1 := otherFolders(t, f)
	// No exec tool: its functions_dir is under the cwd, outside the copied state, which the plan blocks.
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), map[string]any{"native": map[string]any{"async": map[string]any{}}, "mcp": map[string]any{"web/search": map[string]any{}}})
	f.write(t, filepath.Join(v1, "state/work/dev/coder/messages.jsonl"), "{\"a\":1}\n{\"b\":2}\n")
	f.write(t, filepath.Join(v1, "state/work/dev/coder/staging/usage-cache.json"), "{}")
	f.write(t, filepath.Join(v1, "state/.mcp-oauth/web/search.json"), `{"token":"fixture-oauth-value"}`)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(v1, "state/work/dev/coder/messages.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	return f, v1
}

// acceptSharedToolStorage makes the v1 view agree with what v2 prints for an absolute
// tool_storage_dir into the v1 state, so a test of the copy itself passes the comparison; the
// comparison catching that sharing is TestApplyRollsBackWhenV2WritesWhereV1Writes.
func acceptSharedToolStorage(f *migrateFixture, v1 string) func(*migrationApplier) {
	return func(a *migrationApplier) {
		for _, print := range a.v1 {
			for key, value := range print {
				if strings.HasPrefix(key, "write ") && strings.HasPrefix(value, "{state}/") && strings.HasSuffix(value, "/tools") || key == "write tools.native.task.storage_dir" || key == "write tools.native.records.storage_dir" || key == "write tools.native.code_context.storage_dir" || key == "write tools.native.subagent.storage_dir" {
					print[key] = filepath.Join(v1, "state", "work", strings.TrimPrefix(strings.Split(value, ",")[0], "{state}/"))
				}
			}
		}
	}
}

func TestApplyCopiesStateByteForByteAndLeavesCredentialsBehind(t *testing.T) {
	f, v1 := stateCopyFixture(t)
	p := f.planner(t)
	before := hashes(t, f.base)
	result, err := applyPlanned(context.Background(), p, acceptSharedToolStorage(f, v1))
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	source := filepath.Join(v1, "state/work/dev/coder/messages.jsonl")
	dest := filepath.Join(f.jad, "state/work/dev/coder/messages.jsonl")
	a, _ := os.ReadFile(source)
	b, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("transcript copy: %v", err)
	}
	si, _ := os.Stat(source)
	di, _ := os.Stat(dest)
	if !si.ModTime().Equal(di.ModTime()) || si.Mode().Perm() != di.Mode().Perm() {
		t.Fatalf("mtime/mode: %v %v / %v %v", si.ModTime(), si.Mode(), di.ModTime(), di.Mode())
	}
	if _, err := os.Stat(filepath.Join(f.jad, "state/.mcp-oauth")); err == nil {
		t.Fatal("MCP sign-in tokens must not be copied")
	}
	// No temporary file is left behind.
	_ = filepath.WalkDir(f.base, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && strings.Contains(entry.Name(), ".migrate-") {
			t.Fatalf("temporary file left: %s", path)
		}
		return nil
	})
}

func TestApplyRollsBackWhenV2WritesWhereV1Writes(t *testing.T) {
	f, v1 := stateCopyFixture(t) // tool_storage_dir is an absolute folder in the v1 state
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err == nil || result.Rollback == nil || !strings.Contains(err.Error(), "does not load the same as v1") {
		t.Fatalf("want a comparison failure with a rollback: %v %+v", err, result.Rollback)
	}
	if len(result.Rollback.Kept) != 0 || len(result.Rollback.Removed) == 0 {
		t.Fatalf("rollback: %+v", result.Rollback)
	}
	// Everything the run created is gone; the sources are as they were; v1 starts again.
	after := hashes(t, f.base)
	unchanged(t, before, after)
	for path := range after {
		if _, existed := before[path]; !existed && !strings.Contains(path, "/"+journalFolderName+"/") {
			t.Fatalf("a created file is still there: %s", path)
		}
	}
	for _, dir := range []string{filepath.Join(f.jad, "state"), filepath.Join(f.global, "models")} {
		if _, err := os.Stat(dir); err == nil && dir != filepath.Join(f.jad, "state") {
			t.Fatalf("a created folder is still there: %s", dir)
		}
	}
	if _, err := os.Stat(filepath.Join(v1, "state/work/dev/coder/messages.jsonl")); err != nil {
		t.Fatal(err)
	}
	config, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: map[string]string{"HOME": f.home}})
	if err != nil || len(config.Workspaces) != 0 {
		t.Fatalf("after the rollback the folder is v1 again: %v %+v", err, config.Workspaces)
	}
	ops := lastJournalOps(t, result.Journal)
	if ops[len(ops)-1] != journalRolled {
		t.Fatalf("journal: %v", ops)
	}
}

func TestApplyRollsBackWhenAnotherFileAppearsAndNeverReplacesIt(t *testing.T) {
	f, v1 := stateCopyFixture(t)
	p := f.planner(t)
	// A file appears at a later destination after the plan (a state file, copied after G's).
	late := filepath.Join(f.jad, "state/work/dev/coder/messages.jsonl")
	f.write(t, late, "someone else's transcript\n")
	before := hashes(t, f.base)
	result, err := applyPlanned(context.Background(), p, acceptSharedToolStorage(f, v1))
	if err == nil || !strings.Contains(err.Error(), "never replaces") || result.Rollback == nil {
		t.Fatalf("err %v rollback %+v", err, result.Rollback)
	}
	data, _ := os.ReadFile(late)
	if string(data) != "someone else's transcript\n" {
		t.Fatalf("the file that appeared was replaced: %q", data)
	}
	after := hashes(t, f.base)
	unchanged(t, before, after)
	for path := range after {
		if _, existed := before[path]; !existed && !strings.Contains(path, "/"+journalFolderName+"/") {
			t.Fatalf("created file not rolled back: %s", path)
		}
	}
	if len(result.Rollback.Removed) == 0 {
		t.Fatalf("rollback removed nothing: %+v", result.Rollback)
	}
}

func TestApplyStopsOnASourceThatChangedAfterThePlan(t *testing.T) {
	f, v1 := stateCopyFixture(t)
	p := f.planner(t)
	f.write(t, filepath.Join(v1, "state/work/dev/coder/messages.jsonl"), "appended after the plan\n")
	result, err := applyPlanned(context.Background(), p, acceptSharedToolStorage(f, v1))
	if err == nil || !strings.Contains(err.Error(), "changed since the plan") || result.Rollback == nil {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.jad, "state/work")); err == nil {
		t.Fatal("a changed transcript was copied")
	}
	if _, err := os.Stat(filepath.Join(f.jad, "workspace.json")); err == nil {
		t.Fatal("the switch was written")
	}
}

func TestApplyStopsWhenASessionIsInUseAndWritesNothing(t *testing.T) {
	f, v1 := stateCopyFixture(t)
	p := f.planner(t)
	lockPath, err := sessionlock.PathFor(filepath.Join(v1, "state/work/dev/coder/messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := sessionlock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before := hashes(t, f.base)
	treeBefore := withoutLocks(snapshotTree(t, f.base))
	result, err := applyPlanned(context.Background(), p, nil)
	if err == nil || !strings.Contains(err.Error(), "is in use") || result.Journal != "" {
		t.Fatalf("err %v journal %q", err, result.Journal)
	}
	unchanged(t, before, hashes(t, f.base))
	// Only the lock files that jad itself makes when it locks a session may appear.
	if after := withoutLocks(snapshotTree(t, f.base)); len(after) != len(treeBefore) {
		t.Fatal("something was written while a session was in use")
	}
}

func TestRollbackAfterASuccessfulApplyRestoresV1AndKeepsWhatChanged(t *testing.T) {
	f := newMigrateFixture(t)
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := RollbackWorkspaceMigration(result.Journal)
	if err != nil || len(rollback.Kept) != 0 || len(rollback.Removed) != 3 {
		t.Fatalf("rollback: %v %+v", err, rollback)
	}
	after := hashes(t, f.base)
	unchanged(t, before, after)
	if len(after) != len(before)+1 { // the journal only
		t.Fatalf("files: %d before, %d after", len(before), len(after))
	}
	if _, err := os.Stat(filepath.Join(f.global, "mcp")); err == nil {
		t.Fatal("the folders this run made in G are still there")
	}
	if _, err := os.Stat(f.global); err == nil {
		t.Fatal("G, made by this run, is still there")
	}
	// A second run finds nothing to do.
	again, err := RollbackWorkspaceMigration(result.Journal)
	if err != nil || len(again.Removed) != 0 || len(again.Kept) != 0 {
		t.Fatalf("second rollback: %v %+v", err, again)
	}

	// A file changed after the migration is kept, and so is the folder holding it.
	result, err = f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(f.global, "mcp/web/search.json")
	if err := os.WriteFile(g, []byte(`{"edited":"after the migration"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback, err = RollbackWorkspaceMigration(result.Journal)
	if err != nil || len(rollback.Kept) < 2 {
		t.Fatalf("rollback: %v %+v", err, rollback)
	}
	if data, _ := os.ReadFile(g); !strings.Contains(string(data), "edited") {
		t.Fatal("a changed file was removed")
	}
	if _, err := os.Stat(filepath.Join(f.jad, "workspace.json")); err == nil {
		t.Fatal("workspace.json (unchanged) must be removed to go back to v1")
	}
}

func TestRollbackNeedsEvidenceThisRunCreatedTheFile(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "j.jsonl")
	file := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sum := func(content string) string {
		s := sha256.Sum256([]byte(content))
		return hex.EncodeToString(s[:])
	}
	// linked: a crash after the link and before the done entry; the temporary file is still the
	// same file. preexisting: same content, but nothing shows this run made it.
	linked := file("linked", "mine")
	temp := filepath.Join(dir, ".migrate-x-1.tmp")
	if err := os.Link(linked, temp); err != nil {
		t.Fatal(err)
	}
	preexisting := file("preexisting", "mine")
	lines := []journalEntry{
		{Op: journalStart, Workspace: dir, Global: filepath.Join(dir, "g")},
		{Op: journalCreate, Path: linked, Temp: temp, SHA256: sum("mine"), Size: 4},
		{Op: journalCreate, Path: preexisting, Temp: filepath.Join(dir, ".migrate-x-2.tmp"), SHA256: sum("mine"), Size: 4},
	}
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if err := appendJournal(f, line); err != nil {
			t.Fatal(err)
		}
	}
	// A line cut short by a crash is ignored.
	if _, err := f.WriteString(`{"op":"cre`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	rollback, err := RollbackWorkspaceMigration(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(linked); err == nil {
		t.Fatal("a file recognised by its temporary file is removed")
	}
	if _, err := os.Stat(temp); err == nil {
		t.Fatal("the temporary file is removed")
	}
	if _, err := os.Stat(preexisting); err != nil {
		t.Fatal("a file with no evidence is never removed")
	}
	if len(rollback.Removed) != 1 || len(rollback.Kept) != 1 || !strings.Contains(rollback.Kept[0].Reason, "did not create") {
		t.Fatalf("rollback: %+v", rollback)
	}
}

func TestPublishNeverReplacesAndRefusesLinks(t *testing.T) {
	f := newMigrateFixture(t)
	p := f.planner(t)
	a := &migrationApplier{p: p}
	if err := a.openJournal(); err != nil {
		t.Fatal(err)
	}
	defer a.journal.Close()
	dest := filepath.Join(f.jad, "profiles/dev/new.json")
	if err := a.publishBytes(dest, []byte("one"), 0o600, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := a.publishBytes(dest, []byte("two"), 0o600, time.Time{}); err == nil || !strings.Contains(err.Error(), "never replaces") {
		t.Fatalf("replace: %v", err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "one" {
		t.Fatalf("replaced: %s", data)
	}
	// A link on the way is not written through.
	outside := filepath.Join(f.base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.jad, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := a.publishBytes(filepath.Join(f.jad, "linked/x.json"), []byte("x"), 0o600, time.Time{}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("link: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("written through a link")
	}
	// A source whose hash is not the planned one is not published.
	if err := a.publish(filepath.Join(f.jad, "profiles/dev/other.json"), 0o600, time.Time{}, strings.NewReader("data"), "0000"); err == nil || !strings.Contains(err.Error(), "changed since the plan") {
		t.Fatalf("hash: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.jad, "profiles/dev/other.json")); err == nil {
		t.Fatal("published despite the hash")
	}
	if matches, _ := filepath.Glob(filepath.Join(f.jad, "profiles/dev/.migrate-*")); len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func TestApplyDoesNotPublishWhenAVerifiedFileChanged(t *testing.T) {
	f := newMigrateFixture(t)
	p := f.planner(t)
	a := &migrationApplier{p: p}
	if err := a.openJournal(); err != nil {
		t.Fatal(err)
	}
	defer a.journal.Close()
	dest := filepath.Join(f.jad, "profiles/dev/new.json")
	if err := a.publishBytes(dest, []byte("one"), 0o600, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.verify(); err == nil || !strings.Contains(err.Error(), "does not hold what was written") {
		t.Fatalf("verify: %v", err)
	}
	a.created = nil
	a.sources = []MigrationFile{{Source: filepath.Join(f.jad, "profiles/dev/coder.json"), SHA256: "0000"}}
	if err := a.verify(); err == nil || !strings.Contains(err.Error(), "changed while it was copied") {
		t.Fatalf("source verify: %v", err)
	}
}

func TestApplyWritesTheRewrittenToolsetAndLeavesItsSourceAlone(t *testing.T) {
	f := newMigrateFixture(t)
	// Only the tools are elsewhere; state, profiles and sessions stay in W/.jad (in place).
	v1 := filepath.Join(f.base, "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.jad, "tools"), filepath.Join(v1, "tools")); err != nil {
		t.Fatal(err)
	}
	f.setting["catalogs"] = map[string]any{"profiles": "profiles", "models": "models", "tools": filepath.Join(v1, "tools"), "mcp": "mcp", "state": "state"}
	f.writeSetting(t)
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), map[string]any{"native": map[string]any{"async": map[string]any{}, "exec": map[string]any{"env_file": "exec.env"}}, "mcp": map[string]any{"web/search": map[string]any{}}})
	f.write(t, "exec.env", "")
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	moved, err := os.ReadFile(filepath.Join(f.global, "tools/coding/default.json"))
	if err != nil || !strings.Contains(string(moved), filepath.Join(f.jad, "exec.env")) || strings.Contains(string(moved), `"exec.env"`) {
		t.Fatalf("rewritten toolset: %v %s", err, moved)
	}
	source, _ := os.ReadFile(filepath.Join(v1, "tools/coding/default.json"))
	if !strings.Contains(string(source), `"exec.env"`) {
		t.Fatalf("the source toolset was changed: %s", source)
	}
	if result.Compared < 1 {
		t.Fatalf("compared %d", result.Compared)
	}
}

func withoutLocks(tree map[string]string) map[string]string {
	kept := map[string]string{}
	for path, record := range tree {
		if !strings.HasSuffix(path, ".jad-lock") {
			kept[path] = record
		}
	}
	return kept
}

func TestRollbackRemovesNothingOutsideTheFoldersOfTheRun(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("not mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("not mine"))
	temp := filepath.Join(dir, ".migrate-x-1.tmp")
	if err := os.Link(outside, temp); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "j.jsonl")
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []journalEntry{
		{Op: journalStart, Workspace: filepath.Join(dir, "w"), Global: filepath.Join(dir, "g")},
		{Op: journalCreate, Path: outside, Temp: temp, SHA256: hex.EncodeToString(sum[:])},
		{Op: journalDone, Path: outside},
	} {
		if err := appendJournal(f, entry); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	rollback, err := RollbackWorkspaceMigration(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a path outside the run's folders was removed")
	}
	if len(rollback.Removed) != 0 || len(rollback.Kept) != 1 || !strings.Contains(rollback.Kept[0].Reason, "outside") {
		t.Fatalf("rollback: %+v", rollback)
	}
	if _, err := RollbackWorkspaceMigration(outside); err == nil {
		t.Fatal("a file that is not a journal must be refused")
	}
}

func TestPublishLosesToAFileThatAppearsAtTheLastMoment(t *testing.T) {
	f := newMigrateFixture(t)
	p := f.planner(t)
	a := &migrationApplier{p: p}
	if err := a.openJournal(); err != nil {
		t.Fatal(err)
	}
	defer a.journal.Close()
	dest := filepath.Join(f.jad, "profiles/dev/new.json")
	a.beforeLink = func(dest string) {
		if err := os.WriteFile(dest, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.publishBytes(dest, []byte("ours"), 0o600, time.Time{}); err == nil {
		t.Fatal("a file that appeared before the link was replaced")
	}
	if data, _ := os.ReadFile(dest); string(data) != "theirs" {
		t.Fatalf("replaced: %s", data)
	}
	if matches, _ := filepath.Glob(filepath.Join(f.jad, "profiles/dev/.migrate-*")); len(matches) != 0 {
		t.Fatalf("temporary file left: %v", matches)
	}
	// The cleanup of this run does not touch it either: it is not ours.
	entries, _ := readJournal(a.journalPath)
	rollback := rollbackEntries(entries, a.journalPath, a.journal)
	if data, _ := os.ReadFile(dest); string(data) != "theirs" || len(rollback.Removed) != 0 {
		t.Fatalf("rollback touched it: %+v", rollback)
	}
}

func TestApplyVerifiesTheCopiesBeforeTheSwitch(t *testing.T) {
	f := newMigrateFixture(t)
	p := f.planner(t)
	g := filepath.Join(f.global, "mcp/web/search.json")
	result, err := applyPlanned(context.Background(), p, func(a *migrationApplier) {
		a.beforeVerify = func() {
			if err := os.WriteFile(g, []byte("damaged"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil || !strings.Contains(err.Error(), "does not hold what was written") || result.Rollback == nil {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.jad, "workspace.json")); err == nil {
		t.Fatal("the switch was written after a failed verification")
	}
	// The damaged file is not ours any more: it is kept and reported, with its folders.
	if data, _ := os.ReadFile(g); string(data) != "damaged" || len(result.Rollback.Kept) == 0 {
		t.Fatalf("rollback: %+v", result.Rollback)
	}
}

// Decider 4c: a workspace's own tool definitions move to workspace.json and v2 reads them in
// the same order from the same places; the global one stays in global.json.
func TestApplyKeepsToolDefinitionsInTheWorkspaceInWorkspaceJSON(t *testing.T) {
	f := newMigrateFixture(t)
	f.setting["libraries"] = map[string]any{"tool_definition_paths": []string{"tools_defintion/native/exec.json", "../../toolsdef/native/exec.json", filepath.Join(f.jad, "tools_defintion/native/async.json")}}
	f.writeSetting(t)
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	config, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: map[string]string{"HOME": f.home}})
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	want := []string{filepath.Join(f.base, "toolsdef/native/exec.json"), filepath.Join(f.jad, "tools_defintion/native/exec.json"), filepath.Join(f.jad, "tools_defintion/native/async.json")}
	if !reflect.DeepEqual(layout.ToolDefinitionPaths, want) {
		t.Fatalf("v2 reads %q, want %q", layout.ToolDefinitionPaths, want)
	}
	data, _ := os.ReadFile(filepath.Join(f.global, "global.json"))
	if strings.Contains(string(data), "tools_defintion") {
		t.Fatalf("a path in the workspace is in G: %s", data)
	}
	if _, err := RollbackWorkspaceMigration(result.Journal); err != nil {
		t.Fatal(err)
	}
	if config, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: map[string]string{"HOME": f.home}}); err != nil || len(config.Workspaces) != 0 {
		t.Fatalf("v1 again: %v", err)
	}
}

// Decider 4c: storage outside the copied state (here the default tool storage and the exec
// function library under the working folder) is shared by v1 and v2: the plan notes it, the
// comparison accepts the same absolute path, and a rollback keeps whatever v2 wrote there.
func TestApplySharesStorageOutsideTheCopiedStateAndRollbackKeepsWhatV2Wrote(t *testing.T) {
	f := newMigrateFixture(t)
	v1 := otherFolders(t, f)
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), map[string]any{"native": map[string]any{"async": map[string]any{}, "exec": map[string]any{}}, "mcp": map[string]any{"web/search": map[string]any{}}})
	f.write(t, filepath.Join(v1, "state/work/dev/coder/messages.jsonl"), "{}\n")
	f.setting["defaults"] = map[string]any{"cwd": f.workspace}
	f.writeSetting(t)
	p := f.planner(t)
	if !strings.Contains(strings.Join(p.plan.Warnings, "\n"), "shared storage: tool_storage_dir") {
		t.Fatalf("warnings: %q", p.plan.Warnings)
	}
	result, err := applyPlanned(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	shared := filepath.Join(f.jad, "state/work/dev/coder/tools/helper")
	f.write(t, shared, "written by v2")
	rollback, err := RollbackWorkspaceMigration(result.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(shared); err != nil || string(data) != "written by v2" {
		t.Fatalf("what v2 wrote in the shared storage is gone after the rollback: %v %+v", err, rollback)
	}
}

// sameWriteTargets: a copy compares by its place in the state; shared storage by its absolute
// path; a v1 target inside the v1 state that v2 points at by an absolute path is a difference.
func TestSameWriteTargets(t *testing.T) {
	for _, test := range []struct {
		name                     string
		old, now, oldAbs, nowAbs string
		want                     bool
	}{
		{"both in the state", "{state}/a/tools", "{state}/a/tools", "/v1/a/tools", "/w/a/tools", true},
		{"shared, v2 keeps it in its state", "/w/state/s/a/tools", "{state}/a/tools", "/w/state/s/a/tools", "/w/state/s/a/tools", true},
		{"shared and outside both", "/x/y", "/x/y", "/x/y", "/x/y", true},
		{"v1 in the state, v2 absolute", "{state}/a/tools", "/v1/s/a/tools", "/v1/s/a/tools", "/v1/s/a/tools", false},
		{"a list of a copy and a shared target", "{state}/a,/x/y", "{state}/a,{state}/z", "/v1/a,/x/y", "/w/a,/w/z", false},
		{"a list of a copy and shared target, v2 inside", "{state}/a,/w/z", "{state}/a,{state}/z", "/v1/a,/w/z", "/w/a,/w/z", true},
		{"different paths", "/x/y", "{state}/z", "/x/y", "/w/z", false},
		{"lists of different length", "/x/y", "/x/y,/x/z", "/x/y", "/x/y,/x/z", false},
	} {
		if got := sameWriteTargets(test.old, test.now, test.oldAbs, test.nowAbs); got != test.want {
			t.Errorf("%s: got %v", test.name, got)
		}
	}
}

// What a v1 process and a v2 process in one migrated workspace share (decider 4c: "v1 and v2
// cannot run in the same W at once" is a rule of the mode, not something the code enforces).
// The mode is chosen once at start, so a v1 process that was running keeps v1 and a new one
// started there is v2. With the folders in place both resolve the same transcript, state folder
// and lock file, so the transcript lock excludes the other mode; the profile files are shared
// too, and a profile a v2 self-edit gave a profile: model is left out of v1 (a v1 process cannot
// read it), which is the cost of going back to v1 after such an edit.
func TestV1AndV2ProcessesInOneWorkspaceShareTranscriptsLocksAndProfileFiles(t *testing.T) {
	f := newMigrateFixture(t)
	if result, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	env := map[string]string{"HOME": f.home}
	v1, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: env, RootSettingPath: filepath.Join(f.jad, "setting.json")})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.Workspaces) != 0 || len(v2.Workspaces) != 1 {
		t.Fatalf("the mode is chosen at start: v1 %d workspaces, v2 %d", len(v1.Workspaces), len(v2.Workspaces))
	}
	session := func(config Config) SessionConfig {
		t.Helper()
		location, err := Resolve(config.Roots, config.Roots[0].ID+":work")
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadLayoutSessionSettings(location, *config.Roots[0].Layout, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return loaded
	}
	one, two := session(v1), session(v2)
	if one.MessagesPath != two.MessagesPath || one.StateDir != two.StateDir || one.MessagesPath == "" {
		t.Fatalf("v1 and v2 use different transcripts: %s / %s", one.MessagesPath, two.MessagesPath)
	}
	oneLock, err := sessionlock.PathFor(one.MessagesPath)
	if err != nil {
		t.Fatal(err)
	}
	twoLock, err := sessionlock.PathFor(two.MessagesPath)
	if err != nil || oneLock != twoLock {
		t.Fatalf("v1 and v2 lock different files: %s / %s (%v)", oneLock, twoLock, err)
	}
	held, err := sessionlock.Acquire(context.Background(), oneLock)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := sessionlock.TryAcquire(context.Background(), twoLock); !errors.Is(err, sessionlock.ErrBusy) {
		if again != nil {
			again.Close()
		}
		t.Fatalf("a v2 process and the transcript lock a v1 process holds: %v", err)
	}
	held.Close()

	// A profile file a v2 self-edit points at the profile's own model: v2 loads it, v1 cannot.
	f.write(t, "profile-own/dev/coder/models/openai/fast.json", `{"based_on":"workspace:openai/default"}`)
	f.put(t, "profiles/dev/coder.json", map[string]any{"models": []string{"openai/default", "profile:openai/fast"}, "model_default": "profile:openai/fast", "toolset": "coding/default"})
	if _, err := LoadLayoutSessionSettings(mustResolve(t, v2, "work"), *v2.Roots[0].Layout, "", ""); err != nil {
		t.Fatalf("v2 does not load its own profile: %v", err)
	}
	if _, err := LoadLayoutSessionSettings(mustResolve(t, v1, "work"), *v1.Roots[0].Layout, "", ""); err == nil || !strings.Contains(err.Error(), "profile:openai/fast") {
		t.Fatalf("v1 reads a profile with an own model: %v", err)
	}
}

func mustResolve(t *testing.T, config Config, id string) Location {
	t.Helper()
	location, err := Resolve(config.Roots, config.Roots[0].ID+":"+id)
	if err != nil {
		t.Fatal(err)
	}
	return location
}
