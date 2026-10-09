package native

import (
	"fmt"
	"strings"

	"example.com/agent-harness/backend-go/internal/tools"
)

// subagentFileChange is one file a child's write or edit changed.
type subagentFileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// recordChange notes that the child's write or edit changed a file. A path is listed
// once, with the action of its first change; at most max paths are kept, and the rest
// are only counted. It is called from the child's goroutines, so it takes the run lock.
func (r *subagentRun) recordChange(path, action string, max int) {
	r.mu.Lock()
	for _, known := range r.files {
		if known.Path == path {
			r.mu.Unlock()
			return
		}
	}
	if len(r.files) >= max {
		r.filesOmitted++
		r.mu.Unlock()
		return
	}
	r.files = append(r.files, subagentFileChange{Path: path, Action: action})
	r.mu.Unlock()
	r.store.save(r)
}

// changedInformation lists the files the child's write and edit changed. It says
// nothing for a run that is not allowed to change files, and for one that changed
// none it says so, because "none" is also an answer.
func changedInformation(run subagentRunView) []tools.Information {
	if !run.tracked {
		return nil
	}
	if len(run.files) == 0 && run.filesOmitted == 0 {
		return []tools.Information{{Key: "written_files", Value: "none (changes made by bash or exec are not listed)"}}
	}
	parts := make([]string, 0, len(run.files))
	for _, file := range run.files {
		parts = append(parts, fmt.Sprintf("%s (%s)", file.Path, file.Action))
	}
	information := []tools.Information{{Key: "written_files", Value: strings.Join(parts, ", ")}}
	if run.filesOmitted > 0 {
		information = append(information, tools.Information{Key: "written_files_omitted", Value: run.filesOmitted})
	}
	return information
}
