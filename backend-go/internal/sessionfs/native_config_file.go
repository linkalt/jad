package sessionfs

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// The loaders run on every settings load and have no settings struct to carry a limit, so each
// kind of user-supplied file has a fixed ceiling: far above any useful file, well below a
// memory problem. A file exactly at the cap is read.
const (
	// maxConfigTextFileBytes caps an agent prompt.md or an env_file.
	maxConfigTextFileBytes = 1 << 20
	// maxPromptDocumentBytes caps a session or profile prompt document; larger than the
	// others because documents above 1 MiB were legal before the cap existed.
	maxPromptDocumentBytes = 8 << 20
	// maxInternalJSONFileBytes caps a session, profile, model, toolset, agent.json or root
	// setting file.
	maxInternalJSONFileBytes = 4 << 20
)

// readConfigTextFile reads a regular file of at most maxConfigTextFileBytes.
func readConfigTextFile(path string) ([]byte, error) {
	return readRegularFile(path, maxConfigTextFileBytes)
}

// readRegularFile reads a regular file of at most max bytes. O_NONBLOCK keeps the open of a
// FIFO from waiting for a writer, and the type is checked on the opened handle.
func readRegularFile(path string, max int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return contents, nil
}
