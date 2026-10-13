package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	globToolID                  = "native.glob"
	globKillGrace               = 2 * time.Second
	globPatternAlternativeLimit = 64
	globTruncatedLineMarker     = " [... truncated line]"
)

var globInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"File name or path pattern to match. Omit it to list every visible file."},
    "path":{"type":"string","minLength":1,"description":"Directory to search, relative to the workspace root or absolute. Defaults to the workspace root."}
  },
  "required":[],
  "additionalProperties":false
}`)

type compiledGlob struct {
	byPath      bool
	expressions []*regexp.Regexp
}

func (glob *compiledGlob) matches(name, path string) bool {
	value := name
	if glob.byPath {
		value = path
	}
	for _, expression := range glob.expressions {
		if expression.MatchString(value) {
			return true
		}
	}
	return false
}

// expandGlobBraces expands the first brace alternative recursively, stopping at
// the same bounded number of alternatives as the TypeScript implementation.
func expandGlobBraces(pattern string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	relativeClose := strings.IndexByte(pattern[open+1:], '}')
	if relativeClose < 0 {
		return []string{pattern}
	}
	close := open + 1 + relativeClose
	options := strings.Split(pattern[open+1:close], ",")
	if len(options) < 2 {
		return []string{pattern}
	}
	results := make([]string, 0, limit)
	for _, option := range options {
		next := pattern[:open] + option + pattern[close+1:]
		for _, expanded := range expandGlobBraces(next, limit) {
			results = append(results, expanded)
			if len(results) >= limit {
				return results
			}
		}
	}
	return results
}

func globRegexpSource(pattern string) string {
	var source strings.Builder
	source.WriteByte('^')
	for index := 0; index < len(pattern); {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				if index+2 < len(pattern) && pattern[index+2] == '/' {
					// Go's regexp package has no non-capturing groups.
					source.WriteString("(.*/)?")
					index += 3
					continue
				}
				source.WriteString(".*")
				index += 2
				continue
			}
			source.WriteString("[^/]*")
			index++
		case '?':
			source.WriteString("[^/]")
			index++
		default:
			_, size := utf8.DecodeRuneInString(pattern[index:])
			source.WriteString(regexp.QuoteMeta(pattern[index : index+size]))
			index += size
		}
	}
	source.WriteByte('$')
	return source.String()
}

func compileGlob(pattern string) (*compiledGlob, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	compiled := &compiledGlob{byPath: strings.Contains(pattern, "/")}
	for _, alternative := range expandGlobBraces(pattern, globPatternAlternativeLimit) {
		expression, err := regexp.Compile(globRegexpSource(alternative))
		if err != nil {
			return nil, err
		}
		compiled.expressions = append(compiled.expressions, expression)
	}
	return compiled, nil
}

func parseGlobListedFiles(text string, truncated bool) []string {
	entries := strings.Split(text, "\x00")
	if truncated && !strings.HasSuffix(text, "\x00") {
		entries = entries[:len(entries)-1]
	}
	if len(entries) > 0 && entries[len(entries)-1] == "" {
		entries = entries[:len(entries)-1]
	}
	return entries
}

func normalizeGlobArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments)+1)
	for key, value := range arguments {
		normalized[key] = value
	}
	if pattern, ok := normalized["pattern"].(string); ok {
		normalized["pattern"] = strings.TrimSpace(pattern)
	} else {
		alias := arguments["glob"]
		if alias == nil {
			alias = arguments["include"]
		}
		if pattern, ok := alias.(string); ok {
			normalized["pattern"] = strings.TrimSpace(pattern)
		}
	}
	if _, exists := normalized["path"]; !exists {
		if directory, ok := arguments["dir"].(string); ok {
			normalized["path"] = directory
		}
	}
	delete(normalized, "glob")
	delete(normalized, "include")
	delete(normalized, "dir")
	return normalized
}

// NewGlobTool constructs native.glob from normalized per-session settings.
func NewGlobTool(settings sessionfs.NativeGlobSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(globInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native glob schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          globToolID,
			Source:      "native",
			Name:        "glob",
			Description: "List workspace files by glob; use grep to search file contents.",
			InputSchema: append(json.RawMessage(nil), globInputSchema...),
		},
		Validator:          validator,
		ReadOnly:           true,
		ExampleOperation:   json.RawMessage(`{"pattern":"*.test.ts"}`),
		NormalizeArguments: normalizeGlobArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeGlob(ctx, arguments, settings, execution), nil
		},
	}, nil
}

type listedGlobFile struct {
	path string
}

func executeNativeGlob(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeGlobSettings,
	execution tools.ExecutionContext,
) string {
	args := parseGlobArguments(arguments)
	compiled, err := compileGlob(args.pattern)
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_pattern", "The file pattern could not be compiled", tools.Information{Key: "pattern", Value: args.pattern})
	}
	resolved, pathError := resolveExistingTarget(args.path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathError != "" {
		return pathError
	}
	info, err := os.Stat(resolved.realPath)
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "path_unavailable", "The directory is not readable", tools.Information{Key: "path", Value: resolved.displayPath})
	}
	if !info.IsDir() {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "not_a_directory", "The path is not a directory", tools.Information{Key: "path", Value: resolved.displayPath})
	}

	stdoutCollector := newBashOutputCollector(
		bashStreamLimits{headBytes: int(settings.MaxBytes)},
		bashStreamLimits{headBytes: int(settings.MaxBytes)},
		int(settings.MaxBytes),
		false,
	)
	child := runBashCommand(ctx, bashChildRequest{
		file:                     settings.Binary,
		args:                     globCommandArgs(resolved.displayPath, settings.IncludeHidden),
		cwd:                      resolved.workspaceRoot,
		environment:              environmentWithoutServerToken(),
		timeout:                  time.Duration(settings.TimeoutMs) * time.Millisecond,
		killGrace:                globKillGrace,
		killProcessGroupWhenDone: true,
		collector:                stdoutCollector,
	})
	stdout := string(stdoutCollector.stdout.head)
	stdoutTruncated := stdoutCollector.stdout.producedBytes > int64(len(stdoutCollector.stdout.head))

	if child.spawnError != nil {
		unavailable := errors.Is(child.spawnError, exec.ErrNotFound) || errors.Is(child.spawnError, os.ErrNotExist)
		information := []tools.Information{{Key: "binary", Value: settings.Binary}}
		code, summary := "glob_failed", "The file listing could not be started"
		if unavailable {
			code, summary = "ripgrep_unavailable", "ripgrep is not available"
			information = append(information, tools.Information{Key: "suggestion", Value: "install ripgrep or point tools.native.glob.binary at an executable"})
		} else {
			information = append(information, tools.Information{Key: "system_error", Value: child.spawnError.Error()})
		}
		return tools.ExecutionError(execution.CallID, tools.StatusError, code, summary, information...)
	}
	if !child.timedOut && child.hasExitCode && child.exitCode != 0 && child.exitCode != 1 {
		diagnostic := globFirstDiagnosticLine(child.output.text)
		information := []tools.Information{{Key: "path", Value: resolved.displayPath}, {Key: "exit_code", Value: child.exitCode}}
		if diagnostic != "" {
			information = append(information, tools.Information{Key: "system_error", Value: diagnostic})
		}
		return tools.ExecutionError(execution.CallID, tools.StatusError, "glob_failed", "The file listing failed", information...)
	}

	listed := make([]listedGlobFile, 0)
	reachedResultCap := false
	for _, value := range parseGlobListedFiles(stdout, stdoutTruncated) {
		absolute := value
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(resolved.workspaceRoot, absolute)
		}
		name := filepath.Base(absolute)
		patternPath := strings.TrimPrefix(value, "./")
		if compiled != nil && !compiled.matches(name, patternPath) {
			continue
		}
		if pathPolicyError(absolute, resolved.workspaceRoot, settings.Policy, execution.CallID) != "" {
			continue
		}
		if int64(len(listed)) >= settings.MaxResults {
			reachedResultCap = true
			break
		}
		listed = append(listed, listedGlobFile{path: displayPath(resolved.workspaceRoot, absolute)})
	}

	truncated := reachedResultCap || stdoutTruncated || child.timedOut
	truncationReason := ""
	if reachedResultCap {
		truncationReason = "max_results"
	} else if child.timedOut {
		truncationReason = "timeout"
	} else if stdoutTruncated {
		truncationReason = "max_bytes"
	}
	content := make([]string, 0, len(listed))
	for _, file := range listed {
		renderedPath := file.path
		if strings.ContainsAny(file.path, "\n\r") {
			renderedPath = quoteGlobPath(file.path)
		}
		content = append(content, truncateGlobRenderedPath(renderedPath, int(settings.MaxLineChars)))
	}
	contentText := strings.Join(content, "\n")
	hints := make([]string, 0, 2)
	if truncated {
		hints = append(hints, "narrow the pattern or path")
	}
	if len(listed) == 0 {
		hints = append(hints, "files ignored by git are not listed inside a repository; read a known path directly")
	}
	if (truncated || len(listed) == 0) && !globInRepository(resolved.realPath) {
		hints = append(hints, "this tree is not a git repository, so ignore rules do not apply")
	}

	information := []tools.Information{{Key: "path", Value: resolved.displayPath}}
	if args.pattern != "" {
		information = append(information, tools.Information{Key: "pattern", Value: args.pattern})
	}
	information = append(information,
		tools.Information{Key: "files", Value: len(listed)},
		tools.Information{Key: "truncated", Value: truncated},
	)
	if truncationReason != "" {
		information = append(information, tools.Information{Key: "truncation_reason", Value: truncationReason})
	}
	if len(hints) > 0 {
		information = append(information, tools.Information{Key: "hint", Value: strings.Join(hints, "; ")})
	}
	summary := fmt.Sprintf("%d files", len(listed))
	if args.pattern != "" {
		summary = fmt.Sprintf("%d files matching %s", len(listed), args.pattern)
	}
	contentValue := (*tools.Content)(nil)
	if contentText != "" {
		contentValue = &tools.Content{Text: contentText, Encoding: "utf8"}
	}
	rendered, err := tools.RenderToolResult(tools.ResultOptions{
		ID:          execution.CallID,
		Status:      tools.StatusOK,
		Summary:     summary,
		Information: information,
		Content:     contentValue,
	})
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "tool_output_failed", "The file listing could not be rendered")
	}
	return rendered
}

type globArguments struct {
	pattern string
	path    string
}

func parseGlobArguments(arguments map[string]any) globArguments {
	parsed := globArguments{path: "."}
	if pattern, ok := arguments["pattern"].(string); ok {
		parsed.pattern = strings.TrimSpace(pattern)
	}
	if path, ok := arguments["path"].(string); ok && strings.TrimSpace(path) != "" {
		parsed.path = path
	}
	return parsed
}

func globCommandArgs(path string, includeHidden bool) []string {
	arguments := []string{"--files", "--no-config", "--no-messages", "--sort", "path", "--null"}
	if includeHidden {
		arguments = append(arguments, "--hidden")
	}
	arguments = append(arguments, "--glob", "!.git/**", "--", path)
	return arguments
}

func globFirstDiagnosticLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		units := utf16.Encode([]rune(line))
		if len(units) > 200 {
			line = string(utf16.Decode(units[:200]))
		}
		return line
	}
	return ""
}

func quoteGlobPath(path string) string {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(path); err != nil {
		return "\"\""
	}
	return strings.TrimSuffix(output.String(), "\n")
}

func truncateGlobRenderedPath(path string, maxLineChars int) string {
	if maxLineChars <= 0 {
		return path
	}
	characters := []rune(path)
	if len(characters) <= maxLineChars {
		return path
	}
	return string(characters[:maxLineChars]) + globTruncatedLineMarker
}

func globInRepository(directory string) bool {
	for current := filepath.Clean(directory); ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}
