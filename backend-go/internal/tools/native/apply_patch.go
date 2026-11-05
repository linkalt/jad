package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const applyPatchToolID = "native.apply_patch"

var applyPatchInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "patch":{"type":"string","minLength":1,"description":"The whole patch, from *** Begin Patch to *** End Patch."}
  },
  "required":["patch"],
  "additionalProperties":false
}`)

const applyPatchDescription = "Add, change, move and delete files with one patch. Format:\n" +
	"*** Begin Patch\n" +
	"*** Add File: path\n+each line of the new file\n" +
	"*** Update File: path\n*** Move to: new/path (optional)\n@@ optional line to find first\n context line\n-removed line\n+added line\n" +
	"*** Delete File: path\n" +
	"*** End Patch\n" +
	"Give about three lines of context around each change; hunks apply in file order. Every file is checked before any is written, so a patch that does not apply changes nothing."

// NewApplyPatchTool constructs native.apply_patch. It shares the mutation locks, path
// policy, backups and file-change reports of native.write and native.edit.
func NewApplyPatchTool(settings sessionfs.NativeApplyPatchSettings, locks *PathLocks) (tools.Tool, error) {
	validator, err := tools.CompileSchema(applyPatchInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native apply_patch schema: %w", err)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          applyPatchToolID,
			Source:      "native",
			Name:        "apply_patch",
			Description: applyPatchDescription,
			InputSchema: append(json.RawMessage(nil), applyPatchInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"patch":"*** Begin Patch\n*** Update File: example.txt\n@@\n-before\n+after\n*** End Patch"}`),
		LeafBudget:       true,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeApplyPatch(arguments, settings, locks, execution)
		},
	}, nil
}

// plannedPatchFile is one checked operation, ready to write.
type plannedPatchFile struct {
	operation patchOperation
	path      resolvedMutatePath
	target    mutateTarget
	moveTo    resolvedMutatePath
	data      []byte
}

func executeApplyPatch(arguments map[string]any, settings sessionfs.NativeApplyPatchSettings, locks *PathLocks, execution tools.ExecutionContext) (string, error) {
	patch, ok := arguments["patch"].(string)
	if !ok {
		return "", errors.New("patch must be a string")
	}
	callID := execution.CallID
	if int64(len(patch)) > settings.MaxPatchBytes {
		return tools.ExecutionError(callID, tools.StatusBlocked, "size_limit", "The patch exceeds the configured byte limit",
			tools.Information{Key: "bytes", Value: len(patch)}, tools.Information{Key: "max_patch_bytes", Value: settings.MaxPatchBytes}), nil
	}
	operations, err := parsePatch(patch)
	if err != nil {
		information := []tools.Information{}
		var syntax *patchSyntaxError
		if errors.As(err, &syntax) {
			information = append(information, tools.Information{Key: "patch_line", Value: syntax.line})
		}
		information = append(information, tools.Information{Key: "suggestion", Value: "fix the patch format; no file was changed"})
		return tools.ExecutionError(callID, tools.StatusError, "invalid_patch", "The patch could not be read: "+err.Error(), information...), nil
	}
	if int64(len(operations)) > settings.MaxFiles {
		return tools.ExecutionError(callID, tools.StatusBlocked, "too_many_files", "The patch changes more files than the configured limit",
			tools.Information{Key: "files", Value: len(operations)}, tools.Information{Key: "max_files", Value: settings.MaxFiles},
			tools.Information{Key: "suggestion", Value: "split the patch; no file was changed"}), nil
	}
	settings.Policy = scopedPolicy(settings.Policy, execution)

	planned := make([]plannedPatchFile, len(operations))
	seen := map[string]int{}
	var lockKeys []string
	claim := func(path resolvedMutatePath, index int) string {
		if earlier, exists := seen[path.candidate]; exists {
			return patchPlanFailure(callID, tools.StatusError, "duplicate_path", fmt.Sprintf("Operations %d and %d both change %s", earlier+1, index+1, path.displayPath), operations[index], path.errorPath)
		}
		seen[path.candidate] = index
		lockKeys = append(lockKeys, path.candidate)
		return ""
	}
	for index, operation := range operations {
		path, failure := resolveMutatePath(operation.path, execution.WorkspaceRoot, settings.Policy, callID)
		if failure != "" {
			return withPatchOperation(failure, operation), nil
		}
		if failure := claim(path, index); failure != "" {
			return failure, nil
		}
		planned[index] = plannedPatchFile{operation: operation, path: path}
		if operation.moveTo != "" {
			moveTo, failure := resolveMutatePath(operation.moveTo, execution.WorkspaceRoot, settings.Policy, callID)
			if failure != "" {
				return withPatchOperation(failure, operation), nil
			}
			if failure := claim(moveTo, index); failure != "" {
				return failure, nil
			}
			planned[index].moveTo = moveTo
		}
	}
	// Locks are taken in one order, so two patches over the same files cannot deadlock.
	sort.Strings(lockKeys)
	for _, key := range lockKeys {
		release := locks.Lock(key)
		defer release()
	}
	for index := range planned {
		if failure := planPatchFile(&planned[index], settings, callID); failure != "" {
			return failure, nil
		}
	}
	return commitPatch(planned, settings, execution)
}

// planPatchFile checks one operation against the file as it is now and computes the
// bytes to write, without changing anything.
func planPatchFile(file *plannedPatchFile, settings sessionfs.NativeApplyPatchSettings, callID string) string {
	operation := file.operation
	target, failure := inspectMutateTarget(file.path, settings.Policy, callID)
	if failure != "" {
		return withPatchOperation(failure, operation)
	}
	file.target = target
	switch operation.kind {
	case patchOperationAdd:
		if target.existed {
			return patchPlanFailure(callID, tools.StatusError, "already_exists", target.displayPath+" already exists", operation, target.errorPath,
				tools.Information{Key: "suggestion", Value: "use *** Update File to change it, or *** Delete File first; no file was changed"})
		}
		file.data = []byte(joinPatchLines(operation.addLines))
	case patchOperationDelete:
		if !target.existed {
			return patchPlanFailure(callID, tools.StatusNotFound, "not_found", "Could not find "+target.displayPath, operation, target.errorPath)
		}
		return ""
	case patchOperationUpdate:
		if !target.existed {
			return patchPlanFailure(callID, tools.StatusNotFound, "not_found", "Could not find "+target.displayPath, operation, target.errorPath,
				tools.Information{Key: "suggestion", Value: "use *** Add File to create it; no file was changed"})
		}
		if target.size > settings.MaxBytes {
			return patchPlanFailure(callID, tools.StatusBlocked, "size_limit", "The file exceeds the configured byte limit", operation, target.errorPath,
				tools.Information{Key: "bytes", Value: target.size}, tools.Information{Key: "max_bytes", Value: settings.MaxBytes})
		}
		data, after, err := readVerifiedRegularFile(target.realPath, target.identity, settings.MaxBytes)
		if err != nil || after == nil || !os.SameFile(target.identity, after) || int64(len(data)) != after.Size() {
			return patchPlanFailure(callID, tools.StatusError, "changed_during_patch", "The file changed while the patch was checked", operation, target.errorPath)
		}
		text, valid := decodeEditUTF8(data)
		if !valid || strings.IndexByte(text, 0) >= 0 {
			return patchPlanFailure(callID, tools.StatusUnsupportedEncoding, "binary_file", "The file is not UTF-8 text", operation, target.errorPath)
		}
		updated, err := applyPatchHunks(text, operation.hunks)
		if err != nil {
			information := []tools.Information{}
			var mismatch *patchApplyError
			if errors.As(err, &mismatch) {
				information = append(information, tools.Information{Key: "hunk", Value: mismatch.hunk}, tools.Information{Key: "patch_line", Value: mismatch.line})
			}
			information = append(information, tools.Information{Key: "suggestion", Value: "re-read the file and copy its exact lines into the hunk; no file was changed"})
			return patchPlanFailure(callID, tools.StatusNotFound, "no_match", "The patch does not apply to "+target.displayPath+": "+err.Error(), operation, target.errorPath, information...)
		}
		file.data = []byte(updated)
		if file.moveTo.candidate != "" {
			destination, failure := inspectMutateTarget(file.moveTo, settings.Policy, callID)
			if failure != "" {
				return withPatchOperation(failure, operation)
			}
			if destination.existed {
				return patchPlanFailure(callID, tools.StatusError, "already_exists", "The Move to target "+destination.displayPath+" already exists", operation, destination.errorPath)
			}
		}
	}
	if int64(len(file.data)) > settings.MaxBytes {
		return patchPlanFailure(callID, tools.StatusBlocked, "size_limit", "The patched file exceeds the configured byte limit", operation, file.target.errorPath,
			tools.Information{Key: "bytes", Value: len(file.data)}, tools.Information{Key: "max_bytes", Value: settings.MaxBytes})
	}
	return ""
}

// commitPatch writes the planned files in patch order. A failure stops at that file and
// reports which files were already changed.
func commitPatch(planned []plannedPatchFile, settings sessionfs.NativeApplyPatchSettings, execution tools.ExecutionContext) (string, error) {
	callID := execution.CallID
	var applied []tools.Information
	counts := map[string]int{}
	fail := func(file plannedPatchFile, code, summary string, extra ...tools.Information) (string, error) {
		information := []tools.Information{{Key: "operation", Value: string(file.operation.kind)}, {Key: "path", Value: file.target.errorPath}}
		information = append(information, extra...)
		if len(applied) == 0 {
			information = append(information, tools.Information{Key: "applied", Value: "none: no file was changed"})
		} else {
			information = append(information, applied...)
			information = append(information, tools.Information{Key: "suggestion", Value: "the files listed as applied were changed; re-read them before patching again"})
		}
		return tools.ExecutionError(callID, tools.StatusError, code, summary, information...), nil
	}
	commitFailure := func(file plannedPatchFile, commit commitResult) (string, error) {
		extra := []tools.Information{}
		if commit.systemError != "" {
			extra = append(extra, tools.Information{Key: "system_error", Value: commit.systemError})
		}
		switch commit.error {
		case "backup_failed":
			return fail(file, "backup_failed", "Could not create a backup of "+file.target.displayPath, extra...)
		case "target_changed":
			return fail(file, "changed_during_patch", file.target.displayPath+" changed while the patch was written", extra...)
		default:
			return fail(file, "write_failed", "Could not write "+file.target.displayPath, extra...)
		}
	}
	record := func(action, path, backupPath string, target mutateTarget) {
		counts[action]++
		value := path
		if backupPath != "" {
			value += " (backup " + displayPath(target.workspaceRoot, backupPath) + ")"
		}
		applied = append(applied, tools.Information{Key: action, Value: value})
	}
	for _, file := range planned {
		switch file.operation.kind {
		case patchOperationAdd:
			target, failure := createPatchTarget(file.path, settings.Policy, callID)
			if failure != "" {
				return fail(file, "write_failed", "Could not create "+file.path.displayPath)
			}
			if commit := commitFile(target, settings.NativeMutationSettings, file.data, false); commit.error != "" {
				return commitFailure(file, commit)
			}
			reportFileChange(execution, target.displayPath, "created")
			record("added", target.displayPath, "", target)
		case patchOperationDelete:
			commit := removeFile(file.target, settings.NativeMutationSettings)
			if commit.error != "" {
				return commitFailure(file, commit)
			}
			reportFileChange(execution, file.target.displayPath, "deleted")
			record("deleted", file.target.displayPath, commit.backupPath, file.target)
		case patchOperationUpdate:
			if file.moveTo.candidate == "" {
				commit := commitFile(file.target, settings.NativeMutationSettings, file.data, true)
				if commit.error != "" {
					return commitFailure(file, commit)
				}
				reportFileChange(execution, file.target.displayPath, "edited")
				record("updated", file.target.displayPath, commit.backupPath, file.target)
				continue
			}
			destination, failure := createPatchTarget(file.moveTo, settings.Policy, callID)
			if failure != "" {
				return fail(file, "write_failed", "Could not create "+file.moveTo.displayPath)
			}
			if commit := commitFile(destination, settings.NativeMutationSettings, file.data, false); commit.error != "" {
				return commitFailure(file, commit)
			}
			commit := removeFile(file.target, settings.NativeMutationSettings)
			if commit.error != "" {
				record("added", destination.displayPath, "", destination)
				return commitFailure(file, commit)
			}
			reportFileChange(execution, destination.displayPath, "created")
			reportFileChange(execution, file.target.displayPath, "deleted")
			record("moved", file.target.displayPath+" -> "+destination.displayPath, commit.backupPath, file.target)
		}
	}
	var parts []string
	for _, action := range []string{"added", "updated", "moved", "deleted"} {
		if counts[action] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[action], action))
		}
	}
	noun := "files"
	if len(planned) == 1 {
		noun = "file"
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("Applied the patch to %d %s: %s", len(planned), noun, strings.Join(parts, ", ")),
		Information: applied,
	})
}

// createPatchTarget makes the parent directories of a new file and checks that the file
// still does not exist.
func createPatchTarget(path resolvedMutatePath, policy sessionfs.NativePathPolicy, callID string) (mutateTarget, string) {
	if failure := prepareMutateParent(path, policy, callID); failure != "" {
		return mutateTarget{}, failure
	}
	target, failure := inspectMutateTarget(path, policy, callID)
	if failure != "" {
		return mutateTarget{}, failure
	}
	if target.existed {
		return mutateTarget{}, "exists"
	}
	return target, ""
}

// removeFile backs up target and removes it, failing when it changed since it was
// inspected.
func removeFile(target mutateTarget, settings sessionfs.NativeMutationSettings) commitResult {
	parent, ok := pinTargetParent(target.realPath)
	if !ok {
		return commitResult{error: "target_changed"}
	}
	defer parent.directory.handle.Close()
	if !targetStillMatches(target, parent.targetPath) {
		return commitResult{error: "target_changed"}
	}
	backupDirectory, ownsBackupDirectory, ok := pinBackupDirectory(target, settings, parent)
	if !ok {
		return commitResult{error: "backup_failed"}
	}
	if ownsBackupDirectory {
		defer backupDirectory.handle.Close()
	}
	source, ok := pinBackupSource(target, parent.targetPath)
	if !ok {
		return commitResult{error: "target_changed"}
	}
	defer source.Close()
	backupPath, _ := createBackup(target, source, backupDirectory)
	if backupPath == "" {
		return commitResult{error: "backup_failed"}
	}
	if !targetStillMatches(target, parent.targetPath) {
		_ = os.Remove(filepath.Join(backupDirectory.operationPath, filepath.Base(backupPath)))
		return commitResult{error: "target_changed"}
	}
	if err := os.Remove(parent.targetPath); err != nil {
		return commitResult{backupPath: backupPath, error: "write_failed", systemError: systemErrorCode(err)}
	}
	if settings.Fsync {
		// Best effort, as after a rename.
		_ = parent.directory.handle.Sync()
	}
	pruneBackups(target, settings, backupDirectory, backupPath)
	return commitResult{backupPath: backupPath}
}

func joinPatchLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func patchPlanFailure(callID string, status tools.Status, code, summary string, operation patchOperation, errorPath string, extra ...tools.Information) string {
	information := []tools.Information{
		{Key: "operation", Value: string(operation.kind)},
		{Key: "path", Value: errorPath},
		{Key: "patch_line", Value: operation.line},
	}
	hasSuggestion := false
	for _, item := range extra {
		hasSuggestion = hasSuggestion || item.Key == "suggestion"
	}
	information = append(information, extra...)
	if !hasSuggestion {
		information = append(information, tools.Information{Key: "suggestion", Value: "no file was changed"})
	}
	return tools.ExecutionError(callID, status, code, summary, information...)
}

// withPatchOperation adds the failing operation to a path error from the shared helpers.
func withPatchOperation(result string, operation patchOperation) string {
	updated, err := tools.AppendContent(result, tools.Content{Text: fmt.Sprintf("in the patch's %s operation for %s (patch line %d); no file was changed", operation.kind, operation.path, operation.line)})
	if err != nil {
		return result
	}
	return updated
}
