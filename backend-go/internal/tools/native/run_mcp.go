package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"sync"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// RunMCPRequest asks a RunMCPLoader for the stdio MCP servers of one isolated run.
type RunMCPRequest struct {
	// Key names the server set: empty for the session's own servers, else ToolsetMCPKey of the
	// agent whose toolset they belong to.
	Key string
	// Workspace is the run's workspace root, the session's folder inside the run's worktree,
	// where its servers start. Origin is the session's own folder.
	Workspace string
	Origin    string
	// Environment is the effective environment of the agent the servers start for: its sandbox,
	// as narrowed for it (a child can be narrower than the run's first agent, and a delegated
	// run's carries its paths moved into the worktree). The servers start under this policy
	// (MCPSandbox) and are used only by callers whose environment has the same sandbox. Nil: the
	// run has none, and the servers' own configured sandbox applies.
	Environment *tools.Environment
}

// MCPSandbox is the sandbox a stdio MCP server starts in for a run whose environment is
// environment: what a call of a native tool would use (callSandbox), so a run that has an
// environment is bound by its sandbox when it covers mcp and by none when it does not, and
// configured (the sandbox of the servers' own session, when it lists mcp) only for a run
// without one.
func MCPSandbox(configured *sessionfs.SandboxSettings, environment *tools.Environment) *sessionfs.SandboxSettings {
	if environment == nil && !configured.Covers("mcp") {
		return nil
	}
	return callSandbox("mcp", configured, tools.ExecutionContext{Environment: environment})
}

// RunMCPServers are the stdio servers started for one run: the tools they list, the ids of the
// servers (a streamable HTTP server is not among them), and what stops them.
type RunMCPServers struct {
	Tools   []tools.Tool
	Servers []string
	Close   func()
}

// RunMCPLoader starts the stdio MCP servers of an isolated run, in the run's own workspace and
// in the sandbox the session's servers start in. The run keeps what it returns until the run
// ends; a failure fails the run, which never falls back to the session's servers.
type RunMCPLoader func(context.Context, RunMCPRequest) (RunMCPServers, error)

// errRunMCPEnded is the failure of a call to a server of a run that is over.
var errRunMCPEnded = errors.New("the run has ended, and so have its MCP servers")

// runMCP is the stdio MCP servers of one isolated run, by server set and by who they are for,
// and the route of the run's calls to them (tools.MCPRouter). A set is one server set (the
// session's, or a toolset's) started for one workspace and one effective sandbox: the first
// agent of the run (or of a child that works in its worktree) that has tools of the set starts
// it, and an agent with the same workspace and sandbox shares it. A child whose sandbox is
// narrower, or otherwise not the same, gets a set of its own, started under its policy; it never
// calls the servers of a wider one. All stop together when the run's worktree is finished.
// Nothing is shared with another run.
type runMCP struct {
	workspace string
	origin    string

	// loadMu lets one set load at a time; mu guards sets and ended, which a call reads.
	loadMu sync.Mutex
	mu     sync.RWMutex
	sets   map[string]*runMCPSet
	ended  bool
}

// runMCPSet is one started set: its server set key, its tools by id and the servers they
// belong to.
type runMCPSet struct {
	key     string
	tools   map[string]tools.Tool
	servers map[string]struct{}
	stop    func()
	once    sync.Once
}

func newRunMCP(workspace, origin string) *runMCP {
	return &runMCP{workspace: workspace, origin: origin, sets: make(map[string]*runMCPSet)}
}

// runMCPSetID names the set of server set key that a caller in execution uses: the key, the
// workspace and the effective sandbox, so agents that differ in either have separate servers.
// An unreadable sandbox is named by its error, which no other caller's is.
func runMCPSetID(key string, execution tools.ExecutionContext) string {
	sandbox := ""
	if environment := execution.Environment; environment != nil && environment.Sandbox != nil {
		encoded, err := json.Marshal(environment.Sandbox)
		if err != nil {
			encoded = []byte("unreadable: " + err.Error())
		}
		sandbox = string(encoded)
	}
	return key + "\x00" + filepath.Clean(execution.WorkspaceRoot) + "\x00" + sandbox
}

// route is the run's tools.MCPRouter. A call to a tool of a started set's stdio servers is
// covered: it goes to the run's tool, or fails; a tool of any other server is not covered. A
// caller whose workspace and sandbox no set was started for is covered too when a set of the
// key holds the server (it fails), so it is never answered by another agent's servers, and
// never by the session's.
func (r *runMCP) route(key string, definition tools.Definition, execution tools.ExecutionContext) (tools.Tool, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.sets[runMCPSetID(key, execution)]
	if set == nil {
		for _, other := range r.sets {
			if _, stdio := other.servers[definition.Server]; stdio && other.key == key {
				if r.ended {
					return tools.Tool{}, true, errRunMCPEnded
				}
				return tools.Tool{}, true, fmt.Errorf("the MCP server %s was not started for this agent's sandbox and workspace", definition.Server)
			}
		}
		return tools.Tool{}, false, nil
	}
	if _, stdio := set.servers[definition.Server]; !stdio {
		return tools.Tool{}, false, nil
	}
	if r.ended {
		return tools.Tool{}, true, errRunMCPEnded
	}
	tool, offered := set.tools[definition.ID]
	if !offered {
		return tools.Tool{}, true, fmt.Errorf("the MCP server %s started for this run does not offer %s", definition.Server, definition.ID)
	}
	return tool, true, nil
}

// ensure starts the set key names for an agent in execution, unless the run has started it
// for the same workspace and sandbox. A failure leaves the set not started, so the run that
// needed it ends and nothing of it is left running.
func (r *runMCP) ensure(ctx context.Context, key string, execution tools.ExecutionContext, load RunMCPLoader) error {
	id := runMCPSetID(key, execution)
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	r.mu.RLock()
	_, started := r.sets[id]
	ended := r.ended
	r.mu.RUnlock()
	switch {
	case ended:
		return errRunMCPEnded
	case started:
		return nil
	}
	servers, err := callRunMCPLoader(ctx, load, RunMCPRequest{Key: key, Workspace: r.workspace, Origin: r.origin, Environment: execution.Environment})
	if err != nil {
		return err
	}
	set := &runMCPSet{key: key, tools: make(map[string]tools.Tool, len(servers.Tools)), servers: make(map[string]struct{}, len(servers.Servers)), stop: servers.Close}
	for _, tool := range servers.Tools {
		set.tools[tool.Definition.ID] = tool
	}
	for _, id := range servers.Servers {
		set.servers[id] = struct{}{}
	}
	r.mu.Lock()
	if r.ended {
		// The run ended while the servers started.
		r.mu.Unlock()
		set.stopServers()
		return errRunMCPEnded
	}
	r.sets[id] = set
	r.mu.Unlock()
	return nil
}

// callRunMCPLoader runs the loader; a panic in it fails the start, so the run ends and its
// worktree is finished like any other failed start.
func callRunMCPLoader(ctx context.Context, load RunMCPLoader, request RunMCPRequest) (servers RunMCPServers, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("run MCP loader panicked", "panic", recovered, "stack", string(debug.Stack()))
			servers, err = RunMCPServers{}, errors.New("the MCP server loader failed inside the server")
		}
	}()
	return load(ctx, request)
}

// end stops every started set and makes later calls to them fail. It is safe to call twice.
func (r *runMCP) end() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.ended = true
	started := make([]*runMCPSet, 0, len(r.sets))
	for _, set := range r.sets {
		started = append(started, set)
	}
	r.mu.Unlock()
	for _, set := range started {
		set.stopServers()
	}
}

func (s *runMCPSet) stopServers() {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
}

// openRunMCP gives tree's run MCP servers of its own, which start when an agent of the run
// needs them (startRunMCP), and returns them with the route for the run's execution.
func openRunMCP(tree *worktree.Tree, origin string) (tools.MCPRouter, *runMCP) {
	mcp := newRunMCP(tree.Workspace, origin)
	isolatedRuns.Lock()
	path := filepath.Clean(tree.Path)
	if run, ok := isolatedRuns.byPath[path]; ok {
		run.mcp = mcp
		isolatedRuns.byPath[path] = run
	}
	isolatedRuns.Unlock()
	return mcp.route, mcp
}

// endRunMCP stops the MCP servers of tree's run, before what the run left is collected.
func endRunMCP(tree *worktree.Tree) {
	isolatedRuns.RLock()
	mcp := isolatedRuns.byPath[filepath.Clean(tree.Path)].mcp
	isolatedRuns.RUnlock()
	if mcp != nil {
		mcp.end()
	}
}

// startRunMCP starts the servers of set key for a run that works in execution's workspace, when
// that workspace is an isolated run's worktree. It does nothing for a run in the session's own
// folder, which uses the session's servers.
func startRunMCP(ctx context.Context, execution tools.ExecutionContext, key string, load RunMCPLoader) error {
	run, ok := isolatedRunOf(execution.WorkspaceRoot)
	if !ok || run.mcp == nil {
		return nil
	}
	return run.mcp.ensure(ctx, key, execution, load)
}

// StartMCP starts the session's own stdio MCP servers for the delegated run, inside its
// worktree (load is the target session's loader), for the run's execution as it stands once the
// worktree is open: its workspace and its sandbox, moved into the worktree. The run's calls
// to them go to these.
func (d *DelegatedWorktree) StartMCP(ctx context.Context, execution tools.ExecutionContext, load RunMCPLoader) error {
	if d == nil || d.mcp == nil || load == nil {
		return nil
	}
	return d.mcp.ensure(ctx, "", execution, load)
}

// subagentRunMCP is what a run needs to start its stdio MCP servers: the loader, and the key of
// the server set its agent's MCP tools were listed from.
type subagentRunMCP struct {
	load RunMCPLoader
	key  string
}

// runMCPFor is the plan for a run of agent: nil when the session has no loader or the agent has
// no MCP tool.
func (s *subagentTool) runMCPFor(agent subagentAgent) *subagentRunMCP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.runMCP == nil || len(agent.mcpTools) == 0 {
		return nil
	}
	return &subagentRunMCP{load: s.runMCP, key: ToolsetMCPKey(agent.config)}
}
