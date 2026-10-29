package native

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	toolSmithSocketEnv  = "TOOL_SMITH_SOCKET"
	toolSmithTokenEnv   = "TOOL_SMITH_TOKEN"
	toolSmithSocketName = "b.sock"
	// toolSmithNestedAgent is the call id prefix of a nested call whose script has no call id.
	toolSmithNestedAgent = "tool_smith"
)

// ToolSmithNested holds the built, hook-wrapped native tools a script may reach. The
// catalog builder binds it after every tool exists, because tool_smith is built before
// the rest of the list is final. An unbound or nil ToolSmithNested gives a script no tools.
type ToolSmithNested struct {
	mu        sync.RWMutex
	available []tools.Tool
	deny      []string
}

// Bind sets the tools a script may reach and the profile's tools.deny entries.
func (n *ToolSmithNested) Bind(available []tools.Tool, deny []string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.available = append([]tools.Tool(nil), available...)
	n.deny = append([]string(nil), deny...)
}

// allowed is the native tools named in settings.Tools, in that order, minus the always
// excluded ones and what the profile denies.
func (n *ToolSmithNested) allowed(settings sessionfs.NativeToolSmithSettings) []tools.Tool {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	excluded := sessionfs.NativeToolSmithExcludedTools()
	var picked []tools.Tool
	for _, name := range settings.Tools {
		if containsName(excluded, name) {
			continue
		}
		for _, tool := range n.available {
			if (tool.Definition.Source == "native" || tool.Definition.Source == "extension") && tool.Definition.Name == name && !tools.AnyEntryMatches(n.deny, tool.Definition) {
				picked = append(picked, tool)
				break
			}
		}
	}
	return picked
}

type toolSmithBridgeStats struct {
	calls    int64
	perTool  map[string]int64
	writes   int64
	bytes    int64
	duration time.Duration
}

// informationLines renders the counters as `key: value` lines; a run that made no nested
// call adds none, so its result is the one it had without a bridge.
func (s toolSmithBridgeStats) informationLines() []string {
	if s.calls == 0 {
		return nil
	}
	names := make([]string, 0, len(s.perTool))
	for name := range s.perTool {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, s.perTool[name]))
	}
	return []string{
		fmt.Sprintf("nested_calls: %d", s.calls),
		"nested_tools: " + strings.Join(parts, ", "),
		fmt.Sprintf("nested_writes: %d", s.writes),
		fmt.Sprintf("nested_bytes_returned: %d", s.bytes),
		fmt.Sprintf("nested_duration_ms: %d", s.duration.Milliseconds()),
	}
}

// appendToolSmithInformation adds lines to the end of the information block of a rendered result.
func appendToolSmithInformation(result string, lines []string) string {
	const header = "--- INFORMATION ---\n"
	if len(lines) == 0 {
		return result
	}
	start := strings.Index(result, header)
	if start < 0 {
		if !strings.HasSuffix(result, "\n") {
			result += "\n"
		}
		return result + header + strings.Join(lines, "\n") + "\n"
	}
	position := start + len(header)
	insertAt := len(result)
	if strings.HasPrefix(result[position:], "\n") {
		insertAt = position
	} else if end := strings.Index(result[position:], "\n\n"); end >= 0 {
		insertAt = position + end + 1
	}
	added := strings.Join(lines, "\n") + "\n"
	if insertAt == len(result) && !strings.HasSuffix(result, "\n") {
		added = "\n" + added
	}
	return result[:insertAt] + added + result[insertAt:]
}

type bridgeRequest struct {
	ID        json.RawMessage `json:"id"`
	Token     string          `json:"token"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// bridgeResponse answers one frame. BridgeError is set only when the bridge itself
// refused the frame; a tool that failed still answers with ok false and its status.
type bridgeResponse struct {
	ID          json.RawMessage `json:"id"`
	OK          bool            `json:"ok"`
	Status      string          `json:"status"`
	Summary     string          `json:"summary"`
	Content     string          `json:"content"`
	Info        map[string]any  `json:"info"`
	BridgeError string          `json:"bridge_error,omitempty"`
}

func bridgeFailure(id json.RawMessage, format string, arguments ...any) bridgeResponse {
	message := fmt.Sprintf(format, arguments...)
	return bridgeResponse{ID: id, Status: string(tools.StatusError), Summary: message, Info: map[string]any{}, BridgeError: message}
}

// toolSmithBridge serves nested tool calls of one run over a Unix socket.
type toolSmithBridge struct {
	settings   sessionfs.NativeToolSmithSettings
	catalog    *tools.Catalog
	byName     map[string]tools.Tool
	names      []string
	execution  tools.ExecutionContext
	baseCallID string
	listener   net.Listener
	socketPath string
	token      string
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	parallel   chan struct{}
	writer     chan struct{}

	connMu sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	started atomic.Int64

	statsMu sync.Mutex
	stats   toolSmithBridgeStats

	once sync.Once
}

// startToolSmithBridge listens on a socket inside dir (a private directory the caller
// removes) and serves nested calls of the given tools until Close.
func startToolSmithBridge(ctx context.Context, dir string, settings sessionfs.NativeToolSmithSettings, nested []tools.Tool, execution tools.ExecutionContext) (*toolSmithBridge, error) {
	catalog, err := tools.NewCatalog(nested)
	if err != nil {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	socketPath := filepath.Join(dir, toolSmithSocketName)
	if err := requirePrivateDirectory(dir); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		os.Remove(socketPath)
		return nil, err
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	baseCallID := execution.CallID
	if baseCallID == "" {
		baseCallID = toolSmithNestedAgent
	}
	bridge := &toolSmithBridge{
		settings: settings,
		catalog:  catalog,
		byName:   make(map[string]tools.Tool, len(nested)),
		// The nested tools run in the same run as this call: ForNested carries the whole run
		// scope over, so their transcript, profile_message and remote guards see the depth,
		// the root run and the hop count of this call. Only the per-call fields are cleared,
		// and the catalog sets them for each nested call.
		execution:  execution.ForNested(),
		baseCallID: baseCallID,
		listener:   listener,
		socketPath: socketPath,
		token:      hex.EncodeToString(random[:]),
		ctx:        bridgeCtx,
		cancel:     cancel,
		parallel:   make(chan struct{}, max(settings.MaxParallelCalls, 1)),
		writer:     make(chan struct{}, 1),
		conns:      map[net.Conn]struct{}{},
		stats:      toolSmithBridgeStats{perTool: map[string]int64{}},
	}
	for _, tool := range nested {
		bridge.byName[tool.Definition.Name] = tool
		bridge.names = append(bridge.names, tool.Definition.Name)
	}
	bridge.wg.Add(1)
	go bridge.accept()
	return bridge, nil
}

// Close stops the listener, drops the connections, cancels calls still running and waits
// for every goroutine of the bridge. It is safe to call more than once and returns the counters.
func (b *toolSmithBridge) Close() toolSmithBridgeStats {
	b.once.Do(func() {
		b.cancel()
		b.listener.Close()
		os.Remove(b.socketPath)
		b.connMu.Lock()
		b.closed = true
		for conn := range b.conns {
			conn.Close()
		}
		b.connMu.Unlock()
		b.wg.Wait()
	})
	b.statsMu.Lock()
	defer b.statsMu.Unlock()
	copied := b.stats
	copied.perTool = make(map[string]int64, len(b.stats.perTool))
	for name, count := range b.stats.perTool {
		copied.perTool[name] = count
	}
	return copied
}

func (b *toolSmithBridge) accept() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.connMu.Lock()
		if b.closed {
			b.connMu.Unlock()
			conn.Close()
			return
		}
		b.conns[conn] = struct{}{}
		b.wg.Add(1)
		b.connMu.Unlock()
		go b.serveConn(conn)
	}
}

func (b *toolSmithBridge) serveConn(conn net.Conn) {
	defer b.wg.Done()
	defer func() {
		conn.Close()
		b.connMu.Lock()
		delete(b.conns, conn)
		b.connMu.Unlock()
	}()
	reader := bufio.NewReaderSize(conn, 4096)
	var writeMu sync.Mutex
	reply := func(response bridgeResponse) {
		data, err := json.Marshal(response)
		if err != nil {
			data, _ = json.Marshal(bridgeFailure(response.ID, "the result could not be encoded"))
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.Write(append(data, '\n'))
	}
	for {
		line, err := readBridgeFrame(reader, b.settings.NestedRequestMaxBytes)
		if errors.Is(err, errBridgeFrameTooLarge) {
			reply(bridgeFailure(oversizeFrameID(line), "frame too large"))
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return
			}
			continue
		}
		if b.started.Add(1) > int64(b.settings.MaxNestedCalls) {
			reply(bridgeFailure(requestID(line), "max_nested_calls of %d reached", b.settings.MaxNestedCalls))
			if err != nil {
				return
			}
			continue
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() {
				// The model sees a generic failure, so without the log line a panicking
				// helper call would leave no trace (C7).
				if recovered := recover(); recovered != nil {
					slog.Error("tool_smith bridge call failed inside the server", "panic", recovered)
					reply(bridgeFailure(requestID(line), "internal error"))
				}
			}()
			reply(b.handleFrame(line))
		}()
		if err != nil {
			return
		}
	}
}

var errBridgeFrameTooLarge = errors.New("bridge frame too large")

const bridgeOversizePrefixBytes = 4096

var bridgeNumericID = regexp.MustCompile(`"id"\s*:\s*(-?[0-9]+)`)

// oversizeFrameID finds a numeric "id" in the first bytes of a frame that was too large.
func oversizeFrameID(prefix []byte) json.RawMessage {
	match := bridgeNumericID.FindSubmatch(prefix)
	if match == nil {
		return nil
	}
	return json.RawMessage(match[1])
}

// readBridgeFrame reads one newline-terminated frame. When it is longer than limit bytes it
// discards the rest of the line without buffering it and returns the first bytes read with
// errBridgeFrameTooLarge, so a script cannot make the server buffer more.
func readBridgeFrame(reader *bufio.Reader, limit int64) ([]byte, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if int64(len(frame))+int64(len(chunk)) > limit+1 {
			prefix := append(frame, chunk...)
			if len(prefix) > bridgeOversizePrefixBytes {
				prefix = prefix[:bridgeOversizePrefixBytes]
			}
			prefix = append([]byte(nil), prefix...)
			for err == bufio.ErrBufferFull {
				_, err = reader.ReadSlice('\n')
			}
			return prefix, errBridgeFrameTooLarge
		}
		frame = append(frame, chunk...)
		if err != bufio.ErrBufferFull {
			return frame, err
		}
	}
}

func requestID(line []byte) json.RawMessage {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &request) != nil || string(request.ID) == "null" {
		return nil
	}
	return request.ID
}

func (b *toolSmithBridge) handleFrame(line []byte) bridgeResponse {
	var request bridgeRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return bridgeFailure(nil, "malformed frame: not a JSON object")
	}
	id := request.ID
	if len(id) == 0 || string(id) == "null" {
		return bridgeFailure(nil, "malformed frame: id is missing")
	}
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(b.token)) != 1 {
		return bridgeFailure(id, "wrong token")
	}
	tool, known := b.byName[request.Tool]
	if !known {
		return bridgeResponse{
			ID: id, Status: string(tools.StatusInvalidInput), Info: map[string]any{},
			Summary: fmt.Sprintf("tool %s not allowed; allowed: %s", request.Tool, strings.Join(b.names, ", ")),
		}
	}
	rawArguments := string(request.Arguments)
	if len(request.Arguments) == 0 || rawArguments == "null" {
		rawArguments = "{}"
	}
	var decoded map[string]any
	_ = json.Unmarshal([]byte(rawArguments), &decoded)
	if decoded != nil && tool.NormalizeArguments != nil {
		decoded = tool.NormalizeArguments(decoded)
	}
	writes := !tool.ReadOnly || (tool.MayChangeFiles != nil && tool.MayChangeFiles(decoded))

	gate := b.parallel
	if writes {
		gate = b.writer
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-b.ctx.Done():
		return bridgeFailure(id, "the run ended")
	}

	b.statsMu.Lock()
	if b.stats.calls >= b.settings.MaxNestedCalls {
		b.statsMu.Unlock()
		return bridgeFailure(id, "max_nested_calls of %d reached", b.settings.MaxNestedCalls)
	}
	b.stats.calls++
	sequence := b.stats.calls
	b.stats.perTool[request.Tool]++
	b.statsMu.Unlock()

	started := time.Now()
	call, err := json.Marshal(map[string]any{
		"type":      "function_call",
		"call_id":   fmt.Sprintf("%s.n%d", b.baseCallID, sequence),
		"name":      request.Tool,
		"arguments": rawArguments,
	})
	if err != nil {
		return bridgeFailure(id, "the call could not be encoded")
	}
	output, err := b.catalog.ExecuteFunctionCall(b.ctx, call, b.execution)
	response := bridgeResponse{ID: id, Info: map[string]any{}}
	if err != nil {
		response.Status = string(tools.StatusError)
		response.Summary = "the call failed: " + err.Error()
	} else {
		response = b.nestedResponse(id, output)
	}
	b.statsMu.Lock()
	if writes && err == nil && response.Status != string(tools.StatusBlocked) && response.Status != string(tools.StatusInvalidInput) {
		b.stats.writes++
	}
	b.stats.bytes += int64(len(response.Content))
	b.stats.duration += time.Since(started)
	b.statsMu.Unlock()
	return response
}

func (b *toolSmithBridge) nestedResponse(id json.RawMessage, output json.RawMessage) bridgeResponse {
	var item struct {
		Output json.RawMessage `json:"output"`
	}
	text := string(output)
	if json.Unmarshal(output, &item) == nil {
		text = string(item.Output)
		var asString string
		if json.Unmarshal(item.Output, &asString) == nil {
			text = asString
		}
	}
	response := parseToolEnvelope(text)
	response.ID = id
	cut, truncated := tools.CutText(response.Content, b.settings.NestedResultMaxBytes)
	if truncated {
		response.Content = cut
		response.Info["truncated"] = true
	}
	return response
}

// parseToolEnvelope reads the text envelope of tools/output.go: a JSON header line, the
// information block and an optional fenced content section. Information values that
// read true or false become booleans; the rest stay text.
func parseToolEnvelope(text string) bridgeResponse {
	headerLine, rest, _ := strings.Cut(text, "\n")
	var header struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	response := bridgeResponse{Info: map[string]any{}}
	if json.Unmarshal([]byte(headerLine), &header) != nil || header.Status == "" {
		response.Status = string(tools.StatusError)
		response.Summary = "the tool result could not be read"
		return response
	}
	response.Status = header.Status
	response.Summary = header.Summary
	response.OK = header.Status == string(tools.StatusOK)
	_, block, found := strings.Cut(rest, "--- INFORMATION ---\n")
	if !found {
		return response
	}
	infoText, contentPart, hasContent := strings.Cut(block, "\n--- CONTENT token=")
	for _, line := range strings.Split(infoText, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || key == "" {
			continue
		}
		switch value {
		case "true":
			response.Info[key] = true
		case "false":
			response.Info[key] = false
		default:
			response.Info[key] = value
		}
	}
	if !hasContent {
		return response
	}
	token, afterToken, _ := strings.Cut(contentPart, " encoding=")
	encoding, body, _ := strings.Cut(afterToken, " ---\n")
	if index := strings.LastIndex(body, "\n--- END CONTENT token="+token+" ---"); index >= 0 {
		body = body[:index]
	}
	if encoding == "base64" {
		response.Info["encoding"] = encoding
	}
	response.Content = body
	return response
}
