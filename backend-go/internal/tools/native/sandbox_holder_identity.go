package native

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A holder is named by a pid, and a pid can be reused once the process is gone: a stale one
// would send an nsenter join, a /proc/<pid>/root read or a kill to an unrelated process. So the
// holder is known by its pid and its start time (the starttime field of /proc/<pid>/stat, in
// clock ticks since boot, which no process that reuses the pid shares), and by a pidfd where the
// kernel has pidfd_open (Linux 5.3 or later), which names the process itself and not its pid.
// The identity is checked before every join, every kill and every read through the holder's
// root, and a mismatch means the holder is gone. Where there is no pidfd the start time alone is
// checked, and the window between the check and the use of the pid (microseconds, and only
// while the holder is not a child jad still has to wait for) stays open: docs/isolation-decisions.md
// P4 follow-ups.

// holderGoneRoot is the root of a holder that is gone: a path that cannot exist (there is no
// process 0), so a read of the run's files fails, where the host's own folder would show an
// empty worktree.
const holderGoneRoot = "/proc/0/root"

// processIdentity is a process by pid and start time, with its pidfd (-1 when there is none).
type processIdentity struct {
	pid   int
	start uint64
	pidfd int
}

var errProcessGone = errors.New("the process is gone, or its pid belongs to another process now")

// identify names the process with pid as it is now: the pidfd is opened first and the start
// time read after, so both are the same process unless it ended in between, and then the pidfd
// reports it.
func identify(pid int) (processIdentity, error) {
	if pid <= 0 {
		return processIdentity{pidfd: -1}, errProcessGone
	}
	identity := processIdentity{pid: pid, pidfd: openPidfd(pid)}
	start, state, err := readProcessStart(pid)
	if err != nil || state == 'Z' || state == 'X' || (identity.pidfd >= 0 && pidfdExited(identity.pidfd)) {
		identity.close()
		if err == nil {
			err = errProcessGone
		}
		return processIdentity{pidfd: -1}, err
	}
	identity.start = start
	return identity, nil
}

// identityOf is the identity of a process recorded as pid and start (a state file's), checked
// now: errProcessGone when there is no such process, or when the pid is another process's.
func identityOf(pid int, start uint64) (processIdentity, error) {
	identity, err := identify(pid)
	if err != nil {
		return processIdentity{pidfd: -1}, err
	}
	if identity.start != start {
		identity.close()
		return processIdentity{pidfd: -1}, errProcessGone
	}
	return identity, nil
}

// alive reports whether the process is the one named: it exists, is not a zombie, and has the
// start time (and the pidfd, when there is one, has not seen it exit).
func (p processIdentity) alive() bool {
	if p.pid <= 0 {
		return false
	}
	start, state, err := readProcessStart(p.pid)
	if err != nil || state == 'Z' || state == 'X' || start != p.start {
		return false
	}
	return p.pidfd < 0 || !pidfdExited(p.pidfd)
}

// kill sends SIGKILL to the process if it is the one named; a process that is gone, or whose
// pid another process has, is left alone and counts as done.
func (p processIdentity) kill() error {
	if !p.alive() {
		return nil
	}
	return killProcess(p)
}

// waitGone waits, at most timeout, until the process is gone.
func (p processIdentity) waitGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !p.alive() {
			return true
		}
		if timeout > 0 && time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *processIdentity) close() {
	if p.pidfd >= 0 {
		closeFD(p.pidfd)
		p.pidfd = -1
	}
}

// parseProcessStat reads the start time and the state out of the content of /proc/<pid>/stat:
// "pid (comm) state ppid ... starttime ...", where comm may hold spaces and parentheses, so the
// fields are counted from the last ")". The state is field 3, the start time field 22.
func parseProcessStat(stat string) (uint64, byte, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, fmt.Errorf("unreadable process status %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	// fields[0] is field 3 (state), so field 22 is fields[19].
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("unreadable process status %q", stat)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unreadable start time in the process status: %w", err)
	}
	return start, fields[0][0], nil
}
