package sessionfs

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// maxConfigTextFileBytes caps a prompt document, an agent prompt.md or an env_file. The
// loaders run on every settings load and have no settings struct to carry a limit; 1 MiB is
// far above any useful prompt or env file and well below a memory problem.
const maxConfigTextFileBytes = 1 << 20

// readConfigTextFile reads a regular file of at most maxConfigTextFileBytes. O_NONBLOCK keeps
// the open of a FIFO from waiting for a writer, and the type is checked on the opened handle.
func readConfigTextFile(path string) ([]byte, error) {
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
	contents, err := io.ReadAll(io.LimitReader(file, maxConfigTextFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maxConfigTextFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigTextFileBytes)
	}
	return contents, nil
}
