package native

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"jad/backend-go/internal/asyncmgr"
)

// asyncDirName is the folder of the async records inside a profile's tool storage.
const asyncDirName = ".async"

// AsyncHost binds one async manager to the drivers of the tools that start handles. One
// host serves every session that shares a tool storage folder; the owner of a handle is
// the session key of the call that started it.
type AsyncHost struct {
	manager *asyncmgr.Manager
	dir     string

	mu sync.Mutex
	// closed is set by Close before it stops the handles: a run that would start after it
	// is refused, so nothing is added to shellRuns once Close takes its list.
	closed       bool
	shellDrivers map[asyncmgr.Kind]bool
	// shellRuns holds every run whose goroutine has not completed yet; Close waits for
	// those it stopped, so a finisher cannot outlive the host.
	shellRuns    map[string]*shellRun
	shellSwept   map[string]time.Time
	processes    []processStopper
	processAdded bool
	subagents    []*SubagentManager
	subagentSeen bool

	profileMessageSeen bool
	remoteSubagentSeen bool
	profileMessages    map[string]ProfileMessageRun
	// profileStarting holds the handles registered but not yet bound to a run, with the
	// reason of a stop that arrived meanwhile.
	profileStarting map[string]string
}

// AsyncResources is implemented by the application resolver: it hands out the host shared
// by every session of one tool storage folder. A nil AsyncResources makes each catalog
// open its own host, which suits tests.
type AsyncResources interface {
	AsyncHost(config AsyncHostConfig) (*AsyncHost, error)
}

// AsyncHostConfig is what a resolver needs to find or open a host.
type AsyncHostConfig struct {
	Dir    string
	Config asyncmgr.Config
}

// AsyncDir is the async folder inside a tool storage folder.
func AsyncDir(toolStorage string) string { return filepath.Join(toolStorage, asyncDirName) }

// NewAsyncHost opens the async manager in dir (see asyncmgr.Open).
func NewAsyncHost(dir string, cfg asyncmgr.Config, logger *slog.Logger) (*AsyncHost, error) {
	self := currentServerIdentity()
	manager, err := asyncmgr.Open(asyncmgr.Options{
		Dir:    dir,
		Config: cfg,
		Server: asyncmgr.ServerID{PID: self.PID, Start: self.Start},
		ServerAlive: func(id asyncmgr.ServerID) bool {
			return foreignServerAlive(self, serverIdentity{PID: id.PID, Start: id.Start})
		},
		StopWait: time.Duration(cfg.StopWaitMs) * time.Millisecond,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	return &AsyncHost{manager: manager, dir: dir}, nil
}

// Manager is the async manager of the host.
func (h *AsyncHost) Manager() *asyncmgr.Manager { return h.manager }

// Config is the validated configuration of the manager.
func (h *AsyncHost) Config() asyncmgr.Config { return h.manager.Config() }

// Close stops every live handle (reason server_shutdown), waits for the runs it stopped to
// finish, and closes the manager.
func (h *AsyncHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()
	h.manager.Close()
	h.waitShellRuns()
}

// waitShellRuns waits for the shell-run goroutines the stop above just released the
// children of. Each run is bounded by its own kill grace: the stop sends SIGTERM and kills
// the group when the grace ends, so the goroutine writes its result and completes the
// handle within that grace, and a shutdown that waited past it would hang on a child that
// cannot be killed.
func (h *AsyncHost) waitShellRuns() {
	h.mu.Lock()
	runs := make([]*shellRun, 0, len(h.shellRuns))
	for _, run := range h.shellRuns {
		runs = append(runs, run)
	}
	h.mu.Unlock()
	for _, run := range runs {
		if run.grace <= 0 {
			continue
		}
		timer := time.NewTimer(run.grace)
		select {
		case <-run.done:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// asyncHostFor finds the shared host of the session's tool storage, or opens a private one.
func asyncHostFor(config asyncHostSource, resources AsyncResources) (*AsyncHost, error) {
	if config.dir == "" {
		return nil, fmt.Errorf("the async tool needs the profile's tool storage folder")
	}
	cfg := AsyncHostConfig{Dir: AsyncDir(config.dir), Config: config.cfg}
	if resources != nil {
		return resources.AsyncHost(cfg)
	}
	return NewAsyncHost(cfg.Dir, cfg.Config, nil)
}

type asyncHostSource struct {
	dir string
	cfg asyncmgr.Config
}

// attachProcess lets the process driver reach a process manager. The first call registers
// the driver; attaching the same manager again is a no-op.
func (h *AsyncHost) attachProcess(stopper processStopper) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, attached := range h.processes {
		if attached == stopper {
			return
		}
	}
	h.processes = append(h.processes, stopper)
	if !h.processAdded {
		h.processAdded = true
		_ = h.manager.AddDriver(&processDriver{host: h})
	}
}

func (h *AsyncHost) processManagers() []processStopper {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]processStopper(nil), h.processes...)
}

// attachSubagent lets the sub-agent driver reach a sub-agent manager. The first call
// registers the driver; attaching the same manager again is a no-op.
func (h *AsyncHost) attachSubagent(manager *SubagentManager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, attached := range h.subagents {
		if attached == manager {
			return
		}
	}
	h.subagents = append(h.subagents, manager)
	if !h.subagentSeen {
		h.subagentSeen = true
		_ = h.manager.AddDriver(&subagentDriver{host: h})
	}
}

func (h *AsyncHost) subagentManagers() []*SubagentManager {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*SubagentManager(nil), h.subagents...)
}
