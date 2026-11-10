package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// RunPlacement says that a manager starts the stdio servers of one isolated run: the run
// works on Workspace, a copy of Origin (the session's own folder) in a worktree. A server's
// working directory that lies inside Origin is moved to the same place inside Workspace, and
// is not checked on the host, since an overlay run's files exist only inside its mount.
type RunPlacement struct {
	Origin    string
	Workspace string
}

// move is cwd inside Workspace when it is inside Origin, and cwd otherwise. A folder already
// inside Workspace stays: a worktree may itself lie below the session's folder (a repository's
// .git folder).
func (p *RunPlacement) move(cwd string) string {
	if p == nil || p.Origin == "" || p.Workspace == "" {
		return cwd
	}
	if _, inside := relativeInside(p.Workspace, cwd); inside {
		return cwd
	}
	relative, inside := relativeInside(p.Origin, cwd)
	if !inside {
		return cwd
	}
	return filepath.Join(p.Workspace, relative)
}

// relativeInside is path relative to root, and whether path is root or lies inside it.
func relativeInside(root, path string) (string, bool) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

// SetRunPlacement makes the manager the one of an isolated run's servers (see RunPlacement).
// Its tools never route a call elsewhere: they are what a route points at.
func (m *Manager) SetRunPlacement(place RunPlacement) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.place = &place
}

// SetRouteKey makes the tools this manager lists hand the call of a run that has servers of
// its own to those (ExecutionContext.MCPRoute), asking the route for the server set key
// names: empty for the session's own servers, else the key of a toolset's. Tools listed
// before the call keep what they were listed with.
func (m *Manager) SetRouteKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routed, m.routeKey = true, key
}

// callRoute is what a listed tool asks before it calls its own connection.
type callRoute struct {
	enabled bool
	key     string
}

func (m *Manager) callRoute() callRoute {
	m.mu.Lock()
	defer m.mu.Unlock()
	return callRoute{enabled: m.routed, key: m.routeKey}
}

// call runs the call on the run's own tool when the run has one for it. routed is false when
// the tool has to call its own connection; when it is true the result is final, a failure
// included, so a call of a run that has its own servers never reaches the session's.
func (r callRoute) call(ctx context.Context, definition tools.Definition, remoteName string, arguments map[string]any, execution tools.ExecutionContext) (output json.RawMessage, routed bool, err error) {
	if !r.enabled || execution.MCPRoute == nil {
		return nil, false, nil
	}
	target, covered, routeErr := execution.MCPRoute(r.key, definition, execution)
	if !covered {
		return nil, false, nil
	}
	if routeErr != nil {
		output, err = toolErrorResult(definition.Server, remoteName, routeErr.Error())
		return output, true, err
	}
	output, err = target.ExecuteOutput(ctx, arguments, execution)
	return output, true, err
}

// LoadStdioTools is LoadTools for the stdio servers among servers only: the tools they list,
// and the ids of those servers. A streamable HTTP server is left out, so the owner keeps
// using the connection it has to it. The manager answers for the servers it started only;
// the owner closes it.
func (m *Manager) LoadStdioTools(ctx context.Context, servers []sessionfs.MCPServerReference, cwd string) ([]tools.Tool, []string, error) {
	m.mu.Lock()
	place := m.place
	m.mu.Unlock()
	stdio := make([]sessionfs.MCPServerReference, 0, len(servers))
	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		contents, err := os.ReadFile(server.ConfigPath)
		if err != nil {
			return nil, nil, configError(server.ConfigPath, "could not read MCP server config: %v", err)
		}
		config, err := normalizeConfigFor(contents, server, cwd, m.environment, true, place)
		if err != nil {
			return nil, nil, err
		}
		if config.transport.stdio != nil {
			stdio = append(stdio, server)
			ids = append(ids, server.ID)
		}
	}
	loaded, err := m.LoadTools(ctx, stdio, cwd)
	if err != nil {
		return nil, nil, err
	}
	return loaded, ids, nil
}
