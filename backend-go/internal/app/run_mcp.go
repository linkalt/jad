package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// runMCPLoader starts the stdio MCP servers of an isolated run of config's session: a run in a
// git worktree of its own calls its MCP tools on servers started there (docs/isolation-
// decisions.md D10, P6), not on the session's, which work in the session's folder.
//
// The servers of the set the request names are the session's (empty key) or a toolset's, as
// the session lists them; each starts in the run's workspace, in the sandbox of the agent the
// request is for (runServerSandbox: the agent's own, narrowed, when it lists mcp), and joins an
// overlay run's holder. They
// belong to one manager of their own, which the returned Close stops, so nothing is shared
// with another run. A streamable HTTP server is not started: it is a remote endpoint with no
// folder of its own, so the run keeps calling the connection the session has to it.
func (r *Resolver) runMCPLoader(config sessionfs.SessionConfig) native.RunMCPLoader {
	return func(ctx context.Context, request native.RunMCPRequest) (native.RunMCPServers, error) {
		settings := config
		if request.Key != "" {
			found := false
			for _, use := range toolsetMCPUses(config) {
				if use.key == request.Key {
					settings, found = use.config, true
					break
				}
			}
			if !found {
				return native.RunMCPServers{}, fmt.Errorf("the MCP servers %q of the run's agent are not configured", request.Key)
			}
		}
		sandbox, err := runServerSandbox(settings.Sandbox, request.Key != "", request.Environment)
		if err != nil {
			return native.RunMCPServers{}, err
		}
		key := filepath.Clean(config.SettingPath) + "\x00" + config.Profile + "\x00run\x00" + strconv.FormatUint(r.runMCPSeq.Add(1), 10)
		manager, err := r.mcpManagerFor(key)
		if err != nil {
			return native.RunMCPServers{}, err
		}
		manager.SetRunPlacement(mcpclient.RunPlacement{Origin: request.Origin, Workspace: request.Workspace})
		manager.SetCommandWrapper(request.Workspace, sandboxCommandWrapper(sandbox, request.Workspace))
		if request.Key == "" {
			manager.SetSampler(r.mcpSampler(config))
		}
		loaded, ids, err := manager.LoadStdioTools(ctx, settings.MCPServers, request.Workspace)
		if err != nil || len(ids) == 0 {
			r.closeMCPManager(key)
			return native.RunMCPServers{}, err
		}
		return native.RunMCPServers{Tools: loaded, Servers: ids, Close: func() { r.closeMCPManager(key) }}, nil
	}
}

// runServerSandbox is the sandbox the servers of a run start in, for the effective environment
// of the agent they start for. The agent's own sandbox, as narrowed for it, binds them when it
// lists mcp (native.MCPSandbox), so a child with a narrower sandbox has servers no wider than
// it, however the run's first agent was confined; without sandbox.tools listing mcp the servers
// are not confined: only their folder is the run's (and an overlay run's holder is still joined).
// A toolset's servers also keep the toolset's own sandbox when it lists mcp, and must then fit
// inside it: a sandbox of the agent that reaches more than the toolset's own does not start them
// (a failure, never the wider sandbox).
func runServerSandbox(configured *sessionfs.SandboxSettings, toolset bool, environment *tools.Environment) (*sessionfs.SandboxSettings, error) {
	if !toolset || environment == nil {
		return native.MCPSandbox(configured, environment), nil
	}
	own := native.MCPSandbox(configured, nil)
	agent := native.MCPSandbox(nil, environment)
	if own == nil || agent == nil {
		if agent == nil {
			return own, nil
		}
		return agent, nil
	}
	narrowed, err := sessionfs.NarrowSandbox(own, agent, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("the agent's sandbox does not fit inside the sandbox of its toolset's MCP servers: %w", err)
	}
	return narrowed, nil
}

// closeMCPManager stops the servers of the manager under key and forgets it.
func (r *Resolver) closeMCPManager(key string) {
	r.mu.Lock()
	manager := r.mcpManagers[key]
	delete(r.mcpManagers, key)
	r.mu.Unlock()
	_ = manager.Close()
}
