package app

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
)

func storeTestResolver(t *testing.T, open func(sessionfs.SessionConfig, transcript.TurnStoreOptions, transcript.DurabilityMode) (*sharedSession, error)) *Resolver {
	t.Helper()
	resolver, err := NewResolver([]sessionfs.Root{{ID: "local", Directory: t.TempDir()}}, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	resolver.openStore = open
	return resolver
}

func storeTestConfig(dir string) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{MessagesPath: filepath.Join(dir, "messages.jsonl"), StagingDir: filepath.Join(dir, "staging")}
}

func fakeSession(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) *sharedSession {
	return &sharedSession{staging: filepath.Clean(config.StagingDir), durability: durability, options: options, runners: map[string]*session.Runner{}}
}

func TestSharedStoreOpensDifferentKeysInParallel(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		entered <- config.MessagesPath
		<-release
		return fakeSession(config, options, durability), nil
	})
	var done sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		done.Add(1)
		go func() {
			defer done.Done()
			if _, err := resolver.sharedStore(storeTestConfig(filepath.Join(t.TempDir(), name))); err != nil {
				t.Error(err)
			}
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("the second store did not start opening while the first was still opening")
		}
	}
	close(release)
	done.Wait()
}

func TestSharedStoreDoesNotBlockTheResolverWhileOpening(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		close(entered)
		<-release
		return fakeSession(config, options, durability), nil
	})
	go func() { _, _ = resolver.sharedStore(storeTestConfig(t.TempDir())) }()
	<-entered
	locked := make(chan struct{})
	go func() {
		resolver.mu.Lock()
		resolver.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(3 * time.Second):
		t.Error("Resolver.mu stayed held while a transcript store was opening")
	}
	close(release)
}

func TestSharedStoreCoalescesConcurrentOpensOfOneKey(t *testing.T) {
	var opens atomic.Int32
	release := make(chan struct{})
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		opens.Add(1)
		<-release
		return fakeSession(config, options, durability), nil
	})
	config := storeTestConfig(t.TempDir())
	results := make(chan *sharedSession, 8)
	for range 8 {
		go func() {
			shared, err := resolver.sharedStore(config)
			if err != nil {
				t.Error(err)
			}
			results <- shared
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	first := <-results
	for range 7 {
		if got := <-results; got != first {
			t.Fatal("callers of one key got different stores")
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("store opened %d times, want once", got)
	}
}

func TestSharedStoreRetriesAfterAFailedOpen(t *testing.T) {
	var opens atomic.Int32
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		switch opens.Add(1) {
		case 1:
			return nil, errors.New("disk full")
		case 2:
			panic("boom")
		}
		return fakeSession(config, options, durability), nil
	})
	config := storeTestConfig(t.TempDir())
	if _, err := resolver.sharedStore(config); err == nil || err.Error() != "disk full" {
		t.Fatalf("first error = %v", err)
	}
	if _, err := resolver.sharedStore(config); err == nil {
		t.Fatal("a panicking open must come back as an error")
	}
	if shared, err := resolver.sharedStore(config); err != nil || shared == nil {
		t.Fatalf("third open = %v, %v: a failed build must not be cached", shared, err)
	}
}

func TestSharedStoreStillRejectsIncompatibleSettings(t *testing.T) {
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		return fakeSession(config, options, durability), nil
	})
	config := storeTestConfig(t.TempDir())
	if _, err := resolver.sharedStore(config); err != nil {
		t.Fatal(err)
	}
	other := config
	other.StagingDir = filepath.Join(filepath.Dir(config.StagingDir), "other-staging")
	if _, err := resolver.sharedStore(other); err == nil {
		t.Fatal("a different staging directory for the same transcript was accepted")
	}
}

func TestSharedStoreForgottenDuringOpenIsNotHandedOut(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var opens atomic.Int32
	resolver := storeTestResolver(t, func(config sessionfs.SessionConfig, options transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
		if opens.Add(1) == 1 {
			close(entered)
			<-release
		}
		return fakeSession(config, options, durability), nil
	})
	dir := t.TempDir()
	config := storeTestConfig(dir)
	result := make(chan error, 1)
	go func() {
		_, err := resolver.sharedStore(config)
		result <- err
	}()
	<-entered
	resolver.forgetStores(dir)
	close(release)
	if err := <-result; !errors.Is(err, errStoreForgotten) {
		t.Fatalf("error = %v, want the session-deleted error", err)
	}
	if shared, err := resolver.sharedStore(config); err != nil || shared == nil {
		t.Fatalf("open after the forget = %v, %v", shared, err)
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("opens = %d, want 2: the forgotten store must not be reused", got)
	}
}
