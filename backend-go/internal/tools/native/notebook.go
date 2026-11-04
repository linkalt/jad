package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const notebookToolID = "native.notebook"

const notebookDescription = "Read and change the cells of a Jupyter notebook (.ipynb, nbformat 4) instead of its raw JSON. action read shows the cells with their index, id, type, source and outputs (text cut to a limit; images and other binary outputs named only). edit replaces a cell's source (and can change its type), insert adds a cell before index cell (or at the end), delete removes one. Pick a cell by cell (0-based index from a read) or cell_id. Editing a code cell's source clears its outputs and execution count, since they no longer match. The file is rewritten in Jupyter's own layout, so only the changed cell differs in a diff. Nothing is executed."

var notebookInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["read","edit","insert","delete"],"description":"read shows cells; edit, insert and delete change the file."},
    "path":{"type":"string","minLength":1,"description":"Path of the .ipynb file, relative to cwd or absolute."},
    "cell":{"type":"integer","minimum":0,"description":"0-based cell index. read: first cell to show (default 0). insert: the new cell goes before this index (default: at the end). edit and delete: the cell, unless cell_id is given."},
    "count":{"type":"integer","minimum":1,"description":"read only: how many cells to show from cell (default: all)."},
    "cell_id":{"type":"string","minLength":1,"description":"edit and delete: the cell's id, as a read shows it; takes precedence over cell."},
    "source":{"type":"string","description":"edit and insert: the cell's whole new source."},
    "cell_type":{"type":"string","enum":["code","markdown","raw"],"description":"insert: the new cell's type (default code). edit: change the cell to this type."}
  },
  "required":["action","path"],
  "additionalProperties":false
}`)

// NewNotebookTool builds native.notebook. It writes with the edit tool's path rules, size
// limit and backups, through the same locks as write and edit.
func NewNotebookTool(settings sessionfs.NativeNotebookSettings, locks *PathLocks) (tools.Tool, error) {
	validator, err := tools.CompileSchema(notebookInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native notebook schema: %w", err)
	}
	if locks == nil {
		locks = NewPathLocks()
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID: notebookToolID, Source: "native", Name: "notebook", Description: notebookDescription,
			InputSchema: append(json.RawMessage(nil), notebookInputSchema...),
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"action":"read","path":"analysis.ipynb"}`),
		LeafBudget:       true,
		// Not ReadOnly: a read-only sub-agent gets every ReadOnly tool, and this one writes.
		// Its calls therefore run in order with the other changes, reads included.
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNotebook(arguments, settings, locks, execution)
		},
	}, nil
}

type notebookArgs struct {
	action, path, cellID, cellType string
	source                         *string
	cell, count                    *int64
}

func parseNotebookArgs(arguments map[string]any) (notebookArgs, error) {
	var args notebookArgs
	var ok bool
	if args.action, ok = arguments["action"].(string); !ok {
		return args, errors.New("action must be a string")
	}
	if args.path, ok = arguments["path"].(string); !ok {
		return args, errors.New("path must be a string")
	}
	args.cellID, _ = arguments["cell_id"].(string)
	args.cellType, _ = arguments["cell_type"].(string)
	if value, exists := arguments["source"]; exists {
		text, ok := value.(string)
		if !ok {
			return args, errors.New("source must be a string")
		}
		args.source = &text
	}
	for name, target := range map[string]**int64{"cell": &args.cell, "count": &args.count} {
		if value, exists := arguments[name]; exists {
			number, ok := safeInteger(value)
			if !ok {
				return args, fmt.Errorf("%s must be an integer", name)
			}
			*target = &number
		}
	}
	return args, nil
}

func executeNotebook(arguments map[string]any, settings sessionfs.NativeNotebookSettings, locks *PathLocks, execution tools.ExecutionContext) (string, error) {
	args, err := parseNotebookArgs(arguments)
	if err != nil {
		return "", err
	}
	if args.action != "read" && args.action != "delete" && args.source == nil {
		return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "source_required", args.action+" needs source"), nil
	}
	policy := scopedPolicy(settings.Edit.Policy, execution)
	path, failure := resolveMutatePath(args.path, execution.WorkspaceRoot, policy, execution.CallID)
	if failure != "" {
		return failure, nil
	}
	release := locks.Lock(path.candidate)
	defer release()
	target, failure := inspectMutateTarget(path, policy, execution.CallID)
	if failure != "" {
		return failure, nil
	}
	if !target.existed {
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "file_not_found", "The notebook does not exist; create it with write"), nil
	}
	if target.size > settings.Edit.MaxBytes {
		return tools.ExecutionError(execution.CallID, tools.StatusBlocked, "size_limit", "The file exceeds the configured byte limit",
			tools.Information{Key: "path", Value: target.errorPath}, tools.Information{Key: "bytes", Value: target.size}, tools.Information{Key: "max_bytes", Value: settings.Edit.MaxBytes}), nil
	}
	if target.identity == nil {
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "changed_during_edit", "The file changed while it was being read"), nil
	}
	data, after, err := readVerifiedRegularFile(target.realPath, target.identity, settings.Edit.MaxBytes)
	if err != nil || after == nil || !os.SameFile(target.identity, after) || int64(len(data)) != after.Size() {
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "changed_during_edit", "The file changed while it was being read"), nil
	}
	notebook, err := parseNotebook(data)
	if err != nil {
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusUnsupportedEncoding, "not_a_notebook", err.Error(),
			tools.Information{Key: "suggestion", Value: "use read and edit for a file that is not an nbformat 4 notebook"}), nil
	}
	if args.action == "read" {
		return renderNotebook(notebook, args, settings.MaxOutputChars, target.displayPath, execution.CallID)
	}

	index, information, failure := notebook.apply(args, execution.CallID, target.errorPath)
	if failure != "" {
		return failure, nil
	}
	next, err := notebook.encode()
	if err != nil {
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "write_failed", "Could not encode the notebook: "+err.Error()), nil
	}
	if int64(len(next)) > settings.Edit.MaxBytes {
		return tools.ExecutionError(execution.CallID, tools.StatusBlocked, "size_limit", "The change exceeds the configured byte limit",
			tools.Information{Key: "path", Value: target.errorPath}, tools.Information{Key: "bytes", Value: len(next)}, tools.Information{Key: "max_bytes", Value: settings.Edit.MaxBytes}), nil
	}
	commit := commitFile(target, settings.Edit.NativeMutationSettings, next, true)
	switch commit.error {
	case "backup_failed":
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "backup_failed", "Could not create a backup before editing"), nil
	case "target_changed":
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "changed_during_edit", "The file changed while it was being edited"), nil
	case "write_failed":
		extra := []tools.Information{}
		if commit.systemError != "" {
			extra = append(extra, tools.Information{Key: "system_error", Value: commit.systemError})
		}
		return notebookFailure(execution.CallID, target.errorPath, tools.StatusError, "write_failed", "Could not write the file", extra...), nil
	}
	reportFileChange(execution, target.displayPath, "edited")
	result := append([]tools.Information{{Key: "path", Value: target.displayPath}, {Key: "action", Value: args.action}, {Key: "cell", Value: index}}, information...)
	result = append(result, tools.Information{Key: "cells", Value: len(notebook.cells)})
	if commit.backupPath != "" {
		result = append(result, tools.Information{Key: "backup_path", Value: displayPath(target.workspaceRoot, commit.backupPath)})
	}
	verb := map[string]string{"edit": "Edited", "insert": "Inserted", "delete": "Deleted"}[args.action]
	return tools.RenderToolResult(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: fmt.Sprintf("%s cell %d of %s", verb, index, target.displayPath), Information: result})
}

func notebookFailure(callID, path string, status tools.Status, code, summary string, extra ...tools.Information) string {
	return tools.ExecutionError(callID, status, code, summary, append([]tools.Information{{Key: "path", Value: path}}, extra...)...)
}

// notebook is a decoded nbformat 4 file. Every field is kept as decoded (numbers as
// json.Number), so encoding writes back what it read except the cells changed.
type notebook struct {
	top   map[string]any
	cells []any
	minor int64
}

func parseNotebook(data []byte) (*notebook, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var top map[string]any
	if err := decoder.Decode(&top); err != nil || top == nil {
		return nil, errors.New("The file is not a JSON object")
	}
	major, _ := top["nbformat"].(json.Number)
	if major.String() != "4" {
		return nil, fmt.Errorf("The notebook format is %q; only nbformat 4 is supported", major.String())
	}
	cells, ok := top["cells"].([]any)
	if !ok {
		return nil, errors.New("The notebook has no cells list")
	}
	for index, cell := range cells {
		fields, ok := cell.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Cell %d is not an object", index)
		}
		if _, ok := fields["cell_type"].(string); !ok {
			return nil, fmt.Errorf("Cell %d has no cell_type", index)
		}
	}
	minor, _ := top["nbformat_minor"].(json.Number).Int64()
	return &notebook{top: top, cells: cells, minor: minor}, nil
}

// encode writes the notebook as Jupyter does: keys sorted, one space of indent, non-ASCII
// as is, and a final newline.
func (n *notebook) encode() ([]byte, error) {
	n.top["cells"] = n.cells
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(n.top); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (n *notebook) cell(index int) map[string]any { return n.cells[index].(map[string]any) }

// find resolves cell_id, else cell, to an index; -1 and a failure when neither names a cell.
func (n *notebook) find(args notebookArgs, callID, path string) (int, string) {
	if args.cellID != "" {
		for index := range n.cells {
			if id, _ := n.cell(index)["id"].(string); id == args.cellID {
				return index, ""
			}
		}
		return -1, notebookFailure(callID, path, tools.StatusError, "cell_not_found", "No cell has id "+args.cellID, tools.Information{Key: "cells", Value: len(n.cells)})
	}
	if args.cell == nil {
		return -1, notebookFailure(callID, path, tools.StatusInvalidInput, "cell_required", args.action+" needs cell or cell_id")
	}
	if *args.cell >= int64(len(n.cells)) {
		return -1, notebookFailure(callID, path, tools.StatusError, "cell_not_found", fmt.Sprintf("There is no cell %d", *args.cell), tools.Information{Key: "cells", Value: len(n.cells)})
	}
	return int(*args.cell), ""
}

// apply makes the change args ask for and returns the cell's index and what to report.
func (n *notebook) apply(args notebookArgs, callID, path string) (int, []tools.Information, string) {
	switch args.action {
	case "insert":
		index := len(n.cells)
		if args.cell != nil {
			if *args.cell > int64(len(n.cells)) {
				return 0, nil, notebookFailure(callID, path, tools.StatusError, "cell_not_found", fmt.Sprintf("Cannot insert before cell %d", *args.cell), tools.Information{Key: "cells", Value: len(n.cells)})
			}
			index = int(*args.cell)
		}
		cellType := args.cellType
		if cellType == "" {
			cellType = "code"
		}
		cell := map[string]any{"cell_type": cellType, "metadata": map[string]any{}, "source": notebookSourceLines(*args.source)}
		if cellType == "code" {
			cell["execution_count"], cell["outputs"] = nil, []any{}
		}
		information := []tools.Information{{Key: "cell_type", Value: cellType}}
		// Cell ids exist from nbformat 4.5 on.
		if n.minor >= 5 {
			id := newNotebookCellID()
			cell["id"] = id
			information = append(information, tools.Information{Key: "cell_id", Value: id})
		}
		n.cells = append(n.cells[:index], append([]any{cell}, n.cells[index:]...)...)
		return index, information, ""
	case "delete":
		index, failure := n.find(args, callID, path)
		if failure != "" {
			return 0, nil, failure
		}
		n.cells = append(n.cells[:index], n.cells[index+1:]...)
		return index, nil, ""
	default: // edit
		index, failure := n.find(args, callID, path)
		if failure != "" {
			return 0, nil, failure
		}
		cell := n.cell(index)
		cellType, _ := cell["cell_type"].(string)
		information := []tools.Information{}
		if args.cellType != "" && args.cellType != cellType {
			cellType = args.cellType
			cell["cell_type"] = cellType
			if cellType == "code" {
				cell["execution_count"], cell["outputs"] = nil, []any{}
			} else {
				delete(cell, "execution_count")
				delete(cell, "outputs")
			}
			information = append(information, tools.Information{Key: "cell_type", Value: cellType})
		}
		changed := notebookSource(cell["source"]) != *args.source
		cell["source"] = notebookSourceLines(*args.source)
		if cellType == "code" && changed {
			if outputs, _ := cell["outputs"].([]any); len(outputs) > 0 || cell["execution_count"] != nil {
				information = append(information, tools.Information{Key: "outputs_cleared", Value: true})
			}
			cell["execution_count"], cell["outputs"] = nil, []any{}
		}
		return index, information, ""
	}
}

// notebookSource joins a cell source, which nbformat allows as a string or a list of lines.
func notebookSource(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, line := range typed {
			text, _ := line.(string)
			builder.WriteString(text)
		}
		return builder.String()
	}
	return ""
}

// notebookSourceLines is the list-of-lines form Jupyter writes: every line keeps its newline.
func notebookSourceLines(source string) []any {
	lines := []any{}
	for source != "" {
		line, rest, found := strings.Cut(source, "\n")
		if found {
			line += "\n"
		}
		lines = append(lines, line)
		source = rest
	}
	return lines
}

func newNotebookCellID() string {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func renderNotebook(n *notebook, args notebookArgs, maxOutputChars int64, displayPath, callID string) (string, error) {
	start := int64(0)
	if args.cell != nil {
		start = *args.cell
	}
	end := int64(len(n.cells))
	if args.count != nil && start+*args.count < end {
		end = start + *args.count
	}
	if start > end {
		start = end
	}
	var builder strings.Builder
	for index := start; index < end; index++ {
		cell := n.cell(int(index))
		cellType, _ := cell["cell_type"].(string)
		fmt.Fprintf(&builder, "### cell %d [%s]", index, cellType)
		if id, ok := cell["id"].(string); ok {
			fmt.Fprintf(&builder, " id=%s", id)
		}
		if count, ok := cell["execution_count"].(json.Number); ok {
			fmt.Fprintf(&builder, " execution_count=%s", count)
		}
		builder.WriteString("\n")
		if source := notebookSource(cell["source"]); source != "" {
			builder.WriteString(source)
			if !strings.HasSuffix(source, "\n") {
				builder.WriteString("\n")
			}
		}
		outputs, _ := cell["outputs"].([]any)
		for number, output := range outputs {
			renderNotebookOutput(&builder, number+1, output, maxOutputChars)
		}
	}
	information := []tools.Information{{Key: "path", Value: displayPath}, {Key: "cells", Value: len(n.cells)}, {Key: "shown", Value: fmt.Sprintf("%d-%d", start, end-1)}}
	if start == end {
		information[2].Value = "none"
	}
	if language := notebookLanguage(n.top); language != "" {
		information = append(information, tools.Information{Key: "language", Value: language})
	}
	return tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: fmt.Sprintf("Read %d of %d cells of %s", end-start, len(n.cells), displayPath),
		Information: information, Content: &tools.Content{Text: builder.String()}})
}

func renderNotebookOutput(builder *strings.Builder, number int, value any, maxChars int64) {
	output, _ := value.(map[string]any)
	kind, _ := output["output_type"].(string)
	switch kind {
	case "stream":
		name, _ := output["name"].(string)
		fmt.Fprintf(builder, "--- output %d: stream %s ---\n", number, name)
		writeNotebookLine(builder, cutNotebookText(notebookSource(output["text"]), maxChars))
	case "error":
		name, _ := output["ename"].(string)
		message, _ := output["evalue"].(string)
		fmt.Fprintf(builder, "--- output %d: error %s: %s ---\n", number, name, message)
		traceback, _ := output["traceback"].([]any)
		lines := make([]string, 0, len(traceback))
		for _, line := range traceback {
			text, _ := line.(string)
			lines = append(lines, sanitizeBashOutput(text, true))
		}
		writeNotebookLine(builder, cutNotebookText(strings.Join(lines, "\n"), maxChars))
	default: // execute_result, display_data
		data, _ := output["data"].(map[string]any)
		fmt.Fprintf(builder, "--- output %d: %s ---\n", number, kind)
		if text, ok := data["text/plain"]; ok {
			writeNotebookLine(builder, cutNotebookText(notebookSource(text), maxChars))
		}
		for _, mime := range notebookMimeTypes(data) {
			if mime == "text/plain" {
				continue
			}
			fmt.Fprintf(builder, "[%s, %d characters, not shown]\n", mime, len(notebookSource(data[mime])))
		}
	}
}

// writeNotebookLine writes text and ends it with a newline, so the next header starts a line.
func writeNotebookLine(builder *strings.Builder, text string) {
	builder.WriteString(text)
	if text != "" && !strings.HasSuffix(text, "\n") {
		builder.WriteString("\n")
	}
}

// cutNotebookText keeps the first maxChars characters of an output.
func cutNotebookText(text string, maxChars int64) string {
	if maxChars == 0 {
		return fmt.Sprintf("[%d characters, not shown: max_output_chars is 0]\n", utf8.RuneCountInString(text))
	}
	if count := int64(utf8.RuneCountInString(text)); count > maxChars {
		runes := []rune(text)
		return string(runes[:maxChars]) + fmt.Sprintf("\n[... %d more characters]\n", count-maxChars)
	}
	return text
}

func notebookLanguage(top map[string]any) string {
	metadata, _ := top["metadata"].(map[string]any)
	if info, ok := metadata["language_info"].(map[string]any); ok {
		if name, ok := info["name"].(string); ok {
			return name
		}
	}
	if spec, ok := metadata["kernelspec"].(map[string]any); ok {
		if language, ok := spec["language"].(string); ok {
			return language
		}
	}
	return ""
}

func notebookMimeTypes(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
