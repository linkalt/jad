package native

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// subagentManifestVersion is the version written into every manifest. A manifest of
// another version is skipped, not guessed at.
const subagentManifestVersion = 1

// subagentManifestDir is the folder of run manifests under the storage directory. It
// sits beside the per-child transcript folders and holds only small JSON files.
const subagentManifestDir = ".runs"

var subagentManifestName = regexp.MustCompile(`^(sub_[0-9a-f]{8})\.json$`)

// subagentManifest is what survives a restart about one run. It holds no answer: the
// answer is already in the child's transcript, and is read back from there.
type subagentManifest struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	// Session and Root are the session of the run tree and its top-level run. A manifest
	// that predates them has none, and such a run keeps the owner as its only key.
	Session   string    `json:"session,omitempty"`
	Root      string    `json:"root,omitempty"`
	Agent     string    `json:"agent"`
	Task      string    `json:"task"`
	TimeoutMS int64     `json:"timeout_ms"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Rounds    int       `json:"rounds,omitempty"`
	Tokens    int64     `json:"tokens,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Cut       bool      `json:"cut,omitempty"`
	Changes   bool      `json:"changes,omitempty"`
	// Files are the files the child's write and edit changed, FilesOmitted how many more
	// there were than the list keeps.
	Files        []subagentFileChange `json:"files,omitempty"`
	FilesOmitted int                  `json:"files_omitted,omitempty"`
	Tracked      bool                 `json:"tracked,omitempty"`
	// Worktree is the git worktree of a run with isolation worktree: the branch that holds
	// its changes. Absent for any other run.
	Worktree *subagentWorktree `json:"worktree,omitempty"`
	// Config is the fingerprint of the agent definition the run started with; see
	// subagentFingerprint. Absent in a manifest written before it existed, and then no
	// config note is made.
	Config string `json:"config,omitempty"`
	// ResumedFrom is the run this one was resumed from; empty for a run that was not.
	ResumedFrom string `json:"resumed_from,omitempty"`
	// ResumedConfig is the source run's fingerprint at the time of the resume; the config note
	// is rebuilt from it and Config after a restart. Empty when the source had none.
	ResumedConfig string `json:"resumed_config,omitempty"`
	// Caller is the remote caller the run was started for; empty for a local run. A remote
	// resume after a restart still checks it.
	Caller string `json:"caller,omitempty"`
	// Server is the process that last wrote the manifest. A manifest without it comes
	// from a version that did not record one and is treated as owned by no live server.
	Server *serverIdentity `json:"server,omitempty"`
}

// subagentStore keeps one manifest file per run. Writes are best effort: a run goes on
// when its manifest cannot be written, and the failure is logged.
type subagentStore struct {
	dir    string // the manifest folder
	root   string // the storage directory; a run's transcript must lie inside it
	logger *slog.Logger
	// identity is written into every manifest, so another server that starts on the same
	// directory can tell that these runs are still going.
	identity serverIdentity
	mu       sync.Mutex
}

func newSubagentStore(storageDir string, logger *slog.Logger) *subagentStore {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	root := filepath.Clean(storageDir)
	return &subagentStore{dir: filepath.Join(root, subagentManifestDir), root: root, logger: logger, identity: currentServerIdentity()}
}

// save writes the run's current state. The snapshot is taken while holding the store
// lock, so the file on disk is always the newest state that any caller saw.
func (s *subagentStore) save(run *subagentRun) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest := run.manifest()
	manifest.Server = &s.identity
	if err := s.write(manifest); err != nil {
		s.logger.Warn("save sub-agent run manifest", "run", run.id, "error", err)
	}
}

func (s *subagentStore) write(manifest subagentManifest) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.dir, "run-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(s.dir, manifest.ID+".json"))
}

// remove deletes a run's manifest. The child's transcript folder is left alone.
func (s *subagentStore) remove(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("remove sub-agent run manifest", "run", id, "error", err)
	}
}

// load reads every manifest. A file that is unreadable, of another version, named
// differently from the id it holds, or in a state this code does not know is skipped
// and logged. A transcript path outside the storage directory is dropped, so a
// manifest cannot point the reader at another file.
func (s *subagentStore) load() []subagentManifest {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("read sub-agent run manifests", "dir", s.dir, "error", err)
		}
		return nil
	}
	var manifests []subagentManifest
	for _, entry := range entries {
		match := subagentManifestName.FindStringSubmatch(entry.Name())
		if match == nil || entry.IsDir() {
			continue
		}
		manifest, err := s.read(filepath.Join(s.dir, entry.Name()), match[1])
		if err != nil {
			s.logger.Warn("skip sub-agent run manifest", "file", entry.Name(), "error", err)
			continue
		}
		manifests = append(manifests, manifest)
	}
	return manifests
}

func (s *subagentStore) read(path, id string) (subagentManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return subagentManifest{}, err
	}
	var manifest subagentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return subagentManifest{}, err
	}
	switch {
	case manifest.Version != subagentManifestVersion:
		return subagentManifest{}, fmt.Errorf("version %d is not %d", manifest.Version, subagentManifestVersion)
	case manifest.ID != id:
		return subagentManifest{}, fmt.Errorf("holds id %q", manifest.ID)
	case !knownSubagentRunState(manifest.State):
		return subagentManifest{}, fmt.Errorf("unknown state %q", manifest.State)
	case manifest.StartedAt.IsZero():
		return subagentManifest{}, fmt.Errorf("has no start time")
	}
	if manifest.Dir != "" && !s.inside(manifest.Dir) {
		s.logger.Warn("drop sub-agent transcript outside the storage directory", "run", id, "dir", manifest.Dir)
		manifest.Dir = ""
	}
	return manifest, nil
}

func (s *subagentStore) inside(dir string) bool { return insideDirectory(s.root, dir) }

// insideDirectory reports whether dir lies strictly below root.
func insideDirectory(root, dir string) bool {
	relative, err := filepath.Rel(root, filepath.Clean(dir))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// transcriptFolderUnder reports whether dir is a child's transcript folder directly
// under the storage directory: one path element below it, and not the manifest folder.
// Only such a folder is ever deleted, so a bad manifest cannot aim a delete elsewhere.
func transcriptFolderUnder(root, dir string) bool {
	if root == "" || dir == "" || !insideDirectory(root, dir) {
		return false
	}
	relative, err := filepath.Rel(root, filepath.Clean(dir))
	return err == nil && !strings.ContainsRune(relative, filepath.Separator) && relative != subagentManifestDir
}

func knownSubagentRunState(state string) bool {
	switch state {
	case subagentRunQueued, subagentRunRunning, subagentRunDone, subagentRunFailed, subagentRunTimedOut, subagentRunStopped, subagentRunLost:
		return true
	}
	return false
}

// readSubagentManifest is where a run of another profile's storage directory has its transcript
// folder and who started it, read from the run's manifest without touching the directory.
func readSubagentManifest(storageDir, id string) (dir, owner, agent string, found bool) {
	if storageDir == "" {
		return "", "", "", false
	}
	store := newSubagentStore(storageDir, nil)
	manifest, err := store.read(filepath.Join(store.dir, id+".json"), id)
	if err != nil {
		return "", "", "", false
	}
	return manifest.Dir, manifest.Owner, manifest.Agent, true
}

// subagentRunIDs lists the runs that have a manifest in a storage directory.
func subagentRunIDs(storageDir string, mayAgent func(string) bool) []string {
	if storageDir == "" {
		return nil
	}
	var ids []string
	for _, manifest := range newSubagentStore(storageDir, nil).load() {
		if mayAgent != nil && !mayAgent(manifest.Agent) {
			continue
		}
		ids = append(ids, manifest.ID)
	}
	return ids
}
