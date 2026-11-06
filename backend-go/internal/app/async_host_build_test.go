package app

import (
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

// silentDriver never reports a stop, so Close waits its whole stop wait for each handle.
type silentDriver struct{}

func (silentDriver) Kind() asyncmgr.Kind { return "silent" }
func (silentDriver) Stop(string, string) {}

// The async hosts of a resolver are independent folders, so Close closes them together:
// a shutdown waits for the slowest host, not for the sum of their stop waits.
func TestResolverClosesAsyncHostsTogether(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	cfg := asyncmgr.DefaultConfig()
	cfg.StopWaitMs = 800
	for _, name := range []string{"one", "two", "three"} {
		host, err := resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(filepath.Join(workspace, name)), Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Manager().AddDriver(silentDriver{}); err != nil {
			t.Fatal(err)
		}
		if err := host.Manager().Register(asyncmgr.Registration{ID: name, Owner: "s1", Kind: "silent", Label: name}); err != nil {
			t.Fatal(err)
		}
	}
	began := time.Now()
	resolver.Close()
	if took := time.Since(began); took > 1600*time.Millisecond {
		t.Fatalf("Close took %v for three hosts that each wait 800ms; want them closed together", took)
	}
}

// A host whose open ends after the resolver closed is closed by its builder, and Close waits
// for that close: the host's last writes (the end of its handles, its final index) do not
// outlive Resolver.Close.
func TestResolverCloseWaitsForAHostBuiltAfterIt(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	cfg := asyncmgr.DefaultConfig()
	cfg.StopWaitMs = 400
	opened := make(chan *native.AsyncHost, 1)
	release := make(chan struct{})
	resolver.openAsyncHost = func(dir string, cfg asyncmgr.Config, logger *slog.Logger) (*native.AsyncHost, error) {
		host, err := native.NewAsyncHost(dir, cfg, logger)
		if err != nil {
			return nil, err
		}
		// A handle that never reports holds the host's Close for its stop wait.
		if err := host.Manager().AddDriver(silentDriver{}); err != nil {
			return nil, err
		}
		if err := host.Manager().Register(asyncmgr.Registration{ID: "slow", Owner: "s1", Kind: "silent", Label: "slow"}); err != nil {
			return nil, err
		}
		opened <- host
		<-release
		return host, nil
	}
	results := make(chan error, 1)
	go func() {
		_, err := resolver.AsyncHost(native.AsyncHostConfig{Dir: native.AsyncDir(filepath.Join(workspace, "late")), Config: cfg})
		results <- err
	}()
	host := receive(t, "the open to start", opened)
	closed := make(chan struct{})
	go func() {
		resolver.Close()
		close(closed)
	}()
	for deadline := time.Now().Add(3 * time.Second); ; {
		resolver.mu.Lock()
		isClosed := resolver.closed
		resolver.mu.Unlock()
		if isClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the resolver did not close")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	receive(t, "Resolver.Close to return", closed)
	if entry, err := host.Manager().Status("s1", "slow"); err != nil || entry.State != asyncmgr.StateLost {
		t.Fatalf("handle when Resolver.Close returned = %+v, %v; want lost: the host was still closing", entry, err)
	}
	if err := receive(t, "the AsyncHost result", results); err == nil {
		t.Fatal("a host opened after Close was handed out")
	}
}
