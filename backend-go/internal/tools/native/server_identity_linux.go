//go:build linux

package native

import (
	"os"
	"strconv"
)

// processStartTime reads the start time of a process, in clock ticks since boot, from
// /proc/<pid>/stat. It returns 0 when the file cannot be read or parsed.
func processStartTime(pid int) uint64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	return parseProcStatStart(string(data))
}

// parseProcStatStart is the start time of a stat line (field 22, see parseProcessStat), 0 when
// it cannot be read.
func parseProcStatStart(stat string) uint64 {
	start, _, err := parseProcessStat(stat)
	if err != nil {
		return 0
	}
	return start
}
