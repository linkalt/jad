package native

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	processOwnerFile = "owner.json"
	// Stop reasons Recover gives a process the previous server left running. Without an
	// owner record, or when the process cannot be proven to be the one the manifest
	// names, the reason stays backend_restarted and nothing is signalled.
	processReasonRestarted        = "backend_restarted"
	processReasonRestartedStopped = "backend_restarted_stopped"
	processOrphanPollInterval     = 25 * time.Millisecond
)

// processOwnerRecord is kept beside a process manifest, not inside it: the manifest is
// what the model reads, and this is only for recovery. It names the server that started
// the process and the process's own start time, so a later server can tell the process
// from another program that was given the same pid.
type processOwnerRecord struct {
	Server serverIdentity `json:"server"`
	// Start is the process start time in the system's unit; zero means the system does
	// not report one, and then the process is never signalled.
	Start uint64 `json:"start,omitempty"`
}

// SetOrphanPolicy chooses what Recover does with a process a crashed server left
// running: with stop set it stops the process group once it is proven to be the one the
// manifest names, waiting at most grace between the polite and the forced signal. It
// must be called before the first Recover; later calls change nothing for runs already
// recovered.
func (manager *ManagedProcessManager) SetOrphanPolicy(stop bool, grace time.Duration) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopOrphans = stop
	manager.orphanGrace = grace
}

// writeOwnerRecord stores who started the process. A failure is not fatal: the process
// then is only marked lost after a crash, as before.
func (manager *ManagedProcessManager) writeOwnerRecord(id string, pid int) {
	record := processOwnerRecord{Server: currentServerIdentity(), Start: processStartTime(pid)}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	directory := filepath.Join(manager.directory, id)
	temporary, err := os.CreateTemp(directory, "owner-*.tmp")
	if err != nil {
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_, writeErr := temporary.Write(append(encoded, '\n'))
	closeErr := temporary.Close()
	if writeErr != nil || closeErr != nil {
		return
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, processOwnerFile)); err != nil {
		slog.Warn("process owner record not saved; a crash will only mark the process lost", "process", id, "error", err)
	}
}

func (manager *ManagedProcessManager) readOwnerRecord(id string) (processOwnerRecord, bool) {
	data, err := os.ReadFile(filepath.Join(manager.directory, id, processOwnerFile))
	if err != nil {
		return processOwnerRecord{}, false
	}
	var record processOwnerRecord
	if json.Unmarshal(data, &record) != nil || record.Server.PID <= 0 {
		return processOwnerRecord{}, false
	}
	return record, true
}

// orphanVerdict is what Recover decides about a manifest found running at startup.
type orphanVerdict int

const (
	// orphanLost: mark it lost and signal nothing.
	orphanLost orphanVerdict = iota
	// orphanStoppable: the process is proven to be the one the manifest names.
	orphanStoppable
	// orphanElsewhere: the server that started the process is another one that is
	// still running on this directory. The process is its, not ours: leave the
	// manifest untouched and do not load it.
	orphanElsewhere
)

// classifyOrphan decides what to do with a manifest left running. The process is
// signalled only when every check passes: an owner record names a server that is gone,
// and the pid exists with the start time recorded when the process was started. The
// signal goes to the group led by that pid; if the process leads none, nothing is hit.
func (manager *ManagedProcessManager) classifyOrphan(manifest ProcessManifest, stop bool) orphanVerdict {
	record, ok := manager.readOwnerRecord(manifest.ID)
	if !ok {
		return orphanLost
	}
	// Another server that is still running owns the process: never signal it, and
	// do not call it lost either.
	if foreignServerAlive(currentServerIdentity(), record.Server) {
		return orphanElsewhere
	}
	if !stop || manifest.PID <= 0 || record.Start == 0 {
		return orphanLost
	}
	// Written by this very process (an earlier manager of it): not an orphan.
	if record.Server.PID == currentServerIdentity().PID {
		return orphanLost
	}
	// A pid that no process holds reads as start time 0, which never matches.
	if processStartTime(manifest.PID) != record.Start {
		return orphanLost
	}
	// A leader that already exited and only waits to be reaped (an init that does not reap
	// keeps it so) ended before this restart. With no live member left in its group there
	// is nothing to stop, and reporting it stopped by the restart would be untrue.
	if !processExists(manifest.PID) && !processGroupAlive(manifest.PID) {
		return orphanLost
	}
	return orphanStoppable
}

// stopOrphanGroups sends every group the polite signal, waits up to grace for them to
// go, forces the rest, and returns the pids that are gone afterwards.
func stopOrphanGroups(pids []int, grace time.Duration) map[int]bool {
	gone := make(map[int]bool, len(pids))
	for _, pid := range pids {
		signalGroupOfPID(pid, false)
	}
	waitUntilGone := func(limit time.Duration) {
		deadline := time.Now().Add(limit)
		for {
			remaining := false
			for _, pid := range pids {
				if gone[pid] {
					continue
				}
				// The group counts too: a leader that exited (a zombie, or reaped) can leave
				// members that ignore SIGTERM, and those still need the hard kill.
				if processExists(pid) || processGroupAlive(pid) {
					remaining = true
				} else {
					gone[pid] = true
				}
			}
			if !remaining || !time.Now().Before(deadline) {
				return
			}
			time.Sleep(processOrphanPollInterval)
		}
	}
	waitUntilGone(grace)
	for _, pid := range pids {
		if !gone[pid] {
			signalGroupOfPID(pid, true)
		}
	}
	waitUntilGone(grace)
	return gone
}
