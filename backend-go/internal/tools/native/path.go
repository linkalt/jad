package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type resolvedTarget struct {
	workspaceRoot string
	realPath      string
	displayPath   string
	errorPath     string
}

func resolveExistingTarget(requestedPath, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) (resolvedTarget, string) {
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusError, "workspace_unavailable", "Workspace is not available")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusError, "workspace_unavailable", "Workspace is not available")
	}
	root = filepath.Clean(root)
	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusError, "path_resolution_failed", "The requested path could not be resolved")
	}
	candidate = filepath.Clean(candidate)
	if denied := pathPolicyError(candidate, root, policy, callID); denied != "" {
		return resolvedTarget{}, denied
	}
	candidateErrorPath := safeErrorPath(root, candidate)

	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		code, status, summary := "path_resolution_failed", tools.StatusError, "The requested path could not be resolved"
		switch {
		case errors.Is(err, fs.ErrNotExist):
			code, status, summary = "not_found", tools.StatusNotFound, "Could not find "+candidateErrorPath
		case errors.Is(err, fs.ErrPermission):
			code, status, summary = "path_forbidden", tools.StatusForbidden, "The requested path is not accessible"
		}
		return resolvedTarget{}, tools.ExecutionError(callID, status, code, summary, tools.Information{Key: "path", Value: candidateErrorPath})
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusError, "path_resolution_failed", "The requested path could not be resolved", tools.Information{Key: "path", Value: candidateErrorPath})
	}
	realPath = filepath.Clean(realPath)
	if realPath != candidate && !sessionfs.PathWithin(root, realPath) && pathPolicyError(realPath, root, policy, callID) != "" {
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusForbidden, "symlink_outside_workspace", "The requested path resolves outside the workspace", tools.Information{Key: "path", Value: candidateErrorPath})
	}
	if denied := pathPolicyError(realPath, root, policy, callID); denied != "" {
		return resolvedTarget{}, denied
	}
	return resolvedTarget{
		workspaceRoot: root,
		realPath:      realPath,
		displayPath:   displayPath(root, realPath),
		errorPath:     safeErrorPath(root, realPath),
	}, ""
}

func pathPolicyError(target, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) string {
	for _, rule := range policy.Paths {
		if rule.Action == "deny" && sessionfs.PathWithin(rule.Path, target) {
			return tools.ExecutionError(callID, tools.StatusForbidden, "path_denied", "The requested path is denied by the native tool policy", tools.Information{Key: "path", Value: safeErrorPath(workspaceRoot, target)})
		}
	}
	if len(policy.Within) > 0 && !withinAny(policy.Within, target) {
		return tools.ExecutionError(callID, tools.StatusForbidden, "path_outside_agent_paths", "The requested path is outside the paths this agent may change", tools.Information{Key: "path", Value: safeErrorPath(workspaceRoot, target)})
	}
	if pathAllowed(target, workspaceRoot, policy) {
		return ""
	}
	return tools.ExecutionError(callID, tools.StatusForbidden, "path_outside_workspace", "The requested path is outside the workspace", tools.Information{Key: "path", Value: safeErrorPath(workspaceRoot, target)})
}

func pathAllowed(target, workspaceRoot string, policy sessionfs.NativePathPolicy) bool {
	if policy.UnrestrictedPaths || sessionfs.PathWithin(workspaceRoot, target) {
		return true
	}
	for _, rule := range policy.Paths {
		if rule.Action == "allow" && sessionfs.PathWithin(rule.Path, target) {
			return true
		}
	}
	return false
}

func displayPath(root, target string) string {
	if !sessionfs.PathWithin(root, target) {
		return target
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}

func safeErrorPath(root, target string) string {
	if sessionfs.PathWithin(root, target) {
		return displayPath(root, target)
	}
	name := filepath.Base(target)
	if name == "." || name == string(filepath.Separator) {
		return ".."
	}
	return filepath.ToSlash(filepath.Join("..", name))
}

func readVerifiedRegularFile(path string, before os.FileInfo, maxBytes int64) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, nil, errors.New("file changed while being opened")
	}
	limited := io.LimitReader(file, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(after, opened) || int64(len(data)) != after.Size() {
		return nil, nil, errors.New("file changed while being read")
	}
	if int64(len(data)) > maxBytes {
		return nil, after, fmt.Errorf("file grew past configured limit")
	}
	return data, after, nil
}
