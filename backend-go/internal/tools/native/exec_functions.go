package native

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// execFunction is one saved function: a source file whose extension selects its runner.
type execFunction struct {
	name        string
	runner      string
	file        string
	description string
	input       string
}

// execFunctionExtensions maps an extension to the first runner, in table order, that
// declares it, so a file has exactly one runner (.sh belongs to bash, not sh).
func execFunctionExtensions(settings sessionfs.NativeExecSettings) map[string]string {
	extensions := map[string]string{}
	for _, name := range settings.LangNames {
		if ext := settings.Langs[name].Ext; ext != "" {
			if _, taken := extensions[ext]; !taken {
				extensions[ext] = name
			}
		}
	}
	return extensions
}

// openExecFunction resolves one library file inside the functions directory and opens it
// without following a link; problem is non-empty when the file cannot be used.
func openExecFunction(dir, realRoot, file string, maxBytes int64) (*os.File, string) {
	real, err := filepath.EvalSymlinks(filepath.Join(dir, file))
	if err != nil {
		return nil, "cannot be resolved: " + err.Error()
	}
	if !toolSmithInside(dir, real) || !toolSmithInside(realRoot, real) {
		return nil, "symlink leaves the functions directory"
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "is not a regular file"
	}
	handle, opened, problem := openToolSmithFile(real, info)
	if problem != "" {
		return nil, problem
	}
	if opened.Size() > maxBytes {
		handle.Close()
		return nil, fmt.Sprintf("is %d bytes, over function_max_bytes %d", opened.Size(), maxBytes)
	}
	return handle, ""
}

// execFunctionHeader reads the leading comment block of a source file (after a shebang):
// its first line is the description; a later line starting with "input:" documents the input.
func execFunctionHeader(text string) (description, input string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		lines = lines[1:]
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && description == "" {
			continue
		}
		var body string
		switch {
		case strings.HasPrefix(trimmed, "//"):
			body = strings.TrimLeft(trimmed, "/")
		case strings.HasPrefix(trimmed, "--"):
			body = strings.TrimLeft(trimmed, "-")
		case strings.HasPrefix(trimmed, "#"):
			body = strings.TrimLeft(trimmed, "#")
		default:
			return description, input
		}
		body = strings.TrimSpace(body)
		switch {
		case description == "":
			description = body
		case input == "" && strings.HasPrefix(strings.ToLower(body), "input:"):
			input = strings.TrimSpace(body[len("input:"):])
		}
	}
	return description, input
}

// readExecFunctionEntry describes one library file; problem is non-empty when it is malformed.
func readExecFunctionEntry(dir, realRoot, file, name, runner string, settings sessionfs.NativeExecSettings) (execFunction, string) {
	if !toolSmithFunctionName.MatchString(name) {
		return execFunction{}, "name must match ^[a-z][a-z0-9_]*$"
	}
	handle, problem := openExecFunction(dir, realRoot, file, settings.FunctionMaxBytes)
	if problem != "" {
		return execFunction{}, problem
	}
	defer handle.Close()
	head, err := io.ReadAll(io.LimitReader(handle, settings.ListHeaderBytes))
	if err != nil {
		return execFunction{}, "cannot be read: " + err.Error()
	}
	description, input := execFunctionHeader(string(head))
	if description == "" {
		return execFunction{}, "missing a leading comment line (#, // or --) with a description"
	}
	return execFunction{name: name, runner: runner, file: file, description: description, input: input}, ""
}

// scanExecFunctions lists the library files that belong to a runner, cut at list_max_files.
func scanExecFunctions(dir string, settings sessionfs.NativeExecSettings) (files []string, scanned []string, runners map[string]string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	extensions := execFunctionExtensions(settings)
	runners = map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if runner, known := extensions[ext]; known {
			files = append(files, entry.Name())
			runners[entry.Name()] = runner
		}
	}
	scanned = files
	if int64(len(scanned)) > settings.ListMaxFiles {
		scanned = scanned[:settings.ListMaxFiles]
	}
	return files, scanned, runners, nil
}

func executeExecList(arguments map[string]any, settings sessionfs.NativeExecSettings, execution tools.ExecutionContext) string {
	callID := execution.CallID
	for _, key := range []string{"lang", "input"} {
		if value, has := arguments[key]; has && value != nil {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", key+" is only for running; give code, or call exec with no parameters (or only filters) to list the saved helpers")
		}
	}
	var filters []toolSmithFilter
	given, _ := arguments["filters"].([]any)
	for _, item := range given {
		text, ok := item.(string)
		if !ok {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_input", "filters must be strings")
		}
		if text = strings.ToLower(strings.TrimSpace(text)); text != "" {
			filters = append(filters, newToolSmithFilter(text))
		}
	}
	dir, realRoot, err := resolveToolSmithFunctionsDir(settings.FunctionsDir, execution.WorkspaceRoot)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be opened",
			tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	files, scanned, runners, err := scanExecFunctions(dir, settings)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be read",
			tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	var matched []execFunction
	var problems []string
	for _, file := range scanned {
		name := strings.TrimSuffix(file, filepath.Ext(file))
		entry, problem := readExecFunctionEntry(dir, realRoot, file, name, runners[file], settings)
		if problem != "" {
			problems = append(problems, file+": "+problem)
			continue
		}
		if toolSmithMatchesFilters(toolSmithListEntry{name: entry.name, description: entry.description}, filters) {
			matched = append(matched, entry)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].name < matched[j].name })
	shown := matched
	if int64(len(shown)) > settings.ListMaxResults {
		shown = shown[:settings.ListMaxResults]
	}
	lines := make([]string, 0, len(shown)+len(problems)+2)
	for _, entry := range shown {
		line := entry.name + " [" + entry.runner + "] — " + entry.description
		if entry.input != "" {
			line += " (input: " + entry.input + ")"
		}
		lines = append(lines, line)
	}
	if len(problems) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("malformed files (%d):", len(problems)))
		for _, problem := range problems {
			lines = append(lines, "- "+problem)
		}
	}
	summary := fmt.Sprintf("%d function%s", len(shown), pluralSuffix(len(shown)))
	if len(problems) > 0 {
		summary += fmt.Sprintf(", %d malformed file%s", len(problems), pluralSuffix(len(problems)))
	}
	options := tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary}
	if len(shown) < len(matched) || len(scanned) < len(files) {
		cut := fmt.Sprintf("%d of %d", len(shown), len(matched))
		suggestion := "add filters (read*, *file*, or a plain substring of the name or description) to narrow the result"
		if len(scanned) < len(files) {
			cut += fmt.Sprintf("; scanned %d of %d files", len(scanned), len(files))
			suggestion = "the library has more files than list_max_files; raise list_max_files or reduce the library (filters only apply to the scanned files)"
		}
		options.Information = append(options.Information,
			tools.Information{Key: "truncated", Value: true},
			tools.Information{Key: "shown", Value: cut},
			tools.Information{Key: "suggestion", Value: suggestion})
	}
	if len(lines) > 0 {
		options.Content = &tools.Content{Text: strings.Join(lines, "\n")}
	}
	result, err := tools.RenderToolResult(options)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The result could not be rendered",
			tools.Information{Key: "system_error", Value: err.Error()})
	}
	return result
}
