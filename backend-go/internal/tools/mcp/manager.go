package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type connection struct {
	mu                       sync.RWMutex
	callMu                   sync.Mutex
	fingerprint              string
	session                  *mcpSDK.ClientSession
	timeout                  time.Duration
	maxResultBytes           int
	structuredContentCapture *structuredContentCapture
	closed                   bool
}

func (c *connection) listTools(ctx context.Context) (*mcpSDK.ListToolsResult, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, errors.New("MCP connection is closed")
	}
	return c.session.ListTools(ctx, nil)
}

func (c *connection) callTool(ctx context.Context, params *mcpSDK.CallToolParams) (*mcpSDK.CallToolResult, json.RawMessage, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, nil, errors.New("MCP connection is closed")
	}
	pending := c.structuredContentCapture.begin()
	result, err := c.session.CallTool(ctx, params)
	structuredContent := c.structuredContentCapture.finish(pending)
	return result, structuredContent, err
}

func (c *connection) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.session.Close()
}

// Manager owns MCP client connections for one session and exposes their tools
// through the common tool catalog contract.
type Manager struct {
	mu          sync.Mutex
	environment map[string]string
	connections map[string]*connection
	closed      bool
}

// NewManager creates a session-scoped MCP manager using the provided process
// environment for env_from and headers_from references.
func NewManager(environment map[string]string) *Manager {
	return &Manager{environment: cloneStringMap(environment), connections: make(map[string]*connection)}
}

// LoadTools refreshes the enabled MCP servers and returns their validated tools.
func (m *Manager) LoadTools(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string) ([]tools.Tool, error) {
	if ctx == nil {
		return nil, errors.New("MCP tool loading context is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("MCP manager is closed")
	}

	enabledIDs := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if server.Enabled {
			enabledIDs[server.ID] = struct{}{}
		}
	}
	for id, existing := range m.connections {
		if _, enabled := enabledIDs[id]; enabled {
			continue
		}
		delete(m.connections, id)
		_ = existing.close()
	}

	loaded := make([]tools.Tool, 0)
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, err := m.getConnection(ctx, server, cwd)
		if err != nil {
			return nil, err
		}
		listCtx, cancel := context.WithTimeout(ctx, current.timeout)
		listed, err := current.listTools(listCtx)
		cancel()
		if err != nil {
			if m.connections[server.ID] == current {
				delete(m.connections, server.ID)
			}
			_ = current.close()
			return nil, configError(server.ConfigPath, "failed to list tools from MCP server %s (%s)", server.ID, safeErrorType(err))
		}
		if listed == nil {
			return nil, configError(server.ConfigPath, "MCP server %s returned an invalid tools/list result", server.ID)
		}
		serverTools, err := m.toolsFromList(server, current, listed.Tools)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, serverTools...)
	}
	return loaded, nil
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
		schema, err := marshalJSON(remote.InputSchema)
		if err != nil || !objectInputSchema(schema) {
			return nil, configError(server.ConfigPath, "MCP tool %s/%s must provide an object inputSchema", server.ID, remote.Name)
		}
		validator, err := tools.CompileSchema(schema)
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
		approval := ""
		if server.Approval == sessionfs.ApprovalAsk {
			approval = tools.ApprovalAsk
		}
		loaded = append(loaded, tools.Tool{
			Approval: approval,
			Definition: tools.Definition{
				ID:          "mcp:" + server.ID + ":" + remote.Name,
				Source:      "mcp",
				Server:      server.ID,
				Name:        name,
				Description: description,
				InputSchema: append(json.RawMessage(nil), schema...),
			},
			Validator: validator,
			ExecuteOutput: func(ctx context.Context, arguments map[string]any, _ tools.ExecutionContext) (json.RawMessage, error) {
				return executeTool(ctx, current, serverID, remoteName, arguments)
			},
		})
	}
	return loaded, nil
}

func (m *Manager) getConnection(ctx context.Context, server sessionfs.MCPServerReference, cwd string) (*connection, error) {
	contents, err := os.ReadFile(server.ConfigPath)
	if err != nil {
		return nil, configError(server.ConfigPath, "could not read MCP server config: %v", err)
	}
	config, err := normalizeConfig(contents, server, cwd, m.environment)
	if err != nil {
		return nil, err
	}
	if current := m.connections[server.ID]; current != nil && current.fingerprint == config.fingerprint {
		return current, nil
	}
	if current := m.connections[server.ID]; current != nil {
		delete(m.connections, server.ID)
		_ = current.close()
	}

	var transport mcpSDK.Transport
	if config.transport.stdio != nil {
		stdio := config.transport.stdio
		command, err := commandWithEnvironmentPath(stdio.command, stdio.env)
		if err != nil {
			return nil, configError(server.ConfigPath, "failed to start MCP server %s (%s)", server.ID, safeErrorType(err))
		}
		cmd := exec.Command(command, stdio.args...)
		cmd.Dir = stdio.cwd
		cmd.Env = environmentSlice(stdio.env)
		transport = &mcpSDK.CommandTransport{Command: cmd}
	} else {
		httpConfig := config.transport.http
		transport = &mcpSDK.StreamableClientTransport{
			Endpoint:   httpConfig.endpoint,
			HTTPClient: newHeaderHTTPClient(httpConfig.endpoint, httpConfig.headers),
		}
	}

	structuredCapture := newStructuredContentCapture()
	transport = &structuredContentTransport{inner: transport, capture: structuredCapture}
	client := mcpSDK.NewClient(&mcpSDK.Implementation{Name: "jad-" + server.ID, Version: "1.0.0"}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, config.timeout)
	clientSession, err := client.Connect(connectCtx, transport, nil)
	cancel()
	if err != nil {
		if clientSession != nil {
			_ = clientSession.Close()
		}
		return nil, configError(server.ConfigPath, "failed to connect MCP server %s (%s)", server.ID, safeErrorType(err))
	}
	current := &connection{
		fingerprint:              config.fingerprint,
		session:                  clientSession,
		timeout:                  config.timeout,
		maxResultBytes:           config.maxResultBytes,
		structuredContentCapture: structuredCapture,
	}
	m.connections[server.ID] = current
	return current, nil
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
	connections := make([]*connection, 0, len(m.connections))
	for _, current := range m.connections {
		connections = append(connections, current)
	}
	m.connections = make(map[string]*connection)
	m.mu.Unlock()
	for _, current := range connections {
		_ = current.close()
	}
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

func newHeaderHTTPClient(endpoint string, headers map[string]string) *http.Client {
	parsed, _ := url.Parse(endpoint)
	base := http.DefaultTransport
	client := &http.Client{
		Transport: headerRoundTripper{base: base, origin: urlOrigin(parsed), headers: cloneStringMap(headers)},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if !sameOrigin(request.URL, parsed) {
				for name := range headers {
					request.Header.Del(name)
				}
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	return client
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
