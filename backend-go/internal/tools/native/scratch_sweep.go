package native

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// staleScratchAge is older than any live run can be: a call is bounded by the 6-hour timeout
// ceiling (sessionfs maxToolTimeoutMs) plus the kill and drain grace, so a scratch directory
// untouched for longer belongs to a process that died without cleaning up.
const staleScratchAge = 7 * time.Hour

var staleScratchPrefixes = []string{"exec-", "tool-smith-"}

var sweepScratchOnce sync.Once

// sweepStaleScratchOnce removes the scratch directories a crashed server left in the
// temporary directories, once per process, the first time exec or tool_smith is built.
func sweepStaleScratchOnce() {
	sweepScratchOnce.Do(func() {
		now := time.Now()
		for _, dir := range staleScratchDirs() {
			sweepStaleScratch(dir, now, staleScratchAge)
		}
	})
}

// staleScratchDirs is every directory the tools put scratch in: the temporary directory, and
// the short fallback tool_smith uses when the bridge socket path there would be too long
// (makeToolSmithWorkDir). Without the fallback, a crashed server with TMPDIR set elsewhere
// leaves its tool-smith directories in /tmp for ever (P6BM-12).
func staleScratchDirs() []string {
	dirs := []string{os.TempDir()}
	if fallback := filepath.Clean(toolSmithShortTempDir); fallback != filepath.Clean(dirs[0]) {
		dirs = append(dirs, fallback)
	}
	return dirs
}

// sweepStaleScratch removes the private (mode 0700) directories in dir whose name starts
// with an exec or tool_smith scratch prefix and that were last changed more than maxAge
// before now. Links and files are left alone. It returns how many it removed.
func sweepStaleScratch(dir string, now time.Time, maxAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		matches := false
		for _, prefix := range staleScratchPrefixes {
			if strings.HasPrefix(name, prefix) {
				matches = true
			}
		}
		if !matches {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if os.RemoveAll(path) == nil {
			removed++
		}
	}
	return removed
}
