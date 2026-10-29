package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// execHelperName is a helper's file name: a letter or digit first, then letters, digits, _ . -.
var execHelperName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// execFunction is one saved helper: a file in functions_dir/<runner>/.
type execFunction struct {
	name        string
	runner      string
	description string
	input       string
}

// execHelperEnvironment returns what a run of this runner gets so its code can reach the helpers:
// prepend puts functions_dir/<runner> in front of the runner's list variable, extra sets
// JAD_FUNCTIONS_DIR to the real library root. Both are empty when the library does not exist;
// prepend is empty when the runner's directory is missing or does not resolve inside the library.
func execHelperEnvironment(settings sessionfs.NativeExecSettings, runner sessionfs.NativeExecLang, name, workspaceRoot string) (prepend map[string]string, extra map[string]string) {
	root, realRoot, err := resolveToolSmithFunctionsDir(settings.FunctionsDir, workspaceRoot)
	if err != nil {
		return nil, nil
	}
	extra = map[string]string{execFunctionsDirEnv: root}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Not created yet: a program may make it, and fn looks here at call time.
		extra[execRunnerDirEnv] = filepath.Join(root, name)
		return nil, extra
	case err != nil || !toolSmithInside(root, dir) || !toolSmithInside(realRoot, dir):
		return nil, extra
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, extra
	}
	extra[execRunnerDirEnv] = dir
	if runner.LibEnv == "" {
		return nil, extra
	}
	return map[string]string{runner.LibEnv: dir}, extra
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
func readExecFunctionEntry(dir, realRoot, file, runner string, settings sessionfs.NativeExecSettings) (execFunction, string) {
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
	return execFunction{name: filepath.Base(file), runner: runner, description: description, input: input}, ""
}

// scanExecHelpers lists the files of every runner's directory under dir, in runner order, and
// the entries of dir that no runner owns. scanned is cut at list_max_files.
func scanExecHelpers(dir string, settings sessionfs.NativeExecSettings) (files []execHelperFile, scanned []execHelperFile, strays []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	for _, runner := range settings.LangNames {
		if !present[runner] {
			continue
		}
		delete(present, runner)
		inner, err := os.ReadDir(filepath.Join(dir, runner))
		if err != nil {
			strays = append(strays, runner+"/: "+err.Error())
			continue
		}
		for _, entry := range inner {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), execTemplateFilePrefix) {
				continue
			}
			files = append(files, execHelperFile{runner: runner, name: entry.Name()})
		}
	}
	for name := range present {
		if !strings.HasPrefix(name, ".") {
			strays = append(strays, name+": not a runner name, so its files are not helpers")
		}
	}
	sort.Strings(strays)
	scanned = files
	if int64(len(scanned)) > settings.ListMaxFiles {
		scanned = scanned[:settings.ListMaxFiles]
	}
	return files, scanned, strays, nil
}

type execHelperFile struct{ runner, name string }

// execListLegend explains the call column of the listing and the rule that makes fn.<name> callable.
var execListLegend = []string{
	"call as: fn.<name>(...) in ts/node/python/ruby; the file name itself in bash/sh (the folder is on PATH)",
	"fn.<name>(...) runs the helper's entry; every other export stays reachable as fn.<name>.<member>(...)",
	"entry: ts/node = default export, else the export named <name>, else the only function; python = the function named <name>, else the only public function; ruby = self.call, else self.<name>, else the only singleton method (needs arguments; fn.<name> alone is the constant)",
	"",
}

var (
	execJSIdentifier     = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	execPythonIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// execHelperCall is what to type to call the helper stored in file: fn.<name>(...) for a runner with a
// template (bracket or getattr form when the name is not an identifier), the file name for the others.
// The file name follows the call when it differs.
func execHelperCall(file string, lang sessionfs.NativeExecLang) string {
	if lang.Template == "" {
		return file
	}
	name := file
	if lang.Ext != "" {
		name = strings.TrimSuffix(file, lang.Ext)
	}
	var call string
	switch {
	case lang.Template == sessionfs.NativeExecTemplatePython && execPythonIdentifier.MatchString(name):
		call = "fn." + name + "(...)"
	case lang.Template == sessionfs.NativeExecTemplatePython:
		call = `getattr(fn, "` + name + `")(...)`
	case lang.Template == sessionfs.NativeExecTemplateRuby && execJSIdentifier.MatchString(name) && !strings.Contains(name, "$"):
		call = "fn." + name + "(...)"
	case lang.Template == sessionfs.NativeExecTemplateRuby:
		call = `fn.public_send("` + name + `", ...)`
	case execJSIdentifier.MatchString(name):
		call = "fn." + name + "(...)"
	default:
		call = `fn["` + name + `"](...)`
	}
	return call + "   " + file
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
	var files, scanned []execHelperFile
	var strays []string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// a library that was never created simply has no helpers
	case err != nil:
		return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be opened",
			tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
			tools.Information{Key: "system_error", Value: err.Error()})
	default:
		files, scanned, strays, err = scanExecHelpers(dir, settings)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be read",
				tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
				tools.Information{Key: "system_error", Value: err.Error()})
		}
	}
	var matched []execFunction
	problems := append([]string(nil), strays...)
	for _, file := range scanned {
		path := file.runner + "/" + file.name
		if !execHelperName.MatchString(file.name) {
			problems = append(problems, path+": name must match "+execHelperName.String())
			continue
		}
		entry, problem := readExecFunctionEntry(dir, realRoot, path, file.runner, settings)
		if problem != "" {
			problems = append(problems, path+": "+problem)
			continue
		}
		if toolSmithMatchesFilters(toolSmithListEntry{name: entry.name, description: entry.description}, filters) {
			matched = append(matched, entry)
		}
	}
	runnerOrder := map[string]int{}
	for index, name := range settings.LangNames {
		runnerOrder[name] = index
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].runner != matched[j].runner {
			return runnerOrder[matched[i].runner] < runnerOrder[matched[j].runner]
		}
		return matched[i].name < matched[j].name
	})
	shown := matched
	if int64(len(shown)) > settings.ListMaxResults {
		shown = shown[:settings.ListMaxResults]
	}
	// The stray and malformed half is bounded by the library directory, not by list_max_files,
	// so it is capped like the helpers are (P6BM-11).
	shownProblems := problems
	if int64(len(shownProblems)) > settings.ListMaxResults {
		shownProblems = shownProblems[:settings.ListMaxResults]
	}
	lines := make([]string, 0, len(shown)+len(shownProblems)+2)
	if len(shown) > 0 {
		lines = append(lines, execListLegend...)
	}
	for index, entry := range shown {
		if index == 0 || shown[index-1].runner != entry.runner {
			lines = append(lines, entry.runner+":")
		}
		line := "  " + execHelperCall(entry.name, settings.Langs[entry.runner]) + " — " + entry.description
		if entry.input != "" {
			line += " (input: " + entry.input + ")"
		}
		lines = append(lines, line)
	}
	if len(problems) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("malformed entries (%d):", len(problems)))
		for _, problem := range shownProblems {
			lines = append(lines, "- "+problem)
		}
		if hidden := len(problems) - len(shownProblems); hidden > 0 {
			lines = append(lines, fmt.Sprintf("- …and %d more", hidden))
		}
	}
	summary := fmt.Sprintf("%d helper%s", len(shown), pluralSuffix(len(shown)))
	if len(problems) > 0 {
		summary += fmt.Sprintf(", %d malformed", len(problems))
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
