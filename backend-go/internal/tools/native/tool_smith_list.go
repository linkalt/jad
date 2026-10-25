package native

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var toolSmithFunctionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

const toolSmithListLegend = "call as: fn.<name>(input) inside code (await it); each entry: call expression, file, description"

type toolSmithListEntry struct {
	name        string
	description string
	line        string
}

// resolveToolSmithFunctionsDir returns the real functions directory, which must lie inside the real workspace root.
func resolveToolSmithFunctionsDir(configured, workspaceRoot string) (dir, realRoot string, err error) {
	if strings.TrimSpace(configured) == "" {
		return "", "", fmt.Errorf("functions_dir is empty")
	}
	if filepath.IsAbs(configured) || strings.HasPrefix(configured, "/") {
		return "", "", fmt.Errorf("functions_dir %q must be relative to the workspace", configured)
	}
	for _, part := range strings.Split(filepath.ToSlash(configured), "/") {
		if part == ".." {
			return "", "", fmt.Errorf("functions_dir %q must not contain ..", configured)
		}
	}
	if workspaceRoot == "" {
		return "", "", fmt.Errorf("the workspace root is unknown")
	}
	realRoot, err = filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", "", fmt.Errorf("workspace root: %w", err)
	}
	dir, err = filepath.EvalSymlinks(filepath.Join(realRoot, configured))
	if err != nil {
		return "", "", err
	}
	if !toolSmithInside(realRoot, dir) {
		return "", "", fmt.Errorf("functions_dir %q resolves outside the workspace", configured)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("functions_dir %q is not a directory", configured)
	}
	return dir, realRoot, nil
}

func toolSmithInside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func executeToolSmithList(arguments map[string]any, settings sessionfs.NativeToolSmithSettings, execution tools.ExecutionContext) string {
	callID := execution.CallID
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
	var files []string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// a library that was never created simply has no functions
	case err != nil:
		return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be opened",
			tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
			tools.Information{Key: "system_error", Value: err.Error()})
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return tools.ExecutionError(callID, tools.StatusError, "list_failed", "The function library could not be read",
				tools.Information{Key: "functions_dir", Value: settings.FunctionsDir},
				tools.Information{Key: "system_error", Value: err.Error()})
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".ts") {
				files = append(files, entry.Name())
			}
		}
	}
	scanned := files
	if int64(len(scanned)) > settings.ListMaxFiles {
		scanned = scanned[:settings.ListMaxFiles]
	}

	var matched []toolSmithListEntry
	var problems []string
	for _, file := range scanned {
		entry, problem := readToolSmithListEntry(dir, realRoot, file, settings.ListHeaderBytes)
		if problem != "" {
			problems = append(problems, file+": "+problem)
			continue
		}
		if toolSmithMatchesFilters(entry, filters) {
			matched = append(matched, entry)
		}
	}

	shown := matched
	if int64(len(shown)) > settings.ListMaxResults {
		shown = shown[:settings.ListMaxResults]
	}
	lines := make([]string, 0, len(shown)+len(problems)+2)
	if len(shown) > 0 {
		lines = append(lines, toolSmithListLegend)
	}
	for _, entry := range shown {
		lines = append(lines, entry.line)
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
		if len(scanned) < len(files) {
			cut += fmt.Sprintf("; scanned %d of %d files", len(scanned), len(files))
		}
		suggestion := "add filters (read*, *file*, or a plain substring of the name or description) to narrow the result"
		if len(scanned) < len(files) {
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

// toolSmithFilter is one lower-cased filter: a wildcard pattern when it has * or ?, else a substring.
type toolSmithFilter struct {
	text     string
	wildcard bool
}

func newToolSmithFilter(text string) toolSmithFilter {
	return toolSmithFilter{text: text, wildcard: strings.ContainsAny(text, "*?")}
}

// matches reports whether the whole of value matches a wildcard filter (* any run, ? one
// character), or whether a plain filter is contained in value.
func (f toolSmithFilter) matches(value string) bool {
	if !f.wildcard {
		return strings.Contains(value, f.text)
	}
	return wildcardMatch([]rune(f.text), []rune(value))
}

// wildcardMatch matches pattern against all of value with * and ?, in linear time.
func wildcardMatch(pattern, value []rune) bool {
	p, v, star, mark := 0, 0, -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]) && pattern[p] != '*':
			p++
			v++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case star >= 0:
			mark++
			p, v = star+1, mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// toolSmithMatchesFilters is true when every filter matches the name or the description.
func toolSmithMatchesFilters(entry toolSmithListEntry, filters []toolSmithFilter) bool {
	name, description := strings.ToLower(entry.name), strings.ToLower(entry.description)
	for _, filter := range filters {
		if !filter.matches(name) && !filter.matches(description) {
			return false
		}
	}
	return true
}

// readToolSmithListEntry reads the header of one library file; problem is non-empty when the file is malformed.
func readToolSmithListEntry(dir, realRoot, file string, headerBytes int64) (toolSmithListEntry, string) {
	name := strings.TrimSuffix(file, ".ts")
	if !toolSmithFunctionName.MatchString(name) {
		return toolSmithListEntry{}, "name must match ^[a-z][a-z0-9_]*$"
	}
	path := filepath.Join(dir, file)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return toolSmithListEntry{}, "cannot be resolved: " + err.Error()
	}
	if !toolSmithInside(dir, real) || !toolSmithInside(realRoot, real) {
		return toolSmithListEntry{}, "symlink leaves the functions directory"
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return toolSmithListEntry{}, "is not a regular file"
	}
	handle, _, problem := openToolSmithFile(real, info)
	if problem != "" {
		return toolSmithListEntry{}, problem
	}
	defer handle.Close()
	head, err := io.ReadAll(io.LimitReader(handle, headerBytes))
	if err != nil {
		return toolSmithListEntry{}, "cannot be read: " + err.Error()
	}
	text := strings.ReplaceAll(string(head), "\r\n", "\n")

	description := toolSmithHeaderDescription(text)
	if description == "" {
		return toolSmithListEntry{}, "missing leading // header comment with a description"
	}
	signature, found := toolSmithSignature(text, name)
	if !found {
		return toolSmithListEntry{}, fmt.Sprintf("no `export async function %s(` in the first %d bytes", name, headerBytes)
	}
	return toolSmithListEntry{name: name, description: description, line: "fn." + name + "(" + signature + ")   " + file + " — " + description}, ""
}

// toolSmithHeaderDescription returns the first non-empty line of the leading // comment block.
func toolSmithHeaderDescription(text string) string {
	description := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && description == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "//") {
			break
		}
		if description == "" {
			description = strings.TrimSpace(strings.TrimLeft(trimmed, "/"))
		}
	}
	return description
}

// toolSmithSignature returns the parenthesized parameter text of `export async function <name>(...)` on one line.
func toolSmithSignature(text, name string) (string, bool) {
	pattern := regexp.MustCompile(`(?m)^export\s+async\s+function\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	location := pattern.FindStringIndex(text)
	if location == nil {
		return "", false
	}
	rest := text[location[1]:]
	depth := 1
	end := len(rest)
	for index, char := range rest {
		if char == '(' {
			depth++
		} else if char == ')' {
			depth--
			if depth == 0 {
				end = index
				break
			}
		}
	}
	return strings.Join(strings.Fields(rest[:end]), " "), true
}
