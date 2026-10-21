package app

import (
	"sync"

	"jad/backend-go/internal/sessionfs"
)

// listSettingsCache keeps the settings the session list loaded, one entry per session
// file, so a poll that finds every file as it was does not parse them again. An entry is
// only used while its stamp is current; a list drops the entries of sessions it no
// longer sees, which bounds the cache by the number of sessions.
type listSettingsCache struct {
	mu      sync.Mutex
	entries map[string]listSettingsEntry
}

type listSettingsEntry struct {
	config sessionfs.SessionConfig
	stamp  sessionfs.SettingsStamp
}

func (c *listSettingsCache) get(file string) (sessionfs.SessionConfig, bool) {
	c.mu.Lock()
	entry, ok := c.entries[file]
	c.mu.Unlock()
	if !ok {
		return sessionfs.SessionConfig{}, false
	}
	if !entry.stamp.Current() {
		c.drop(file)
		return sessionfs.SessionConfig{}, false
	}
	return entry.config, true
}

func (c *listSettingsCache) put(file string, entry listSettingsEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]listSettingsEntry)
	}
	c.entries[file] = entry
}

func (c *listSettingsCache) drop(file string) {
	c.mu.Lock()
	delete(c.entries, file)
	c.mu.Unlock()
}

// keepOnly drops every entry whose file is not in seen.
func (c *listSettingsCache) keepOnly(seen map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for file := range c.entries {
		if _, ok := seen[file]; !ok {
			delete(c.entries, file)
		}
	}
}

func (c *listSettingsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// listSettings returns the settings of one session for the list: the cached ones while
// their stamp is current, else a fresh load that is cached.
func (r *Resolver) listSettings(location sessionfs.Location, scans *sessionfs.CatalogScans) (sessionfs.SessionConfig, error) {
	if config, ok := r.listSettingsCache.get(location.File); ok {
		return config, nil
	}
	config, stamp, err := sessionfs.LoadSessionSettingsStampedScoped(location, "", "", scans)
	if err != nil {
		r.listSettingsCache.drop(location.File)
		return sessionfs.SessionConfig{}, err
	}
	r.listSettingsCache.put(location.File, listSettingsEntry{config: config, stamp: stamp})
	return config, nil
}
