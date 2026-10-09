package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const writeToolID = "native.write"

var writeInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1,"description":"Path relative to cwd or an absolute path."},
    "content":{"type":"string"},
    "contentEncoding":{"type":"string","enum":["utf8","base64"],"description":"Encoding of content. Defaults to utf8."}
  },
  "required":["path","content"],
  "additionalProperties":false
}`)

// NewWriteTool constructs native.write with normalized session limits and
// shared mutation coordination.
func NewWriteTool(settings sessionfs.NativeWriteSettings, locks *PathLocks) (tools.Tool, error) {
	validator, err := tools.CompileSchema(writeInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native write schema: %w", err)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          writeToolID,
			Source:      "native",
			Name:        "write",
			Description: "Write file content, creating parent directories as needed. Identical content is left unchanged.",
			InputSchema: append(json.RawMessage(nil), writeInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"path":"example.txt","content":"..."}`),
		NormalizeArguments: normalizeWriteArguments,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeWrite(arguments, settings, locks, execution)
		},
	}, nil
}

type writeArgs struct {
	Path            string
	Content         string
	ContentEncoding string
}

func normalizeWriteArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	if encoding, ok := normalizeEncoding(arguments["contentEncoding"]); ok {
		normalized["contentEncoding"] = encoding
	}
	return normalized
}

func parseWriteArgs(arguments map[string]any) (writeArgs, error) {
	path, ok := arguments["path"].(string)
	if !ok {
		return writeArgs{}, errors.New("path must be a string")
	}
	content, ok := arguments["content"].(string)
	if !ok {
		return writeArgs{}, errors.New("content must be a string")
	}
	result := writeArgs{Path: path, Content: content, ContentEncoding: "utf8"}
	if encoding, ok := arguments["contentEncoding"].(string); ok {
		result.ContentEncoding = encoding
	}
	return result, nil
}

func executeWrite(arguments map[string]any, settings sessionfs.NativeWriteSettings, locks *PathLocks, execution tools.ExecutionContext) (string, error) {
	args, err := parseWriteArgs(arguments)
	if err != nil {
		return "", err
	}
	settings.Policy = scopedPolicy(settings.Policy, execution)
	path, pathFailure := resolveMutatePath(args.Path, execution.WorkspaceRoot, settings.Policy, execution.CallID)
	if pathFailure != "" {
		return pathFailure, nil
	}
	data, err := writeContentBytes(args.Content, args.ContentEncoding)
	if err != nil {
		return writeFailure(execution.CallID, path.errorPath, "invalid_base64", "The content is not valid base64", tools.Information{Key: "suggestion", Value: "base64 content must use the standard alphabet and padding; use contentEncoding \"utf8\" for text"}), nil
	}
	if int64(len(data)) > settings.MaxBytes {
		return tools.ExecutionError(
			execution.CallID,
			tools.StatusBlocked,
			"size_limit",
			"The write exceeds the configured byte limit",
			tools.Information{Key: "path", Value: path.errorPath},
			tools.Information{Key: "bytes", Value: len(data)},
			tools.Information{Key: "max_bytes", Value: settings.MaxBytes},
		), nil
	}

	release := locks.Lock(path.candidate)
	defer release()
	return applyWrite(path, data, settings, execution)
}

func writeContentBytes(content, encoding string) ([]byte, error) {
	if encoding == "utf8" || encoding == "" {
		return []byte(content), nil
	}
	if encoding != "base64" {
		return nil, errors.New("unsupported content encoding")
	}
	var compact strings.Builder
	for _, character := range content {
		if !unicode.IsSpace(character) {
			compact.WriteRune(character)
		}
	}
	normalized := compact.String()
	if normalized == "" {
		return []byte{}, nil
	}
	padding := strings.IndexByte(normalized, '=')
	unpadded := normalized
	if padding >= 0 {
		if len(normalized)-padding > 2 || strings.Trim(normalized[padding:], "=") != "" {
			return nil, errors.New("invalid base64 padding")
		}
		unpadded = normalized[:padding]
	}
	if len(normalized)%4 == 1 {
		return nil, errors.New("invalid base64 length")
	}
	for _, character := range unpadded {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '+' && character != '/' {
			return nil, errors.New("invalid base64 character")
		}
	}
	data, err := base64.RawStdEncoding.DecodeString(unpadded)
	if err != nil {
		return nil, err
	}
	canonical := strings.TrimRight(base64.StdEncoding.EncodeToString(data), "=")
	if canonical != unpadded {
		return nil, errors.New("non-canonical base64 content")
	}
	return data, nil
}

func applyWrite(path resolvedMutatePath, data []byte, settings sessionfs.NativeWriteSettings, execution tools.ExecutionContext) (string, error) {
	if failure := prepareMutateParent(path, settings.Policy, execution.CallID); failure != "" {
		return failure, nil
	}
	target, failure := inspectMutateTarget(path, settings.Policy, execution.CallID)
	if failure != "" {
		return failure, nil
	}
	information := func(action string, backupPath string) []tools.Information {
		items := []tools.Information{
			{Key: "path", Value: target.displayPath},
			{Key: "action", Value: action},
			{Key: "bytes", Value: len(data)},
		}
		if backupPath != "" {
			items = append(items, tools.Information{Key: "backup_path", Value: displayPath(target.workspaceRoot, backupPath)})
		}
		return items
	}
	if target.existed && hasSameContent(target, data) {
		return tools.RenderToolResult(tools.ResultOptions{
			ID:          execution.CallID,
			Status:      tools.StatusOK,
			Summary:     "Unchanged " + target.displayPath,
			Information: information("unchanged", ""),
		})
	}

	action := "created"
	if target.existed {
		action = "updated"
	}
	commit := commitFile(target, settings.NativeMutationSettings, data, true)
	switch commit.error {
	case "backup_failed":
		return writeFailure(execution.CallID, target.errorPath, "backup_failed", "Could not create a backup before writing"), nil
	case "target_changed":
		return writeFailure(execution.CallID, target.errorPath, "changed_during_write", "The target changed while it was being written"), nil
	case "write_failed":
		information := []tools.Information{}
		if commit.systemError != "" {
			information = append(information, tools.Information{Key: "system_error", Value: commit.systemError})
		}
		if commit.backupPath != "" {
			information = append(information, tools.Information{Key: "backup_path", Value: displayPath(target.workspaceRoot, commit.backupPath)})
		}
		return writeFailure(execution.CallID, target.errorPath, "write_failed", "Could not write the file", information...), nil
	}
	reportFileChange(execution, target.displayPath, action)
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          execution.CallID,
		Status:      tools.StatusOK,
		Summary:     strings.ToUpper(action[:1]) + action[1:] + " " + target.displayPath,
		Information: information(action, commit.backupPath),
	})
}

func writeFailure(callID, errorPath, code, summary string, extra ...tools.Information) string {
	information := []tools.Information{{Key: "path", Value: errorPath}}
	information = append(information, extra...)
	return tools.ExecutionError(callID, tools.StatusError, code, summary, information...)
}
