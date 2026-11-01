// Package sessiongate holds the per-session gate that serializes a mutation of a profile's
// transcript (compaction, reset, checkpoint restore, state context) against the runs that
// start on it, and the in-process lock of one session. The HTTP handler and the native tools
// share one Gate, so a refusal is the same whichever path asks.
package sessiongate

import "sync"

// Gate is the mutation gate and the session locks. The zero value is ready to use.
type Gate struct {
	mu sync.Mutex
	// mutations holds the keys of the profiles a mutation has taken.
	mutations map[string]struct{}
	// restoring holds the keys with a real checkpoint restore in flight.
	restoring map[string]struct{}
	// sessions holds the in-process lock of each session.
	sessions keyedLocker
}

// Begin takes the gate for key. It returns false when another mutation holds it.
func (g *Gate) Begin(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mutations == nil {
		g.mutations = make(map[string]struct{})
	}
	if _, exists := g.mutations[key]; exists {
		return false
	}
	g.mutations[key] = struct{}{}
	return true
}

// End releases the gate for key.
func (g *Gate) End(key string) {
	g.mu.Lock()
	delete(g.mutations, key)
	g.mu.Unlock()
}

// WhileFree runs fn under the gate's lock when no mutation holds key, and returns false
// without running it otherwise. A run start registers itself inside fn, so the gate check
// and the registration are one step: no mutation can begin between them.
func (g *Gate) WhileFree(key string, fn func() error) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.mutations[key]; exists {
		return false, nil
	}
	return true, fn()
}

// Held reports whether a mutation holds key.
func (g *Gate) Held(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, exists := g.mutations[key]
	return exists
}

// MarkRestoring records that a real checkpoint restore holds key, and returns the function
// that clears it. The caller already holds the gate.
func (g *Gate) MarkRestoring(key string) func() {
	g.mu.Lock()
	if g.restoring == nil {
		g.restoring = make(map[string]struct{})
	}
	g.restoring[key] = struct{}{}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		delete(g.restoring, key)
		g.mu.Unlock()
	}
}

// Restoring reports whether a checkpoint restore is in flight for key.
func (g *Gate) Restoring(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, exists := g.restoring[key]
	return exists
}

// Lock takes the in-process lock of one session and returns the function that frees it.
func (g *Gate) Lock(session string) func() {
	return g.sessions.lock(session)
}

type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

func (l *keyedLocker) lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*keyedLockEntry)
	}
	entry := l.locks[key]
	if entry == nil {
		entry = &keyedLockEntry{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 && l.locks[key] == entry {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
