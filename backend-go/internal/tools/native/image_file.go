package native

import (
	"errors"
	"fmt"
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
	policy = workspacePolicy(policy, execution)
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
	info, err := os.Lstat(target.openPath)
	if err != nil {
		return nil, imageReadFailure(execution.CallID, err, target.errorPath)
	}
	if !info.Mode().IsRegular() {
		return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_file", prefix+target.errorPath+" is not a regular file")
	}
	if info.Size() > maxBytes {
		return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%s%s is larger than %d bytes", prefix, target.errorPath, maxBytes))
	}
	// The same open and identity check as read: the file read is the one Lstat described.
	data, _, err := readVerifiedTarget(target, info, maxBytes)
	if err != nil {
		if strings.Contains(err.Error(), "past configured limit") {
			return nil, tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "image_too_large", fmt.Sprintf("%s%s is larger than %d bytes", prefix, target.errorPath, maxBytes))
		}
		return nil, imageReadFailure(execution.CallID, err, target.errorPath)
	}
	return data, ""
}

func imageReadFailure(callID string, err error, path string) string {
	if errors.Is(err, fs.ErrNotExist) {
		return tools.ExecutionError(callID, tools.StatusNotFound, "not_found", "Could not find "+path, tools.Information{Key: "path", Value: path})
	}
	return tools.ExecutionError(callID, tools.StatusError, "image_read_failed", "Could not read "+path, tools.Information{Key: "path", Value: path})
}
