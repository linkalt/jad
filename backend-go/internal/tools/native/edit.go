package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const editToolID = "native.edit"

var editInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"Path relative to cwd or an absolute path."},
    "oldText":{"type":"string","minLength":1,"description":"Exact text to replace, copied from a read."},
    "newText":{"type":"string","description":"Replacement text. Empty deletes oldText."},
    "replaceAll":{"type":"boolean","description":"Replace every occurrence instead of requiring exactly one. Defaults to false."}
  },
  "required":["path","oldText","newText"],
  "additionalProperties":false
}`)

// NewEditTool constructs native.edit with normalized session limits and the
// same mutation coordinator used by native.write.
func NewEditTool(settings sessionfs.NativeEditSettings, locks *PathLocks) (tools.Tool, error) {
	validator, err := tools.CompileSchema(editInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native edit schema: %w", err)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          editToolID,
			Source:      "native",
			Name:        "edit",
			Description: "Replace exact text in an existing file. Use replaceAll only for every occurrence.",
			InputSchema: append(json.RawMessage(nil), editInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"path":"example.txt","oldText":"before","newText":"after"}`),
		LeafBudget:       true,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeEdit(arguments, settings, locks, execution)
		},
	}, nil
}

type editArgs struct {
	Path       string
	OldText    string
	NewText    string
	ReplaceAll bool
}

func parseEditArgs(arguments map[string]any) (editArgs, error) {
	path, ok := arguments["path"].(string)
	if !ok {
		return editArgs{}, errors.New("path must be a string")
	}
	oldText, ok := arguments["oldText"].(string)
	if !ok {
		return editArgs{}, errors.New("oldText must be a string")
	}
	newText, ok := arguments["newText"].(string)
	if !ok {
		return editArgs{}, errors.New("newText must be a string")
	}
	result := editArgs{Path: path, OldText: oldText, NewText: newText}
	if replaceAll, exists := arguments["replaceAll"]; exists {
		value, ok := replaceAll.(bool)
		if !ok {
			return editArgs{}, errors.New("replaceAll must be a boolean")
		}
		result.ReplaceAll = value
	}
	return result, nil
}

func executeEdit(arguments map[string]any, settings sessionfs.NativeEditSettings, locks *PathLocks, execution tools.ExecutionContext) (string, error) {
	args, err := parseEditArgs(arguments)
	if err != nil {
		return "", err
	}
	settings.Policy = scopedPolicy(settings.Policy, execution)
	path, pathFailure := resolveMutatePath(args.Path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathFailure != "" {
		return pathFailure, nil
	}
	release := locks.Lock(path.candidate)
	defer release()
	target, failure := inspectMutateTarget(path, settings.Policy, execution.CallID)
	if failure != "" {
		return failure, nil
	}
	if !target.existed {
		return editNotFound(execution.CallID, target), nil
	}
	if target.size > settings.MaxBytes {
		return tools.ExecutionError(
			execution.CallID,
			tools.StatusBlocked,
			"size_limit",
			"The file exceeds the configured byte limit",
			tools.Information{Key: "path", Value: target.errorPath},
			tools.Information{Key: "bytes", Value: target.size},
			tools.Information{Key: "max_bytes", Value: settings.MaxBytes},
		), nil
	}
	return applyEdit(target, args, settings, execution)
}

func applyEdit(target mutateTarget, args editArgs, settings sessionfs.NativeEditSettings, execution tools.ExecutionContext) (string, error) {
	if target.identity == nil {
		return editFailure(execution.CallID, target.errorPath, "changed_during_edit", "The file changed while it was being edited"), nil
	}
	data, after, err := readVerifiedRegularFile(target.realPath, target.identity, settings.MaxBytes)
	if err != nil || after == nil || !after.Mode().IsRegular() || !os.SameFile(target.identity, after) || int64(len(data)) != after.Size() {
		return editFailure(execution.CallID, target.errorPath, "changed_during_edit", "The file changed while it was being edited"), nil
	}
	text, valid := decodeEditUTF8(data)
	if !valid || strings.IndexByte(text, 0) >= 0 {
		return tools.ExecutionError(
			execution.CallID,
			tools.StatusUnsupportedEncoding,
			"binary_file",
			"The file is not UTF-8 text",
			tools.Information{Key: "path", Value: target.errorPath},
			tools.Information{Key: "suggestion", Value: "use write with contentEncoding \"base64\""},
		), nil
	}

	lineEnding, hasLineEnding := detectEditLineEnding(text)
	oldText := withEditLineEnding(args.OldText, lineEnding, hasLineEnding)
	newText := withEditLineEnding(args.NewText, lineEnding, hasLineEnding)
	occurrences := countEditOccurrences(text, oldText)
	if occurrences == 0 {
		return editNoMatch(execution.CallID, target, text, oldText, lineEnding, hasLineEnding), nil
	}
	if occurrences > 1 && !args.ReplaceAll {
		information := []tools.Information{
			{Key: "path", Value: target.errorPath},
			{Key: "occurrences", Value: occurrences},
		}
		for _, line := range editOccurrenceLineNumbers(text, oldText, 5) {
			information = append(information, tools.Information{Key: "line", Value: line})
		}
		information = append(information, tools.Information{Key: "suggestion", Value: "add surrounding context to oldText or set replaceAll: true"})
		return tools.ExecutionError(execution.CallID, tools.StatusError, "ambiguous_match", fmt.Sprintf("oldText occurs %d times in %s", occurrences, target.displayPath), information...), nil
	}

	replacements := occurrences
	if !args.ReplaceAll {
		replacements = 1
	}
	delta := int64(len(newText)) - int64(len(oldText))
	estimatedBytes := int64(len(data))
	if delta > 0 && int64(replacements) > (math.MaxInt64-estimatedBytes)/delta {
		estimatedBytes = math.MaxInt64
	} else {
		estimatedBytes += int64(replacements) * delta
	}
	if estimatedBytes > settings.MaxBytes {
		return tools.ExecutionError(
			execution.CallID,
			tools.StatusBlocked,
			"size_limit",
			"The edit exceeds the configured byte limit",
			tools.Information{Key: "path", Value: target.errorPath},
			tools.Information{Key: "bytes", Value: estimatedBytes},
			tools.Information{Key: "max_bytes", Value: settings.MaxBytes},
		), nil
	}

	replacedText, replacementCount, firstIndex, hasFirst := replaceEditOccurrences(text, oldText, newText, replacements)
	nextData := []byte(replacedText)
	information := func(action string) []tools.Information {
		items := []tools.Information{
			{Key: "path", Value: target.displayPath},
			{Key: "action", Value: action},
			{Key: "replacements", Value: replacementCount},
			{Key: "bytes", Value: len(nextData)},
		}
		if hasLineEnding {
			items = append(items, tools.Information{Key: "line_ending", Value: string(lineEnding)})
		}
		if action == "edited" && replacementCount == 1 && hasFirst {
			startLine := lineNumberAt(replacedText, firstIndex)
			items = append(items,
				tools.Information{Key: "start_line", Value: startLine},
				tools.Information{Key: "end_line", Value: startLine + insertedEditLineSpan(newText)},
			)
		}
		return items
	}
	if bytesEqual(nextData, data) {
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          execution.CallID,
			Status:      tools.StatusOK,
			Summary:     "Unchanged " + target.displayPath,
			Information: information("unchanged"),
		})
	}

	commit := commitFile(target, settings.NativeMutationSettings, nextData, true)
	switch commit.error {
	case "backup_failed":
		return editFailure(execution.CallID, target.errorPath, "backup_failed", "Could not create a backup before editing"), nil
	case "target_changed":
		return editFailure(execution.CallID, target.errorPath, "changed_during_edit", "The file changed while it was being edited"), nil
	case "write_failed":
		extra := []tools.Information{}
		if commit.systemError != "" {
			extra = append(extra, tools.Information{Key: "system_error", Value: commit.systemError})
		}
		if commit.backupPath != "" {
			extra = append(extra, tools.Information{Key: "backup_path", Value: displayPath(target.workspaceRoot, commit.backupPath)})
		}
		return editFailure(execution.CallID, target.errorPath, "write_failed", "Could not write the file", extra...), nil
	}

	reportFileChange(execution, target.displayPath, "edited")
	resultInformation := information("edited")
	if commit.backupPath != "" {
		resultInformation = append(resultInformation, tools.Information{Key: "backup_path", Value: displayPath(target.workspaceRoot, commit.backupPath)})
	}
	summary := "Edited " + target.displayPath
	if replacementCount > 1 {
		summary = fmt.Sprintf("Edited %s (%d replacements)", target.displayPath, replacementCount)
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          execution.CallID,
		Status:      tools.StatusOK,
		Summary:     summary,
		Information: resultInformation,
	})
}

func editNotFound(callID string, target mutateTarget) string {
	return tools.ExecutionError(
		callID,
		tools.StatusNotFound,
		"not_found",
		"Could not find "+target.displayPath,
		tools.Information{Key: "path", Value: target.errorPath},
		tools.Information{Key: "suggestion", Value: "use write to create the file"},
	)
}

func editNoMatch(callID string, target mutateTarget, text, oldText string, lineEnding editLineEnding, hasLineEnding bool) string {
	information := []tools.Information{{Key: "path", Value: target.errorPath}}
	if hasLineEnding {
		information = append(information, tools.Information{Key: "line_ending", Value: string(lineEnding)})
	}
	diagnostic := diagnoseEditMatch(text, oldText)
	if diagnostic != nil {
		information = append(information,
			tools.Information{Key: "nearest_line", Value: diagnostic.nearestLine},
			tools.Information{Key: "matched_lines", Value: diagnostic.matchedLines},
			tools.Information{Key: "expected", Value: diagnostic.expected},
			tools.Information{Key: "actual", Value: diagnostic.actual},
		)
		if diagnostic.whitespaceOnly {
			information = append(information, tools.Information{Key: "difference", Value: "whitespace"})
		}
	}
	suggestion := "re-read the file and use its exact text"
	if diagnostic != nil {
		suggestion = "re-read the file around nearest_line and use its exact text"
	}
	information = append(information, tools.Information{Key: "suggestion", Value: suggestion})
	return tools.ExecutionError(callID, tools.StatusNotFound, "no_match", "No exact match in "+target.displayPath, information...)
}

func editFailure(callID, errorPath, code, summary string, extra ...tools.Information) string {
	information := []tools.Information{{Key: "path", Value: errorPath}}
	information = append(information, extra...)
	return tools.ExecutionError(callID, tools.StatusError, code, summary, information...)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
