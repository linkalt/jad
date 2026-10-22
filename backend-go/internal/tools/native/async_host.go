package native

import (
	"fmt"
	"log/slog"
	"path/filepath"
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
	return &AsyncHost{manager: manager}, nil
}

// Manager is the async manager of the host.
func (h *AsyncHost) Manager() *asyncmgr.Manager { return h.manager }

// Config is the validated configuration of the manager.
func (h *AsyncHost) Config() asyncmgr.Config { return h.manager.Config() }

// Close stops every live handle (reason server_shutdown) and closes the manager.
func (h *AsyncHost) Close() { h.manager.Close() }

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
