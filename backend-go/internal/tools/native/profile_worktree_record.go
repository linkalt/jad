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
	temporary, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return "", fmt.Errorf("record the run's worktree: %w", err)
	}
	_, err = temporary.Write(encoded)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
		return "", fmt.Errorf("record the run's worktree: %w", err)
	}
	return path, nil
}

// DelegatedWorktreeRecovery is the end of one lost delegated run's worktree.
type DelegatedWorktreeRecovery struct {
	Branch  string
	Changed bool
	// Path is the worktree folder when it was kept; Error says why.
	Path  string
	Error string
}

// RecoverDelegatedWorktrees finishes the worktrees of the delegated runs recorded in
// stateDir whose server stopped while they went: what each run left is committed to its
// branch and its folder removed, on the host (the run's sandbox is not known any more), with
// settings' git options. A run of this server, or of another server that still runs, is left
// alone. Each record is removed once tried; a record that cannot be read is removed too, and
// logged.
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
		if record.Server == self || foreignServerAlive(self, record.Server) {
			// A run that still goes, here or in another server on the same folders.
			continue
		}
		recovered = append(recovered, recoverDelegatedWorktree(ctx, options, record, logger))
		_ = os.Remove(path)
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
	outcome, err := tree.Finish(ctx, message, nil)
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
