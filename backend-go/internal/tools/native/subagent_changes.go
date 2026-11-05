package native

import (
	"fmt"
	"strings"

	"jad/backend-go/internal/tools"
)

// subagentFileChange is one file a child changed, as its exec calls' change scan reported it.
type subagentFileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// recordChange notes that the child changed a file. A path is listed
// once, with the action of its first change; at most max paths are kept, and the rest
// are only counted. A path that would take the kept list past maxBytes, as the rendered
// list reads, is counted too. It is called from the child's goroutines, so it takes the run lock.
func (r *subagentRun) recordChange(path, action string, max int, maxBytes int64) {
	r.mu.Lock()
	for _, known := range r.files {
		if known.Path == path {
			r.mu.Unlock()
			return
		}
	}
	if len(r.files) >= max || filesValueBytes(r.files, subagentFileChange{Path: path, Action: action}) > maxBytes {
		r.filesOmitted++
		r.mu.Unlock()
		return
	}
	r.files = append(r.files, subagentFileChange{Path: path, Action: action})
	r.mu.Unlock()
	r.store.save(r)
}

// changedInformation lists the files the child changed. It says
// nothing for a run that is not allowed to change files, and for one that changed
// none it says so, because "none" is also an answer.
func changedInformation(run subagentRunView) []tools.Information {
	information := append(changedFilesInformation(run), worktreeInformation(run)...)
	if run.note != "" {
		information = append(information, tools.Information{Key: "background_note", Value: run.note})
	}
	if run.configNote != "" {
		information = append(information, tools.Information{Key: "config_note", Value: run.configNote})
	}
	if run.processesStopped > 0 {
		information = append(information, tools.Information{Key: "stopped_processes", Value: run.processesStopped})
	}
	return information
}

func changedFilesInformation(run subagentRunView) []tools.Information {
	if !run.tracked {
		return nil
	}
	if len(run.files) == 0 && run.filesOmitted == 0 {
		return []tools.Information{{Key: "written_files", Value: "none (exec's changes are listed only with hooks.shell_changes scan)"}}
	}
	information := []tools.Information{{Key: "written_files", Value: filesValue(run.files)}}
	if run.filesOmitted > 0 {
		information = append(information, tools.Information{Key: "written_files_omitted", Value: run.filesOmitted})
	}
	return information
}

// changedFilesText is the changed-file list of a run as one line, and the worktree branch
// of an isolated run as a second, for the async handle's summary and status. It is empty
// for a run that does neither.
func changedFilesText(run subagentRunView) string {
	return strings.TrimSpace(changedListText(run) + "\n" + worktreeText(run))
}

func changedListText(run subagentRunView) string {
	if !run.tracked {
		return ""
	}
	if len(run.files) == 0 && run.filesOmitted == 0 {
		return "changed files: none (exec's changes are listed only with hooks.shell_changes scan)"
	}
	text := "changed files: " + filesValue(run.files)
	if run.filesOmitted > 0 {
		text += fmt.Sprintf(", and %d more", run.filesOmitted)
	}
	return text
}

// filesValue is the changed-file list as one value: each file as "path (action)", joined by
// commas.
func filesValue(files []subagentFileChange) string {
	parts := make([]string, 0, len(files))
	for _, file := range files {
		parts = append(parts, fmt.Sprintf("%s (%s)", file.Path, file.Action))
	}
	return strings.Join(parts, ", ")
}

// filesValueBytes is the length of filesValue(files) with one more file added: each entry
// is its path, " (", its action and ")", and entries are joined by ", ".
func filesValueBytes(files []subagentFileChange, more subagentFileChange) int64 {
	size := len(more.Path) + len(" (") + len(more.Action) + len(")")
	for _, file := range files {
		size += len(file.Path) + len(" (") + len(file.Action) + len(")") + len(", ")
	}
	return int64(size)
}
