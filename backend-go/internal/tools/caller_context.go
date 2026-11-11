package tools

// CallerContext is what a caller hands to a run it starts (docs/design_unified_agent_runs.md,
// section 3.3): the run scope of the calling tool call, plus the caller facts that are not in
// ExecutionContext. It is built beside the fields it mirrors (phase R1) and is not the only
// carrier. The local profile_message plan reads it (run node, run depth, deny, model keys,
// remote hops). An ephemeral run does not: SubagentRequest.Caller reaches RunSpec.Caller
// unread (kept reserved for U1; removal is a separate R7 cleanup if U1 is declined), and the run's scope comes from its Execution, which the subagent tool and the
// manager derive from the same call (DECISIONS.md, "Unified agent runs: completion
// assessment").
//
// It describes the caller, not the started run. The run's own scope (its depth, handle owner,
// worktree root and path scope, narrowed environment) is derived by the starter and never
// written back here.
//
// Some caller facts stay context values. The cost meter and hook reporter are session types,
// and session imports tools. The caller's transcript is a key private to the app package. The
// profile_message chain is read from the run registry, and its max_chain and child-call mark
// are context values of the call. They reach a run through SubagentRequest.CallerValues or the
// call's context.
//
// TestCallerContextCarriesEveryRunScopedField walks both structs and fails when a run-scoped
// ExecutionContext field has no counterpart here.
type CallerContext struct {
	SessionKey string
	// Workspace is the caller's WorkspaceRoot. WorkspaceOrigin and PathScope are the caller's,
	// before a worktree of the started run moves them.
	Workspace       string
	WorkspaceOrigin string
	SessionDir      string
	PathScope       *PathScope
	// Environment is the caller's sandbox ceiling as its call carries it. A run started from
	// here is narrowed by it, never widened.
	Environment *Environment
	PlanMode    bool
	PlanGuarded bool
	// AgentName is the sub-agent the caller runs as; empty for the session's own run.
	AgentName string
	// Depth is the caller's AgentDepth: 0 in the session's own run. A child runs at Depth+1.
	Depth   int
	RootRun string
	// RunNode is the caller's ExecutionContext.RunNode: the node a run it starts hangs off.
	RunNode string
	// ChainDepth is the caller's ChainDepth, its depth across both kinds of run; RunDepth is
	// the one max_depth is checked on. A run it starts, of either kind, runs at RunDepth()+1.
	ChainDepth int
	// Deny is the caller's ExecutionContext.Deny: what it hands every run it starts as a ceiling.
	Deny []string
	// ModelKeyEnvs is the caller's ExecutionContext.ModelKeyEnvs: the key variables a sub-agent run
	// or a local profile_message run it starts hides beside its own models'.
	ModelKeyEnvs []string
	// RunTools is the caller's ExecutionContext.RunTools, its own tools. It is not handed on: a run
	// the caller starts sets its own.
	RunTools func(Definition) bool
	// HandleOwner is the caller's ProcessOwner (empty for the session's own run), and
	// ProcessAsync its handle mode.
	HandleOwner           string
	ProcessAsync          bool
	RemoteHops            int
	OnFileChange          func(path, action string)
	RequestInteraction    InteractionHandler
	OnInteractionResolved InteractionResolvedHandler
	MCPRoute              MCPRouter
	// RemoteCaller is "remote:<token id>" when the remote API made the call, "" for a local one.
	RemoteCaller string
	// ModelChoice and SandboxChoice are the model and sandbox the call named; empty when it
	// named none.
	ModelChoice   string
	SandboxChoice string
}

// Caller returns the caller context of a call made with e: every run-scoped field of e, which
// is the set ForNested keeps. The fields ExecutionContext has no counterpart for (RemoteCaller,
// ModelChoice, SandboxChoice) are left for the tool that knows them.
func (e ExecutionContext) Caller() CallerContext {
	return CallerContext{
		SessionKey:            e.SessionKey,
		Workspace:             e.WorkspaceRoot,
		WorkspaceOrigin:       e.WorkspaceOrigin,
		SessionDir:            e.SessionDir,
		PathScope:             e.PathScope,
		Environment:           e.Environment,
		PlanMode:              e.PlanMode,
		PlanGuarded:           e.PlanGuarded,
		AgentName:             e.AgentName,
		Depth:                 e.AgentDepth,
		RootRun:               e.RootRun,
		RunNode:               e.RunNode,
		ChainDepth:            e.ChainDepth,
		Deny:                  e.Deny,
		ModelKeyEnvs:          e.ModelKeyEnvs,
		RunTools:              e.RunTools,
		HandleOwner:           e.ProcessOwner,
		ProcessAsync:          e.ProcessAsync,
		RemoteHops:            e.RemoteHops,
		OnFileChange:          e.OnFileChange,
		RequestInteraction:    e.RequestInteraction,
		OnInteractionResolved: e.OnInteractionResolved,
		MCPRoute:              e.MCPRoute,
	}
}

// RunDepth is the caller's ExecutionContext.RunDepth.
func (c CallerContext) RunDepth() int {
	return max(c.ChainDepth, c.Depth)
}
