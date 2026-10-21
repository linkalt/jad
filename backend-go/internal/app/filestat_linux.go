//go:build linux

package app

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// statFile reads size, times, device, inode and creation time with one statx call.
func statFile(path string) (fileStat, error) {
	var raw unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &raw)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
			return statFileGeneric(path)
		}
		return fileStat{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	stat := fileStat{
		Size:      int64(raw.Size),
		ModTimeNS: raw.Mtime.Sec*1_000_000_000 + int64(raw.Mtime.Nsec),
		Dev:       unix.Mkdev(raw.Dev_major, raw.Dev_minor),
	}
	if raw.Mask&unix.STATX_INO != 0 {
		stat.Ino, stat.HasIdentity = raw.Ino, true
	}
	if raw.Mask&unix.STATX_CTIME != 0 {
		stat.ChangeSec, stat.ChangeNsec, stat.HasChange = raw.Ctime.Sec, int64(raw.Ctime.Nsec), true
	}
	if raw.Mask&unix.STATX_BTIME != 0 {
		stat.BirthMS = raw.Btime.Sec*1000 + int64(raw.Btime.Nsec)/1_000_000
	}
	return stat, nil
}
