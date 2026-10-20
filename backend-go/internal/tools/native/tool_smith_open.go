package native

import "os"

// openToolSmithFile opens the resolved library file without following a symlink and checks that
// the handle is the regular file that was inspected, so a swap after the checks fails.
func openToolSmithFile(real string, before os.FileInfo) (*os.File, os.FileInfo, string) {
	handle, err := os.OpenFile(real, os.O_RDONLY|toolSmithOpenNoFollow, 0)
	if err != nil {
		return nil, nil, "cannot be read: " + err.Error()
	}
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, nil, "cannot be read: " + err.Error()
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		handle.Close()
		return nil, nil, "is not a regular file"
	}
	return handle, info, ""
}
