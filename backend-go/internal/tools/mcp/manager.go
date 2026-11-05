package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type connection struct {
	mu     sync.RWMutex
	callMu sync.Mutex
	// ctx is cancelled by close before it takes mu, so an in-flight call stops
	// waiting on the server instead of delaying shutdown for its own timeout.
	ctx                      context.Context
	cancel                   context.CancelFunc
	fingerprint              string
	serverID                 string
	session                  *mcpSDK.ClientSession
	timeout                  time.Duration
	maxResultBytes           int
	maxTools                 int
	maxSchemaBytes           int
	maxDescriptionBytes      int
	structuredContentCapture *structuredContentCapture
	// stdioCommand is the server process of a stdio transport, retained so close can
	// stop the rest of its process group after the SDK stopped the direct child.
	stdioCommand *exec.Cmd
	closed       bool
	// state is shared with the client's handlers: the call running now and the sampling count.
	state *clientState
	// wrapKey is the command wrapper the stdio server was started through.
	wrapKey string
	// logLevel is server_log when on: sent with each call, which is how protocol
	// 2026-07-28 asks for log messages (older ones take logging/setLevel once).
	logLevel string
}

// listTools follows the tools/list cursors until the server reports the end of the list,
// within the connection's discovery budgets.
func (c *connection) listTools(ctx context.Context) ([]*mcpSDK.Tool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, errors.New("MCP connection is closed")
	}
	return collectToolPages(ctx, c.maxTools, func(ctx context.Context, cursor string) (toolListPage, error) {
		var params *mcpSDK.ListToolsParams
		if cursor != "" {
			params = &mcpSDK.ListToolsParams{Cursor: cursor}
		}
		result, err := c.session.ListTools(ctx, params)
		if err != nil {
			return toolListPage{}, err
		}
		if result == nil {
			return toolListPage{}, errors.New("MCP server returned an invalid tools/list result")
		}
		return toolListPage{tools: result.Tools, nextCursor: result.NextCursor}, nil
	})
}

// toolListPage is one tools/list response: the tools of that page and the cursor that
// asks for the next one, empty at the end of the list.
type toolListPage struct {
	tools      []*mcpSDK.Tool
	nextCursor string
}

// toolListLimitError is a tools/list that exceeded a discovery bound. The message names
// the setting; loadServer reports it as a configuration error, because the generic list
// failure path reduces an error to its type.
type toolListLimitError struct {
	message string
}

func (e *toolListLimitError) Error() string { return e.message }

// collectToolPages follows NextCursor until it is empty. maxTools bounds the tools and,
// through maxTools+1 pages, the requests: a server that returns at least one tool per
// page reaches the tool bound first, and the extra page is the one whose tools would
// exceed it, while a server that keeps answering with a fresh cursor and no tool still
// stops. A cursor that repeats is a loop and is refused at once instead of after
// maxTools+1 requests.
func collectToolPages(ctx context.Context, maxTools int, listPage func(context.Context, string) (toolListPage, error)) ([]*mcpSDK.Tool, error) {
	requested := make(map[string]struct{})
	collected := make([]*mcpSDK.Tool, 0)
	cursor := ""
	for page := 1; ; page++ {
		if page > maxTools+1 {
			return nil, &toolListLimitError{message: fmt.Sprintf("did not finish tools/list within max_tools (%d) pages", maxTools)}
		}
		current, err := listPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		collected = append(collected, current.tools...)
		if len(collected) > maxTools {
			return nil, &toolListLimitError{message: fmt.Sprintf("returned more than max_tools (%d) tools", maxTools)}
		}
		if current.nextCursor == "" {
			return collected, nil
		}
		if _, loop := requested[current.nextCursor]; loop {
			return nil, &toolListLimitError{message: fmt.Sprintf("repeated the tools/list cursor on page %d", page+1)}
		}
		requested[current.nextCursor] = struct{}{}
		cursor = current.nextCursor
	}
}

func (c *connection) callTool(ctx context.Context, params *mcpSDK.CallToolParams) (*mcpSDK.CallToolResult, json.RawMessage, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, nil, errors.New("MCP connection is closed")
	}
	callCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// callMu makes this the connection's only call, so its user is the one to ask.
	if c.state != nil {
		c.state.setCall(activeCallFrom(ctx))
		defer c.state.setCall(nil)
	}
	if c.logLevel != "" {
		meta := params.GetMeta()
		if meta == nil {
			meta = map[string]any{}
		}
		meta[mcpSDK.MetaKeyLogLevel] = c.logLevel
		params.SetMeta(meta)
	}
	pending := c.structuredContentCapture.begin()
	result, err := c.session.CallTool(callCtx, params)
	structuredContent := c.structuredContentCapture.finish(pending)
	return result, structuredContent, err
}

func (c *connection) close() error {
	// Cancel first: a call holding the read lock must stop waiting on the server
	// before the write lock below can be taken.
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.session.Close()
	// The SDK ends the direct child only (stdin close, SIGTERM, SIGKILL); kill what a
	// wrapper started so a grandchild cannot outlive the manager.
	killStdioProcessGroup(c.stdioCommand)
	return err
}

// maxConcurrentServerLoads bounds how many MCP servers one LoadTools call connects to and
// lists at the same time.
const maxConcurrentServerLoads = 8

// Manager owns MCP client connections for one session and exposes their tools
// through the common tool catalog contract. mu guards only the maps and the closed
// flag; connecting and listing happen outside it.
type Manager struct {
	mu          sync.Mutex
	environment map[string]string
	connections map[string]*connection
	serverLocks map[string]*sync.Mutex
	schemas     *schemaCache
	closed      bool
	// connectCtx is cancelled by Close and bounds every connect, so a connect
	// cannot outlive the manager; connects counts them so Close can wait.
	connectCtx     context.Context
	cancelConnects context.CancelFunc
	connects       sync.WaitGroup
	// events keeps the servers' resource change notifications and subscriptions.
	events resourceEvents
	// acks wakes a subscribe when the server acknowledged it (listen protocol).
	acks subscriptionAcks
	// sampler answers the servers' sampling requests and logger takes their server_log
	// messages; both are set by the owner and guarded by mu.
	sampler Sampler
	logger  *slog.Logger
	// wrapCommand, when set, starts each stdio server through it (a sandbox); wrapKey
	// names the wrapper, so a changed one reconnects the servers. Guarded by mu.
	wrapCommand CommandWrapper
	wrapKey     string
}

// CommandWrapper returns how to start a stdio server's program with args from cwd: the
// file to run, its argv[0] and its arguments.
type CommandWrapper func(program string, args []string, cwd string) (string, string, []string, error)

// SetCommandWrapper starts the stdio servers through wrap (nil: directly). key names the
// wrapper; a server connected under another key is connected again.
func (m *Manager) SetCommandWrapper(key string, wrap CommandWrapper) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wrapCommand, m.wrapKey = wrap, key
}

// NewManager creates a session-scoped MCP manager using the provided process
// environment for env_from and headers_from references.
func NewManager(environment map[string]string) *Manager {
	connectCtx, cancelConnects := context.WithCancel(context.Background())
	return &Manager{
		environment:    cloneStringMap(environment),
		connections:    make(map[string]*connection),
		serverLocks:    make(map[string]*sync.Mutex),
		schemas:        newSchemaCache(defaultSchemaCacheEntries),
		connectCtx:     connectCtx,
		cancelConnects: cancelConnects,
	}
}

type serverLoad struct {
	tools []tools.Tool
	err   error
	done  bool
}

// LoadTools refreshes the enabled MCP servers and returns their validated tools in the
// configured server order. Servers load concurrently; the error returned is the one the
// first failing server in configured order produced.
func (m *Manager) LoadTools(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string) ([]tools.Tool, error) {
	if ctx == nil {
		return nil, errors.New("MCP tool loading context is required")
	}
	enabledIDs := make(map[string]struct{}, len(servers))
	enabled := make([]sessionfs.MCPServerReference, 0, len(servers))
	for _, server := range servers {
		if server.Enabled {
			enabledIDs[server.ID] = struct{}{}
			enabled = append(enabled, server)
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("MCP manager is closed")
	}
	var stale []*connection
	for id, existing := range m.connections {
		if _, ok := enabledIDs[id]; ok {
			continue
		}
		delete(m.connections, id)
		stale = append(stale, existing)
	}
	m.mu.Unlock()
	for _, existing := range stale {
		_ = existing.close()
	}

	results := make([]serverLoad, len(enabled))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		resultsMu sync.Mutex
		resolved  int
		next      atomic.Int64
		wg        sync.WaitGroup
	)
	finish := func(index int, serverTools []tools.Tool, err error) {
		resultsMu.Lock()
		defer resultsMu.Unlock()
		results[index] = serverLoad{tools: serverTools, err: err, done: true}
		for resolved < len(results) && results[resolved].done && results[resolved].err == nil {
			resolved++
		}
		if resolved < len(results) && results[resolved].done {
			cancel()
		}
	}
	workers := min(len(enabled), maxConcurrentServerLoads)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= len(enabled) {
					return
				}
				if err := runCtx.Err(); err != nil {
					finish(index, nil, err)
					continue
				}
				serverTools, err := m.loadServer(runCtx, ctx, enabled[index], cwd)
				finish(index, serverTools, err)
			}
		}()
	}
	wg.Wait()

	loaded := make([]tools.Tool, 0)
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		loaded = append(loaded, result.tools...)
	}
	return loaded, nil
}

// loadServer connects to one server, lists its tools and validates them. A list failure
// that only follows the cancellation of runCtx by an earlier server's failure leaves the
// healthy connection in place.
func (m *Manager) loadServer(runCtx, callerCtx context.Context, server sessionfs.MCPServerReference, cwd string) ([]tools.Tool, error) {
	current, err := m.getConnection(runCtx, server, cwd)
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(runCtx, current.timeout)
	remoteTools, err := current.listTools(listCtx)
	cancel()
	if err != nil {
		var limit *toolListLimitError
		if errors.As(err, &limit) {
			return nil, configError(server.ConfigPath, "MCP server %s %s", server.ID, limit.Error())
		}
		if callerCtx.Err() != nil || runCtx.Err() == nil {
			m.mu.Lock()
			if m.connections[server.ID] == current {
				delete(m.connections, server.ID)
			}
			m.mu.Unlock()
			_ = current.close()
		}
		return nil, configError(server.ConfigPath, "failed to list tools from MCP server %s (%s)", server.ID, safeErrorType(err))
	}
	loaded, err := m.toolsFromList(server, current, remoteTools)
	if err != nil {
		return nil, err
	}
	features, err := m.featureTools(server, current)
	if err != nil {
		return nil, err
	}
	return append(loaded, features...), nil
}

func (m *Manager) toolsFromList(server sessionfs.MCPServerReference, current *connection, remoteTools []*mcpSDK.Tool) ([]tools.Tool, error) {
	denied := make(map[string]struct{}, len(server.DenyTools))
	for _, name := range server.DenyTools {
		denied[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(remoteTools))
	loaded := make([]tools.Tool, 0, len(remoteTools))
	for _, remote := range remoteTools {
		if remote == nil || strings.TrimSpace(remote.Name) == "" {
			return nil, configError(server.ConfigPath, "MCP server %s returned a tool with no name", server.ID)
		}
		if _, blocked := denied[remote.Name]; blocked {
			continue
		}
		if _, exists := seen[remote.Name]; exists {
			return nil, configError(server.ConfigPath, "MCP server %s returned duplicate tool %s", server.ID, remote.Name)
		}
		seen[remote.Name] = struct{}{}
		if len(remote.Description) > current.maxDescriptionBytes {
			return nil, configError(server.ConfigPath, "MCP tool %s/%s description is %d bytes, above max_description_bytes (%d)", server.ID, remote.Name, len(remote.Description), current.maxDescriptionBytes)
		}
		schema, err := tools.MarshalJSON(remote.InputSchema)
		if err != nil || !objectInputSchema(schema) {
			return nil, configError(server.ConfigPath, "MCP tool %s/%s must provide an object inputSchema", server.ID, remote.Name)
		}
		if len(schema) > current.maxSchemaBytes {
			return nil, configError(server.ConfigPath, "MCP tool %s/%s inputSchema is %d bytes, above max_schema_bytes (%d)", server.ID, remote.Name, len(schema), current.maxSchemaBytes)
		}
		validator, err := m.schemas.get(schema)
		if err != nil {
			return nil, configError(server.ConfigPath, "invalid inputSchema for %s/%s: %v", server.ID, remote.Name, err)
		}
		name := exposedToolName(server.ID, remote.Name)
		description := remote.Description
		if description == "" {
			description = fmt.Sprintf("MCP tool %s from %s", remote.Name, server.ID)
		}
		remoteName := remote.Name
		serverID := server.ID
		loaded = append(loaded, tools.Tool{
			Definition: tools.Definition{
				ID:          "mcp:" + server.ID + ":" + remote.Name,
				Source:      "mcp",
				Server:      server.ID,
				Name:        name,
				Description: description,
				InputSchema: append(json.RawMessage(nil), schema...),
			},
			Validator: validator,
			ExecuteOutput: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
				// A server's elicitation request during the call asks this call's user.
				ctx = withActiveCall(ctx, &activeCall{request: execution.RequestInteraction, identity: tools.InteractionIdentity{Tool: execution.ToolName, ToolCallID: execution.CallID}})
				return executeTool(ctx, current, serverID, remoteName, arguments)
			},
		})
	}
	return loaded, nil
}

func (m *Manager) getConnection(ctx context.Context, server sessionfs.MCPServerReference, cwd string) (*connection, error) {
	serverLock := m.serverLock(server.ID)
	serverLock.Lock()
	defer serverLock.Unlock()

	contents, err := os.ReadFile(server.ConfigPath)
	if err != nil {
		return nil, configError(server.ConfigPath, "could not read MCP server config: %v", err)
	}
	config, err := normalizeConfig(contents, server, cwd, m.environment)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("MCP manager is closed")
	}
	m.connects.Add(1)
	defer m.connects.Done()
	wrap, wrapKey := m.wrapCommand, m.wrapKey
	existing := m.connections[server.ID]
	if existing != nil && existing.fingerprint == config.fingerprint && (config.transport.stdio == nil || existing.wrapKey == wrapKey) {
		m.mu.Unlock()
		return existing, nil
	}
	if existing != nil {
		delete(m.connections, server.ID)
	}
	m.mu.Unlock()
	if existing != nil {
		_ = existing.close()
	}

	var transport mcpSDK.Transport
	var stdioCommand *exec.Cmd
	var stored *storedOAuth
	if config.transport.stdio != nil {
		stdio := config.transport.stdio
		command, err := commandWithEnvironmentPath(stdio.command, stdio.env)
		if err != nil {
			return nil, configError(server.ConfigPath, "failed to start MCP server %s (%s)", server.ID, safeErrorType(err))
		}
		cmd := exec.Command(command, stdio.args...)
		if wrap != nil {
			file, argv0, arguments, err := wrap(command, stdio.args, stdio.cwd)
			if err != nil {
				return nil, configError(server.ConfigPath, "failed to start MCP server %s in the sandbox (%s)", server.ID, safeErrorType(err))
			}
			cmd = exec.Command(file, arguments...)
			cmd.Args[0] = argv0
		}
		cmd.Dir = stdio.cwd
		cmd.Env = environmentSlice(stdio.env)
		configureStdioProcessGroup(cmd)
		stdioCommand = cmd
		transport = &mcpSDK.CommandTransport{Command: cmd}
	} else {
		httpConfig := config.transport.http
		var handler auth.OAuthHandler
		handler, stored, err = oauthHandler(httpConfig.oauth, httpConfig.endpoint, server.TokenPath, server.ConfigPath, config.timeout)
		if err != nil {
			return nil, err
		}
		transport = &mcpSDK.StreamableClientTransport{
			Endpoint:     httpConfig.endpoint,
			OAuthHandler: handler,
			HTTPClient:   newHeaderHTTPClient(httpConfig.endpoint, httpConfig.headers, config.maxResultBytes),
			// The SDK copies this verbatim (it does not resolve 0 to a default) and
			// treats a non-positive value as no cap, so a missing value would leave
			// one streamable_http event unbounded. A result has to fit
			// max_result_bytes anyway, so that limit bounds an event as well.
			MaxEventSize: config.maxResultBytes,
		}
	}

	structuredCapture := newStructuredContentCapture()
	transport = &structuredContentTransport{inner: transport, capture: structuredCapture}
	state := &clientState{}
	options := m.clientOptions(server.ID, config.resourceEventsMax)
	m.featureClientOptions(options, server.ID, config.features, state, config.resourceEventsMax)
	client := mcpSDK.NewClient(&mcpSDK.Implementation{Name: "jad-" + server.ID, Version: "1.0.0"}, options)
	client.AddReceivingMiddleware(m.subscriptionAckMiddleware(server.ID))
	if config.features.Roots == RootsWorkspace {
		if root := workspaceRoot(cwd); root != nil {
			client.AddRoots(root)
		}
	}
	connectCtx, cancel := context.WithTimeout(ctx, config.timeout)
	stopConnect := context.AfterFunc(m.connectCtx, cancel)
	clientSession, err := client.Connect(connectCtx, transport, nil)
	stopConnect()
	cancel()
	if err != nil {
		if clientSession != nil {
			_ = clientSession.Close()
		}
		killStdioProcessGroup(stdioCommand)
		if stored != nil && stored.needLogin.Load() {
			return nil, configError(server.ConfigPath, "MCP server %s needs an OAuth sign-in: run jad mcp login %s", server.ID, loginName(server.ConfigPath))
		}
		return nil, configError(server.ConfigPath, "failed to connect MCP server %s (%s)", server.ID, safeErrorType(err))
	}
	connectionCtx, cancelConnection := context.WithCancel(context.Background())
	current := &connection{
		ctx:                      connectionCtx,
		cancel:                   cancelConnection,
		fingerprint:              config.fingerprint,
		serverID:                 server.ID,
		session:                  clientSession,
		timeout:                  config.timeout,
		maxResultBytes:           config.maxResultBytes,
		maxTools:                 config.maxTools,
		maxSchemaBytes:           config.maxSchemaBytes,
		maxDescriptionBytes:      config.maxDescriptionBytes,
		structuredContentCapture: structuredCapture,
		stdioCommand:             stdioCommand,
		state:                    state,
		wrapKey:                  wrapKey,
	}
	if _, logging := serverLogLevels[config.features.ServerLog]; logging {
		current.logLevel = config.features.ServerLog
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = current.close()
		return nil, errors.New("MCP manager is closed")
	}
	m.connections[server.ID] = current
	m.mu.Unlock()
	m.resubscribe(ctx, current)
	m.setServerLogLevel(ctx, current, config.features)
	return current, nil
}

func (m *Manager) serverLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.serverLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.serverLocks[id] = lock
	}
	return lock
}

// Close shuts down all owned MCP sessions. It is safe to call more than once.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancelConnects()
	connections := make([]*connection, 0, len(m.connections))
	for _, current := range m.connections {
		connections = append(connections, current)
	}
	m.connections = make(map[string]*connection)
	m.mu.Unlock()
	// A connect in flight is cancelled above; wait for it to settle so no session
	// can be created after Close returns.
	m.connects.Wait()
	var closing sync.WaitGroup
	for _, current := range connections {
		closing.Add(1)
		go func() {
			defer closing.Done()
			_ = current.close()
		}()
	}
	closing.Wait()
	return nil
}

func objectInputSchema(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	var schemaType string
	return json.Unmarshal(fields["type"], &schemaType) == nil && schemaType == "object"
}

func exposedToolName(serverID, toolName string) string {
	slug := func(value string) string {
		var builder strings.Builder
		underscore := false
		for _, character := range strings.ToLower(value) {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
				builder.WriteRune(character)
				underscore = false
			} else if !underscore {
				builder.WriteByte('_')
				underscore = true
			}
		}
		result := strings.Trim(builder.String(), "_")
		if result == "" {
			return "tool"
		}
		return result
	}
	digest := fmt.Sprintf("%x", sha256Sum([]byte(serverID+"\x00"+toolName)))[:10]
	readable := "mcp_" + slug(serverID) + "_" + slug(toolName)
	if len(readable) > 53 {
		readable = readable[:53]
	}
	return readable + "_" + digest
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}

// safeErrorType names a failure the way the TypeScript client does: by the
// JavaScript error class. The TypeScript SDK raises McpError for a JSON-RPC
// error response, a closed connection and a request timeout, and a plain Error
// for everything else (a missing executable, for example). The name is the only
// part of the failure shown to the model, so it must not leak Go type names.
func safeErrorType(err error) string {
	var rpcError *jsonrpc.Error
	switch {
	case errors.As(err, &rpcError),
		errors.Is(err, mcpSDK.ErrConnectionClosed),
		errors.Is(err, context.DeadlineExceeded):
		return "McpError"
	default:
		return "Error"
	}
}

func environmentSlice(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, name+"="+environment[name])
	}
	return values
}

func commandWithEnvironmentPath(command string, environment map[string]string) (string, error) {
	if filepath.IsAbs(command) || strings.ContainsRune(command, filepath.Separator) {
		return command, nil
	}
	path := environment["PATH"]
	if path == "" {
		return exec.LookPath(command)
	}
	for _, directory := range filepath.SplitList(path) {
		candidate := filepath.Join(directory, command)
		resolved, err := exec.LookPath(candidate)
		if err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("executable not found in configured PATH")
}

func newHeaderHTTPClient(endpoint string, headers map[string]string, maxResponseBytes int) *http.Client {
	parsed, _ := url.Parse(endpoint)
	base := headerRoundTripper{base: http.DefaultTransport, origin: urlOrigin(parsed), headers: cloneStringMap(headers)}
	client := &http.Client{
		Transport: bodyLimitRoundTripper{base: base, limit: int64(maxResponseBytes)},
		// A redirect is a boundary: the endpoint has no reason to send this request to
		// another origin, and the body the server wrote would follow it to whatever the
		// redirect names (loopback, a private address). The configured headers never did
		// follow (headerRoundTripper attaches them to the configured origin only); the
		// request itself must not either. Same-origin redirects are still followed.
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if !sameOrigin(request.URL, parsed) {
				return fmt.Errorf("refusing redirect to another origin: %s", request.URL.Redacted())
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	return client
}

// bodyLimitRoundTripper bounds a response body the HTTP transport would otherwise
// buffer whole: the SDK reads a streamable_http JSON answer with io.ReadAll. An event
// stream is exempt, because its body is long lived and only the size of one event is
// bounded, by StreamableClientTransport.MaxEventSize.
type bodyLimitRoundTripper struct {
	base  http.RoundTripper
	limit int64
}

func (r bodyLimitRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	if isEventStreamMediaType(response.Header.Get("Content-Type")) {
		return response, nil
	}
	response.Body = &limitedResponseBody{inner: response.Body, limit: r.limit}
	return response, nil
}

// limitedResponseBody fails the read once the body is larger than its limit. The excess
// is never buffered, so a server cannot choose the size of an allocation.
type limitedResponseBody struct {
	inner io.ReadCloser
	limit int64
	read  int64
}

func (b *limitedResponseBody) Read(buffer []byte) (int, error) {
	count, err := b.inner.Read(buffer)
	b.read += int64(count)
	if b.read > b.limit {
		return 0, &responseTooLargeError{limit: b.limit}
	}
	return count, err
}

// responseTooLargeError is an HTTP answer larger than max_result_bytes.
type responseTooLargeError struct{ limit int64 }

func (e *responseTooLargeError) Error() string {
	return fmt.Sprintf("MCP HTTP response body exceeds the max_result_bytes limit of %d bytes", e.limit)
}

func (b *limitedResponseBody) Close() error {
	return b.inner.Close()
}

func isEventStreamMediaType(contentType string) bool {
	if index := strings.IndexByte(contentType, ';'); index >= 0 {
		contentType = contentType[:index]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
}

type headerRoundTripper struct {
	base    http.RoundTripper
	origin  string
	headers map[string]string
}

func (r headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.base == nil {
		return nil, errors.New("MCP HTTP transport is not initialized")
	}
	if urlOrigin(request.URL) == r.origin && len(r.headers) > 0 {
		request = request.Clone(request.Context())
		for name, value := range r.headers {
			request.Header.Set(name, value)
		}
	}
	return r.base.RoundTrip(request)
}

func urlOrigin(value *url.URL) string {
	if value == nil {
		return ""
	}
	scheme := strings.ToLower(value.Scheme)
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else if scheme == "http" {
			port = "80"
		}
	}
	return scheme + "://" + host + ":" + port
}

func sameOrigin(left, right *url.URL) bool {
	return urlOrigin(left) == urlOrigin(right)
}
