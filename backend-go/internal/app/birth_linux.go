//go:build linux

package app

import "golang.org/x/sys/unix"

// fileBirthMillis returns the file's creation time in whole milliseconds, or 0
// when the filesystem does not record one (Node reports the same for its
// birthtimeMs on such files).
func fileBirthMillis(path string) int64 {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BTIME, &stat); err != nil {
		return 0
	}
	if stat.Mask&unix.STATX_BTIME == 0 {
		return 0
	}
	return stat.Btime.Sec*1000 + int64(stat.Btime.Nsec)/1_000_000
}
