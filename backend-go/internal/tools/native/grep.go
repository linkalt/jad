package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const grepToolID = "native.grep"

var grepInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","minLength":1,"description":"ripgrep regular expression (Rust syntax: no lookaround or backreferences). Use literal when the text contains regex characters."},
    "path":{"type":"string","minLength":1,"description":"File or directory to search, relative to cwd or absolute. Defaults to the workspace root. Explicit paths are searched even when ignored or hidden."},
    "include":{"type":"string","minLength":1,"description":"Only search files matching this glob, for example \"*.ts\" or \"src/**/*.{ts,tsx}\"."},
    "literal":{"type":"boolean","description":"Treat the pattern as exact text instead of a regular expression."},
    "context":{"type":"integer","minimum":0,"maximum":20,"description":"Lines of context to show before and after each match."}
  },
  "required":["pattern"],
  "additionalProperties":false
}`)

type grepArgs struct {
	pattern string
	path    string
	include string
	literal bool
	context int64
}

// NewGrepTool constructs native.grep from normalized per-session settings.
func NewGrepTool(settings sessionfs.NativeGrepSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(grepInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native grep schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          grepToolID,
			Source:      "native",
			Name:        "grep",
			Description: "Search workspace file contents and group matching lines by file.",
			InputSchema: append(json.RawMessage(nil), grepInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"pattern":"TODO","path":"src"}`),
		NormalizeArguments: normalizeGrepArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeGrep(ctx, arguments, settings, execution), nil
		},
	}, nil
}

func normalizeGrepArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if contextLines, ok := safeInteger(arguments["context"]); ok {
		if contextLines < 0 {
			contextLines = 0
		}
		if contextLines > 20 {
			contextLines = 20
		}
		normalized["context"] = json.Number(strconv.FormatInt(contextLines, 10))
	}
	return normalized
}

func parseGrepArguments(arguments map[string]any) grepArgs {
	parsed := grepArgs{path: "."}
	parsed.pattern, _ = arguments["pattern"].(string)
	if path, ok := arguments["path"].(string); ok {
		parsed.path = path
	}
	parsed.include, _ = arguments["include"].(string)
	parsed.literal, _ = arguments["literal"].(bool)
	parsed.context, _ = safeInteger(arguments["context"])
	return parsed
}

func executeNativeGrep(
	ctx context.Context,
	arguments map[string]any,
	settings sessionfs.NativeGrepSettings,
	execution tools.ExecutionContext,
) string {
	args := parseGrepArguments(arguments)
	root, pathError := resolveExistingTarget(args.path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathError != "" {
		return pathError
	}

	flags := grepSearchFlags(args)
	flags = append(flags, deniedGrepTraversalFlags(settings.Policy, root.workspaceRoot)...)
	result := runRipgrep(ctx, settings, flags, args.pattern, []string{root.displayPath}, execution.WorkspaceRoot)
	if result.failure != nil {
		return grepFailureResult(execution.CallID, root.errorPath, args.pattern, result.failure)
	}

	allowed := make([]ripgrepGroup, 0, len(result.groups))
	matches := 0
	for _, group := range result.groups {
		resolved, denied := resolveExistingTarget(group.path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
		if denied != "" || resolved.displayPath == "" {
			continue
		}
		group.path = resolved.displayPath
		allowed = append(allowed, group)
		matches += group.matches
	}

	text := renderRipgrepGroups(allowed)
	files := len(allowed)
	information := []tools.Information{
		{Key: "path", Value: root.displayPath},
		{Key: "pattern", Value: args.pattern},
	}
	if args.include != "" {
		information = append(information, tools.Information{Key: "include", Value: args.include})
	}
	if args.literal {
		information = append(information, tools.Information{Key: "literal", Value: true})
	}
	if args.context > 0 {
		information = append(information, tools.Information{Key: "context", Value: args.context})
	}
	information = append(information,
		tools.Information{Key: "files", Value: files},
		tools.Information{Key: "matches", Value: matches},
		tools.Information{Key: "truncated", Value: result.truncated},
	)
	if result.truncationReason != "" {
		information = append(information, tools.Information{Key: "truncation_reason", Value: result.truncationReason})
	}
	if result.perFileLimitReached {
		information = append(information, tools.Information{Key: "per_file_limit_reached", Value: true})
	}
	if result.binarySkipped > 0 {
		information = append(information, tools.Information{Key: "binary_files_skipped", Value: result.binarySkipped})
	}
	if hints := grepSearchHints(result, matches); len(hints) > 0 {
		information = append(information, tools.Information{Key: "hint", Value: strings.Join(hints, "; ")})
	}

	content := (*tools.Content)(nil)
	if text != "" {
		content = &tools.Content{Text: text, Encoding: "utf8"}
	}
	rendered, err := tools.RenderToolResult(tools.ResultOptions{
		ID:          execution.CallID,
		Status:      tools.StatusOK,
		Summary:     grepSummary(result, args.pattern, files, matches),
		Information: information,
		Content:     content,
	})
	if err != nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "tool_output_failed", "The search result could not be rendered")
	}
	return rendered
}

func grepSearchFlags(args grepArgs) []string {
	flags := make([]string, 0, 5)
	if args.literal {
		flags = append(flags, "--fixed-strings")
	}
	if args.include != "" {
		flags = append(flags, "--glob", args.include)
	}
	if args.context > 0 {
		contextLines := args.context
		if contextLines > 20 {
			contextLines = 20
		}
		flags = append(flags, "--context", strconv.FormatInt(contextLines, 10))
	}
	return flags
}

func deniedGrepTraversalFlags(policy sessionfs.NativePathPolicy, workspaceRoot string) []string {
	flags := make([]string, 0)
	for _, rule := range policy.Paths {
		if rule.Action != "deny" || !isWithin(workspaceRoot, rule.Path) {
			continue
		}
		path := displayPath(workspaceRoot, rule.Path)
		if path == "." {
			continue
		}
		flags = append(flags, "--glob", "!"+path, "--glob", "!"+path+"/**")
	}
	return flags
}

func renderRipgrepGroups(groups []ripgrepGroup) string {
	sections := make([]string, 0, len(groups))
	for _, group := range groups {
		lines := make([]string, 0, len(group.entries)+1)
		lines = append(lines, group.path)
		for _, entry := range group.entries {
			separator := ":"
			if entry.kind == ripgrepContext {
				separator = "-"
			}
			lines = append(lines, strconv.FormatInt(entry.line, 10)+separator+entry.text)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func grepSummary(result ripgrepResult, pattern string, files, matches int) string {
	if matches == 0 {
		if result.truncated {
			return "No matches for " + pattern + " before the search stopped"
		}
		return "No matches for " + pattern
	}
	if result.truncated {
		return fmt.Sprintf("Showing the first %d matches in %d files for %s", matches, files, pattern)
	}
	return fmt.Sprintf("%d matches in %d files for %s", matches, files, pattern)
}

func grepSearchHints(result ripgrepResult, matches int) []string {
	hints := make([]string, 0, 2)
	if result.truncated {
		if result.truncationReason == "timeout" {
			hints = append(hints, "the search timed out; narrow the path, include, or pattern")
		} else {
			hints = append(hints, "narrow the path, include, or pattern")
		}
	} else if matches == 0 {
		hints = append(hints, "hidden files and files ignored by git are not searched; pass an explicit path to search them")
	}
	if result.perFileLimitReached {
		hints = append(hints, "a file reached max_matches_per_file; search that path directly for the rest")
	}
	return hints
}

func grepFailureResult(callID, errorPath, pattern string, failure *ripgrepFailure) string {
	if failure.kind == "invalid_pattern" {
		return tools.ExecutionError(
			callID,
			tools.StatusInvalidInput,
			"invalid_pattern",
			"The search pattern is not a valid ripgrep regular expression",
			tools.Information{Key: "pattern", Value: pattern},
			tools.Information{Key: "system_error", Value: failure.message},
			tools.Information{Key: "suggestion", Value: "use literal: true to search for the exact text"},
		)
	}
	if failure.kind == "unavailable" {
		return tools.ExecutionError(
			callID,
			tools.StatusError,
			"ripgrep_unavailable",
			"The ripgrep executable could not be started",
			tools.Information{Key: "path", Value: errorPath},
			tools.Information{Key: "system_error", Value: failure.message},
			tools.Information{Key: "suggestion", Value: "install ripgrep or point tools.native.grep.binary at an executable"},
		)
	}
	return tools.ExecutionError(
		callID,
		tools.StatusError,
		"grep_failed",
		"The search could not be completed",
		tools.Information{Key: "path", Value: errorPath},
		tools.Information{Key: "system_error", Value: failure.message},
	)
}
