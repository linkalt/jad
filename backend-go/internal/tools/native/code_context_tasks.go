package native

import (
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// codeContextTasks answers a call without a task: the tasks of the storage, most recently
// written first, with their descriptions and entry counts. It reads only the task files,
// never the code, so it says nothing of staleness; list with task does.
func codeContextTasks(store codeContextStore, settings sessionfs.NativeCodeContextSettings, callID string) string {
	files, total, err := store.tasks("", settings.MaxScanTasks)
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}
	var output strings.Builder
	switch {
	case total == 0:
		output.WriteString("# code_context: no tasks yet; read or add a range with a task to start one\ntasks: []\n")
		return output.String()
	case total > len(files):
		fmt.Fprintf(&output, "# code_context: the %d most recently written of %d tasks (max_scan_tasks); list one with task\n", len(files), total)
	default:
		fmt.Fprintf(&output, "# code_context: %d tasks; list one with task\n", total)
	}
	output.WriteString("tasks:\n")
	for _, file := range files {
		fmt.Fprintf(&output, "  - task: %s\n", yamlString(file.task))
		document, err := store.load(file.task)
		if err != nil {
			fmt.Fprintf(&output, "    error: %s\n", yamlString(oneLine(err.Error())))
			continue
		}
		if document.Description == "" {
			output.WriteString("    description: \"\"\n")
		} else {
			writeCodeContextYAMLText(&output, "description", document.Description, "    ")
		}
		fmt.Fprintf(&output, "    entries: %d\n    generation: %d\n", len(document.Entries), document.Generation)
		if document.LastUpdatedBy != "" {
			fmt.Fprintf(&output, "    last_updated_by: %s\n", yamlString(document.LastUpdatedBy))
		}
		fmt.Fprintf(&output, "    updated_at: %s\n", yamlString(file.modified.UTC().Format(time.RFC3339)))
	}
	return output.String()
}

// writeCodeContextYAMLText writes key as a literal block of text at indent; an empty
// text writes nothing.
func writeCodeContextYAMLText(output *strings.Builder, key, text, indent string) {
	if text == "" {
		return
	}
	output.WriteString(indent + key + ": |-\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			output.WriteString("\n")
			continue
		}
		output.WriteString(indent + "  " + line + "\n")
	}
}

// codeContextTaskDescriptionLine is the task's description as get heads its answer
// with, starting with a newline; empty when the task has none.
func codeContextTaskDescriptionLine(document codeContextDocument) string {
	if document.Description == "" {
		return ""
	}
	return "\ntask description: " + strings.ReplaceAll(document.Description, "\n", "\n  ")
}
