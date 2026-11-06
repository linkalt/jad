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
	PlanMode      bool
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
}

// executionContextPerCall names the fields of ExecutionContext that describe one call
// rather than the run it belongs to. The catalog fills them for every call it dispatches,
// so ForNested clears them; every other field carries over to the nested call.
// TestForNestedKeepsEveryRunScopedField walks all the fields and fails when one is
// neither copied nor listed here.
var executionContextPerCall = []string{"CallID", "ToolName", "RawArguments", "BatchWaiting"}

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
	// A lazy tool is listed only once it is loaded. Nil means always listed. It does not affect
	// dispatch: a call by name still reaches the tool.
	Listed func() bool
	// Refused, when set and true for a call, skips the argument checks (normalize and schema) so the
	// call goes straight to the tool, whose own Execute refuses it. The refusal then runs inside the
	// tool's wrappers, so its event hooks fire like any other call. A lazy tool that is not loaded
	// refuses every call. Nil never refuses.
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
	// PlanGuarded is set by Catalog.PlanMode on a tool whose calls that could change files
	// are refused before they run (planModeGuard): no call of it changes files, so none
	// takes the checkpoint.
	PlanGuarded        bool
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

// ListedDefinitions returns the provider definitions of the tools that are listed now, in the
// registry's stable order: the same tools ResponseTools sends the model.
func (c *Catalog) ListedDefinitions() []Definition {
	if c == nil {
		return []Definition{}
	}
	definitions := make([]Definition, 0, len(c.tools))
	for _, tool := range c.tools {
		if tool.Listed != nil && !tool.Listed() {
			continue
		}
		definition := tool.Definition
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		definitions = append(definitions, definition)
	}
	return definitions
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

// IsUserDecisionCall reports whether the call must be resolved before any
// co-batched calls execute.
func (c *Catalog) IsUserDecisionCall(call json.RawMessage) bool {
	if c == nil {
		return false
	}
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
