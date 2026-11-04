package native

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestMain lets the test binary serve as a fake language server (JAD_FAKE_LSP=1), so the
// client is tested over real pipes without any language server installed.
func TestMain(m *testing.M) {
	if os.Getenv("JAD_FAKE_LSP") != "" {
		runFakeLSP(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeLSP(in io.Reader, out io.Writer) {
	reader := bufio.NewReader(in)
	headers := textproto.NewReader(reader)
	send := func(message map[string]any) {
		message["jsonrpc"] = "2.0"
		body, _ := json.Marshal(message)
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	texts := map[string]string{}
	publish := func(uri string, version int) {
		text := texts[uri]
		if strings.Contains(text, "SILENT") {
			return
		}
		diagnostics := []any{}
		for number, line := range strings.Split(text, "\n") {
			if index := strings.Index(line, "BAD"); index >= 0 {
				character := len(utf16.Encode([]rune(line[:index])))
				diagnostics = append(diagnostics, map[string]any{
					"range":    map[string]any{"start": map[string]any{"line": number, "character": character}, "end": map[string]any{"line": number, "character": character + 3}},
					"severity": 1, "source": "fake", "code": "F1", "message": fmt.Sprintf("bad thing v%d", version),
				})
			}
		}
		send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": uri, "version": version, "diagnostics": diagnostics}})
	}
	var pendingInit json.RawMessage
	for {
		header, err := headers.ReadMIMEHeader()
		if err != nil {
			return
		}
		length, _ := strconv.Atoi(header.Get("Content-Length"))
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(body, &message)
		var params struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Text    string `json:"text"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
			Position map[string]any `json:"position"`
			Query    string         `json:"query"`
		}
		_ = json.Unmarshal(message.Params, &params)
		uri := params.TextDocument.URI
		switch message.Method {
		case "initialize":
			// Ask the client something first: the answer must come before initialize is answered.
			pendingInit = message.ID
			send(map[string]any{"id": "cfg-1", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{"section": "fake"}, map[string]any{"section": "other"}}}})
		case "":
			if string(message.ID) == `"cfg-1"` && pendingInit != nil {
				if string(message.Result) == "[null,null]" {
					send(map[string]any{"id": pendingInit, "result": map[string]any{"capabilities": map[string]any{}}})
				} else {
					send(map[string]any{"id": pendingInit, "error": map[string]any{"code": -1, "message": "bad configuration answer " + string(message.Result)}})
				}
			}
		case "textDocument/didOpen":
			texts[uri] = params.TextDocument.Text
			publish(uri, params.TextDocument.Version)
		case "textDocument/didChange":
			texts[uri] = params.ContentChanges[0].Text
			publish(uri, params.TextDocument.Version)
		case "textDocument/definition", "textDocument/typeDefinition", "textDocument/implementation":
			// The requested position comes back as a link, and a place outside any workspace.
			position := params.Position
			send(map[string]any{"id": message.ID, "result": []any{
				map[string]any{"targetUri": uri, "targetRange": map[string]any{"start": position, "end": position}, "targetSelectionRange": map[string]any{"start": position, "end": position}},
				map[string]any{"uri": "file:///nonexistent/lib/x.go", "range": map[string]any{"start": map[string]any{"line": 9, "character": 4}, "end": map[string]any{"line": 9, "character": 4}}},
			}})
		case "textDocument/references":
			locations := []any{}
			for line := range 5 {
				locations = append(locations, map[string]any{"uri": uri, "range": map[string]any{"start": map[string]any{"line": line, "character": 0}, "end": map[string]any{"line": line, "character": 1}}})
			}
			send(map[string]any{"id": message.ID, "result": locations})
		case "textDocument/hover":
			if strings.Contains(texts[uri], "CRASH") {
				fmt.Fprintln(os.Stderr, "fake crashed on purpose")
				os.Exit(3)
			}
			send(map[string]any{"id": message.ID, "result": map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nfunc target() int\n```"}}})
		case "textDocument/documentSymbol":
			send(map[string]any{"id": message.ID, "result": []any{map[string]any{
				"name": "Server", "kind": 23, "detail": "struct", "range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 3, "character": 1}},
				"selectionRange": map[string]any{"start": map[string]any{"line": 0, "character": 5}, "end": map[string]any{"line": 0, "character": 11}},
				"children":       []any{map[string]any{"name": "Start", "kind": 6, "range": map[string]any{"start": map[string]any{"line": 1, "character": 1}, "end": map[string]any{"line": 1, "character": 6}}, "selectionRange": map[string]any{"start": map[string]any{"line": 1, "character": 1}, "end": map[string]any{"line": 1, "character": 6}}}},
			}}})
		case "workspace/symbol":
			send(map[string]any{"id": message.ID, "result": []any{map[string]any{"name": params.Query + "Thing", "kind": 12, "containerName": "pkg", "location": map[string]any{"uri": "file:///nonexistent/lib/y.go", "range": map[string]any{"start": map[string]any{"line": 2, "character": 5}, "end": map[string]any{"line": 2, "character": 9}}}}}})
		case "shutdown":
			send(map[string]any{"id": message.ID, "result": nil})
		case "exit":
			return
		default:
			if len(message.ID) > 0 {
				send(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			}
		}
	}
}
