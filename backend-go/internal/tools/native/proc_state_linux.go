//go:build linux

package native

import (
	"os"
	"strconv"
	"strings"
)

// procStat is the part of /proc/<pid>/stat the liveness checks read: the state letter
// (field 3) and the process group id (field 5).
type procStat struct {
	state byte
	pgrp  int
}

// readProcStat reads /proc/<pid>/stat. ok is false when the process does not exist or
// the line cannot be parsed.
func readProcStat(pid int) (procStat, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(string(data))
}

// parseProcStat parses a stat line. The command name (field 2) sits in parentheses and may
// hold spaces and parentheses, so fields are counted from the last closing parenthesis;
// after it the first field is number 3.
func parseProcStat(stat string) (procStat, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return procStat{}, false
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return procStat{}, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return procStat{}, false
	}
	return procStat{state: fields[0][0], pgrp: pgrp}, true
}

// procStateDead reports a process that has exited and only waits to be reaped (Z), or is
// being torn down (X). Signals no longer reach it.
func procStateDead(state byte) bool {
	return state == 'Z' || state == 'X'
}

// processIsZombie reports whether pid has exited and waits to be reaped. An unreadable or
// missing stat line is not a zombie: the caller's own check decides then.
func processIsZombie(pid int) bool {
	stat, ok := readProcStat(pid)
	return ok && procStateDead(stat.state)
}

// groupHasLiveMember reports whether process group pgid has a member that has not exited.
// A zombie still counts for kill(-pgid, 0), and in a container whose init does not reap,
// an orphaned member stays a zombie for good, so liveness has to look at the members'
// states. known is false when /proc cannot be listed; the caller then keeps the answer of
// kill(-pgid, 0).
func groupHasLiveMember(pgid int) (live, known bool) {
	// The leader is usually the live member; checking it first avoids the full scan.
	if stat, ok := readProcStat(pgid); ok && stat.pgrp == pgid && !procStateDead(stat.state) {
		return true, true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name[0] < '1' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid == pgid {
			continue
		}
		stat, ok := readProcStat(pid)
		if ok && stat.pgrp == pgid && !procStateDead(stat.state) {
			return true, true
		}
	}
	return false, true
}
