package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"sync"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

// mcpScopeHold is one run's use of the MCP manager of its narrowing (runMCPManager): a
// profile_message run whose caller's sandbox moved where the target's stdio servers start lists
// and calls them on a manager of that narrowing, never on the profile's shared one. The manager
// lives while a hold of it is held, and the release of the last hold closes it, so the servers of
// a narrowing run only while some run of it, or what such a run left going, can call them.
//
// A run holds its manager from before it lists its tools until its lineage drains
// (releaseMCPScopeAfter): its runner returned and no handle or sub-agent run started under its
// lineage key is live in any manager of the resolver. Those are all that can still call the
// run's tools (a background child's catalog, an async exec's script); a resumed child gets the
// catalog of the run that resumes it. A session delete or the server's shutdown closes the
// manager even while held; the release that follows finds it gone and closes nothing more.
type mcpScopeHold struct {
	resolver *Resolver
	key      string
	manager  *mcpclient.Manager
	once     sync.Once
}

// holdMCPScope returns a hold of the manager of the narrowing scope names for a run of config
// (profileDelegation.mcpScope), starting a new manager when none is held; nil for an empty scope
// (the run uses the profile's shared manager). Runs narrowed the same way at the same time share
// one manager.
func (r *Resolver) holdMCPScope(config sessionfs.SessionConfig, scope string) (*mcpScopeHold, error) {
	if scope == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(scope))
	key := filepath.Clean(config.SettingPath) + "\x00" + config.Profile + "\x00confined\x00" + hex.EncodeToString(sum[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	manager := r.mcpManagerForLocked(key)
	if r.mcpScopeRefs == nil {
		r.mcpScopeRefs = make(map[*mcpclient.Manager]int)
	}
	r.mcpScopeRefs[manager]++
	return &mcpScopeHold{resolver: r, key: key, manager: manager}, nil
}

// release ends the hold, once. The release of the last hold of the manager closes it, unless a
// session delete or shutdown closed it already (it is no longer the manager under its key). wait
// false closes it in a goroutine of its own: a release a handle's end triggers does not keep that
// end's caller waiting for the servers to stop.
func (h *mcpScopeHold) release(wait bool) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		r := h.resolver
		r.mu.Lock()
		r.mcpScopeRefs[h.manager]--
		last := r.mcpScopeRefs[h.manager] <= 0
		if last {
			delete(r.mcpScopeRefs, h.manager)
		}
		owned := last && r.mcpManagers[h.key] == h.manager
		if owned {
			delete(r.mcpManagers, h.key)
		}
		r.mu.Unlock()
		switch {
		case !owned:
		case wait:
			_ = h.manager.Close()
		default:
			go func() { _ = h.manager.Close() }()
		}
	})
}

// mcpScopeDrains are the holds of runs that ended while work under their lineage key was still
// live, by that key; each is released once the work is gone (checkMCPDrains).
type mcpScopeDrains struct {
	mu      sync.Mutex
	pending map[string]*mcpScopeHold
}

// releaseMCPScopeAfter releases hold once nothing is live under the run's lineage key: at once
// when nothing is, else when the last handle or sub-agent run under it ends (checkMCPDrains,
// which the async and sub-agent managers call on such an end). An empty key (the run never got
// one) releases at once. The hold is recorded before the look, so an end between the two finds it.
func (r *Resolver) releaseMCPScopeAfter(hold *mcpScopeHold, key string) {
	if hold == nil {
		return
	}
	if key == "" {
		hold.release(true)
		return
	}
	r.mcpDrains.mu.Lock()
	if r.mcpDrains.pending == nil {
		r.mcpDrains.pending = make(map[string]*mcpScopeHold)
	}
	r.mcpDrains.pending[key] = hold
	r.mcpDrains.mu.Unlock()
	r.drainMCPScope(key, true)
}

// checkMCPDrains releases the pending holds whose lineage has nothing live any more. It is called
// from the end callback of every async and sub-agent manager of the resolver (lineageWorkEnded),
// outside their locks; with nothing pending it costs one lock.
func (r *Resolver) checkMCPDrains() {
	r.mcpDrains.mu.Lock()
	if len(r.mcpDrains.pending) == 0 {
		r.mcpDrains.mu.Unlock()
		return
	}
	keys := make([]string, 0, len(r.mcpDrains.pending))
	for key := range r.mcpDrains.pending {
		keys = append(keys, key)
	}
	r.mcpDrains.mu.Unlock()
	for _, key := range keys {
		r.drainMCPScope(key, false)
	}
}

// drainMCPScope releases the hold pending under key when nothing is live under it. Work under a
// key is started only by the run of the key or by live work under it, so once none is live none
// is started any more, and the release cannot come too early.
func (r *Resolver) drainMCPScope(key string, wait bool) {
	if r.lineageLive(key) {
		return
	}
	r.mcpDrains.mu.Lock()
	hold := r.mcpDrains.pending[key]
	delete(r.mcpDrains.pending, key)
	r.mcpDrains.mu.Unlock()
	hold.release(wait)
}

// lineageLive reports whether a handle or sub-agent run whose lineage holds key is live in any
// async or sub-agent manager of the resolver.
func (r *Resolver) lineageLive(key string) bool {
	match := func(k string) bool { return k == key }
	for _, manager := range r.asyncManagers() {
		if manager.HasLiveLineage(match) {
			return true
		}
	}
	for _, manager := range r.subagentManagerList() {
		if manager.HasLiveLineage(match) {
			return true
		}
	}
	return false
}

// stopStdioServers stops the stdio servers of the held manager (an isolated run's, whose calls go
// to its worktree's servers); nil does nothing.
func (h *mcpScopeHold) stopStdioServers() {
	if h != nil {
		h.manager.StopStdioServers()
	}
}
