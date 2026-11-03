package app

import (
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
)

// idleStoreResolver is a resolver whose stores leave the cache after idle and whose opener
// counts the opens.
func idleStoreResolver(t *testing.T, idle time.Duration) (*Resolver, *int) {
	t.Helper()
	opens := 0
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		opens++
		return fakeSession(config, options, durability), nil
	})
	resolver.catalog.StoreIdle = idle
	return resolver, &opens
}

func cachedStore(resolver *Resolver, config sessionfs.SessionConfig) *storeEntry {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.stores[filepath.Clean(config.MessagesPath)]
}

// waitForDrop waits until the store of config has left the cache.
func waitForDrop(t *testing.T, resolver *Resolver, config sessionfs.SessionConfig) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); cachedStore(resolver, config) != nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle store never left the cache")
		}
	}
}

func TestAStoreNobodyUsesLeavesTheCacheAfterTheIdleTime(t *testing.T) {
	resolver, opens := idleStoreResolver(t, 20*time.Millisecond)
	config := storeTestConfig(t.TempDir())
	first, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	release()
	waitForDrop(t, resolver, config)
	second, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if second == first || *opens != 2 {
		t.Fatalf("the next use after the drop must open the store again: same=%v opens=%d", second == first, *opens)
	}
}

func TestAStoreInUseStaysCachedPastTheIdleTime(t *testing.T) {
	resolver, opens := idleStoreResolver(t, 10*time.Millisecond)
	config := storeTestConfig(t.TempDir())
	first, releaseFirst, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	// A short use that ends while the long one goes on must not start the idle clock.
	_, releaseShort, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	releaseShort()
	releaseShort() // a second release must not end the long use
	time.Sleep(60 * time.Millisecond)
	again, releaseAgain, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if again != first || *opens != 1 {
		t.Fatalf("a store in use left the cache: same=%v opens=%d", again == first, *opens)
	}
	releaseAgain()
	releaseFirst()
	waitForDrop(t, resolver, config)
}

func TestAUseThatStartsBeforeTheDropKeepsTheStore(t *testing.T) {
	resolver, opens := idleStoreResolver(t, 300*time.Millisecond)
	config := storeTestConfig(t.TempDir())
	_, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Uses keep arriving within the idle time: the idle period restarts every time.
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		_, release, err := resolver.sharedStore(config)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if cachedStore(resolver, config) == nil || *opens != 1 {
		t.Fatalf("a store used within the idle time was dropped: opens=%d", *opens)
	}
}

func TestZeroStoreIdleKeepsStoresCached(t *testing.T) {
	resolver, _ := idleStoreResolver(t, 0)
	config := storeTestConfig(t.TempDir())
	_, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	release()
	time.Sleep(20 * time.Millisecond)
	entry := cachedStore(resolver, config)
	if entry == nil || entry.idleTimer != nil {
		t.Fatal("with session_store_idle_ms 0 a store stays cached and no drop is scheduled")
	}
}

// TestBoundRunnerAndMutationUseTheCachedStoreAfterADrop pins why the bound values lease
// per operation: a runner resolved before its store was dropped must not write through
// the dropped store while another request uses the reopened one, or two run locks
// would guard one transcript.
func TestBoundRunnerAndMutationUseTheCachedStoreAfterADrop(t *testing.T) {
	resolver, _ := idleStoreResolver(t, 10*time.Millisecond)
	config := storeTestConfig(t.TempDir())
	resolved, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	release()
	runner := boundRunner{resolver: resolver, shared: resolved, config: config}
	mutation := boundMutation{resolver: resolver, shared: resolved, config: config}
	waitForDrop(t, resolver, config)
	current, releaseCurrent, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCurrent()
	if current == resolved {
		t.Fatal("the store was not reopened after the drop")
	}
	releaseRunner, err := runner.hold()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRunner()
	releaseMutation, err := mutation.hold()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMutation()
	if runner.shared != current || mutation.shared != current {
		t.Fatal("a bound runner or mutation kept the dropped store instead of the cached one")
	}
}

func TestCloseCancelsPendingDrops(t *testing.T) {
	resolver, _ := idleStoreResolver(t, time.Hour)
	config := storeTestConfig(t.TempDir())
	_, release, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	release()
	entry := cachedStore(resolver, config)
	if entry == nil || entry.idleTimer == nil {
		t.Fatal("the release of the last use must schedule the drop")
	}
	resolver.Close()
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if entry.idleTimer != nil {
		t.Fatal("Close must stop the pending drop timers")
	}
}
