package native

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// projectInstructionTools are the tools whose paths make project_instructions look for a
// subfolder's instruction file.
var projectInstructionTools = []string{"read", "write", "edit", "apply_patch", "notebook"}

// projectInstructionTracker remembers the subfolder files already given in this catalog,
// which is built for one run, so each file is given once per run.
type projectInstructionTracker struct {
	mu        sync.Mutex
	delivered map[string]struct{}
}

// claim reports whether path is new to the tracker, and marks it given.
func (tracker *projectInstructionTracker) claim(path string) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, given := tracker.delivered[path]; given {
		return false
	}
	tracker.delivered[path] = struct{}{}
	return true
}

// withProjectInstructionsAll wraps the file tools so that a call touching a path in a
// subfolder of the workspace gets that subfolder's instruction file (AGENTS.md, ...)
// added to its result the first time in the run. The files at and above the working
// directory are already in the prompt.
func withProjectInstructionsAll(loaded []tools.Tool, settings sessionfs.ProjectInstructionsSettings) []tools.Tool {
	if !settings.SubfoldersActive() {
		return loaded
	}
	tracker := &projectInstructionTracker{delivered: map[string]struct{}{}}
	for index, tool := range loaded {
		if tool.Definition.Source != "native" || !slices.Contains(projectInstructionTools, tool.Definition.Name) || tool.Execute == nil {
			continue
		}
		loaded[index] = withProjectInstructions(tool, settings, tracker)
	}
	return loaded
}

func withProjectInstructions(tool tools.Tool, settings sessionfs.ProjectInstructionsSettings, tracker *projectInstructionTracker) tools.Tool {
	inner := tool.Execute
	name := tool.Definition.Name
	tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		result, err := inner(ctx, arguments, execution)
		if err != nil || execution.WorkspaceRoot == "" {
			return result, err
		}
		notes := subfolderInstructions(projectInstructionPaths(name, arguments), execution.WorkspaceRoot, settings, tracker)
		if len(notes) == 0 {
			return result, nil
		}
		updated, appendErr := tools.AppendContent(result, tools.Content{Text: strings.Join(notes, "\n\n")})
		if appendErr != nil {
			return result, nil
		}
		return updated, nil
	}
	return tool
}

// projectInstructionPaths returns the paths a call names: its path argument, or for
// apply_patch the files of the patch.
func projectInstructionPaths(name string, arguments map[string]any) []string {
	if name != "apply_patch" {
		if path, ok := arguments["path"].(string); ok && strings.TrimSpace(path) != "" {
			return []string{path}
		}
		return nil
	}
	text, _ := arguments["patch"].(string)
	operations, err := parsePatch(text)
	if err != nil {
		return nil
	}
	var paths []string
	for _, operation := range operations {
		paths = append(paths, operation.path)
		if operation.moveTo != "" {
			paths = append(paths, operation.moveTo)
		}
	}
	return paths
}

// subfolderInstructions finds, for each path inside root, the instruction files of the
// folders between root (not included) and the path's folder, outermost first, that the
// run has not been given yet.
func subfolderInstructions(paths []string, root string, settings sessionfs.ProjectInstructionsSettings, tracker *projectInstructionTracker) []string {
	root = filepath.Clean(root)
	var notes []string
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		relative, err := filepath.Rel(root, filepath.Dir(filepath.Clean(path)))
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		dir := root
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			dir = filepath.Join(dir, part)
			file, found, err := sessionfs.FindProjectInstructionFile(dir, settings.FileNames, settings.SubfolderMaxBytes)
			if err != nil || !found || !tracker.claim(file.Path) {
				continue
			}
			shown, _ := filepath.Rel(root, file.Path)
			notes = append(notes, sessionfs.ProjectInstructionText(file, filepath.ToSlash(shown))+"\n\n(These instructions apply to the files under "+filepath.ToSlash(filepath.Dir(shown))+"/.)")
		}
	}
	return notes
}
