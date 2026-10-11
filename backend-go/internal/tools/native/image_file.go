package native

import (
	"fmt"
	"os"
	"strings"

	"example.com/agent-harness/backend-go/internal/imagestore"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
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
		data, _, err := store.Open(name)
		if err != nil {
			return nil, decideError(execution.CallID, tools.StatusNotFound, "image_not_found", prefix+"the stored image cannot be read: "+oneLine(err.Error()))
		}
		if int64(len(data)) > maxBytes {
			return nil, decideError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%sthe image is %d bytes; the limit is %d", prefix, len(data), maxBytes))
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
		return nil, decideError(execution.CallID, tools.StatusInvalidInput, "not_a_file", prefix+target.errorPath+" is not a regular file")
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
		return nil, decideError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%s%s is larger than %d bytes", prefix, target.errorPath, maxBytes))
	}
	return data, ""
}
