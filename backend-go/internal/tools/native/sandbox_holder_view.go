package native

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The in-process view of an overlay run (docs/isolation-sandbox-design.md §2.4, §9.5): its
// files exist only in its holder's mount namespace, so a reader resolves a path of the run
// inside /proc/<holder>/root and opens it there with openat2(RESOLVE_IN_ROOT). Paths outside
// the run's worktree are the host's, as the holder shares the rest of the file system.

// maxSymlinks bounds the links one resolution follows, as the kernel's own limit (ELOOP).
const maxSymlinks = 40

// runView is the root a path is read through: the holder's root for a path inside an
// overlay run's worktree, else empty (the host).
func runView(path string) string {
	if run, ok := isolatedRunOf(path); ok && run.holder != nil && pathInside(filepath.Clean(path), run.path) {
		return run.holder.root()
	}
	return ""
}

// evalSymlinksIn is filepath.EvalSymlinks of the absolute path inside root: a link's absolute
// target starts again at root, so the result is the path as a process inside sees it.
func evalSymlinksIn(root, path string) (string, error) {
	resolved := string(filepath.Separator)
	pending := strings.Split(filepath.Clean(path), string(filepath.Separator))
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(root + next)
		if err != nil {
			return "", &os.PathError{Op: "lstat", Path: next, Err: underlying(err)}
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > maxSymlinks {
			return "", &os.PathError{Op: "lstat", Path: next, Err: syscall.ELOOP}
		}
		target, err := os.Readlink(root + next)
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: next, Err: underlying(err)}
		}
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return resolved, nil
}

func underlying(err error) error {
	if pathError, ok := err.(*os.PathError); ok {
		return pathError.Err
	}
	return err
}

// statInRun is os.Stat of path as the run that holds it sees it.
func statInRun(path string) (os.FileInfo, error) {
	root := runView(path)
	if root == "" {
		return os.Stat(path)
	}
	resolved, err := evalSymlinksIn(root, path)
	if err != nil {
		return nil, err
	}
	return os.Stat(root + resolved)
}

// viewPath is where the host reaches path, a folder without links, as the run that holds it
// sees it.
func viewPath(path string) string {
	return runView(path) + path
}

// evalSymlinksInRun is filepath.EvalSymlinks of path as the run that holds it sees it.
func evalSymlinksInRun(path string) (string, error) {
	if root := runView(path); root != "" {
		return evalSymlinksIn(root, path)
	}
	return filepath.EvalSymlinks(path)
}
