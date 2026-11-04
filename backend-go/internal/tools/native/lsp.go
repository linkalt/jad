package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const lspToolID = "native.lsp"

const lspDescription = "Code intelligence from the project's language servers (for example gopls, pyright, typescript-language-server, rust-analyzer): exact answers where grep guesses. diagnostics lists the compiler and linter errors and warnings of a file as the server sees it now (use it after an edit). definition, references, type_definition and implementation find where a symbol is declared, used, typed or implemented; hover shows its type and documentation; symbols outlines a file; workspace_symbols searches symbol names across the project (path picks the language). Point at a symbol with line (1-based) and either column (1-based) or symbol, its text on that line. Results are file:line:column with the line's text. The server sees the files as saved on disk."

var lspInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["diagnostics","definition","references","hover","type_definition","implementation","symbols","workspace_symbols"],"description":"What to ask the language server."},
    "path":{"type":"string","minLength":1,"description":"File, relative to cwd or absolute. Its extension picks the language server."},
    "line":{"type":"integer","minimum":1,"description":"1-based line of the symbol (definition, references, hover, type_definition, implementation)."},
    "column":{"type":"integer","minimum":1,"description":"1-based column of the symbol on line."},
    "symbol":{"type":"string","minLength":1,"description":"The symbol's text on line, instead of column; its first occurrence on the line is used."},
    "query":{"type":"string","description":"workspace_symbols: part of the symbol name to search for."}
  },
  "required":["action","path"],
  "additionalProperties":false
}`)

// NewLSPTool builds native.lsp. It changes no file, so it is read-only.
func NewLSPTool(settings sessionfs.NativeLSPSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(lspInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native lsp schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{ID: lspToolID, Source: "native", Name: "lsp", Description: lspDescription, InputSchema: append(json.RawMessage(nil), lspInputSchema...)},
		Validator:  validator, ReadOnly: true, LeafBudget: true,
		ExampleOperation: json.RawMessage(`{"action":"definition","path":"main.go","line":12,"symbol":"Run"}`),
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeLSP(ctx, arguments, settings, execution)
		},
	}, nil
}

type lspArgs struct {
	action, path, symbol, query string
	line, column                int64
}

func parseLSPArgs(arguments map[string]any) (lspArgs, error) {
	var args lspArgs
	var ok bool
	if args.action, ok = arguments["action"].(string); !ok {
		return args, errors.New("action must be a string")
	}
	if args.path, ok = arguments["path"].(string); !ok {
		return args, errors.New("path must be a string")
	}
	args.symbol, _ = arguments["symbol"].(string)
	args.query, _ = arguments["query"].(string)
	for name, target := range map[string]*int64{"line": &args.line, "column": &args.column} {
		if value, exists := arguments[name]; exists {
			number, ok := safeInteger(value)
			if !ok {
				return args, fmt.Errorf("%s must be an integer", name)
			}
			*target = number
		}
	}
	return args, nil
}

var lspPositionActions = map[string]string{
	"definition": "textDocument/definition", "references": "textDocument/references", "hover": "textDocument/hover",
	"type_definition": "textDocument/typeDefinition", "implementation": "textDocument/implementation",
}

func executeLSP(ctx context.Context, arguments map[string]any, settings sessionfs.NativeLSPSettings, execution tools.ExecutionContext) (string, error) {
	args, err := parseLSPArgs(arguments)
	if err != nil {
		return "", err
	}
	callID := execution.CallID
	resolved, failure := resolveExistingTarget(args.path, execution.WorkspaceRoot, scopedPolicy(settings.Policy, execution), callID)
	if failure != "" {
		return failure, nil
	}
	server := settings.ServerFor(resolved.realPath)
	if server == nil {
		var extensions []string
		for _, configured := range settings.Servers {
			extensions = append(extensions, configured.Extensions...)
		}
		return tools.ExecutionError(callID, tools.StatusError, "no_language_server", "No language server is configured for this file",
			tools.Information{Key: "path", Value: resolved.displayPath}, tools.Information{Key: "configured_extensions", Value: strings.Join(extensions, " ")},
			tools.Information{Key: "suggestion", Value: "add a server to tools.native.lsp.servers, or use grep"}), nil
	}
	info, err := os.Stat(resolved.realPath)
	if err != nil || info.IsDir() {
		return tools.ExecutionError(callID, tools.StatusError, "not_a_file", "The path is not a readable file", tools.Information{Key: "path", Value: resolved.displayPath}), nil
	}
	if info.Size() > settings.MaxFileBytes {
		return tools.ExecutionError(callID, tools.StatusBlocked, "size_limit", "The file exceeds tools.native.lsp.max_file_bytes",
			tools.Information{Key: "path", Value: resolved.displayPath}, tools.Information{Key: "bytes", Value: info.Size()}, tools.Information{Key: "max_bytes", Value: settings.MaxFileBytes}), nil
	}
	data, err := os.ReadFile(resolved.realPath)
	if err != nil || !utf8.Valid(data) {
		return tools.ExecutionError(callID, tools.StatusError, "unreadable_file", "The file could not be read as UTF-8 text", tools.Information{Key: "path", Value: resolved.displayPath}), nil
	}
	text := string(data)
	var position map[string]any
	if _, needs := lspPositionActions[args.action]; needs {
		var failure string
		if position, failure = lspPosition(args, text, resolved.displayPath, callID); failure != "" {
			return failure, nil
		}
	}

	root := resolved.workspaceRoot
	client, err := languageServers.acquire(ctx, *server, root, settings)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "server_unavailable", "The language server could not be started",
			tools.Information{Key: "server", Value: server.Name}, tools.Information{Key: "system_error", Value: err.Error()}), nil
	}
	defer client.end()
	uri := lspFileURI(resolved.realPath)
	changed, before, err := client.sync(uri, text)
	if err != nil {
		return lspFailure(callID, server.Name, err), nil
	}
	view := lspView{root: root, policy: scopedPolicy(settings.Policy, execution), maxResults: int(settings.MaxResults), maxFileBytes: settings.MaxFileBytes}
	information := []tools.Information{{Key: "path", Value: resolved.displayPath}, {Key: "action", Value: args.action}, {Key: "server", Value: server.Name}}

	if args.action == "diagnostics" {
		items, fresh := client.storedDiagnostics(uri)
		if changed || !fresh {
			var arrived bool
			items, arrived = client.waitDiagnostics(ctx, uri, before, time.Duration(settings.DiagnosticsWaitMs)*time.Millisecond)
			if !arrived {
				information = append(information, tools.Information{Key: "note", Value: fmt.Sprintf("the server published nothing new within %d ms (setting: tools.native.lsp.diagnostics_wait_ms); shown is what it published before, if anything", settings.DiagnosticsWaitMs)})
			}
		}
		body, count, cut := view.diagnostics(resolved.displayPath, text, items)
		return lspResult(callID, fmt.Sprintf("%d diagnostics in %s", count, resolved.displayPath), information, count, cut, body)
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.RequestTimeoutMs)*time.Millisecond)
	defer cancel()
	document := map[string]any{"uri": uri}
	switch args.action {
	case "symbols":
		result, err := client.request(requestCtx, "textDocument/documentSymbol", map[string]any{"textDocument": document})
		if err != nil {
			return lspFailure(callID, server.Name, err), nil
		}
		body, count, cut := view.documentSymbols(result, text)
		return lspResult(callID, fmt.Sprintf("%d symbols in %s", count, resolved.displayPath), information, count, cut, body)
	case "workspace_symbols":
		result, err := client.request(requestCtx, "workspace/symbol", map[string]any{"query": args.query})
		if err != nil {
			return lspFailure(callID, server.Name, err), nil
		}
		body, count, cut := view.workspaceSymbols(result)
		return lspResult(callID, fmt.Sprintf("%d symbols match %q", count, args.query), information, count, cut, body)
	}
	params := map[string]any{"textDocument": document, "position": position}
	if args.action == "references" {
		params["context"] = map[string]any{"includeDeclaration": true}
	}
	result, err := client.request(requestCtx, lspPositionActions[args.action], params)
	if err != nil {
		return lspFailure(callID, server.Name, err), nil
	}
	if args.action == "hover" {
		body := lspHoverText(result)
		count := 0
		if body != "" {
			count = 1
		}
		return lspResult(callID, fmt.Sprintf("Hover at %s:%d", resolved.displayPath, args.line), information, count, false, body)
	}
	body, count, cut := view.locations(result)
	return lspResult(callID, fmt.Sprintf("%d locations", count), information, count, cut, body)
}

// lspPosition turns line and column (or symbol) into an LSP position, whose character is
// counted in UTF-16 code units.
func lspPosition(args lspArgs, text, displayPath, callID string) (map[string]any, string) {
	if args.line < 1 {
		return nil, tools.ExecutionError(callID, tools.StatusInvalidInput, "position_required", args.action+" needs line and column or symbol")
	}
	lines := strings.Split(text, "\n")
	if args.line > int64(len(lines)) {
		return nil, tools.ExecutionError(callID, tools.StatusInvalidInput, "line_out_of_range", fmt.Sprintf("The file has %d lines", len(lines)), tools.Information{Key: "path", Value: displayPath})
	}
	line := strings.TrimSuffix(lines[args.line-1], "\r")
	var runeColumn int
	switch {
	case args.symbol != "":
		index := strings.Index(line, args.symbol)
		if index < 0 {
			return nil, tools.ExecutionError(callID, tools.StatusInvalidInput, "symbol_not_on_line", fmt.Sprintf("%q is not on line %d", args.symbol, args.line),
				tools.Information{Key: "path", Value: displayPath}, tools.Information{Key: "line_text", Value: strings.TrimSpace(line)})
		}
		runeColumn = utf8.RuneCountInString(line[:index])
	case args.column >= 1:
		runeColumn = int(args.column - 1)
		if runeColumn > utf8.RuneCountInString(line) {
			return nil, tools.ExecutionError(callID, tools.StatusInvalidInput, "column_out_of_range", fmt.Sprintf("Line %d has %d characters", args.line, utf8.RuneCountInString(line)))
		}
	default:
		return nil, tools.ExecutionError(callID, tools.StatusInvalidInput, "position_required", args.action+" needs column or symbol")
	}
	return map[string]any{"line": args.line - 1, "character": lspUTF16Offset(line, runeColumn)}, ""
}

func lspUTF16Offset(line string, runes int) int {
	offset := 0
	for index, r := range []rune(line) {
		if index >= runes {
			break
		}
		offset += len(utf16.Encode([]rune{r}))
	}
	return offset
}

// lspRuneColumn turns a UTF-16 character offset on line back into a 0-based rune column.
func lspRuneColumn(line string, character int) int {
	units := 0
	for index, r := range []rune(line) {
		if units >= character {
			return index
		}
		units += len(utf16.Encode([]rune{r}))
	}
	return utf8.RuneCountInString(line)
}

func lspFailure(callID, server string, err error) string {
	return tools.ExecutionError(callID, tools.StatusError, "server_error", "The language server did not answer", tools.Information{Key: "server", Value: server}, tools.Information{Key: "system_error", Value: err.Error()})
}

func lspResult(callID, summary string, information []tools.Information, count int, cut bool, body string) (string, error) {
	information = append(information, tools.Information{Key: "results", Value: count})
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: "only the first results are shown (setting: tools.native.lsp.max_results)"})
	}
	options := tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: information}
	if body != "" {
		options.Content = &tools.Content{Text: body}
	}
	return tools.RenderToolResult(options)
}

type lspDiagnostic struct {
	Range    lspRange        `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
}

type lspRange struct {
	Start lspPos `json:"start"`
	End   lspPos `json:"end"`
}

type lspPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspLocation struct {
	URI       string   `json:"uri"`
	Range     lspRange `json:"range"`
	TargetURI string   `json:"targetUri"`
	// TargetSelectionRange is the name's range in a LocationLink.
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

// lspView renders results: paths relative to the workspace, 1-based lines and columns, and
// the line's text for files the tool's read rules allow.
type lspView struct {
	root         string
	policy       sessionfs.NativePathPolicy
	maxResults   int
	maxFileBytes int64
	files        map[string][]string
}

func (v *lspView) lines(path string) []string {
	if v.files == nil {
		v.files = map[string][]string{}
	}
	if lines, ok := v.files[path]; ok {
		return lines
	}
	var lines []string
	if v.readable(path) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() <= v.maxFileBytes {
			if data, err := os.ReadFile(path); err == nil && utf8.Valid(data) {
				lines = strings.Split(string(data), "\n")
			}
		}
	}
	v.files[path] = lines
	return lines
}

// readable reports whether the tool's read rules allow path; outside them only the path and
// position are shown, never the text.
func (v *lspView) readable(path string) bool {
	_, failure := resolveExistingTarget(path, v.root, v.policy, "")
	return failure == ""
}

func (v *lspView) place(path string, position lspPos) (string, string) {
	shown := displayPath(v.root, path)
	lines := v.lines(path)
	column := position.Character + 1
	text := ""
	if position.Line < len(lines) {
		line := strings.TrimSuffix(lines[position.Line], "\r")
		column = lspRuneColumn(line, position.Character) + 1
		text = strings.TrimSpace(line)
	}
	return fmt.Sprintf("%s:%d:%d", shown, position.Line+1, column), text
}

var lspSeverities = map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}

func (v *lspView) diagnostics(displayPath, text string, items []lspDiagnostic) (string, int, bool) {
	sorted := append([]lspDiagnostic(nil), items...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].Range.Start.Line != sorted[b].Range.Start.Line {
			return sorted[a].Range.Start.Line < sorted[b].Range.Start.Line
		}
		return sorted[a].Range.Start.Character < sorted[b].Range.Start.Character
	})
	lines := strings.Split(text, "\n")
	var builder strings.Builder
	for index, item := range sorted {
		if index == v.maxResults {
			return builder.String(), len(sorted), true
		}
		severity := lspSeverities[item.Severity]
		if severity == "" {
			severity = "error"
		}
		column := item.Range.Start.Character + 1
		if item.Range.Start.Line < len(lines) {
			column = lspRuneColumn(strings.TrimSuffix(lines[item.Range.Start.Line], "\r"), item.Range.Start.Character) + 1
		}
		fmt.Fprintf(&builder, "%s:%d:%d: %s: %s", displayPath, item.Range.Start.Line+1, column, severity, item.Message)
		var labels []string
		if item.Source != "" {
			labels = append(labels, item.Source)
		}
		if code := strings.Trim(string(item.Code), `"`); code != "" && code != "null" {
			labels = append(labels, code)
		}
		if len(labels) > 0 {
			fmt.Fprintf(&builder, " [%s]", strings.Join(labels, " "))
		}
		builder.WriteString("\n")
	}
	return builder.String(), len(sorted), false
}

func (v *lspView) locations(raw json.RawMessage) (string, int, bool) {
	var many []lspLocation
	if json.Unmarshal(raw, &many) != nil {
		var one lspLocation
		if json.Unmarshal(raw, &one) != nil || (one.URI == "" && one.TargetURI == "") {
			return "", 0, false
		}
		many = []lspLocation{one}
	}
	var builder strings.Builder
	for index, location := range many {
		if index == v.maxResults {
			return builder.String(), len(many), true
		}
		uri, start := location.URI, location.Range.Start
		if location.TargetURI != "" {
			uri = location.TargetURI
			if location.TargetSelectionRange != nil {
				start = location.TargetSelectionRange.Start
			}
		}
		path := lspURIPath(uri)
		if path == "" {
			fmt.Fprintf(&builder, "%s:%d:%d\n", uri, start.Line+1, start.Character+1)
			continue
		}
		place, text := v.place(path, start)
		builder.WriteString(place)
		if text != "" {
			builder.WriteString("  " + text)
		}
		builder.WriteString("\n")
	}
	return builder.String(), len(many), false
}

var lspSymbolKinds = []string{"", "file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface", "function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum_member", "struct", "event", "operator", "type_parameter"}

func lspKind(kind int) string {
	if kind > 0 && kind < len(lspSymbolKinds) {
		return lspSymbolKinds[kind]
	}
	return "symbol"
}

type lspSymbol struct {
	Name           string      `json:"name"`
	Detail         string      `json:"detail"`
	Kind           int         `json:"kind"`
	Range          *lspRange   `json:"range"`
	SelectionRange *lspRange   `json:"selectionRange"`
	Children       []lspSymbol `json:"children"`
	Location       *struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	} `json:"location"`
	ContainerName string `json:"containerName"`
}

func (v *lspView) documentSymbols(raw json.RawMessage, text string) (string, int, bool) {
	var symbols []lspSymbol
	_ = json.Unmarshal(raw, &symbols)
	lines := strings.Split(text, "\n")
	var builder strings.Builder
	count, cut := 0, false
	var walk func([]lspSymbol, int)
	walk = func(list []lspSymbol, depth int) {
		for _, symbol := range list {
			if count == v.maxResults {
				cut = true
				return
			}
			count++
			start := lspPos{}
			switch {
			case symbol.SelectionRange != nil:
				start = symbol.SelectionRange.Start
			case symbol.Range != nil:
				start = symbol.Range.Start
			case symbol.Location != nil:
				start = symbol.Location.Range.Start
			}
			column := start.Character + 1
			if start.Line < len(lines) {
				column = lspRuneColumn(strings.TrimSuffix(lines[start.Line], "\r"), start.Character) + 1
			}
			fmt.Fprintf(&builder, "%s%s %s", strings.Repeat("  ", depth), lspKind(symbol.Kind), symbol.Name)
			if symbol.Detail != "" {
				builder.WriteString(" " + symbol.Detail)
			}
			if symbol.ContainerName != "" {
				builder.WriteString(" (in " + symbol.ContainerName + ")")
			}
			fmt.Fprintf(&builder, "  %d:%d\n", start.Line+1, column)
			walk(symbol.Children, depth+1)
		}
	}
	walk(symbols, 0)
	return builder.String(), count, cut
}

func (v *lspView) workspaceSymbols(raw json.RawMessage) (string, int, bool) {
	var symbols []struct {
		Name          string          `json:"name"`
		Kind          int             `json:"kind"`
		ContainerName string          `json:"containerName"`
		Location      json.RawMessage `json:"location"`
	}
	_ = json.Unmarshal(raw, &symbols)
	var builder strings.Builder
	for index, symbol := range symbols {
		if index == v.maxResults {
			return builder.String(), len(symbols), true
		}
		var location lspLocation
		_ = json.Unmarshal(symbol.Location, &location)
		place := location.URI
		if path := lspURIPath(location.URI); path != "" {
			place, _ = v.place(path, location.Range.Start)
		}
		fmt.Fprintf(&builder, "%s %s", lspKind(symbol.Kind), symbol.Name)
		if symbol.ContainerName != "" {
			builder.WriteString(" (in " + symbol.ContainerName + ")")
		}
		builder.WriteString("  " + place + "\n")
	}
	return builder.String(), len(symbols), false
}

// lspHoverText joins the contents of a Hover: MarkupContent, a MarkedString or a list.
func lspHoverText(raw json.RawMessage) string {
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &hover) != nil || len(hover.Contents) == 0 || string(hover.Contents) == "null" {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(hover.Contents, &parts) != nil {
		parts = []json.RawMessage{hover.Contents}
	}
	var texts []string
	for _, part := range parts {
		var plain string
		if json.Unmarshal(part, &plain) == nil {
			texts = append(texts, plain)
			continue
		}
		var marked struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(part, &marked) == nil && marked.Value != "" {
			texts = append(texts, marked.Value)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n\n"))
}
