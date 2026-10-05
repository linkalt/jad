package native

import "sync"

// PathLocks serializes native mutations of the same canonical request path.
// A Resolver owns one instance and shares it with each session's tool catalog.
type PathLocks struct {
	mu      sync.Mutex
	entries map[string]*pathLockEntry
}

type pathLockEntry struct {
	mu   sync.Mutex
	refs int
}

func NewPathLocks() *PathLocks {
	return &PathLocks{entries: make(map[string]*pathLockEntry)}
}

// Lock acquires the lock for key and returns an idempotent release function.
func (locks *PathLocks) Lock(key string) func() {
	if locks == nil {
		locks = NewPathLocks()
	}
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*pathLockEntry)
	}
	entry := locks.entries[key]
	if entry == nil {
		entry = &pathLockEntry{}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			locks.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(locks.entries, key)
			}
			locks.mu.Unlock()
		})
	}
}
