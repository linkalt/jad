package native

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type resolvedMutatePath struct {
	workspaceRoot string
	candidate     string
	displayPath   string
	errorPath     string
}

type mutateTarget struct {
	workspaceRoot string
	realPath      string
	displayPath   string
	errorPath     string
	existed       bool
	mode          os.FileMode
	size          int64
	identity      os.FileInfo
}

func mutationMode(info os.FileInfo) os.FileMode {
	mode := info.Mode().Perm()
	for _, special := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if info.Mode()&special != 0 {
			mode |= special
		}
	}
	return mode
}

func resolveMutatePath(requestedPath, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) (resolvedMutatePath, string) {
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return resolvedMutatePath{}, tools.ExecutionError(callID, tools.StatusError, "workspace_unavailable", "Workspace is not available")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return resolvedMutatePath{}, tools.ExecutionError(callID, tools.StatusError, "workspace_unavailable", "Workspace is not available")
	}
	root = filepath.Clean(root)

	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return resolvedMutatePath{}, tools.ExecutionError(callID, tools.StatusError, "path_resolution_failed", "The requested path could not be resolved")
	}
	candidate = filepath.Clean(candidate)
	if denied := pathPolicyError(candidate, root, policy, callID); denied != "" {
		return resolvedMutatePath{}, denied
	}
	errorPath := safeErrorPath(root, candidate)
	if candidate == root {
		return resolvedMutatePath{}, tools.ExecutionError(callID, tools.StatusError, "target_is_directory", "The target is a directory", tools.Information{Key: "path", Value: errorPath})
	}
	if strings.HasSuffix(requestedPath, string(filepath.Separator)) {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			return resolvedMutatePath{}, mutationPathError(callID, root, candidate, "invalid_path", "The path ends with a directory separator but is not a directory", tools.StatusError)
		}
	}
	return resolvedMutatePath{
		workspaceRoot: root,
		candidate:     candidate,
		displayPath:   displayPath(root, candidate),
		errorPath:     errorPath,
	}, ""
}

func inspectMutateTarget(path resolvedMutatePath, policy sessionfs.NativePathPolicy, callID string) (mutateTarget, string) {
	info, err := os.Lstat(path.candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return mutateTarget{
				workspaceRoot: path.workspaceRoot,
				realPath:      path.candidate,
				displayPath:   path.displayPath,
				errorPath:     path.errorPath,
			}, ""
		}
		if errors.Is(err, fs.ErrPermission) {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_forbidden", "The target could not be inspected", tools.StatusForbidden)
		}
		return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_stat_failed", "The target could not be inspected", tools.StatusError)
	}

	realPath := path.candidate
	var targetInfo os.FileInfo
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path.candidate)
		if err != nil {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "symlink_forbidden", "The target is a symlink", tools.StatusForbidden)
		}
		realPath, err = filepath.Abs(resolved)
		if err != nil || !resolvedPathAllowed(realPath, path.workspaceRoot, policy, callID) {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "symlink_forbidden", "The target is a symlink", tools.StatusForbidden)
		}
		if denied := pathPolicyError(realPath, path.workspaceRoot, policy, callID); denied != "" {
			return mutateTarget{}, denied
		}
		targetInfo, err = os.Stat(realPath)
		if err != nil {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The target could not be resolved", tools.StatusError)
		}
	} else {
		resolved, err := filepath.EvalSymlinks(path.candidate)
		if err != nil {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The target could not be resolved", tools.StatusError)
		}
		realPath, err = filepath.Abs(resolved)
		if err != nil {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The target could not be resolved", tools.StatusError)
		}
		realPath = filepath.Clean(realPath)
		if realPath != path.candidate {
			if !resolvedPathAllowed(realPath, path.workspaceRoot, policy, callID) {
				return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "symlink_forbidden", "The target is a symlink", tools.StatusForbidden)
			}
			if denied := pathPolicyError(realPath, path.workspaceRoot, policy, callID); denied != "" {
				return mutateTarget{}, denied
			}
		}
		targetInfo, err = os.Lstat(realPath)
		if err != nil {
			return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The target could not be resolved", tools.StatusError)
		}
	}

	if targetInfo.IsDir() {
		return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "target_is_directory", "The target is a directory", tools.StatusError)
	}
	if !targetInfo.Mode().IsRegular() {
		return mutateTarget{}, mutationPathError(callID, path.workspaceRoot, path.candidate, "unsupported_file_type", "The target is not a regular file", tools.StatusError)
	}
	return mutateTarget{
		workspaceRoot: path.workspaceRoot,
		realPath:      realPath,
		displayPath:   displayPath(path.workspaceRoot, realPath),
		errorPath:     path.errorPath,
		existed:       true,
		mode:          mutationMode(targetInfo),
		size:          targetInfo.Size(),
		identity:      targetInfo,
	}, ""
}

func prepareMutateParent(path resolvedMutatePath, policy sessionfs.NativePathPolicy, callID string) string {
	parent := filepath.Dir(path.candidate)
	nearestPath, nearestRealPath, err := nearestExistingParent(path.candidate)
	if err != nil {
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "parent_resolution_failed", "The parent directory could not be resolved", tools.StatusError)
	}
	if nearestPath != nearestRealPath {
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "symlink_forbidden", "The path contains a symlinked directory", tools.StatusForbidden)
	}
	nearestInfo, err := os.Stat(nearestRealPath)
	if err != nil || !nearestInfo.IsDir() {
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "parent_not_directory", "The parent directory is not a directory", tools.StatusError)
	}
	if sessionfs.PathWithin(path.workspaceRoot, path.candidate) {
		if denied := pathPolicyError(nearestRealPath, path.workspaceRoot, policy, callID); denied != "" {
			return denied
		}
	}
	if err := os.MkdirAll(parent, 0o777); err != nil {
		code := "directory_create_failed"
		status := tools.StatusError
		if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, fs.ErrExist) {
			code = "parent_not_directory"
		} else if errors.Is(err, fs.ErrPermission) {
			code = "parent_forbidden"
			status = tools.StatusForbidden
		}
		return mutationPathError(callID, path.workspaceRoot, path.candidate, code, "Could not create the parent directory", status)
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return mutationPathError(callID, path.workspaceRoot, path.candidate, "parent_not_found", "The parent directory could not be resolved", tools.StatusNotFound)
		}
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The parent directory could not be resolved", tools.StatusError)
	}
	realParent, err = filepath.Abs(realParent)
	if err != nil {
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "path_resolution_failed", "The parent directory could not be resolved", tools.StatusError)
	}
	realParent = filepath.Clean(realParent)
	if realParent != parent && !resolvedPathAllowed(realParent, path.workspaceRoot, policy, callID) {
		return mutationPathError(callID, path.workspaceRoot, path.candidate, "symlink_forbidden", "The path contains a symlinked directory", tools.StatusForbidden)
	}
	if sessionfs.PathWithin(path.workspaceRoot, path.candidate) {
		return pathPolicyError(realParent, path.workspaceRoot, policy, callID)
	}
	return ""
}

func nearestExistingParent(candidate string) (string, string, error) {
	current := filepath.Dir(candidate)
	for {
		realPath, err := filepath.EvalSymlinks(current)
		if err == nil {
			absolute, absErr := filepath.Abs(realPath)
			if absErr != nil {
				return "", "", absErr
			}
			return filepath.Clean(current), filepath.Clean(absolute), nil
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", fs.ErrNotExist
		}
		current = parent
	}
}

func resolvedPathAllowed(target, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) bool {
	return pathPolicyError(target, workspaceRoot, policy, callID) == ""
}

func mutationPathError(callID, workspaceRoot, candidate, code, summary string, status tools.Status) string {
	return tools.ExecutionError(callID, status, code, summary, tools.Information{Key: "path", Value: safeErrorPath(workspaceRoot, candidate)})
}
