package native

import "os"

// serverIdentity names the server process that wrote a run manifest. A pid alone can be
// reused by another program, so where the system reports it the process start time is
// kept too: the same pid with another start time is a different process.
type serverIdentity struct {
	PID int `json:"pid"`
	// Start is the process start time in the system's own unit (clock ticks since boot on
	// Linux). Zero means the system does not report one, and only the pid is compared.
	Start uint64 `json:"start,omitempty"`
}

// currentServerIdentity is the identity of this process.
func currentServerIdentity() serverIdentity {
	pid := os.Getpid()
	return serverIdentity{PID: pid, Start: processStartTime(pid)}
}

// foreignServerAlive reports whether identity is another server that is still running,
// as opposed to this process (a manager of an earlier start included) or a server that
// has stopped. Runs of such a live server are not this manager's to mark lost or prune.
func foreignServerAlive(self, identity serverIdentity) bool {
	if identity.PID == self.PID && (identity.Start == self.Start || identity.Start == 0 || self.Start == 0) {
		return false
	}
	return serverAlive(identity)
}

// serverAlive reports whether the process behind identity is still running. When the
// start time was recorded and the system can report it, a process with another start
// time is not the same one. Where liveness cannot be checked (Windows) it reports false,
// which keeps the old rule: every run found going at startup is lost.
func serverAlive(identity serverIdentity) bool {
	if identity.PID <= 0 || !processExists(identity.PID) {
		return false
	}
	if identity.Start == 0 {
		return true
	}
	current := processStartTime(identity.PID)
	return current == 0 || current == identity.Start
}
