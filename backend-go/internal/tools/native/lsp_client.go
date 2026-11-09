package native

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// lspStderrTail is how much of a language server's stderr is kept to explain a failure.
const lspStderrTail = 4096

// lspPool holds the running language servers of the process, one per server setting and
// workspace root. A server outlives the catalog that started it (catalogs are rebuilt per
// run) and stops after its idle time, at ShutdownLanguageServers, or when its stdin closes
// because jad exited.
type lspPool struct {
	mu    sync.Mutex
	slots map[string]*lspSlot
}

type lspSlot struct {
	mu     sync.Mutex
	client *lspClient
}

var languageServers = &lspPool{slots: map[string]*lspSlot{}}

// ShutdownLanguageServers stops every language server the process started.
func ShutdownLanguageServers() {
	languageServers.mu.Lock()
	slots := languageServers.slots
	languageServers.slots = map[string]*lspSlot{}
	languageServers.mu.Unlock()
	for _, slot := range slots {
		slot.mu.Lock()
		if slot.client != nil {
			slot.client.close()
		}
		slot.mu.Unlock()
	}
}

func lspKey(server sessionfs.NativeLSPServer, root string, sandbox *sessionfs.SandboxSettings, place sandboxPlace) string {
	identity, _ := json.Marshal(server)
	return string(identity) + "\x00" + root + "\x00" + sandboxKey(sandbox, place)
}

// acquire returns the running server for root in sandbox, starting and initializing it when it
// is not running. A server started in one sandbox never serves a call of another. The caller
// calls release when its call is done.
func (p *lspPool) acquire(ctx context.Context, server sessionfs.NativeLSPServer, root string, settings sessionfs.NativeLSPSettings, sandbox *sessionfs.SandboxSettings, place sandboxPlace) (*lspClient, error) {
	key := lspKey(server, root, sandbox, place)
	p.mu.Lock()
	slot := p.slots[key]
	if slot == nil {
		slot = &lspSlot{}
		p.slots[key] = slot
	}
	p.mu.Unlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.client != nil && slot.client.tryBegin() {
		return slot.client, nil
	}
	client, err := startLSPClient(ctx, server, root, settings, sandbox, place)
	if err != nil {
		return nil, err
	}
	slot.client = client
	client.tryBegin()
	return client, nil
}

type lspResponse struct {
	result json.RawMessage
	err    *lspError
}

type lspError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *lspError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type lspDiagnostics struct {
	seq   int64
	items []lspDiagnostic
}

type lspDocument struct {
	version int
	digest  [32]byte
}

// lspClient is one running language server and the JSON-RPC connection to it.
type lspClient struct {
	server   sessionfs.NativeLSPServer
	root     string
	settings sessionfs.NativeLSPSettings
	command  *exec.Cmd
	stdin    io.WriteCloser
	stderr   *lspTail

	writeMu sync.Mutex

	mu          sync.Mutex
	nextID      int64
	pending     map[int64]chan lspResponse
	diagnostics map[string]lspDiagnostics
	diagSeq     int64
	published   chan struct{} // closed and replaced at every publish
	documents   map[string]*lspDocument
	inflight    int
	idle        *time.Timer
	closing     bool

	done    chan struct{}
	doneErr error
}

func startLSPClient(ctx context.Context, server sessionfs.NativeLSPServer, root string, settings sessionfs.NativeLSPSettings, sandbox *sessionfs.SandboxSettings, place sandboxPlace) (*lspClient, error) {
	program, err := exec.LookPath(server.Command[0])
	if err != nil {
		return nil, fmt.Errorf("the language server %s (%s) cannot be run: %w", server.Name, server.Command[0], err)
	}
	launch, err := sandboxed(sandbox, place, server.Command[0], program, server.Command[1:])
	if err != nil {
		return nil, fmt.Errorf("start the language server %s in the sandbox: %w", server.Name, err)
	}
	command := exec.Command(launch.resolved, launch.args...)
	command.Args[0] = launch.configured
	command.Dir = launch.dir
	command.Env = environmentWithoutServerToken()
	for name, value := range server.Env {
		command.Env = append(command.Env, name+"="+value)
	}
	configureProcessGroup(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &lspTail{}
	command.Stderr = tail
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start the language server %s: %w", server.Name, err)
	}
	client := &lspClient{
		server: server, root: root, settings: settings, command: command, stdin: stdin, stderr: tail,
		pending: map[int64]chan lspResponse{}, diagnostics: map[string]lspDiagnostics{}, published: make(chan struct{}),
		documents: map[string]*lspDocument{}, done: make(chan struct{}),
	}
	go client.read(stdout)
	go func() {
		err := command.Wait()
		client.finish(fmt.Errorf("the language server %s exited: %v%s", server.Name, err, tail.explain()))
	}()
	initCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.RequestTimeoutMs)*time.Millisecond)
	defer cancel()
	if _, err := client.request(initCtx, "initialize", client.initializeParams()); err != nil {
		client.close()
		return nil, fmt.Errorf("initialize the language server %s: %w", server.Name, err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (c *lspClient) initializeParams() map[string]any {
	rootURI := lspFileURI(c.root)
	params := map[string]any{
		"processId":  os.Getpid(),
		"clientInfo": map[string]any{"name": "jad"},
		"rootUri":    rootURI, "rootPath": c.root,
		"workspaceFolders": []any{map[string]any{"uri": rootURI, "name": filepath.Base(c.root)}},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"dynamicRegistration": false, "didSave": false},
				"publishDiagnostics": map[string]any{"versionSupport": true},
				"definition":         map[string]any{"linkSupport": false},
				"typeDefinition":     map[string]any{"linkSupport": false},
				"implementation":     map[string]any{"linkSupport": false},
				"references":         map[string]any{},
				"hover":              map[string]any{"contentFormat": []string{"plaintext", "markdown"}},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
			},
			"workspace": map[string]any{"workspaceFolders": true, "configuration": true, "symbol": map[string]any{}},
			"window":    map[string]any{"workDoneProgress": false},
		},
	}
	if len(c.server.InitializationOptions) > 0 {
		params["initializationOptions"] = c.server.InitializationOptions
	}
	return params
}

// tryBegin marks a call in progress unless the server has ended or is stopping; end marks
// it done and restarts the idle countdown. Checking and counting under one lock keeps the
// idle shutdown from stopping a server a call has just taken.
func (c *lspClient) tryBegin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return false
	default:
	}
	if c.closing {
		return false
	}
	c.inflight++
	if c.idle != nil {
		c.idle.Stop()
	}
	return true
}

func (c *lspClient) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	if c.inflight > 0 || c.closing {
		return
	}
	wait := time.Duration(c.settings.IdleShutdownMs) * time.Millisecond
	if c.idle == nil {
		c.idle = time.AfterFunc(wait, c.idleShutdown)
	} else {
		c.idle.Reset(wait)
	}
}

func (c *lspClient) idleShutdown() {
	c.mu.Lock()
	if c.inflight > 0 || c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	c.mu.Unlock()
	c.shutdown()
}

// close asks the server to shut down and exit, and kills its process group if it does not
// within the request timeout.
func (c *lspClient) close() {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		<-c.done
		return
	}
	c.closing = true
	if c.idle != nil {
		c.idle.Stop()
	}
	c.mu.Unlock()
	c.shutdown()
}

func (c *lspClient) shutdown() {
	wait := time.Duration(c.settings.RequestTimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	_, _ = c.request(ctx, "shutdown", nil)
	cancel()
	_ = c.notify("exit", nil)
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(wait):
		signalProcessGroup(c.command, true)
		<-c.done
	}
}

func (c *lspClient) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.doneErr = err
	close(c.done)
	for id, waiter := range c.pending {
		waiter <- lspResponse{err: &lspError{Code: -32099, Message: err.Error()}}
		delete(c.pending, id)
	}
}

func (c *lspClient) write(message map[string]any) error {
	message["jsonrpc"] = "2.0"
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

func (c *lspClient) notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return c.write(message)
}

// request sends a request and waits for its answer, the context or the server's end.
func (c *lspClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	waiter := make(chan lspResponse, 1)
	c.pending[id] = waiter
	c.mu.Unlock()
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if err := c.write(message); err != nil {
		c.forget(id)
		return nil, fmt.Errorf("send %s: %w", method, err)
	}
	select {
	case response := <-waiter:
		if response.err != nil {
			return nil, response.err
		}
		return response.result, nil
	case <-ctx.Done():
		c.forget(id)
		_ = c.notify("$/cancelRequest", map[string]any{"id": id})
		return nil, fmt.Errorf("%s: no answer within the request timeout (setting: tools.native.lsp.request_timeout_ms)", method)
	case <-c.done:
		return nil, c.doneErr
	}
}

func (c *lspClient) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// read dispatches the server's messages: answers to requests, diagnostics, and requests
// from the server, which get a neutral answer so the server never waits on jad.
func (c *lspClient) read(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	headers := textproto.NewReader(reader)
	for {
		header, err := headers.ReadMIMEHeader()
		if err != nil {
			return
		}
		length, err := strconv.Atoi(header.Get("Content-Length"))
		if err != nil || length < 0 {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *lspError       `json:"error"`
		}
		if json.Unmarshal(body, &message) != nil {
			continue
		}
		switch {
		case message.Method != "" && len(message.ID) > 0:
			c.answerServerRequest(message.ID, message.Method, message.Params)
		case message.Method == "textDocument/publishDiagnostics":
			c.storeDiagnostics(message.Params)
		case message.Method == "":
			var id int64
			if json.Unmarshal(message.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			waiter := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if waiter != nil {
				waiter <- lspResponse{result: message.Result, err: message.Error}
			}
		}
	}
}

func (c *lspClient) answerServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	if method == "workspace/configuration" {
		var request struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &request)
		result = make([]any, len(request.Items))
	}
	_ = c.write(map[string]any{"id": id, "result": result})
}

func (c *lspClient) storeDiagnostics(params json.RawMessage) {
	var publish struct {
		URI         string          `json:"uri"`
		Diagnostics []lspDiagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(params, &publish) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diagSeq++
	c.diagnostics[lspNormalizeURI(publish.URI)] = lspDiagnostics{seq: c.diagSeq, items: publish.Diagnostics}
	close(c.published)
	c.published = make(chan struct{})
}

// sync sends the file's current text: didOpen the first time, didChange when it changed.
// It reports whether the server got new text and the diagnostics sequence before it.
func (c *lspClient) sync(uri, text string) (changed bool, before int64, err error) {
	digest := sha256.Sum256([]byte(text))
	c.mu.Lock()
	document := c.documents[uri]
	before = c.diagnostics[uri].seq
	switch {
	case document == nil:
		document = &lspDocument{version: 1, digest: digest}
		c.documents[uri] = document
		c.mu.Unlock()
		return true, before, c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": c.server.LanguageID, "version": 1, "text": text}})
	case document.digest != digest:
		document.version++
		document.digest = digest
		version := document.version
		c.mu.Unlock()
		return true, before, c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": version}, "contentChanges": []any{map[string]any{"text": text}}})
	}
	c.mu.Unlock()
	return false, before, nil
}

// waitDiagnostics waits until the server publishes diagnostics for uri after sequence
// before, or wait passes. It returns what is stored and whether a new publish arrived.
func (c *lspClient) waitDiagnostics(ctx context.Context, uri string, before int64, wait time.Duration) ([]lspDiagnostic, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		stored := c.diagnostics[uri]
		published := c.published
		c.mu.Unlock()
		if stored.seq > before {
			return stored.items, true
		}
		select {
		case <-published:
		case <-deadline.C:
			return stored.items, false
		case <-ctx.Done():
			return stored.items, false
		case <-c.done:
			return stored.items, false
		}
	}
}

func (c *lspClient) storedDiagnostics(uri string) ([]lspDiagnostic, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored, ok := c.diagnostics[uri]
	return stored.items, ok
}

// lspTail keeps the end of the server's stderr.
type lspTail struct {
	mu   sync.Mutex
	data []byte
}

func (t *lspTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > lspStderrTail {
		t.data = append([]byte(nil), t.data[len(t.data)-lspStderrTail:]...)
	}
	return len(p), nil
}

func (t *lspTail) explain() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.TrimSpace(string(bytes.ToValidUTF8(t.data, []byte("?"))))
	if text == "" {
		return ""
	}
	return "; its stderr ends with: " + text
}

func lspFileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// lspURIPath is the file path of a file URI, or "" for another scheme.
func lspURIPath(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(parsed.Path)
}

// lspNormalizeURI makes URIs that name the same file compare equal (servers differ in
// which characters they escape).
func lspNormalizeURI(uri string) string {
	if path := lspURIPath(uri); path != "" {
		return lspFileURI(path)
	}
	return uri
}
