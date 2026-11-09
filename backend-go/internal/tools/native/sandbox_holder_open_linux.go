//go:build linux

package native

import (
	"os"

	"golang.org/x/sys/unix"
)

// openInRoot opens path (absolute, as seen inside root) for reading with openat2 and
// RESOLVE_IN_ROOT, so every link on the way is resolved inside root, never on the host.
func openInRoot(root, path string) (*os.File, error) {
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer unix.Close(dir)
	fd, err := unix.Openat2(dir, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
