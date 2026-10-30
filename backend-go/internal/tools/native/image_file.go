package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// readImageFile reads one image by path under a path policy, or from the image store when
// the path is a stored-image reference. It returns a ready tool result on failure. A
// non-empty label prefixes each message, so a call with several images names the part.
func readImageFile(path string, policy sessionfs.NativePathPolicy, store *imagestore.Store, maxBytes int64, execution tools.ExecutionContext, label string) ([]byte, string) {
	prefix := ""
	if label != "" {
		prefix = label + ": "
	}
	if name, isReference := strings.CutPrefix(path, imagestore.RefPrefix); isReference {
		data, _, err := store.OpenWithin(name, maxBytes)
		var tooLarge *imagestore.TooLargeError
		if errors.As(err, &tooLarge) {
			return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%sthe image is %d bytes; the limit is %d", prefix, tooLarge.Size, maxBytes))
		}
		if err != nil {
			return nil, tools.ExecutionError(execution.CallID, tools.StatusNotFound, "image_not_found", prefix+"the stored image cannot be read: "+oneLine(err.Error()))
		}
		return data, ""
	}
	target, failure := resolveExistingTarget(path, execution.WorkspaceRoot, policy, execution.CallID)
	if failure != "" {
		return nil, failure
	}
	info, err := os.Lstat(target.realPath)
	if err != nil {
		return nil, imageReadFailure(execution.CallID, err, target.errorPath)
	}
	if !info.Mode().IsRegular() {
		return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_file", prefix+target.errorPath+" is not a regular file")
	}
	file, err := os.Open(target.realPath)
	if err != nil {
		return nil, imageReadFailure(execution.CallID, err, target.errorPath)
	}
	defer file.Close()
	// Read one byte more than the limit to see that the file is over it, without reading
	// the whole file.
	data, within, err := readWithin(file, maxBytes)
	if err != nil {
		return nil, imageReadFailure(execution.CallID, err, target.errorPath)
	}
	if !within {
		return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%s%s is larger than %d bytes", prefix, target.errorPath, maxBytes))
	}
	return data, ""
}

// readWithin reads at most limit bytes. It reads one byte more to learn that the source
// is over the limit, and stops there, so a source of any size costs limit+1 bytes.
func readWithin(source io.Reader, limit int64) (data []byte, within bool, err error) {
	data, err = io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) <= limit, nil
}

func imageReadFailure(callID string, err error, path string) string {
	if errors.Is(err, fs.ErrNotExist) {
		return tools.ExecutionError(callID, tools.StatusNotFound, "not_found", "Could not find "+path, tools.Information{Key: "path", Value: path})
	}
	return tools.ExecutionError(callID, tools.StatusError, "image_read_failed", "Could not read "+path, tools.Information{Key: "path", Value: path})
}
