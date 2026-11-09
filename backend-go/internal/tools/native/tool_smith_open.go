package native

import (
	"os"
	"path/filepath"
)

// openLibraryFile resolves one file of the functions directory and opens it: it must resolve
// inside dir and inside the real root of the library, and it must be the regular file that was
// inspected. problem is non-empty when the file cannot be used. This is the one containment and
// open sequence of the library; the exec helper reader, the tool_smith staging copy and the list
// reader all go through it (P6BM-17).
func openLibraryFile(dir, realRoot, file string) (*os.File, os.FileInfo, string) {
	real, err := evalSymlinksInRun(filepath.Join(dir, file))
	if err != nil {
		return nil, nil, "cannot be resolved: " + err.Error()
	}
	if !toolSmithInside(dir, real) || !toolSmithInside(realRoot, real) {
		return nil, nil, "symlink leaves the functions directory"
	}
	// In an overlay run the file is reached through the run's holder (viewPath).
	info, err := os.Stat(viewPath(real))
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, "is not a regular file"
	}
	return openToolSmithFile(viewPath(real), info)
}

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
