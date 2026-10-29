package native

import (
	"context"

	"jad/backend-go/internal/tools"
)

// processOwnership is what a process manager must offer to be shared with sub-agents:
// who started a process, and a way to stop everything one owner started.
type processOwnership interface {
	ownerOf(id string) (owner string, found bool)
	stopOwned(owner, reason string) int
}

// ownerOf reports who started a process. A process known only from a manifest (one
// recovered after a restart) has no owner, so a child never sees it.
func (manager *ManagedProcessManager) ownerOf(id string) (string, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry := manager.entries[id]
	if entry == nil {
		return "", false
	}
	return entry.owner, true
}

// runningOwnedLocked counts the running processes the owner started; the caller holds
// manager.mu. A start in progress counts, because its entry is added under that lock.
func (manager *ManagedProcessManager) runningOwnedLocked(owner string) int {
	count := 0
	for id, entry := range manager.entries {
		if entry.owner == owner && manager.manifests[id].State == ProcessRunning {
			count++
		}
	}
	return count
}

// stopOwned stops every running process the owner started and returns how many it
// stopped. An empty owner stops nothing: the session's own processes are not a
// sub-agent's to clean up.
func (manager *ManagedProcessManager) stopOwned(owner, reason string) int {
	if owner == "" {
		return 0
	}
	manager.mu.Lock()
	entries := make([]*managedProcess, 0, len(manager.entries))
	ids := make([]string, 0, len(manager.entries))
	for id, entry := range manager.entries {
		if entry.owner == owner {
			entries = append(entries, entry)
			ids = append(ids, id)
		}
	}
	manager.mu.Unlock()
	// The entries are inspected outside manager.mu: a manifest write takes entry.mu first and
	// manager.mu second (recordLiveManifest), so holding manager.mu while taking entry.mu here
	// deadlocks with a Stop that is recording its own manifest. Close is written the same way.
	var running []string
	for index, entry := range entries {
		entry.mu.Lock()
		state := entry.manifest.State
		entry.mu.Unlock()
		if state == ProcessRunning {
			running = append(running, ids[index])
		}
	}
	stopped := 0
	for _, id := range running {
		if _, found := manager.Stop(id, reason); found {
			stopped++
		}
	}
	return stopped
}

// ownedProcessManager is the process manager as one owner sees it: its own processes
// only. Every other id answers "not found", the same as an id that never existed, so
// a child learns nothing about the parent's processes.
type ownedProcessManager struct {
	inner  ProcessManager
	owners processOwnership
	owner  string
}

// scopeProcessManager narrows manager to one owner. It fails when the manager cannot
// tell who started a process, which only a stub in a test does.
func scopeProcessManager(manager ProcessManager, owner string) (ProcessManager, bool) {
	owners, ok := manager.(processOwnership)
	if !ok {
		return nil, false
	}
	return &ownedProcessManager{inner: manager, owners: owners, owner: owner}, true
}

func (m *ownedProcessManager) owns(id string) bool {
	owner, found := m.owners.ownerOf(id)
	return found && owner == m.owner
}

func (m *ownedProcessManager) Recover() error { return m.inner.Recover() }

func (m *ownedProcessManager) Start(request ProcessStartRequest) (ProcessSnapshot, error) {
	request.Owner = m.owner
	return m.inner.Start(request)
}

func (m *ownedProcessManager) SnapshotWithLogs(id string, stdoutLimits, stderrLimits ProcessStreamLimits, maxLineChars int, stripANSI bool) (*ProcessSnapshot, bool) {
	if !m.owns(id) {
		return nil, false
	}
	return m.inner.SnapshotWithLogs(id, stdoutLimits, stderrLimits, maxLineChars, stripANSI)
}

func (m *ownedProcessManager) List(filter string) []ProcessSnapshot {
	all := m.inner.List(filter)
	own := make([]ProcessSnapshot, 0, len(all))
	for _, snapshot := range all {
		if m.owns(snapshot.ID) {
			own = append(own, snapshot)
		}
	}
	return own
}

func (m *ownedProcessManager) Wait(id string, timeoutMs int64) ProcessWaitResult {
	return m.WaitContext(context.Background(), id, timeoutMs)
}

func (m *ownedProcessManager) WaitContext(ctx context.Context, id string, timeoutMs int64) ProcessWaitResult {
	if !m.owns(id) {
		return ProcessWaitResult{}
	}
	return waitProcess(ctx, m.inner, id, timeoutMs)
}

func (m *ownedProcessManager) Stop(id, reason string) (*ProcessSnapshot, bool) {
	if !m.owns(id) {
		return nil, false
	}
	return m.inner.Stop(id, reason)
}

// Close does nothing: the manager belongs to the session, and a child must not close it.
func (m *ownedProcessManager) Close() error { return nil }

// processOwnerUnavailable is the error a child gets when the manager cannot scope.
func processOwnerUnavailable(callID string) string {
	return tools.ExecutionError(callID, tools.StatusError, "process_manager_unavailable", "The process manager cannot limit a sub-agent to its own processes")
}
