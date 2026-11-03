//go:build !windows

package native

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A live manifest write (here a stop's) ends in an fsync. It used to run with entry.mu held,
// the lock the child's output writers and every snapshot take, so a slow disk stalled the
// child's pipes (audit D1). Parked inside the write, the entry must still be readable and
// writable.
func TestLiveManifestWriteDoesNotHoldTheOutputLock(t *testing.T) {
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	snapshot, err := manager.Start(processTestRequest(directory, "sleep 30", 1024))
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.entries[snapshot.ID]
	manager.mu.Unlock()

	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	manager.liveHook = func(string) {
		once.Do(func() { close(parked) })
		<-release
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		manager.Stop(snapshot.ID, "test")
	}()
	<-parked

	free := make(chan struct{})
	go func() {
		defer close(free)
		_, _ = processStreamWriter{entry: entry, stream: bashStdout}.Write([]byte("output during the write\n"))
		manager.Snapshot(snapshot.ID)
	}()
	select {
	case <-free:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the output writer and the snapshot waited for a manifest write")
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
}
