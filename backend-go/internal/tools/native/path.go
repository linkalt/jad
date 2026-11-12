package native

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// resolvedTarget is a path a tool reads in-process. realPath is the file as the run sees it,
// checked against the policy, shown and given to child processes; openPath is where jad
// reaches it: the same path, or, in an overlay run, the path through the holder's root (view),
// since the run's files exist only there (sandbox_holder_view.go).
type resolvedTarget struct {
	workspaceRoot string
	realPath      string
	openPath      string
	view          string
	displayPath   string
	errorPath     string
}

// open opens the target for reading; in an overlay run with openat2 inside the holder's root,
// so no link swapped in meanwhile resolves on the host.
func (t resolvedTarget) open() (*os.File, error) {
	if t.view != "" {
		return openInRoot(t.view, t.realPath)
	}
	return os.Open(t.realPath)
}

func resolveExistingTarget(requestedPath, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) (resolvedTarget, string) {
	view := runView(workspaceRoot)
	evalSymlinks := filepath.EvalSymlinks
	if view != "" {
		evalSymlinks = func(path string) (string, error) { return evalSymlinksIn(view, path) }
	}
	root, err := evalSymlinks(workspaceRoot)
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

	realPath, err := evalSymlinks(candidate)
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
	if policy.Hides(candidate) || policy.Hides(realPath) {
		// Answered as a missing file at the requested path, links followed (design §6, fix C).
		return resolvedTarget{}, tools.ExecutionError(callID, tools.StatusNotFound, "not_found", "Could not find "+candidateErrorPath, tools.Information{Key: "path", Value: candidateErrorPath})
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
		openPath:      view + realPath,
		view:          view,
		displayPath:   displayPath(root, realPath),
		errorPath:     safeErrorPath(root, realPath),
	}, ""
}

func pathPolicyError(target, workspaceRoot string, policy sessionfs.NativePathPolicy, callID string) string {
	for _, rule := range policy.Paths {
		if rule.Action == "deny" && (sessionfs.PathWithin(rule.Path, target) || sessionfs.PathWithin(sessionfs.CanonicalRulePath(rule.Path), target)) {
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

// readVerifiedTarget reads the regular file target, which must still be the file before
// describes, up to maxBytes; it is opened as the run sees it (resolvedTarget.open).
func readVerifiedTarget(target resolvedTarget, before os.FileInfo, maxBytes int64) ([]byte, os.FileInfo, error) {
	file, err := target.open()
	if err != nil {
		return nil, nil, err
	}
	return readVerifiedFile(file, before, maxBytes)
}

// readVerifiedFile reads the opened file, which must still be before, up to maxBytes, and
// closes it.
func readVerifiedFile(file *os.File, before os.FileInfo, maxBytes int64) ([]byte, os.FileInfo, error) {
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, nil, errors.New("file changed while being opened")
	}
	// Sized from the file so the read does not grow its buffer by doubling; a file
	// that grew meanwhile still reads to the limit.
	var buffer bytes.Buffer
	if size := opened.Size(); size > 0 && size <= maxBytes {
		buffer.Grow(int(size) + bytes.MinRead)
	}
	if _, err := buffer.ReadFrom(io.LimitReader(file, maxBytes+1)); err != nil {
		return nil, nil, err
	}
	data := buffer.Bytes()
	after, err := file.Stat()
	if err != nil || !os.SameFile(after, opened) || int64(len(data)) != after.Size() {
		return nil, nil, errors.New("file changed while being read")
	}
	if int64(len(data)) > maxBytes {
		return nil, after, fmt.Errorf("file grew past configured limit")
	}
	return data, after, nil
}

// readTarget reads the whole of target, opened as the run sees it.
func readTarget(target resolvedTarget) ([]byte, error) {
	file, err := target.open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}
