package app

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

func storeConfig(settingPath, dir string) sessionfs.SessionConfig {
	return sessionfs.SessionConfig{
		SettingPath:  settingPath,
		NativeTask:   &sessionfs.NativeTaskSettings{StorageDir: filepath.Join(dir, "tasks")},
		NativeMemory: &sessionfs.NativeMemorySettings{StorageDir: filepath.Join(dir, "memory")},
	}
}

func storeCounts(r *Resolver) (tasks, memory, sessions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.taskStores), len(r.memoryStores), len(r.storeSessions)
}

func TestReleasingSessionsDropsTheirTaskAndMemoryStores(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	const sessions = 50
	for index := 0; index < sessions; index++ {
		path := filepath.Join(f.root, "sessions", fmt.Sprintf("s%03d.json", index))
		config := storeConfig(path, filepath.Join(f.cwd, fmt.Sprintf("s%03d", index)))
		if resolver.sharedTaskStore(config) == nil || resolver.sharedMemoryStore(config) == nil {
			t.Fatal("store not created")
		}
	}
	if tasks, memory, _ := storeCounts(resolver); tasks != sessions || memory != sessions {
		t.Fatalf("stores = %d/%d, want %d each", tasks, memory, sessions)
	}
	for index := 0; index < sessions; index++ {
		resolver.releaseSessionChildren(filepath.Join(f.root, "sessions", fmt.Sprintf("s%03d.json", index)))
	}
	if tasks, memory, tracked := storeCounts(resolver); tasks+memory+tracked != 0 {
		t.Fatalf("after release: %d task, %d memory stores, %d tracked keys; want 0", tasks, memory, tracked)
	}
}

func TestSharedStoreSurvivesUntilItsLastSessionIsReleased(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	first := filepath.Join(f.root, "sessions", "a.json")
	second := filepath.Join(f.root, "sessions", "b.json")
	shared := filepath.Join(f.cwd, "shared")
	one := resolver.sharedTaskStore(storeConfig(first, shared))
	if two := resolver.sharedTaskStore(storeConfig(second, shared)); two != one {
		t.Fatal("sessions with one storage directory must share the store")
	}
	resolver.releaseSessionChildren(first)
	if got := resolver.sharedTaskStore(storeConfig(second, shared)); got != one {
		t.Fatal("store released while another session still uses it")
	}
	resolver.releaseSessionChildren(second)
	if tasks, _, _ := storeCounts(resolver); tasks != 0 {
		t.Fatalf("%d task stores left", tasks)
	}
	if again := resolver.sharedTaskStore(storeConfig(first, shared)); again == nil || again == one {
		t.Fatal("an access after the delete must build a fresh store")
	}
}

func TestStoreReleaseIsSafeWithInFlightUse(t *testing.T) {
	f := newLayoutFixture(t)
	resolver := f.service.resolver
	path := filepath.Join(f.root, "sessions", "busy.json")
	config := storeConfig(path, filepath.Join(f.cwd, "busy"))
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				resolver.sharedMemoryStore(config)
				if store := resolver.sharedTaskStore(config); store != nil {
					_, _ = store.List(native.TaskListFilter{})
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		resolver.releaseSessionChildren(path)
	}
	wg.Wait()
	resolver.releaseSessionChildren(path)
	if tasks, memory, tracked := storeCounts(resolver); tasks+memory+tracked != 0 {
		t.Fatalf("stores left: %d/%d/%d", tasks, memory, tracked)
	}
}
