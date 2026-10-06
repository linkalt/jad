package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/logging"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// File names under the session staging directory.
const (
	MetadataFile     = "checkpoints.json"
	GitDirectoryName = "checkpoints.git"

	schemaVersion = 1
	refPrefix     = "refs/checkpoints/"
	// maxGitOutputBytes is the largest single git output accepted: a listing or
	// diff of a workspace at the file-count ceiling.
	maxGitOutputBytes = 256 * 1024 * 1024
	// backupFilePattern matches backups write and edit leave beside their file;
	// restoring must not roll the safety copies back.
	backupFilePattern = "*.bak-[0-9]*-*"
)

// Kinds and states of a checkpoint record.
const (
	KindRun        = "run"
	KindPreRestore = "pre_restore"

	StateOpen     = "open"
	StateComplete = "complete"
	StateSkipped  = "skipped"
	StateFailed   = "failed"
)

// Restore modes.
const (
	ModeSafe = "safe"
	ModeFull = "full"
)

// Record is one checkpoint. The JSON shape is shared with the TypeScript backend.
type Record struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	TurnID      string `json:"turn_id"`
	SessionTurn int64  `json:"session_turn"`
	CreatedAt   string `json:"created_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	State       string `json:"state"`
	// BeforeTree is the workspace before the run changed it; empty when the
	// snapshot was skipped or failed.
	BeforeTree string `json:"before_tree,omitempty"`
	// AfterTree is the workspace when the run ended; empty while the run is open
	// or when it ended abnormally.
	AfterTree       string `json:"after_tree,omitempty"`
	FileCount       *int   `json:"file_count,omitempty"`
	RepositoryBytes *int64 `json:"repository_bytes,omitempty"`
	// Warning says why a snapshot was skipped or failed, or what a restore did.
	Warning string `json:"warning,omitempty"`
	// RestoredFrom is, for a pre_restore record, the checkpoint that was restored.
	RestoredFrom string `json:"restored_from,omitempty"`
}

// RestoreResult is what a restore (or a dry run of one) did.
type RestoreResult struct {
	CheckpointID string   `json:"checkpoint_id"`
	Mode         string   `json:"mode"`
	DryRun       bool     `json:"dry_run"`
	Restored     []string `json:"restored"`
	Deleted      []string `json:"deleted"`
	Conflicts    []string `json:"conflicts"`
	// UndoCheckpointID names the checkpoint that undoes this restore; absent for
	// a dry run or when nothing changed.
	UndoCheckpointID string `json:"undo_checkpoint_id,omitempty"`
}

// RestoreOptions selects a dry run and a mode; an empty Mode takes the setting's default.
type RestoreOptions struct {
	DryRun bool
	Mode   string
}

// Error codes of Error.
const (
	CodeNotFound    = "not_found"
	CodeConflict    = "conflict"
	CodeUnavailable = "unavailable"
)

// Error is a refusal the caller can turn into a response: not_found, conflict or unavailable.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// skipped marks a snapshot that was not taken on purpose (a limit), as opposed to one that failed.
type skipped struct{ reason string }

func (s *skipped) Error() string { return s.reason }

// Options configure a Service.
type Options struct {
	Settings sessionfs.CheckpointSettings
	// Workspace is the directory the agent works in; its files are what a checkpoint covers.
	Workspace string
	// StagingDir holds checkpoints.json and checkpoints.git.
	StagingDir string
	// HarnessPaths are absolute paths owned by the harness that may sit inside the
	// workspace and must never be snapshotted.
	HarnessPaths []string
	// Durability is the session's persistence durability: process, turn or strict.
	Durability string
	Logger     *slog.Logger
}

// Service is the checkpoint service of one session.
type Service struct {
	options      Options
	metadataPath string
	git          *shadowGit
	logger       *slog.Logger
	mu           *sync.Mutex
}

// locks serialize services that share one staging directory, so a run's
// snapshots and a restore never interleave their git commands.
var locks sync.Map

// NewService builds the service for the given options.
func NewService(options Options) *Service {
	mutex, _ := locks.LoadOrStore(filepath.Clean(options.StagingDir), &sync.Mutex{})
	return &Service{
		options:      options,
		metadataPath: filepath.Join(options.StagingDir, MetadataFile),
		git: &shadowGit{
			gitDir:    filepath.Join(options.StagingDir, GitDirectoryName),
			workDir:   options.Workspace,
			binary:    options.Settings.GitBinary,
			timeout:   time.Duration(options.Settings.TimeoutMS) * time.Millisecond,
			maxOutput: maxGitOutputBytes,
		},
		logger: logging.Or(options.Logger),
		mu:     mutex.(*sync.Mutex),
	}
}

// ForSession builds the service of a session from its resolved settings.
func ForSession(config sessionfs.SessionConfig, logger *slog.Logger) *Service {
	return NewService(Options{
		Settings:     config.Checkpoint,
		Workspace:    config.WorkingDir,
		StagingDir:   config.StagingDir,
		HarnessPaths: HarnessPaths(config),
		Durability:   config.Persistence.Durability,
		Logger:       logger,
	})
}

// Enabled reports whether the session records checkpoints.
func (s *Service) Enabled() bool { return s.options.Settings.Enabled }

// SnapshotOn is "first_mutation" or "run_start".
func (s *Service) SnapshotOn() string { return s.options.Settings.SnapshotOn }

// IncludeMCPTools reports whether a call to an MCP tool counts as a change.
func (s *Service) IncludeMCPTools() bool { return s.options.Settings.IncludeMCPTools }

// ---- exclusions ------------------------------------------------------------

func escapeGitPattern(path string) string {
	var escaped strings.Builder
	for _, character := range path {
		switch character {
		case '\\', '*', '?', '[', ']':
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(character)
	}
	result := escaped.String()
	if strings.HasPrefix(result, "!") || strings.HasPrefix(result, "#") {
		result = `\` + result
	}
	return result
}

// ExclusionPatterns are the patterns kept out of the private repository: harness
// files inside the workspace, and the backups write and edit make.
func ExclusionPatterns(workspace string, harnessPaths []string) []string {
	patterns := []string{backupFilePattern}
	seen := map[string]bool{backupFilePattern: true}
	for _, path := range harnessPaths {
		inside, err := filepath.Rel(workspace, path)
		if err != nil || inside == "" || inside == "." || strings.HasPrefix(inside, "..") || filepath.IsAbs(inside) {
			continue
		}
		pattern := "/" + escapeGitPattern(filepath.ToSlash(inside))
		if !seen[pattern] {
			seen[pattern] = true
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// HarnessPaths are the absolute harness-owned paths of a session that may fall inside its workspace.
func HarnessPaths(config sessionfs.SessionConfig) []string {
	storage := config.StoragePaths
	candidates := []string{
		config.StagingDir, config.MessagesPath, config.MessagesBackupPath, config.SettingPath, config.ModelPath,
		storage.Messages, storage.MessagesBackup, storage.CompactionCache, storage.HistoryCache, storage.UsageCache,
		storage.CursorCache, storage.RunErrorCache, storage.StateContext, storage.Staging, storage.HTTPDebug, storage.ToolStorage,
		config.UsageCachePath,
	}
	if config.NativeTask != nil {
		candidates = append(candidates, config.NativeTask.StorageDir)
	}
	if config.NativeMemory != nil {
		candidates = append(candidates, config.NativeMemory.StorageDir)
	}
	if config.NativeFetch != nil {
		candidates = append(candidates, config.NativeFetch.StorageDir)
	}
	if config.NativeProcess != nil {
		candidates = append(candidates, config.NativeProcess.StorageDir)
	}
	if config.NativeBash != nil {
		candidates = append(candidates, config.NativeBash.Spill.StorageDir)
	}
	if config.NativeWrite != nil && config.NativeWrite.BackupDir != nil {
		candidates = append(candidates, *config.NativeWrite.BackupDir)
	}
	if config.NativeEdit != nil && config.NativeEdit.BackupDir != nil {
		candidates = append(candidates, *config.NativeEdit.BackupDir)
	}
	// The session directory itself is harness-owned unless the workspace is that directory.
	if sessionDir := filepath.Dir(config.SettingPath); config.SettingPath != "" && filepath.Clean(sessionDir) != filepath.Clean(config.WorkingDir) {
		candidates = append(candidates, sessionDir)
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(config.WorkingDir, candidate)
		}
		paths = append(paths, filepath.Clean(candidate))
	}
	return paths
}

// ---- metadata --------------------------------------------------------------

func (s *Service) readAll() []Record {
	data, err := os.ReadFile(s.metadataPath)
	if err != nil {
		return nil
	}
	var document struct {
		Checkpoints []Record `json:"checkpoints"`
	}
	if json.Unmarshal(data, &document) != nil {
		// A damaged file only loses the list; the snapshots stay in the private repository.
		return nil
	}
	return document.Checkpoints
}

func (s *Service) writeAll(records []Record) error {
	if records == nil {
		records = []Record{}
	}
	encoded, err := json.Marshal(struct {
		SchemaVersion int      `json:"schema_version"`
		Checkpoints   []Record `json:"checkpoints"`
	}{schemaVersion, records})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.metadataPath), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(s.metadataPath, append(encoded, '\n'), s.options.Durability)
}

func writeFileAtomic(path string, data []byte, durability string) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if durability != "process" {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	if durability == "strict" {
		if directory, err := os.Open(filepath.Dir(path)); err == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}
	return nil
}

// List returns the checkpoints, newest first. Reading never needs git, so a
// missing binary does not hide the list.
func (s *Service) List() []Record {
	records := s.readAll()
	listed := make([]Record, 0, len(records))
	for index := len(records) - 1; index >= 0; index-- {
		listed = append(listed, records[index])
	}
	return listed
}

// ---- snapshots ---------------------------------------------------------------

func (s *Service) assertWorkspaceAllowed() error {
	workspace := filepath.Clean(s.options.Workspace)
	home, _ := os.UserHomeDir()
	if filepath.Dir(workspace) == workspace || (home != "" && workspace == filepath.Clean(home)) {
		return &skipped{fmt.Sprintf("the workspace %s is a filesystem root or the home directory; set a narrower cwd to use checkpoints", workspace)}
	}
	return nil
}

type capture struct {
	tree      string
	fileCount int
	bytes     int64
}

// capture stages the workspace and returns its tree, or a *skipped error over a limit.
func (s *Service) capture() (capture, error) {
	if err := s.assertWorkspaceAllowed(); err != nil {
		return capture{}, err
	}
	if err := s.git.prepare(ExclusionPatterns(s.options.Workspace, s.options.HarnessPaths)); err != nil {
		return capture{}, err
	}
	before, err := s.git.repositoryBytes()
	if err != nil {
		return capture{}, err
	}
	if before > s.options.Settings.MaxTotalBytes {
		return capture{}, &skipped{fmt.Sprintf("the checkpoint repository holds %d bytes, over checkpoint.max_total_bytes (%d)", before, s.options.Settings.MaxTotalBytes)}
	}
	tree, fileCount, err := s.git.snapshot()
	if err != nil {
		return capture{}, err
	}
	if int64(fileCount) > s.options.Settings.MaxFiles {
		return capture{}, &skipped{fmt.Sprintf("the workspace has %d files, over checkpoint.max_files (%d)", fileCount, s.options.Settings.MaxFiles)}
	}
	size, err := s.git.repositoryBytes()
	if err != nil {
		return capture{}, err
	}
	return capture{tree: tree, fileCount: fileCount, bytes: size}, nil
}

func (s *Service) keep(id, suffix, tree, message string) error {
	commit, err := s.git.commit(tree, message)
	if err != nil {
		return err
	}
	return s.git.setRef(refPrefix+id+"-"+suffix, commit)
}

func (s *Service) prune(records []Record) []Record {
	excess := int64(len(records)) - s.options.Settings.MaxCheckpoints
	if excess <= 0 {
		return records
	}
	for _, dropped := range records[:excess] {
		for _, suffix := range []string{"before", "after"} {
			_ = s.git.deleteRef(refPrefix + dropped.ID + "-" + suffix)
		}
	}
	return records[excess:]
}

func newCheckpointID() string {
	var random [4]byte
	_, _ = rand.Read(random[:])
	return "ckpt-" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(random[:])
}

func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func stateFor(err error) string {
	var skip *skipped
	if errors.As(err, &skip) {
		return StateSkipped
	}
	return StateFailed
}

// Begin records the state before a run changes anything. It never fails the run:
// a snapshot that cannot be taken is a record with a warning. The returned id is
// empty when no checkpoint is open.
func (s *Service) Begin(turnID string, sessionTurn int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := Record{ID: newCheckpointID(), Kind: KindRun, TurnID: turnID, SessionTurn: sessionTurn, CreatedAt: timestamp(), State: StateOpen}
	if captured, err := s.capture(); err != nil {
		record.State = stateFor(err)
		record.Warning = "Checkpoint not taken: " + err.Error()
		record.FinishedAt = record.CreatedAt
	} else if err := s.keep(record.ID, "before", captured.tree, "before "+turnID); err != nil {
		record.State = stateFor(err)
		record.Warning = "Checkpoint not taken: " + err.Error()
		record.FinishedAt = record.CreatedAt
	} else {
		record.BeforeTree = captured.tree
		record.FileCount = &captured.fileCount
		record.RepositoryBytes = &captured.bytes
	}
	if err := s.writeAll(s.prune(append(s.readAll(), record))); err != nil {
		// Without the record there is nothing to finish or restore later.
		s.logger.Error("checkpoint: cannot record checkpoint", "checkpoint", record.ID, "error", err)
		return ""
	}
	if record.State == StateOpen {
		return record.ID
	}
	return ""
}

// Finish records the state when the run ended. It never fails the run.
func (s *Service) Finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.readAll()
	index := -1
	for position := range records {
		if records[position].ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return
	}
	record := &records[index]
	captured, err := s.capture()
	if err == nil {
		err = s.keep(record.ID, "after", captured.tree, "after "+record.TurnID)
	}
	if err != nil {
		record.State = stateFor(err)
		record.Warning = "The end of the run was not recorded: " + err.Error()
	} else {
		record.AfterTree = captured.tree
		record.FileCount = &captured.fileCount
		record.RepositoryBytes = &captured.bytes
		record.State = StateComplete
	}
	record.FinishedAt = timestamp()
	if err := s.writeAll(records); err != nil {
		s.logger.Error("checkpoint: cannot finish checkpoint", "checkpoint", id, "error", err)
	}
}

// ---- restore -------------------------------------------------------------------

type plan struct {
	restore   []TreeChange
	remove    []TreeChange
	conflicts []string
}

func paths(changes []TreeChange) []string {
	listed := make([]string, 0, len(changes))
	for _, change := range changes {
		listed = append(listed, change.Path)
	}
	return listed
}

// Restore reverts what a checkpoint's run changed. A restore first records its
// own checkpoint, so it can be undone. It returns *Error for a refusal.
func (s *Service) Restore(id string, options RestoreOptions) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.readAll()
	var record *Record
	for position := range records {
		if records[position].ID == id {
			record = &records[position]
			break
		}
	}
	if record == nil {
		return RestoreResult{}, &Error{CodeNotFound, fmt.Sprintf("checkpoint %s does not exist", id)}
	}
	if record.BeforeTree == "" {
		reason := record.Warning
		if reason == "" {
			reason = record.State
		}
		return RestoreResult{}, &Error{CodeConflict, fmt.Sprintf("checkpoint %s has no saved state to restore (%s)", id, reason)}
	}
	mode := options.Mode
	if mode == "" {
		mode = ModeFull
		if s.options.Settings.SafeRestore {
			mode = ModeSafe
		}
	}
	if mode == ModeSafe && record.AfterTree == "" {
		return RestoreResult{}, &Error{CodeConflict, fmt.Sprintf("checkpoint %s has no run-end snapshot, so a safe restore cannot tell what the run changed; use mode \"full\"", id)}
	}
	result, err := s.restore(*record, mode, options.DryRun)
	if err == nil {
		return result, nil
	}
	var refusal *Error
	if errors.As(err, &refusal) {
		return RestoreResult{}, err
	}
	var skip *skipped
	var gitErr *GitError
	if errors.As(err, &skip) || errors.As(err, &gitErr) {
		return RestoreResult{}, &Error{CodeUnavailable, err.Error()}
	}
	return RestoreResult{}, err
}

func (s *Service) restore(record Record, mode string, dryRun bool) (RestoreResult, error) {
	if err := s.git.prepare(ExclusionPatterns(s.options.Workspace, s.options.HarnessPaths)); err != nil {
		return RestoreResult{}, err
	}
	current, err := s.capture()
	if err != nil {
		return RestoreResult{}, err
	}
	changes, err := s.plan(record, current.tree, mode)
	if err != nil {
		return RestoreResult{}, err
	}
	result := RestoreResult{
		CheckpointID: record.ID, Mode: mode, DryRun: dryRun,
		Restored: paths(changes.restore), Deleted: paths(changes.remove), Conflicts: changes.conflicts,
	}
	if result.Conflicts == nil {
		result.Conflicts = []string{}
	}
	if dryRun || (len(changes.restore) == 0 && len(changes.remove) == 0) {
		return result, nil
	}

	undo := Record{
		ID: newCheckpointID(), Kind: KindPreRestore, TurnID: "restore-" + record.ID, SessionTurn: record.SessionTurn,
		CreatedAt: timestamp(), State: StateOpen, BeforeTree: current.tree, RestoredFrom: record.ID,
	}
	if err := s.keep(undo.ID, "before", current.tree, "before restore of "+record.ID); err != nil {
		return RestoreResult{}, err
	}
	if err := s.git.checkout(record.BeforeTree, result.Restored); err != nil {
		return RestoreResult{}, err
	}
	workspace := filepath.Clean(s.options.Workspace)
	for _, change := range changes.remove {
		target := filepath.Join(workspace, filepath.FromSlash(change.Path))
		if !strings.HasPrefix(target, workspace+string(filepath.Separator)) {
			continue
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return RestoreResult{}, err
		}
	}
	after, err := s.capture()
	if err != nil {
		return RestoreResult{}, err
	}
	if err := s.keep(undo.ID, "after", after.tree, "after restore of "+record.ID); err != nil {
		return RestoreResult{}, err
	}
	undo.AfterTree = after.tree
	undo.FileCount = &after.fileCount
	undo.RepositoryBytes = &after.bytes
	undo.State = StateComplete
	undo.FinishedAt = timestamp()
	undo.Warning = fmt.Sprintf("Restored %d and deleted %d path(s) from %s", len(changes.restore), len(changes.remove), record.ID)
	if len(changes.conflicts) > 0 {
		undo.Warning += fmt.Sprintf("; left %d changed path(s) alone", len(changes.conflicts))
	}
	if err := s.writeAll(s.prune(append(s.readAll(), undo))); err != nil {
		return RestoreResult{}, err
	}
	result.UndoCheckpointID = undo.ID
	return result, nil
}

// plan decides which paths to write back, which to delete, and which the user
// changed after the run.
func (s *Service) plan(record Record, currentTree, mode string) (plan, error) {
	if mode == ModeFull {
		// Turn the current workspace into `before`: what exists only now goes, the rest is written back.
		changes, err := s.git.diff(currentTree, record.BeforeTree)
		if err != nil {
			return plan{}, err
		}
		var result plan
		for _, change := range changes {
			if change.Status == "D" {
				result.remove = append(result.remove, change)
			} else {
				result.restore = append(result.restore, change)
			}
		}
		return result, nil
	}
	ran, err := s.git.diff(record.BeforeTree, record.AfterTree)
	if err != nil {
		return plan{}, err
	}
	since, err := s.git.diff(record.AfterTree, currentTree)
	if err != nil {
		return plan{}, err
	}
	changedSince := make(map[string]bool, len(since))
	for _, change := range since {
		changedSince[change.Path] = true
	}
	var result plan
	for _, change := range ran {
		switch {
		case changedSince[change.Path]:
			result.conflicts = append(result.conflicts, change.Path)
		case change.Status == "A":
			result.remove = append(result.remove, change)
		default:
			result.restore = append(result.restore, change)
		}
	}
	return result, nil
}
