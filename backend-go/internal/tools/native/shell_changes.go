package native

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fileStamp is what the scan compares: a file changed when its size or modification time did.
type fileStamp struct {
	size    int64
	modTime int64
}

// WithShellChangeScan returns tool unchanged unless hooks.shell_changes is scan. Otherwise
// it returns a copy that records the workspace's regular files before and after each call
// and tells the call's OnFileChange about every file the call created, changed or
// deleted, so post_edit checks and the sub-agent change list see what a shell command
// did. A call nobody listens to (no OnFileChange) is not scanned. A scan that reaches
// hooks.shell_scan_max_files reports nothing and says so in the result.
func WithShellChangeScan(tool tools.Tool, hooks sessionfs.HooksSettings) tools.Tool {
	if !hooks.ShellScanActive() || tool.Execute == nil {
		return tool
	}
	inner := tool.Execute
	tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		if execution.OnFileChange == nil || execution.WorkspaceRoot == "" {
			return inner(ctx, arguments, execution)
		}
		// An overlay run's files are read through its holder (viewPath).
		before, complete := scanWorkspaceFiles(viewPath(execution.WorkspaceRoot), hooks)
		result, err := inner(ctx, arguments, execution)
		if err != nil {
			return result, err
		}
		if !complete {
			return appendShellScanNote(result, fmt.Sprintf("Changed files were not looked for: the workspace holds more than %d files (hooks.shell_scan_max_files).", hooks.ShellScanMaxFiles))
		}
		after, complete := scanWorkspaceFiles(viewPath(execution.WorkspaceRoot), hooks)
		if !complete {
			return appendShellScanNote(result, fmt.Sprintf("Changed files were not looked for: the workspace holds more than %d files (hooks.shell_scan_max_files).", hooks.ShellScanMaxFiles))
		}
		changes := diffWorkspaceFiles(before, after)
		reported := changes
		if int64(len(reported)) > hooks.ShellMaxChangedFiles {
			reported = reported[:hooks.ShellMaxChangedFiles]
		}
		for _, change := range reported {
			execution.OnFileChange(change.path, change.action)
		}
		if len(reported) < len(changes) {
			return appendShellScanNote(result, fmt.Sprintf("The call changed %d files; only the first %d were reported (hooks.shell_max_changed_files).", len(changes), len(reported)))
		}
		return result, nil
	}
	return tool
}

type fileChange struct {
	path   string
	action string
}

// scanWorkspaceFiles records the regular files under root by their slash path relative
// to root, without following symlinks and without entering a directory named in
// hooks.shell_scan_skip_dirs. complete is false when the scan stopped at the file limit.
func scanWorkspaceFiles(root string, hooks sessionfs.HooksSettings) (map[string]fileStamp, bool) {
	files := make(map[string]fileStamp)
	complete := true
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped; the rest is still compared.
			if entry != nil && entry.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && slices.Contains(hooks.ShellScanSkipDirs, entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if int64(len(files)) >= hooks.ShellScanMaxFiles {
			complete = false
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files[filepath.ToSlash(relative)] = fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
		return nil
	})
	return files, complete
}

// diffWorkspaceFiles lists the files created, changed or deleted between two scans, in
// path order. The actions are those write and edit report: created, updated, deleted.
func diffWorkspaceFiles(before, after map[string]fileStamp) []fileChange {
	var changes []fileChange
	for path, stamp := range after {
		previous, existed := before[path]
		switch {
		case !existed:
			changes = append(changes, fileChange{path: path, action: "created"})
		case previous != stamp:
			changes = append(changes, fileChange{path: path, action: "updated"})
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			changes = append(changes, fileChange{path: path, action: "deleted"})
		}
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].path < changes[right].path })
	return changes
}

func appendShellScanNote(result, note string) (string, error) {
	updated, err := tools.AppendContent(result, tools.Content{Text: note})
	if err != nil {
		return result, nil
	}
	return updated, nil
}
