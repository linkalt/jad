package app

import (
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools/native"
)

func TestAsyncHostOpenDoesNotBlockResolver(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	slow := native.AsyncDir(filepath.Join(workspace, "slow"))
	other := native.AsyncDir(filepath.Join(workspace, "other"))
	started := make(chan string, 4)
	release := make(chan struct{})
	var slowOpens atomic.Int32
	resolver.openAsyncHost = func(dir string, cfg asyncmgr.Config, logger *slog.Logger) (*native.AsyncHost, error) {
		if dir == filepath.Clean(slow) {
			slowOpens.Add(1)
			started <- dir
			<-release
		}
		return native.NewAsyncHost(dir, cfg, logger)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	results := make(chan error, 2)
	open := func(dir string) {
		_, err := resolver.AsyncHost(native.AsyncHostConfig{Dir: dir, Config: asyncmgr.DefaultConfig()})
		results <- err
	}
	go open(slow)
	receive(t, "the slow open to start", started)
	go open(slow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resolver.asyncManagers()
		if _, err := resolver.AsyncHost(native.AsyncHostConfig{Dir: other, Config: asyncmgr.DefaultConfig()}); err != nil {
			t.Error(err)
		}
	}()
	receive(t, "other Resolver calls while an open is held", done)

	close(release)
	for range 2 {
		if err := receive(t, "an AsyncHost result", results); err != nil {
			t.Fatal(err)
		}
	}
	if got := slowOpens.Load(); got != 1 {
		t.Fatalf("the directory was opened %d times, want 1", got)
	}
}
