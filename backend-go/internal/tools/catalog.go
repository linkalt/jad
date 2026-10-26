package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	CallID        string
	ToolName      string
	WorkspaceRoot string
	SessionDir    string
	// SessionKey identifies the session and profile the call belongs to. A tool that
	// keeps state beyond one call uses it to scope that state, so a user stop or a
	// reset of this session reaches only its own state.
	SessionKey            string
	RequestInteraction    InteractionHandler
	OnInteractionResolved InteractionResolvedHandler
	// RawArguments retains the provider's original JSON text for tools whose
	// persisted format depends on object insertion order.
	RawArguments json.RawMessage
	// PathScope narrows the paths a file-changing tool may touch for this call, on top
	// of the tool's own policy. The sub-agent tool sets it for an agent with paths; nil
	// means no further narrowing.
	PathScope *PathScope
	// OnFileChange, when set, is told about each file a native write or edit changed
	// (path as the tool reports it, action created, updated or edited). A call that
	// left the file as it was, or failed, is not reported. The sub-agent tool uses it
	// to list what a child changed.
	OnFileChange func(path, action string)
	// ProcessOwner, when set, limits the process tool to the processes this owner
	// started: it lists, reads and stops only those, and what it starts belongs to the
	// owner. The sub-agent tool sets it to the run's id for a child that lists process,
	// so a child cannot see or stop the parent's processes. Empty means no limit.
	ProcessOwner string
	// ProcessOwnerLimit is the most processes ProcessOwner may have running at once;
	// zero means no limit beyond the session's own. It only counts with ProcessOwner.
	ProcessOwnerLimit int64
	// ProcessAsync, with ProcessOwner, makes the child's processes handles of the async
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
	// ReadOnly marks a tool that never changes state and depends on none, so its
	// calls may run beside other calls of the same response. MayChangeFiles can
	// still make one call of such a tool count as a change.
	ReadOnly bool
	// MayChangeFiles, when set, decides per call whether a call of this tool can
	// change files, for a tool whose effect depends on its arguments (the subagent
	// tool, whose agent may or may not be allowed to write). A true answer makes the
	// call take the checkpoint and run in order, like a write. Nil means the
	// ReadOnly flag alone decides.
	MayChangeFiles     func(arguments map[string]any) bool
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
}

// NewCatalog validates the tool set and rejects duplicate exposed names.
func NewCatalog(tools []Tool) (*Catalog, error) {
	catalog := &Catalog{tools: append([]Tool(nil), tools...), byName: make(map[string]Tool, len(tools))}
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

// ResponseTools returns the function-tool objects accepted by the Responses
// API. Native tool definitions are non-strict, as in the TypeScript backend.
func (c *Catalog) ResponseTools() ([]json.RawMessage, error) {
	if c == nil {
		return []json.RawMessage{}, nil
	}
	tools := make([]json.RawMessage, 0, len(c.tools))
	for _, tool := range c.tools {
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
		object = tool.NormalizeArguments(object)
		arguments, err = json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode normalized arguments for tool %s: %w", name, err)
		}
	}
	issues, err := tool.Validator.Validate(arguments)
	if err != nil {
		return nil, fmt.Errorf("validate arguments for tool %s: %w", name, err)
	}
	if len(issues) > 0 {
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

	var output json.RawMessage
	if tool.ExecuteOutput != nil {
		output, err = executeOutputRecovered(tool, ctx, object, execution)
		if err != nil || !json.Valid(output) {
			output, _ = json.Marshal(ExecutionError(callID, StatusError, "tool_execution_failed", "Tool "+name+" failed during execution"))
		}
	} else {
		result, executeErr := executeRecovered(tool, ctx, object, execution)
		if executeErr != nil {
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

func recoverToolPanic(err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("tool panicked: %v", recovered)
	}
}

func encodeFunctionCallOutput(callID, output string) (json.RawMessage, error) {
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode function call output value: %w", err)
	}
	return encodeFunctionCallOutputValue(callID, encodedOutput)
}

func encodeFunctionCallOutputValue(callID string, output json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(struct {
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
