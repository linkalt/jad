package sessionfs

import (
	"path/filepath"
	"sync"
)

// profileLoadAttempts is how many times a session load reads a profile's files before it gives
// up on a profile whose self-edit keeps publishing (decider 3c.3b).
const profileLoadAttempts = 3

// publishTracker counts the self-edit publishes of this process, so a session load can tell
// whether the profile files it read are of one revision (decider 3c.3b). An edit publishes its
// own files and the profile file one rename at a time; a load that runs meanwhile could pair an
// old profile file with new own toolsets, models or documents. The load takes no lock (a trial
// and the catalog checks already load under a writer's lock); it notes the generation before it
// reads and checks afterwards that no publish of the profiles it read began, ran or ended since.
// It is a check of one process, like the profile file lock.
type publishTracker struct {
	mu sync.Mutex
	// generation goes up at each start and end of a publish.
	generation uint64
	// running is the number of publishes under way.
	running int
	// profiles are the profile files that have been published, by cleaned path.
	profiles map[string]*publishState
}

type publishState struct {
	// changed is the generation of the publish's last start or end.
	changed uint64
	// done is open while a publish is under way and closed when it ends.
	done chan struct{}
}

var profilePublishes = &publishTracker{profiles: map[string]*publishState{}}

// now is the generation a load notes before it reads anything.
func (t *publishTracker) now() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.generation
}

// begin marks the profile file at path as being published until the returned function runs.
// The caller holds the profile file lock, so a profile has one publish at a time.
func (t *publishTracker) begin(path string) func() {
	path = filepath.Clean(path)
	t.mu.Lock()
	t.generation++
	t.running++
	state := t.profiles[path]
	if state == nil {
		state = &publishState{}
		t.profiles[path] = state
	}
	state.changed, state.done = t.generation, make(chan struct{})
	done := state.done
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.generation++
		t.running--
		state.changed, state.done = t.generation, nil
		t.mu.Unlock()
		close(done)
	}
}

// unsettled is the profile files among paths with a publish under way, or one that began or
// ended after since, and the publishes still under way; empty when the files a load read since
// are of one revision each.
func (t *publishTracker) unsettled(since uint64, paths []string) (changed []string, running []chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.generation == since && t.running == 0 {
		return nil, nil
	}
	for _, path := range paths {
		state := t.profiles[filepath.Clean(path)]
		if state == nil || (state.done == nil && state.changed <= since) {
			continue
		}
		changed = append(changed, path)
		if state.done != nil {
			running = append(running, state.done)
		}
	}
	return changed, running
}
