package app

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

// heldBuilds replaces the resolver's persistent-manager builder with one that signals
// when it starts and waits for release, and counts how often it ran per directory.
type heldBuilds struct {
	started chan string
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   map[string]int
	built   []*native.SubagentManager
}

// let releases every held build; safe to call more than once.
func (h *heldBuilds) let() { h.once.Do(func() { close(h.release) }) }

func holdBuilds(t *testing.T, resolver *Resolver) *heldBuilds {
	t.Helper()
	held := &heldBuilds{started: make(chan string, 16), release: make(chan struct{}), calls: map[string]int{}}
	resolver.newPersistentSubagentManager = func(options native.SubagentManagerOptions) *native.SubagentManager {
		held.mu.Lock()
		held.calls[options.StorageDir]++
		held.mu.Unlock()
		held.started <- options.StorageDir
		<-held.release
		manager := native.NewSubagentManager()
		held.mu.Lock()
		held.built = append(held.built, manager)
		held.mu.Unlock()
		return manager
	}
	// Runs before the resolver's own Close (cleanups run last-in first): Close waits for
	// a pending build, so a test that fails while a build is held must release it first.
	t.Cleanup(held.let)
	return held
}

func (h *heldBuilds) count(dir string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[dir]
}

func persistentConfig(dir string) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: dir, PersistRuns: true, KeepFinished: 5}}
}

func newBuildTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	workspace := t.TempDir()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: workspace}}, map[string]string{}, CatalogConfig{MaxPageTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	return resolver, workspace
}

// receive takes one value from ch or fails the test after a limit, so a regression
// shows up as a failure and not as a hung test run.
func receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func within(t *testing.T, what string, limit time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not finish within %v", what, limit)
	}
}

// Building a persistent manager recovers, prunes and may delete many files. While one
// is being built, every other user of the resolver lock must still get through: this
// is the bug where the build ran with the lock held.
func TestSlowSubagentManagerBuildDoesNotBlockOtherResolverLockUsers(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	held := holdBuilds(t, resolver)
	dir := filepath.Join(workspace, "kids")

	built := make(chan *native.SubagentManager, 1)
	go func() { built <- resolver.sharedSubagentManager(persistentConfig(dir)) }()
	receive(t, "the build to start", held.started) // the build is under way and stays open

	within(t, "sharedTaskStore during a build", 2*time.Second, func() {
		if resolver.sharedTaskStore(sessionfs.SessionConfig{NativeTask: &sessionfs.NativeTaskSettings{StorageDir: filepath.Join(workspace, "tasks")}}) == nil {
			t.Error("no task store")
		}
	})
	within(t, "sharedSubagentManager of another directory during a build", 2*time.Second, func() {
		other := resolver.sharedSubagentManager(sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{StorageDir: filepath.Join(workspace, "memory-only")}})
		if other == nil {
			t.Error("no manager for an in-memory directory")
		}
	})
	within(t, "CancelSessionChildren during a build", 2*time.Second, func() { resolver.CancelSessionChildren("ref", "profile") })

	held.let()
	if manager := receive(t, "the build's manager", built); manager == nil {
		t.Fatal("the build produced no manager")
	}
}

// Two callers for one directory build it once and share the result; the recovery of a
// directory must not run twice.
func TestConcurrentCallersOfOneDirectoryBuildItOnce(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	held := holdBuilds(t, resolver)
	dir := filepath.Join(workspace, "kids")

	const callers = 6
	results := make(chan *native.SubagentManager, callers)
	for range callers {
		go func() { results <- resolver.sharedSubagentManager(persistentConfig(dir)) }()
	}
	receive(t, "the build to start", held.started)
	waitForWaiters(t, resolver, callers-1)
	held.let()

	var first *native.SubagentManager
	for range callers {
		manager := receive(t, "a caller's manager", results)
		if manager == nil {
			t.Fatal("a caller got no manager")
		}
		if first == nil {
			first = manager
		} else if manager != first {
			t.Fatal("callers of one directory got different managers")
		}
	}
	if got := held.count(dir); got != 1 {
		t.Fatalf("the directory was built %d times, want 1", got)
	}
	if again := resolver.sharedSubagentManager(persistentConfig(dir)); again != first {
		t.Fatal("a later caller did not get the published manager")
	}
}

// Two directories build side by side: a slow one does not queue the other behind it.
func TestDifferentDirectoriesBuildAtTheSameTime(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	held := holdBuilds(t, resolver)
	first, second := filepath.Join(workspace, "one"), filepath.Join(workspace, "two")

	var done atomic.Int32
	for _, dir := range []string{first, second} {
		go func() { resolver.sharedSubagentManager(persistentConfig(dir)); done.Add(1) }()
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case dir := <-held.started:
			seen[dir] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 builds started while the first was held: the builds are serialized", len(seen))
		}
	}
	held.let()
	within(t, "both builds", 2*time.Second, func() {
		for done.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
	})
}

// A resolver that closes while a manager is being built never publishes it, and the
// manager is closed (Close could not see it).
func TestResolverClosedDuringABuildClosesTheManagerAndPublishesNothing(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	held := holdBuilds(t, resolver)
	var built *native.SubagentManager
	finished := make(chan struct{})
	go func() {
		built = resolver.sharedSubagentManager(persistentConfig(filepath.Join(workspace, "kids")))
		close(finished)
	}()
	receive(t, "the build to start", held.started)

	// Close waits for the build (no file work of a build outlives Close) and the build
	// closes its own manager. Close must not finish before the build does.
	closeDone := make(chan struct{})
	go func() { resolver.Close(); close(closeDone) }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while a build was still running")
	case <-time.After(100 * time.Millisecond):
	}
	held.let()
	receive(t, "Close after the build ended", closeDone)
	within(t, "the build after Close", 2*time.Second, func() { <-finished })

	if built != nil {
		t.Error("a closed resolver handed out the manager")
	}
	held.mu.Lock()
	orphans := append([]*native.SubagentManager(nil), held.built...)
	held.mu.Unlock()
	if len(orphans) != 1 || !orphans[0].Closed() {
		t.Errorf("the manager built while the resolver closed was not closed (%d built)", len(orphans))
	}
	if got := resolver.sharedSubagentManager(persistentConfig(filepath.Join(workspace, "late"))); got != nil {
		t.Error("a closed resolver built a new manager")
	}
	resolver.mu.Lock()
	published, building := len(resolver.subagentManagers), len(resolver.subagentBuilds)
	resolver.mu.Unlock()
	if published != 0 || building != 0 {
		t.Errorf("after Close: %d managers published, %d builds pending, want 0 and 0", published, building)
	}
}

// A builder that panics must not leave the directory marked as being built: the next
// caller builds it.
func TestAPanickingBuildDoesNotStrandWaiters(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	dir := filepath.Join(workspace, "kids")
	var attempts atomic.Int32
	resolver.newPersistentSubagentManager = func(native.SubagentManagerOptions) *native.SubagentManager {
		if attempts.Add(1) == 1 {
			panic("disk gone")
		}
		return native.NewSubagentManager()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		resolver.sharedSubagentManager(persistentConfig(dir))
	}()
	within(t, "the call after a panicking build", 2*time.Second, func() {
		if resolver.sharedSubagentManager(persistentConfig(dir)) == nil {
			t.Error("no manager after the first build panicked")
		}
	})
	if attempts.Load() != 2 {
		t.Errorf("builder ran %d times, want 2", attempts.Load())
	}
}

// waitForWaiters blocks until n callers are parked on the pending build of the only
// directory in flight. They are goroutines inside sharedSubagentManager; the resolver
// has no counter for them, so the test counts goroutines parked on the build channel.
func waitForWaiters(t *testing.T, resolver *Resolver, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if parkedOnBuild() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("fewer than %d callers reached the wait for the pending build", n)
}

func waitUntilClosed(t *testing.T, resolver *Resolver) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resolver.mu.Lock()
		closed := resolver.closed
		resolver.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the resolver did not close")
}

func parkedOnBuild() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "(*Resolver).sharedSubagentManager") - strings.Count(string(buf), "finishSubagentBuild")
}

// A caller parked on a build whose builder panics is released and builds the
// directory itself.
func TestAWaiterOfAPanickingBuildBuildsItselfAfterwards(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	dir := filepath.Join(workspace, "kids")
	release := make(chan struct{})
	var attempts atomic.Int32
	started := make(chan struct{}, 4)
	resolver.newPersistentSubagentManager = func(native.SubagentManagerOptions) *native.SubagentManager {
		if attempts.Add(1) == 1 {
			started <- struct{}{}
			<-release
			panic("disk gone")
		}
		return native.NewSubagentManager()
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	go func() {
		defer func() { _ = recover() }()
		resolver.sharedSubagentManager(persistentConfig(dir))
	}()
	receive(t, "the first build to start", started)
	waiter := make(chan *native.SubagentManager, 1)
	go func() { waiter <- resolver.sharedSubagentManager(persistentConfig(dir)) }()
	waitForWaiters(t, resolver, 1)
	close(release)
	manager := receive(t, "the waiter's manager", waiter)
	if manager == nil {
		t.Fatal("the waiter got no manager after the first build panicked")
	}
	if attempts.Load() != 2 {
		t.Errorf("builder ran %d times, want 2 (the panicking one and the waiter's)", attempts.Load())
	}
}

// A caller parked on a build when the resolver closes gets nil, not a manager the
// resolver would never close.
func TestAWaiterGetsNilWhenTheResolverClosesDuringTheBuild(t *testing.T) {
	resolver, workspace := newBuildTestResolver(t)
	held := holdBuilds(t, resolver)
	dir := filepath.Join(workspace, "kids")
	builder := make(chan *native.SubagentManager, 1)
	go func() { builder <- resolver.sharedSubagentManager(persistentConfig(dir)) }()
	receive(t, "the build to start", held.started)
	waiter := make(chan *native.SubagentManager, 1)
	go func() { waiter <- resolver.sharedSubagentManager(persistentConfig(dir)) }()
	waitForWaiters(t, resolver, 1)

	closeDone := make(chan struct{})
	go func() { resolver.Close(); close(closeDone) }()
	// Release the build only once Close has marked the resolver closed; before that the
	// build would publish normally and the test would prove nothing.
	waitUntilClosed(t, resolver)
	held.let()
	receive(t, "Close", closeDone)
	if got := receive(t, "the builder's result", builder); got != nil {
		t.Error("the builder of a closed resolver got a manager")
	}
	if got := receive(t, "the waiter's result", waiter); got != nil {
		t.Error("the waiter of a closed resolver got a manager")
	}
}
