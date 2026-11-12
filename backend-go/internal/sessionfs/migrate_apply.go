package sessionfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/sessionlock"
)

// The applier of "jad migrate workspace --apply" (docs/design_workspace_layout.md §24, phase
// 4b). It plans again, then copies what the plan says and nothing else:
//
//   - a file is published without replacing anything: it is written to a temporary file in its
//     destination folder and hard-linked into place, so a destination that appeared meanwhile
//     is an error and keeps its content;
//   - no source is changed or deleted; the only things written next to sources are the lock
//     files a running jad also makes (sessionlock);
//   - a journal (W/.jad/migrations/<run>.jsonl, append-only, synced) records each folder and
//     file this run created, with its hash, before it is used. Cleanup removes only what the
//     journal shows this run created and whose content still has that hash;
//   - workspace.json is written last, and the copies are verified by hash before it: it is the
//     switch to the workspace layout (workspaceAt), and deleting it goes back to v1.

// Journal operations.
const (
	journalStart    = "start"
	journalMkdir    = "mkdir"
	journalTemp     = "temp"
	journalCreate   = "create"
	journalDone     = "done"
	journalSwitch   = "switch"
	journalComplete = "complete"
	journalFailed   = "failed"
	journalRemoved  = "removed"
	journalKept     = "kept"
	journalRolled   = "rolled_back"
)

// journalFolderName is the folder of W/.jad that holds the journals. It is not removed by a
// rollback: a journal is the record of what was done.
const journalFolderName = "migrations"

type journalEntry struct {
	Op     string `json:"op"`
	Time   string `json:"time,omitempty"`
	Path   string `json:"path,omitempty"`
	Temp   string `json:"temp,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Workspace and Global are the folders of the run (start entry): a cleanup removes nothing
	// outside them, whatever the journal says.
	Workspace string `json:"workspace,omitempty"`
	Global    string `json:"global,omitempty"`
}

// MigrationCreated is a file the migration created.
type MigrationCreated struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// MigrationRollback is what a cleanup removed and what it kept, with the reason.
type MigrationRollback struct {
	Journal string          `json:"journal"`
	Removed []string        `json:"removed,omitempty"`
	Folders []string        `json:"folders,omitempty"`
	Kept    []MigrationNote `json:"kept,omitempty"`
}

// MigrationApplyResult is the outcome of ApplyWorkspaceMigration.
type MigrationApplyResult struct {
	Plan    MigrationPlan      `json:"plan"`
	Journal string             `json:"journal,omitempty"`
	Created []MigrationCreated `json:"created,omitempty"`
	Folders []string           `json:"folders,omitempty"`
	// Compared is the number of (session, profile) pairs whose v1 and v2 loads were compared.
	Compared int      `json:"compared"`
	Notes    []string `json:"notes,omitempty"`
	// Rollback is set when the apply failed after it had written something and removed it.
	Rollback *MigrationRollback `json:"rollback,omitempty"`
}

// MigrationBlockedError is returned by ApplyWorkspaceMigration for a plan with blockers;
// nothing was written.
type MigrationBlockedError struct{ Blockers int }

func (e *MigrationBlockedError) Error() string {
	return fmt.Sprintf("the migration is blocked by %d problems; nothing was written", e.Blockers)
}

type migrationApplier struct {
	p           *migrationPlanner
	run         string
	journal     *os.File
	journalPath string
	created     []journalEntry
	folders     []string
	temps       int
	locks       []*sessionlock.Lock
	// sources are the copied files with the hash the plan read from them.
	sources []MigrationFile
	// v1 are the v1 loads taken before anything was written.
	v1 map[string]migrationPrint
	// beforeLink and beforeVerify are seams for tests (a file appears, or changes, at that
	// moment); nil in production.
	beforeLink   func(dest string)
	beforeVerify func()
}

// ApplyWorkspaceMigration plans the migration of W and applies it. A blocked plan writes
// nothing. A failure after the first write removes what this run created (the journal says
// what that is) and reports what it kept; the v1 setting and files are never changed.
func ApplyWorkspaceMigration(ctx context.Context, options MigrateOptions) (MigrationApplyResult, error) {
	p, err := newMigrationPlanner(options)
	if err != nil {
		return MigrationApplyResult{}, err
	}
	p.run()
	sort.SliceStable(p.plan.Files, func(i, j int) bool { return p.plan.Files[i].Destination < p.plan.Files[j].Destination })
	return applyPlanned(ctx, p, nil)
}

// applyPlanned applies a finished plan. before, when set, runs after the v1 loads and the
// locks and before the first write (tests change the applier's state there).
func applyPlanned(ctx context.Context, p *migrationPlanner, before func(*migrationApplier)) (MigrationApplyResult, error) {
	result := MigrationApplyResult{Plan: *p.plan}
	if p.plan.Blocked() {
		return result, &MigrationBlockedError{Blockers: len(p.plan.Blockers)}
	}
	a := &migrationApplier{p: p}
	defer a.release()
	// Locks and v1 loads come first, before anything is written: a busy session stops here,
	// and once workspace.json exists a load of W is v2.
	if err := a.lockSources(ctx); err != nil {
		return result, err
	}
	a.collectSources()
	a.v1 = p.sessionPrints(p.config.Roots, p.root.ID, *p.layout, p.sessions)
	if before != nil {
		before(a)
	}
	if err := a.openJournal(); err != nil {
		return result, err
	}
	result.Journal = a.journalPath
	defer a.journal.Close()
	applyErr := a.apply(ctx, &result)
	result.Created = createdOf(a.created)
	result.Folders = append([]string(nil), a.folders...)
	if applyErr == nil {
		_ = a.log(journalEntry{Op: journalComplete})
		return result, nil
	}
	_ = a.log(journalEntry{Op: journalFailed, Detail: applyErr.Error()})
	entries, readErr := readJournal(a.journalPath)
	if readErr != nil {
		return result, fmt.Errorf("the migration failed and its journal cannot be read, so nothing was removed (journal %s: %v): %w", a.journalPath, readErr, applyErr)
	}
	rollback := rollbackEntries(entries, a.journalPath, a.journal)
	result.Rollback = &rollback
	return result, fmt.Errorf("the migration failed and what it created was removed where it was unchanged (journal %s): %w", a.journalPath, applyErr)
}

func createdOf(entries []journalEntry) []MigrationCreated {
	created := make([]MigrationCreated, 0, len(entries))
	for _, entry := range entries {
		created = append(created, MigrationCreated{Path: entry.Path, SHA256: entry.SHA256, Size: entry.Size})
	}
	return created
}

func (a *migrationApplier) release() {
	for _, lock := range a.locks {
		_ = lock.Close()
	}
	a.locks = nil
}

func (a *migrationApplier) apply(ctx context.Context, result *MigrationApplyResult) error {
	p := a.p
	// Additions to G first: they do not change what the workspace reads.
	for _, global := range []bool{true, false} {
		for _, file := range p.plan.Files {
			if file.Action != MigrationCopy || file.Global != global {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := a.copyFile(file); err != nil {
				return err
			}
		}
	}
	for _, setting := range p.plan.Settings {
		if setting.Action == MigrationCreate && filepath.Base(setting.Path) == globalSettingName {
			if err := a.publishBytes(setting.Path, setting.Content, 0o600, time.Time{}); err != nil {
				return err
			}
		}
	}
	if a.beforeVerify != nil {
		a.beforeVerify()
	}
	if err := a.verify(); err != nil {
		return err
	}
	// The switch: nothing after this line is needed to start in the workspace layout.
	for _, setting := range p.plan.Settings {
		if setting.Action == MigrationCreate && filepath.Base(setting.Path) == workspaceSettingName {
			if err := a.log(journalEntry{Op: journalSwitch, Path: setting.Path}); err != nil {
				return err
			}
			if err := a.publishBytes(setting.Path, setting.Content, 0o600, time.Time{}); err != nil {
				return err
			}
		}
	}
	differences, compared, notes := a.compare()
	result.Compared, result.Notes = compared, notes
	if len(differences) > 0 {
		return fmt.Errorf("the workspace layout does not load the same as v1 (%d differences): %s", len(differences), strings.Join(differences, "; "))
	}
	return nil
}

// lockSources takes the lock of each copied session file and transcript without waiting: a
// running jad holds them, and a copy of a transcript it is writing would be cut.
func (a *migrationApplier) lockSources(ctx context.Context) error {
	for _, file := range a.p.plan.Files {
		if file.Action != MigrationCopy {
			continue
		}
		if file.Group != "sessions" && !(file.Group == "state" && filepath.Base(file.Source) == "messages.jsonl") {
			continue
		}
		path, err := sessionlock.PathFor(file.Source)
		if err != nil {
			return err
		}
		lock, err := sessionlock.TryAcquire(ctx, path)
		if errors.Is(err, sessionlock.ErrBusy) {
			return fmt.Errorf("%s is in use (its lock is held by a running jad); stop it and run the migration again", file.Source)
		}
		if err != nil {
			return fmt.Errorf("lock %s: %w", file.Source, err)
		}
		a.locks = append(a.locks, lock)
	}
	return nil
}

func (a *migrationApplier) collectSources() {
	for _, file := range a.p.plan.Files {
		if file.Action == MigrationCopy {
			a.sources = append(a.sources, file)
		}
	}
}

// copyFile publishes one planned copy. The source is read once, hashed as it is written, and
// must still have the hash the plan read; a rewritten file is rebuilt from those bytes.
func (a *migrationApplier) copyFile(file MigrationFile) error {
	source, info, err := openRegular(file.Source)
	if err != nil {
		return err
	}
	defer source.Close()
	if file.Rewritten {
		data, err := io.ReadAll(source)
		if err != nil {
			return fmt.Errorf("read %s: %w", file.Source, err)
		}
		if sum := sha256Hex(data); sum != file.SHA256 {
			return fmt.Errorf("%s changed since the plan was made", file.Source)
		}
		candidate, _, err := a.p.globalCandidate(file.Group, file.Source, data)
		if err != nil {
			return fmt.Errorf("rewrite %s: %w", file.Source, err)
		}
		return a.publishBytes(file.Destination, candidate, info.Mode().Perm(), info.ModTime())
	}
	return a.publish(file.Destination, info.Mode().Perm(), info.ModTime(), source, file.SHA256)
}

// openRegular opens a regular file that is not a link: the file opened is the one Lstat saw.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s changed while it was opened", path)
	}
	return file, after, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) (string, error) {
	file, _, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// verify checks every created file against its journal hash and every copied source against
// the hash the plan read, before the switch.
func (a *migrationApplier) verify() error {
	for _, entry := range a.created {
		sum, err := fileSHA256(entry.Path)
		if err != nil {
			return fmt.Errorf("verify %s: %w", entry.Path, err)
		}
		if sum != entry.SHA256 {
			return fmt.Errorf("verify %s: it does not hold what was written", entry.Path)
		}
	}
	for _, file := range a.sources {
		sum, err := fileSHA256(file.Source)
		if err != nil {
			return fmt.Errorf("verify %s: %w", file.Source, err)
		}
		if sum != file.SHA256 {
			return fmt.Errorf("%s changed while it was copied; nothing was switched", file.Source)
		}
	}
	return nil
}

func (a *migrationApplier) publishBytes(dest string, data []byte, mode fs.FileMode, modTime time.Time) error {
	return a.publish(dest, mode, modTime, strings.NewReader(string(data)), "")
}

// publish creates dest from source without replacing anything. wantSHA, when set, is the hash
// the source must have.
func (a *migrationApplier) publish(dest string, mode fs.FileMode, modTime time.Time, source io.Reader, wantSHA string) error {
	base := a.p.jad
	if PathWithin(a.p.global, dest) {
		base = a.p.global
		if err := a.ensureBase(base); err != nil {
			return err
		}
	}
	dir := filepath.Dir(dest)
	if err := a.ensureFolder(base, dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists; the migration never replaces a file", dest)
	}
	a.temps++
	temp := filepath.Join(dir, fmt.Sprintf(".migrate-%s-%d.tmp", a.run, a.temps))
	if err := a.log(journalEntry{Op: journalTemp, Path: dest, Temp: temp}); err != nil {
		return err
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), source)
	if err == nil {
		err = file.Sync()
	}
	if err == nil && mode != 0 {
		err = file.Chmod(mode)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write %s: %w", dest, err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if wantSHA != "" && sum != wantSHA {
		_ = os.Remove(temp)
		return fmt.Errorf("the source of %s changed since the plan was made", dest)
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(temp, modTime, modTime)
	}
	entry := journalEntry{Op: journalCreate, Path: dest, Temp: temp, SHA256: sum, Size: size}
	if err := a.log(entry); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if a.beforeLink != nil {
		a.beforeLink(dest)
	}
	if err := os.Link(temp, dest); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	a.created = append(a.created, entry)
	if err := a.log(journalEntry{Op: journalDone, Path: dest}); err != nil {
		return err
	}
	_ = os.Remove(temp)
	syncFolder(dir)
	return nil
}

func syncFolder(dir string) {
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
}

// ensureBase makes the base folder of the additions (G) when it is missing; its parent must
// exist. A base that is a link to a folder is used as it is.
func (a *migrationApplier) ensureBase(base string) error {
	info, err := os.Stat(base)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", base)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(base, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", base, err)
	}
	a.folders = append(a.folders, base)
	return a.log(journalEntry{Op: journalMkdir, Path: base})
}

// ensureFolder makes dir below base one part at a time, refusing a link or a file on the way,
// and journals every folder it made.
func (a *migrationApplier) ensureFolder(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is not below %s", dir, base)
	}
	current := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("%s is a link or not a folder; the migration does not write through it", current)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(current, 0o700); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return fmt.Errorf("create %s: %w", current, err)
				}
				continue
			}
			a.folders = append(a.folders, current)
			if err := a.log(journalEntry{Op: journalMkdir, Path: current}); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

func (a *migrationApplier) openJournal() error {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	a.run = time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random[:])
	dir := filepath.Join(a.p.jad, journalFolderName)
	if info, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	} else if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a folder, not a link", dir)
	}
	a.journalPath = filepath.Join(dir, a.run+".jsonl")
	file, err := os.OpenFile(a.journalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create the journal: %w", err)
	}
	a.journal = file
	return a.log(journalEntry{Op: journalStart, Workspace: a.p.workspace, Global: a.p.global})
}

// log appends one entry: the journal is written before what it describes is used.
func (a *migrationApplier) log(entry journalEntry) error {
	return appendJournal(a.journal, entry)
}

// appendJournal writes one entry. The entries that decide what a cleanup may remove (start,
// mkdir, create, switch, failed) are synced; temp, done, removed and kept are not: without a
// done entry a file is still recognised by its temporary file, and without that it is kept.
func appendJournal(file *os.File, entry journalEntry) error {
	entry.Time = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write the journal: %w", err)
	}
	switch entry.Op {
	case journalTemp, journalDone, journalRemoved, journalKept, journalRolled, journalComplete:
		return nil
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync the journal: %w", err)
	}
	return nil
}

// readJournal reads the entries of a journal; a last line cut short by a crash is ignored.
func readJournal(path string) ([]journalEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []journalEntry
	lines := strings.Split(string(data), "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			if index >= len(lines)-2 {
				break
			}
			return nil, fmt.Errorf("%s: line %d is not a journal entry: %w", path, index+1, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// RollbackWorkspaceMigration removes what the journal shows a migration run created: a file
// only when it is recorded as created by that run (done, or its temporary file still is the
// same file) and still holds the recorded hash, a folder only when it is empty. Anything else
// is kept and reported. The journal stays; running it again finds nothing more to do.
func RollbackWorkspaceMigration(journalPath string) (MigrationRollback, error) {
	entries, err := readJournal(journalPath)
	if err != nil {
		return MigrationRollback{}, err
	}
	if len(entries) == 0 || entries[0].Op != journalStart {
		return MigrationRollback{}, fmt.Errorf("%s is not a migration journal", journalPath)
	}
	file, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return MigrationRollback{}, err
	}
	defer file.Close()
	return rollbackEntries(entries, journalPath, file), nil
}

func rollbackEntries(entries []journalEntry, journalPath string, journal *os.File) MigrationRollback {
	rollback := MigrationRollback{Journal: journalPath}
	note := func(entry journalEntry) {
		if journal != nil {
			_ = appendJournal(journal, entry)
		}
	}
	done := map[string]bool{}
	for _, entry := range entries {
		if entry.Op == journalDone {
			done[entry.Path] = true
		}
	}
	inside := func(path string) bool {
		start := entries[0]
		return start.Workspace != "" && PathWithin(start.Workspace, path) || start.Global != "" && PathWithin(start.Global, path)
	}
	removedBefore := map[string]bool{}
	for _, entry := range entries {
		if entry.Op == journalRemoved {
			removedBefore[entry.Path] = true
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Op != journalCreate || removedBefore[entry.Path] {
			continue
		}
		if !inside(entry.Path) {
			rollback.Kept = append(rollback.Kept, MigrationNote{Path: entry.Path, Reason: "it is outside the workspace and global folders of this run"})
			continue
		}
		info, err := os.Lstat(entry.Path)
		if errors.Is(err, fs.ErrNotExist) {
			removeTemp(entry)
			continue
		}
		ours := done[entry.Path]
		if !ours {
			if temp, tempErr := os.Lstat(entry.Temp); tempErr == nil && err == nil && os.SameFile(temp, info) {
				ours = true
			}
		}
		keep := func(reason string) {
			rollback.Kept = append(rollback.Kept, MigrationNote{Path: entry.Path, Reason: reason})
			note(journalEntry{Op: journalKept, Path: entry.Path, Detail: reason})
		}
		switch {
		case err != nil:
			keep(fmt.Sprintf("cannot inspect it: %v", err))
		case !ours:
			keep("this run did not create it (it was there already)")
		case !info.Mode().IsRegular():
			keep("it is no longer a regular file")
		default:
			if sum, sumErr := fileSHA256(entry.Path); sumErr != nil || sum != entry.SHA256 {
				keep("it changed after the migration wrote it")
			} else if rmErr := os.Remove(entry.Path); rmErr != nil {
				keep(fmt.Sprintf("remove failed: %v", rmErr))
			} else {
				rollback.Removed = append(rollback.Removed, entry.Path)
				note(journalEntry{Op: journalRemoved, Path: entry.Path})
			}
		}
		removeTemp(entry)
	}
	// Temporary files of a run that stopped before it logged the file.
	for _, entry := range entries {
		if entry.Op == journalTemp && entry.Temp != "" && inside(entry.Temp) {
			if info, err := os.Lstat(entry.Temp); err == nil && info.Mode().IsRegular() {
				_ = os.Remove(entry.Temp)
			}
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Op != journalMkdir || !inside(entry.Path) {
			continue
		}
		if info, err := os.Lstat(entry.Path); err != nil || !info.IsDir() {
			continue
		}
		if err := os.Remove(entry.Path); err == nil {
			rollback.Folders = append(rollback.Folders, entry.Path)
			note(journalEntry{Op: journalRemoved, Path: entry.Path, Detail: "folder"})
		} else {
			rollback.Kept = append(rollback.Kept, MigrationNote{Path: entry.Path, Reason: "a folder this run made; it is not empty or cannot be removed"})
		}
	}
	note(journalEntry{Op: journalRolled})
	return rollback
}

// removeTemp removes the temporary file of a created entry when it is still there and is a
// regular file: it was named for this run.
func removeTemp(entry journalEntry) {
	if entry.Temp == "" || !strings.HasPrefix(filepath.Base(entry.Temp), ".migrate-") {
		return
	}
	if info, err := os.Lstat(entry.Temp); err == nil && info.Mode().IsRegular() {
		_ = os.Remove(entry.Temp)
	}
}
