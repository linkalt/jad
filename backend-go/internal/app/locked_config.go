package app

import (
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// lockedConfigMaxAge is how long a config loaded by LockSession can still serve the
// Resolve that follows it. A run start calls the two back to back, so the gap is
// microseconds; the bound only keeps a parked entry from answering a much later call
// with a config whose prompt documents, env files or sub-agent files have changed. It
// is an internal constant, see DECISIONS.md.
const lockedConfigMaxAge = time.Second

type lockedConfigKey struct {
	file, profile, modelPath string
}

type lockedConfig struct {
	config sessionfs.SessionConfig
	stamp  sessionfs.SettingsStamp
	loaded time.Time
}

// lockedConfigs holds the config LockSession loaded so the Resolve that follows it in
// a run start does not load the same files again. An entry is used once.
type lockedConfigs struct {
	mu      sync.Mutex
	entries map[lockedConfigKey]lockedConfig
}

func (c *lockedConfigs) put(key lockedConfigKey, entry lockedConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[lockedConfigKey]lockedConfig)
	}
	c.entries[key] = entry
}

func (c *lockedConfigs) drop(key lockedConfigKey) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// take returns and removes the entry for key when it is young enough and the files it
// read have not changed.
func (c *lockedConfigs) take(key lockedConfigKey, now time.Time) (sessionfs.SessionConfig, bool) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	delete(c.entries, key)
	c.mu.Unlock()
	if !ok || now.Sub(entry.loaded) > lockedConfigMaxAge || !entry.stamp.Current() {
		return sessionfs.SessionConfig{}, false
	}
	return entry.config, true
}
