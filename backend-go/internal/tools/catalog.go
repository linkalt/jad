package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"jad/backend-go/internal/sessionfs"
)

// Definition is the provider-facing metadata for one tool.
type Definition struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Server      string          `json:"server,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ExecutionContext carries per-call filesystem roots without coupling tools to
// the HTTP or session packages.
type ExecutionContext struct {
	CallID string
	// PlanMode is set for the calls of a plan-mode run. A tool that starts child processes
	// in a sandbox starts them in the sandbox's plan-mode form (read-only project).
	PlanMode bool
	// PlanGuarded is set for the calls of a tool that plan mode guards (planModeGuard) and it
	// carries into nested runs (ForNested): a tool that starts runs of its own (the sub-agent
	// tool) applies the same refusal to the calls those runs make, at every depth. A tool an
	// allow entry keeps whole is not guarded, and nor are the calls below it.
	PlanGuarded   bool
	ToolName      string
	WorkspaceRoot string
	// WorkspaceOrigin, when set, is the session's own working directory and WorkspaceRoot
	// is the same folder in a git worktree of it (a sub-agent with isolation worktree). Path
	// rules written for the origin apply to the same paths in the worktree instead.
	WorkspaceOrigin string
	SessionDir      string
	// SessionKey identifies the session and profile the call belongs to. A tool that
	// keeps state beyond one call uses it to scope that state, so a user stop or a
	// reset of this session reaches only its own state.
	SessionKey            string
	RequestInteraction    InteractionHandler
	OnInteractionResolved InteractionResolvedHandler
	// RawArguments retains the provider's original JSON text for tools whose
	// persisted format depends on object insertion order.
	RawArguments json.RawMessage
	// PathScope narrows the paths a tool may touch for this call, on top of the tool's own
	// policy. The sub-agent tool sets it for a child in a worktree; nil means no further
	// narrowing.
	PathScope *PathScope
	// OnFileChange, when set, is told about each file a call changed (path as reported,
	// action created, updated, edited or deleted); for exec the hooks.shell_changes scan
	// finds them. The sub-agent tool uses it to list what a child changed.
	OnFileChange func(path, action string)
	// ProcessOwner, when set, is the owner of the async handles this call starts (an exec
	// run that outlasts block_ms, a nested child): the sub-agent tool sets it to the run's
	// id, so a child sees and stops only its own handles. Empty means the session's.
	ProcessOwner string
	// ProcessAsync, with ProcessOwner, makes the child's long calls handles of the async
	// manager owned by ProcessOwner. It is set only for a child that has the async tool.
	ProcessAsync bool
	// AgentName is the sub-agent the call runs for; empty in the session's own run.
	AgentName string
	// AgentDepth is how deep the call runs: 0 in the session's own run, 1 in a child, 2 in
	// the child of a child. RootRun is the id of the top-level run the call belongs to;
	// empty at depth 0.
	AgentDepth int
	RootRun    string
	// RemoteHops is how many JAD servers the request that started this run passed through
	// (0 for a run a user started). A remote call made from here sends RemoteHops+1.
	RemoteHops int
	// BatchWaiting says that other calls of the same model response cannot start until this
	// one returns (the batch runs one call at a time, or has fewer slots than calls). A tool
	// that would wait long by default, like subagent and profile_message, then waits only
	// the async default_block_ms, so it does not hold the rest of the response back.
	BatchWaiting bool
	// Dispatcher is the catalog that dispatches this call. The catalog sets it for every call
	// it runs, so a runner tool (load_tool run) dispatches its target through the catalog the
	// run executes with: a plan-mode or narrowed catalog refuses what it dropped.
	Dispatcher Dispatcher
	// ViaRunner says that a runner tool dispatches this call, not the model: a lazy tool
	// accepts only such a call. The runner sets it on the call it dispatches.
	ViaRunner bool
	// Environment, when set, is the environment of the run the call belongs to: every child
	// process of a tool its sandbox covers starts in it, and the in-process readers see only
	// what it shows. The bound runner sets it from the session's sandbox; a sub-agent with a
	// sandbox of its own, or a profile_message target narrowed by its caller, gets its own.
	// ForNested copies it, so a nested call stays inside its parent's. Nil: each tool's own
	// configured sandbox, as before.
	Environment *Environment
	// MCPRoute, when set, is how the run's calls to MCP tools reach the stdio servers started
	// for that run alone (an isolated run works on a worktree of its own, and so do its
	// servers). An MCP tool asks it before it calls its own connection. ForNested copies it, so
	// a nested call of the run keeps the run's servers. Nil: every MCP tool calls its own
	// connection, the session's.
	MCPRoute MCPRouter
}

// MCPRouter says whether a call to the MCP tool definition, of the server set key names
// (empty: the session's; else the key of a toolset's servers), goes to a tool of the run
// instead, and which. execution is the call's own: the servers a run starts are for one
// workspace and one effective sandbox, so the agent that calls (a child may run in a narrower
// sandbox than the run's first agent) is answered by the servers started for it. covered false
// means the run has no server of its own behind this tool (a streamable HTTP server, or a set
// the run did not start), so the tool uses its own connection. covered true with an error means
// the run's server cannot take the call (its tool is gone, none was started for this caller's
// sandbox, or the run has ended): the call fails and never reaches the session's server.
type MCPRouter func(key string, definition Definition, execution ExecutionContext) (tool Tool, covered bool, err error)

// Environment is what a run's calls start in (docs/isolation-sandbox-design.md §9.4).
type Environment struct {
	// Sandbox is the run's resolved sandbox; nil runs every child without one.
	Sandbox *sessionfs.SandboxSettings
}

// Dispatcher is what a runner tool needs of the catalog that runs it: dispatch one call and
// tell whether a tool is in the catalog. *Catalog implements it.
type Dispatcher interface {
	ExecuteFunctionCall(ctx context.Context, call json.RawMessage, execution ExecutionContext) (json.RawMessage, error)
	Has(name string) bool
}

// executionContextPerCall names the fields of ExecutionContext that describe one call
// rather than the run it belongs to. The catalog fills them for every call it dispatches,
// so ForNested clears them; every other field carries over to the nested call.
// TestForNestedKeepsEveryRunScopedField walks all the fields and fails when one is
// neither copied nor listed here.
var executionContextPerCall = []string{"CallID", "ToolName", "RawArguments", "BatchWaiting", "Dispatcher", "ViaRunner"}

// ForNested returns the context of a call made inside this one: a nested call of a
// tool_smith script, or the tools of a sub-agent. The whole run scope carries over — above
// all AgentDepth, RootRun and RemoteHops, which the guards of transcript, transcripts,
// profile_message and the remote calls read — so a nested call is not silently treated as
// the session's own run. Only the per-call identity is cleared, because the catalog sets
// it for each call it dispatches. A caller that must not pass a hook on clears it itself.
func (e ExecutionContext) ForNested() ExecutionContext {
	nested := e
	nested.CallID = ""
	nested.ToolName = ""
	nested.RawArguments = nil
	nested.BatchWaiting = false
	nested.Dispatcher = nil
	nested.ViaRunner = false
	return nested
}

// PathScope is a per-call narrowing of a tool's path policy. Allow, when not empty,
// limits the call to paths inside one of its entries; Deny refuses paths inside one.
// A relative entry is taken from the call's workspace root.
type PathScope struct {
	Allow []string
	Deny  []string
}

// Tool is one compiled and executable function tool.
type Tool struct {
	Definition Definition
	Validator  *SchemaValidator
	// ExampleOperation is a sample argument object shown to the model when its
	// arguments are rejected. It is raw JSON so the declared key order survives;
	// nil omits the example.
	ExampleOperation     json.RawMessage
	RequiresUserDecision bool
	// Listed, when set, says whether the tool is in the tools array sent to the model right now.
	// A lazy tool is never listed (WithLazyGate): load_tool describes and runs it, so the array
	// the model sends does not depend on its use. Nil means always listed. It does not affect
	// dispatch: a call by name still reaches the tool.
	Listed func() bool
	// Lazy marks a tool a lazy set gated (WithLazyGate): it is not in the tools array, and load_tool
	// lists, describes and runs it. Only the tools route reads it (RouteDefinitions), so a client can
	// show the tool as lazy; it is no part of Definition and never reaches the provider payload.
	Lazy bool
	// Refused, when set and true for a call, skips the argument checks (normalize and schema) so the
	// call goes straight to the tool, whose own Execute refuses it. The refusal then runs inside the
	// tool's wrappers, so its event hooks fire like any other call. A lazy tool refuses every call
	// that a runner (load_tool run) does not dispatch. Nil never refuses.
	Refused func(execution ExecutionContext) bool
	// ReadOnly marks a tool that never changes state and depends on none, so its
	// calls may run beside other calls of the same response. MayChangeFiles can
	// still make one call of such a tool count as a change.
	ReadOnly bool
	// LeafBudget marks a tool whose call is a leaf: it runs its own work, never waits on
	// another tool call or run, and never calls runToolBatch. Its calls take a slot of the
	// process-wide leaf budget (see session.runToolBatch). Otherwise the call runs inline with no slot.
	LeafBudget bool
	// MayChangeFiles, when set, decides per call whether a call of this tool can
	// change files, for a tool whose effect depends on its arguments (the subagent
	// tool, whose agent may or may not be allowed to write). A true answer makes the
	// call take the checkpoint and run in order, like a write. Nil means the
	// ReadOnly flag alone decides.
	MayChangeFiles func(arguments map[string]any) bool
	// PlanModeRefusal, when set, decides the calls of a plan-mode run in place of
	// MayChangeFiles, for a tool whose calls plan mode confines instead of refusing (exec,
	// whose children start in a sandbox that keeps the project read-only): it returns why a
	// call is refused, or "" for a call that may run. execution is the call's own when it is
	// about to run, and nil when the call is only scheduled or judged for the checkpoint.
	PlanModeRefusal func(arguments map[string]any, execution *ExecutionContext) string
	// PlanGuarded is set by Catalog.PlanMode on a tool whose calls that could change files
	// are refused before they run (planModeGuard): no call of it changes files, so none
	// takes the checkpoint.
	PlanGuarded bool
	// Delegate, set on a runner tool (load_tool), parses the call's arguments into the tool
	// it runs: the target as the model named it (a model-facing name or an ID), and the
	// arguments for it. ok is false for a call that runs nothing (a list or a describe, or
	// arguments that are not a run). It is a pure parse: it checks nothing against the
	// catalog. The catalog judges a delegating call as its target when the target can be lazy
	// (ResolveCall; a run naming a native tool runs nothing and is judged as the runner), and the
	// event hooks of a delegating call are the target's, not the runner's.
	Delegate           func(arguments map[string]any) (target string, targetArguments map[string]any, ok bool)
	NormalizeArguments func(map[string]any) map[string]any
	// RejectedProperties maps an argument name the schema no longer has to the message
	// shown instead of "must NOT have additional properties".
	RejectedProperties map[string]string
	Execute            func(context.Context, map[string]any, ExecutionContext) (string, error)
	// ExecuteOutput is for tools whose provider-facing output is a JSON value
	// other than a string, such as MCP's multimodal content array.
	ExecuteOutput func(context.Context, map[string]any, ExecutionContext) (json.RawMessage, error)
}

// Catalog validates and dispatches function calls by their exposed provider
// name. Its tools are immutable after construction and safe for concurrent use.
type Catalog struct {
	tools  []Tool
	byName map[string]Tool
	// delegating is true when a tool has a Delegate, so ResolveCall has something to resolve;
	// a catalog with none answers every judgment from the call's own name without parsing.
	delegating bool
	// encoded caches each tool's Responses definition, index-aligned with tools. A tool is
	// encoded the first time it is listed, so the run loop does not re-marshal every schema on
	// every round. The bytes are shared and must not be written to. Guarded by encodedMu.
	encodedMu sync.Mutex
	encoded   []json.RawMessage
}

// NewCatalog validates the tool set and rejects duplicate exposed names.
func NewCatalog(tools []Tool) (*Catalog, error) {
	catalog := &Catalog{tools: append([]Tool(nil), tools...), byName: make(map[string]Tool, len(tools)), encoded: make([]json.RawMessage, len(tools))}
	for _, tool := range tools {
		if strings.TrimSpace(tool.Definition.ID) == "" || strings.TrimSpace(tool.Definition.Name) == "" {
			return nil, errors.New("tool id and name are required")
		}
		if tool.Validator == nil {
			return nil, fmt.Errorf("tool %s has no compiled input schema", tool.Definition.Name)
		}
		if tool.Execute == nil && tool.ExecuteOutput == nil {
			return nil, fmt.Errorf("tool %s has no executor", tool.Definition.Name)
		}
		if tool.Execute != nil && tool.ExecuteOutput != nil {
			return nil, fmt.Errorf("tool %s has multiple executors", tool.Definition.Name)
		}
		if _, exists := catalog.byName[tool.Definition.Name]; exists {
			return nil, fmt.Errorf("duplicate tool name %s", tool.Definition.Name)
		}
		catalog.byName[tool.Definition.Name] = tool
		catalog.delegating = catalog.delegating || tool.Delegate != nil
	}
	return catalog, nil
}

// MergeCatalogs composes immutable catalogs in order and rejects duplicate
// provider-facing names across their boundaries.
func MergeCatalogs(catalogs ...*Catalog) (*Catalog, error) {
	combined := make([]Tool, 0)
	for _, catalog := range catalogs {
		if catalog != nil {
			combined = append(combined, catalog.tools...)
		}
	}
	return NewCatalog(combined)
}

// WithDescription is a catalog like c in which the tool with the given ID has the given description.
// The receiver is unchanged. It is for a tool whose description depends on what is loaded after the
// tool is built (the subagent tool's agent list). A catalog without the tool is returned as it is.
func (c *Catalog) WithDescription(id, description string) (*Catalog, error) {
	if c == nil {
		return c, nil
	}
	changed := false
	described := make([]Tool, len(c.tools))
	for index, tool := range c.tools {
		if tool.Definition.ID == id {
			tool.Definition.Description = description
			changed = true
		}
		described[index] = tool
	}
	if !changed {
		return c, nil
	}
	return NewCatalog(described)
}

// Filter splits the catalog by keep. The first result holds the tools keep accepts, in
// the original order, and is what a model may see and call; the second lists the
// definitions of the rest. A tool that is not kept cannot be called through the first
// catalog (it answers UnknownToolError), while the receiver is unchanged, so whoever
// holds it can still reach every tool.
func (c *Catalog) Filter(keep func(Definition) bool) (*Catalog, []Definition, error) {
	if c == nil {
		return nil, []Definition{}, nil
	}
	kept := make([]Tool, 0, len(c.tools))
	dropped := []Definition{}
	for _, tool := range c.tools {
		if keep(tool.Definition) {
			kept = append(kept, tool)
			continue
		}
		definition := tool.Definition
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		dropped = append(dropped, definition)
	}
	catalog, err := NewCatalog(kept)
	return catalog, dropped, err
}

// Definitions returns provider definitions in the registry's stable order.
func (c *Catalog) Definitions() []Definition {
	if c == nil {
		return []Definition{}
	}
	definitions := make([]Definition, 0, len(c.tools))
	for _, tool := range c.tools {
		definition := tool.Definition
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		definitions = append(definitions, definition)
	}
	return definitions
}

// RouteDefinition is one row of the tools route: a definition, and whether the tool is lazy. A
// lazy tool is not in the tools array the model sees; the route lists it so a client can show it.
type RouteDefinition struct {
	Definition
	Lazy bool `json:"lazy,omitempty"`
}

// RouteDefinitions returns the rows of the tools route, in the registry's stable order: the tools
// that are listed now (the ones ResponseTools sends the model), and the lazy ones, marked lazy. It
// never changes what ResponseTools returns.
func (c *Catalog) RouteDefinitions() []RouteDefinition {
	if c == nil {
		return []RouteDefinition{}
	}
	rows := make([]RouteDefinition, 0, len(c.tools))
	for _, tool := range c.tools {
		if tool.Lazy {
			rows = append(rows, RouteDefinition{Definition: cloneDefinition(tool.Definition), Lazy: true})
			continue
		}
		if tool.Listed != nil && !tool.Listed() {
			continue
		}
		rows = append(rows, RouteDefinition{Definition: cloneDefinition(tool.Definition)})
	}
	return rows
}

func cloneDefinition(definition Definition) Definition {
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	return definition
}

// ResponseTools returns the function-tool objects accepted by the Responses
// API, for the tools that are listed now, in the catalog order. Native tool
// definitions are non-strict, as in the TypeScript backend.
func (c *Catalog) ResponseTools() ([]json.RawMessage, error) {
	if c == nil {
		return []json.RawMessage{}, nil
	}
	tools := make([]json.RawMessage, 0, len(c.tools))
	for index, tool := range c.tools {
		if tool.Listed != nil && !tool.Listed() {
			continue
		}
		c.encodedMu.Lock()
		cached := c.encoded[index]
		c.encodedMu.Unlock()
		if cached != nil {
			tools = append(tools, cached)
			continue
		}
		encoded, err := json.Marshal(struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		}{
			Type:        "function",
			Name:        tool.Definition.Name,
			Description: tool.Definition.Description,
			Parameters:  tool.Definition.InputSchema,
			Strict:      false,
		})
		if err != nil {
			return nil, fmt.Errorf("encode response tool %s: %w", tool.Definition.Name, err)
		}
		c.encodedMu.Lock()
		c.encoded[index] = encoded
		c.encodedMu.Unlock()
		tools = append(tools, encoded)
	}
	return tools, nil
}

// Has reports whether the catalog holds a tool with this provider-facing name.
func (c *Catalog) Has(name string) bool {
	if c == nil {
		return false
	}
	_, exists := c.byName[name]
	return exists
}

// ResolveCall returns the call a runner tool's call stands for: the same call_id, the target
// tool's name and its arguments, for a call of a tool with a Delegate that delegates to a tool of
// this catalog (by name or by ID) that can be lazy (CanBeLazy: an MCP or an extension tool). Any
// other call comes back unchanged: a call whose target is not in the catalog, is itself a runner,
// or is a native tool. The runner (load_tool) runs only a lazy tool, so a run that names a native
// tool runs nothing and is judged as the runner itself. A call is resolved at most once.
// The judgments below ask about the resolved call, so a run of an MCP tool takes the checkpoint,
// the scheduling and the image source of that tool.
func (c *Catalog) ResolveCall(call json.RawMessage) json.RawMessage {
	if c == nil || !c.delegating {
		return call
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call, &fields) != nil {
		return call
	}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil {
		return call
	}
	runner, exists := c.byName[name]
	if !exists || runner.Delegate == nil {
		return call
	}
	var text string
	if json.Unmarshal(fields["arguments"], &text) != nil {
		return call
	}
	decoded, err := jsonschemaDecode([]byte(text))
	if err != nil {
		return call
	}
	arguments, isObject := decoded.(map[string]any)
	if !isObject {
		return call
	}
	if runner.NormalizeArguments != nil {
		arguments = runner.NormalizeArguments(arguments)
	}
	target, targetArguments, ok := runner.Delegate(arguments)
	if !ok {
		return call
	}
	resolved, found := c.byTarget(target)
	if !found || resolved.Delegate != nil || !CanBeLazy(resolved.Definition) {
		return call
	}
	encoded, err := json.Marshal(targetArguments)
	if err != nil {
		return call
	}
	nameJSON, err := json.Marshal(resolved.Definition.Name)
	if err != nil {
		return call
	}
	argumentsJSON, err := json.Marshal(string(encoded))
	if err != nil {
		return call
	}
	fields["name"] = nameJSON
	fields["arguments"] = argumentsJSON
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return call
	}
	return rewritten
}

// byTarget finds a tool by its provider-facing name or by its ID.
func (c *Catalog) byTarget(target string) (Tool, bool) {
	if tool, exists := c.byName[target]; exists {
		return tool, true
	}
	for _, tool := range c.tools {
		if tool.Definition.ID == target {
			return tool, true
		}
	}
	return Tool{}, false
}

// IsUserDecisionCall reports whether the call must be resolved before any
// co-batched calls execute.
func (c *Catalog) IsUserDecisionCall(call json.RawMessage) bool {
	if c == nil {
		return false
	}
	call = c.ResolveCall(call)
	var fields map[string]json.RawMessage
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil {
		return false
	}
	return c.byName[name].RequiresUserDecision
}

// ChangesState reports whether a call can change state, or depend on a change made
// by another call of the same response. An unknown name runs nothing, so it does
// not count; a tool that is not marked ReadOnly (every MCP tool included) does.
func (c *Catalog) ChangesState(call json.RawMessage) bool {
	if c == nil {
		return false
	}
	call = c.ResolveCall(call)
	var fields struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	tool, exists := c.byName[fields.Name]
	if !exists {
		return false
	}
	return !tool.ReadOnly || c.CallMayChangeFiles(call)
}

// IsLeafCall reports whether a call runs under the leaf budget: its tool opts in with
// LeafBudget. An unknown name runs nothing, so it is not a leaf.
func (c *Catalog) IsLeafCall(call json.RawMessage) bool {
	if c == nil {
		return false
	}
	call = c.ResolveCall(call)
	var fields struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	return c.byName[fields.Name].LeafBudget
}

// CallMayChangeFiles reports whether a call to a tool that decides per call (see
// Tool.MayChangeFiles) can change files. A tool without that hook answers false: the
// checkpoint judges those by name.
func (c *Catalog) CallMayChangeFiles(call json.RawMessage) bool {
	if c == nil {
		return false
	}
	call = c.ResolveCall(call)
	var fields struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	tool, exists := c.byName[fields.Name]
	if !exists || tool.MayChangeFiles == nil {
		return false
	}
	var text string
	if json.Unmarshal(fields.Arguments, &text) != nil {
		return false
	}
	var arguments map[string]any
	if json.Unmarshal([]byte(text), &arguments) != nil || arguments == nil {
		return false
	}
	if tool.NormalizeArguments != nil {
		arguments = tool.NormalizeArguments(arguments)
	}
	return tool.MayChangeFiles(arguments)
}

// IsPlanGuardedCall reports whether a call goes to a tool that plan mode guards (see
// Tool.PlanGuarded). An unknown name is not guarded.
func (c *Catalog) IsPlanGuardedCall(call json.RawMessage) bool {
	if c == nil {
		return false
	}
	call = c.ResolveCall(call)
	var fields struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	return c.byName[fields.Name].PlanGuarded
}

// SourceOf returns where a called tool comes from ("native" or "mcp"), or "" for
// an unknown name.
func (c *Catalog) SourceOf(call json.RawMessage) string {
	if c == nil {
		return ""
	}
	call = c.ResolveCall(call)
	var fields struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return ""
	}
	tool, exists := c.byName[fields.Name]
	if !exists {
		return ""
	}
	return tool.Definition.Source
}

// ExecuteFunctionCall validates arguments and returns a Responses
// function_call_output item. It leaves argument values unchanged unless the
// tool explicitly supplies a normalization function.
func (c *Catalog) ExecuteFunctionCall(ctx context.Context, call json.RawMessage, execution ExecutionContext) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(call, &fields); err != nil || fields == nil {
		return nil, errors.New("function call must be a JSON object")
	}
	var callID string
	if json.Unmarshal(fields["call_id"], &callID) != nil || callID == "" {
		return nil, errors.New("function call is missing call_id")
	}
	var name string
	_ = json.Unmarshal(fields["name"], &name)
	tool, exists := c.byName[name]
	if !exists {
		return encodeFunctionCallOutput(callID, UnknownToolError(callID, name))
	}
	execution.CallID = callID
	execution.ToolName = tool.Definition.Name
	execution.Dispatcher = c
	if tool.Refused != nil && tool.Refused(execution) {
		// The tool's own Execute refuses the call, so the arguments are not checked against the schema.
		// The wrappers still see what the model sent, so a pre_tool command reports the real call.
		return c.runTool(ctx, callID, tool, refusedCallArguments(fields["arguments"]), execution)
	}

	var rawArguments string
	if json.Unmarshal(fields["arguments"], &rawArguments) != nil {
		return encodeFunctionCallOutput(callID, SchemaError(callID, "Invalid arguments for "+name, []map[string]string{{
			"keyword": "type",
			"message": "function call arguments must be a JSON string",
		}}, tool.ExampleOperation))
	}
	arguments := []byte(rawArguments)
	execution.RawArguments = append(json.RawMessage(nil), arguments...)
	if !json.Valid(arguments) {
		return encodeFunctionCallOutput(callID, InvalidArgumentsJSONError(callID, name, tool.ExampleOperation))
	}

	decoded, err := jsonschemaDecode(arguments)
	if err != nil {
		return nil, fmt.Errorf("decode arguments for tool %s: %w", name, err)
	}
	object, isObject := decoded.(map[string]any)
	if isObject && tool.NormalizeArguments != nil {
		object, err = normalizeRecovered(tool, object)
		if err != nil {
			// The model sees the generic result below, so this line is the only
			// trace a panicking normalizer leaves.
			logToolFailure(name, callID, err)
			return encodeFunctionCallOutput(callID, ExecutionError(callID, StatusError, "tool_execution_failed", "Tool "+name+" failed during execution"))
		}
		arguments, err = json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode normalized arguments for tool %s: %w", name, err)
		}
	}
	issues, err := validateRecovered(tool.Validator, arguments)
	if err != nil {
		return nil, fmt.Errorf("validate arguments for tool %s: %w", name, err)
	}
	if len(issues) > 0 {
		// The report is persisted and re-sent on every round, so it must not
		// grow with the arguments: keep a sample and count the rest.
		issues = boundValidationIssues(issues)
		for index, issue := range issues {
			if property, _ := issue.Params["additionalProperty"].(string); property != "" {
				if hint, ok := tool.RejectedProperties[property]; ok {
					issues[index].Message = hint
				}
			}
		}
		return encodeFunctionCallOutput(callID, SchemaError(callID, "Invalid arguments for "+name, issues, tool.ExampleOperation))
	}
	if !isObject {
		return encodeFunctionCallOutput(callID, SchemaError(callID, "Invalid arguments for "+name, []map[string]string{{
			"keyword": "type",
			"message": "arguments must be a JSON object",
		}}, tool.ExampleOperation))
	}
	return c.runTool(ctx, callID, tool, object, execution)
}

// runTool runs a tool whose arguments have passed the checks (or that is refused) and encodes its
// output as a function_call_output item.
func (c *Catalog) runTool(ctx context.Context, callID string, tool Tool, object map[string]any, execution ExecutionContext) (json.RawMessage, error) {
	name := tool.Definition.Name
	var err error
	var output json.RawMessage
	if tool.ExecuteOutput != nil {
		output, err = executeOutputRecovered(tool, ctx, object, execution)
		if err != nil || !json.Valid(output) {
			logToolFailure(name, callID, err)
			output, _ = json.Marshal(ExecutionError(callID, StatusError, "tool_execution_failed", "Tool "+name+" failed during execution"))
		}
	} else {
		result, executeErr := executeRecovered(tool, ctx, object, execution)
		if executeErr != nil {
			logToolFailure(name, callID, executeErr)
			result = ExecutionError(callID, StatusError, "tool_execution_failed", "Tool "+name+" failed during execution")
		}
		output, err = json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("encode function call output for tool %s: %w", name, err)
		}
	}
	return encodeFunctionCallOutputValue(callID, output)
}

// A call runs on a batch worker goroutine that nothing above can recover, so a
// panicking tool would end the whole process. These turn it into an error.
func executeRecovered(tool Tool, ctx context.Context, arguments map[string]any, execution ExecutionContext) (result string, err error) {
	defer recoverToolPanic(&err)
	return tool.Execute(ctx, arguments, execution)
}

func executeOutputRecovered(tool Tool, ctx context.Context, arguments map[string]any, execution ExecutionContext) (output json.RawMessage, err error) {
	defer recoverToolPanic(&err)
	return tool.ExecuteOutput(ctx, arguments, execution)
}

// normalizeRecovered runs the tool's own normalizer inside the panic boundary,
// for the same reason as executeRecovered: it is arbitrary tool code, and one
// caller (the remote API goroutine) has no recover of its own.
func normalizeRecovered(tool Tool, object map[string]any) (normalized map[string]any, err error) {
	defer recoverToolPanic(&err)
	return tool.NormalizeArguments(object), nil
}

// validateRecovered validates inside the same boundary, so a panicking schema
// library becomes an error instead of ending the process.
func validateRecovered(validator *SchemaValidator, arguments []byte) (issues []ValidationIssue, err error) {
	defer recoverToolPanic(&err)
	return validator.Validate(arguments)
}

func recoverToolPanic(err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("tool panicked: %v", recovered)
	}
}

// logToolFailure records why a call failed. ExecuteFunctionCall replaces the
// error with a generic model-facing result, so without this line a panicking
// tool would leave no trace anywhere.
func logToolFailure(name, callID string, err error) {
	attributes := []any{"tool", name, "call_id", callID}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	slog.Error("tool call failed during execution", attributes...)
}

func encodeFunctionCallOutput(callID, output string) (json.RawMessage, error) {
	encodedOutput, err := MarshalJSON(output)
	if err != nil {
		return nil, fmt.Errorf("encode function call output value: %w", err)
	}
	return encodeFunctionCallOutputValue(callID, encodedOutput)
}

// encodeFunctionCallOutputValue builds the envelope with the shared marshaler, so the
// escaping done on the value (raw <, >, & and the U+2028/U+2029 separators) survives it:
// json.Marshal would compact the RawMessage with HTML escaping on and undo it.
func encodeFunctionCallOutputValue(callID string, output json.RawMessage) (json.RawMessage, error) {
	encoded, err := MarshalJSON(struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}{Type: "function_call_output", CallID: callID, Output: output})
	if err != nil {
		return nil, fmt.Errorf("encode function call output: %w", err)
	}
	return encoded, nil
}

func jsonschemaDecode(raw []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// refusedCallArguments decodes the arguments of a refused call for its wrappers. Arguments that
// are not a JSON object of a JSON string give an empty map, and they are never validated.
func refusedCallArguments(raw json.RawMessage) map[string]any {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return map[string]any{}
	}
	decoded, err := jsonschemaDecode([]byte(text))
	if err != nil {
		return map[string]any{}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

// DecodeArguments decodes a call's JSON arguments the way the catalog does before a tool
// runs (numbers as json.Number), and fails unless they are one JSON object.
func DecodeArguments(raw []byte) (map[string]any, error) {
	decoded, err := jsonschemaDecode(raw)
	if err != nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}
