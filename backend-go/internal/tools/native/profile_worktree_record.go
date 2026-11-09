package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/worktree"
)

// A delegated profile run's worktree is recorded in the target session's state folder while
// the run goes (docs/isolation-sandbox-design.md §10.3), so a server that stops meanwhile
// can finish it at its next start: what the run left is committed to its branch and the
// folder removed, as a sub-agent's lost run is (recoverWorktree). The state folder is
// read-only in every sandbox (D27), so a run cannot write a record that would point the
// host's git at folders of its choosing.

// delegatedWorktreeRecordDir is the folder, inside a session's state folder, that holds the
// records of its delegated runs' worktrees.
const delegatedWorktreeRecordDir = "delegated-worktrees"

// delegatedWorktreeRecordVersion is the version of the record's format.
const delegatedWorktreeRecordVersion = 1

// delegatedWorktreeRecord is the file a delegated run's worktree is recorded in.
type delegatedWorktreeRecord struct {
	Version   int            `json:"version"`
	Server    serverIdentity `json:"server"`
	Path      string         `json:"path"`
	Workspace string         `json:"workspace"`
	Branch    string         `json:"branch"`
	Base      string         `json:"base"`
	Start     string         `json:"start,omitempty"`
	Admin     string         `json:"admin"`
	Common    string         `json:"common"`
	Top       string         `json:"top"`
	// Message is the commit message of the run's work.
	Message string `json:"message"`
	// Pending is set once collecting the run's work failed: the run is over, its folders are
	// kept, and the record stays so that a later server start, or the next delegated run, tries
	// again. Attempts counts the tries (the first is the end of the run), Error is the last
	// failure. After worktree.collect_attempts tries the record stays for manual recovery and is
	// not tried again; nothing is deleted.
	Pending  bool   `json:"pending,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (r delegatedWorktreeRecord) state() worktree.State {
	return worktree.State{Path: r.Path, Workspace: r.Workspace, Branch: r.Branch, Base: r.Base, Start: r.Start, Admin: r.Admin, Common: r.Common, Top: r.Top}
}

// valid reports whether the record names a worktree jad made: every folder absolute, and the
// admin folder one of the repository's worktree folders.
func (r delegatedWorktreeRecord) valid() bool {
	for _, path := range []string{r.Path, r.Workspace, r.Admin, r.Common, r.Top} {
		if !filepath.IsAbs(path) {
			return false
		}
	}
	admins := filepath.Join(r.Common, "worktrees")
	return r.Version == delegatedWorktreeRecordVersion && r.Branch != "" && sessionfs.PathWithin(admins, r.Admin) && filepath.Clean(r.Admin) != admins
}

// writeDelegatedWorktreeRecord records tree in stateDir and returns the record's path; empty
// when stateDir is (a session without a state folder keeps no record).
func writeDelegatedWorktreeRecord(stateDir string, tree *worktree.Tree, message string) (string, error) {
	if stateDir == "" {
		return "", nil
	}
	state := tree.State()
	record := delegatedWorktreeRecord{Version: delegatedWorktreeRecordVersion, Server: currentServerIdentity(),
		Path: state.Path, Workspace: state.Workspace, Branch: state.Branch, Base: state.Base, Start: state.Start,
		Admin: state.Admin, Common: state.Common, Top: state.Top, Message: message}
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(stateDir, delegatedWorktreeRecordDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("make the folder of the worktree records: %w", err)
	}
	path := filepath.Join(dir, filepath.Base(state.Admin)+".json")
	if err := writeRecordFile(dir, path, encoded); err != nil {
		return "", err
	}
	return path, nil
}

// writeRecordFile writes data to path (in dir) through a temporary file and a rename.
func writeRecordFile(dir, path string, data []byte) error {
	temporary, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("record the run's worktree: %w", err)
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
		return fmt.Errorf("record the run's worktree: %w", err)
	}
	return nil
}

// readDelegatedWorktreeRecord reads the record at path.
func readDelegatedWorktreeRecord(path string) (delegatedWorktreeRecord, error) {
	var record delegatedWorktreeRecord
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	return record, err
}

// keepDelegatedWorktreeRecord rewrites the record at path as one whose collection failed with
// reason, after attempts tries: it stays, and a later start or run tries again.
func keepDelegatedWorktreeRecord(path string, record delegatedWorktreeRecord, attempts int, reason string) error {
	record.Pending, record.Attempts, record.Error = true, attempts, boundedReason(reason)
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRecordFile(filepath.Dir(path), path, encoded)
}

// DelegatedWorktreeRecovery is the end of one lost delegated run's worktree.
type DelegatedWorktreeRecovery struct {
	Branch  string
	Changed bool
	// Path is the worktree folder when it was kept; Error says why. Busy is set when a running
	// process still holds the run's layers, so the run was not tried.
	Path  string
	Error string
	Busy  bool
}

// RecoverDelegatedWorktrees finishes the worktrees of the delegated runs recorded in
// stateDir whose server stopped while they went, and tries again those whose collection failed:
// what each run left is committed to its branch and its folder removed, on the host (the run's
// sandbox is not known any more; an overlay run in a holder of its own), with settings' git
// options. A run of this server, or of another server that still runs, is left alone, and so is
// a record another process is working on. A record is removed once its run is finished; one whose
// collection fails stays, with one more attempt, until settings' collect_attempts tries have
// failed, and then stays for manual recovery (logged, never deleted); a record that cannot be
// read is removed, and logged.
func RecoverDelegatedWorktrees(ctx context.Context, stateDir string, settings *sessionfs.NativeSubagentSettings, logger *slog.Logger) []DelegatedWorktreeRecovery {
	if stateDir == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Join(stateDir, delegatedWorktreeRecordDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("read the records of delegated runs' worktrees", "dir", dir, "error", err)
		}
		return nil
	}
	var worktreeSettings sessionfs.NativeSubagentWorktreeSettings
	if settings != nil {
		worktreeSettings = settings.Worktree
	}
	options := worktreeOptions(worktreeSettings)
	self := currentServerIdentity()
	var recovered []DelegatedWorktreeRecovery
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		var record delegatedWorktreeRecord
		if err == nil {
			err = json.Unmarshal(data, &record)
		}
		if err != nil || !record.valid() {
			logger.Warn("drop an unreadable record of a delegated run's worktree", "record", path, "error", err)
			_ = os.Remove(path)
			continue
		}
		if !record.Pending && (record.Server == self || foreignServerAlive(self, record.Server)) {
			// A run that still goes, here or in another server on the same folders.
			continue
		}
		if record.Pending && record.Attempts >= options.CollectAttempts {
			logger.Warn("a delegated profile run's worktree could not be collected and is kept for manual recovery", "branch", record.Branch, "path", record.Path, "record", path, "attempts", record.Attempts, "error", record.Error)
			continue
		}
		lock, err := lockRecord(path + ".lock")
		if err != nil || lock == nil {
			// Another process is collecting it (or the lock cannot be made): not tried here.
			continue
		}
		recovery := recoverDelegatedWorktree(ctx, options, record, logger)
		recovered = append(recovered, recovery)
		switch {
		case recovery.Path == "":
			// Finished, or the folder is gone: nothing is left to try.
			_ = os.Remove(path)
		case recovery.Busy:
			// A process still holds the run's layers: tried again later, not counted.
		default:
			attempts := record.Attempts + 1
			if err := keepDelegatedWorktreeRecord(path, record, attempts, recovery.Error); err != nil {
				logger.Warn("keep the record of a delegated run's worktree whose collection failed", "record", path, "error", err)
			}
			if attempts >= options.CollectAttempts {
				logger.Warn("a delegated profile run's worktree could not be collected after repeated tries; it is kept for manual recovery and not tried again", "branch", record.Branch, "path", record.Path, "record", path, "attempts", attempts, "error", recovery.Error)
			}
		}
		_ = lock.Close()
		if recovery.Path == "" {
			_ = os.Remove(path + ".lock")
		}
	}
	return recovered
}

// recoverDelegatedWorktree finishes one lost run's worktree.
func recoverDelegatedWorktree(ctx context.Context, options worktree.Options, record delegatedWorktreeRecord, logger *slog.Logger) DelegatedWorktreeRecovery {
	recovery := DelegatedWorktreeRecovery{Branch: record.Branch}
	tree, err := worktree.Open(ctx, options, record.state())
	if err != nil {
		if !errors.Is(err, worktree.ErrGone) {
			recovery.Path, recovery.Error = record.Path, err.Error()
			recovery.Busy = errors.Is(err, worktree.ErrHeld)
		}
		logger.Warn("a delegated profile run's worktree was lost with the server and could not be finished", "branch", record.Branch, "path", record.Path, "error", err)
		return recovery
	}
	message := strings.TrimRight(record.Message, "\n")
	if title, rest, found := strings.Cut(message, "\n"); found {
		message = title + " (finished after the server stopped)\n" + rest
	} else {
		message += " (finished after the server stopped)"
	}
	outcome, err := recoverIsolatedTree(ctx, tree, message, options.Timeout)
	recovery.Changed = outcome.Changed
	if err != nil {
		recovery.Error = err.Error()
		if outcome.Kept {
			recovery.Path = record.Path
		}
		logger.Warn("finish a delegated profile run's worktree lost with the server", "branch", record.Branch, "path", recovery.Path, "error", err)
		return recovery
	}
	if outcome.Changed {
		logger.Info("a delegated profile run lost with the server left work; it is on its branch", "branch", record.Branch, "commit", outcome.Commit)
	} else {
		logger.Info("a delegated profile run lost with the server left no work; its worktree was removed", "branch", record.Branch)
	}
	return recovery
}
