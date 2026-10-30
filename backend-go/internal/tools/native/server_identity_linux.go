//go:build linux

package native

import (
	"os"
	"strconv"
	"strings"
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

// parseProcStatStart takes field 22 of a stat line. The command name (field 2) sits in
// parentheses and may hold spaces and parentheses, so the fields are counted from the
// last closing parenthesis; after it the first field is number 3.
func parseProcStatStart(stat string) uint64 {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(stat[end+1:])
	const startIndex = 22 - 3
	if len(fields) <= startIndex {
		return 0
	}
	start, err := strconv.ParseUint(fields[startIndex], 10, 64)
	if err != nil {
		return 0
	}
	return start
}
