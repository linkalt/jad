package tools

import (
	"context"
	"encoding/json"
)

// PlanModeErrorCode is the error_code of a call a plan-mode run refuses.
const PlanModeErrorCode = "plan_mode"

// PlanMode returns the catalog a plan-mode run sees: the tools marked ReadOnly, and the
// ones an allow entry covers. A ReadOnly tool that decides per call whether it changes
// files (MayChangeFiles, such as a sub-agent that may write) stays listed, but a call
// that would change files is refused with status forbidden and error_code plan_mode. A
// tool with PlanModeRefusal is guarded by it instead, and its calls are scheduled as it says.
// An allow entry keeps a tool whole, with no such refusal. Every kept tool's calls carry
// ExecutionContext.PlanMode, so a sandboxed child starts with the project read-only. The
// receiver is unchanged.
func (c *Catalog) PlanMode(allow []string) (*Catalog, error) {
	if c == nil {
		return nil, nil
	}
	kept := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		switch {
		case AnyEntryMatches(allow, tool.Definition):
			kept = append(kept, planModeCalls(tool))
		case tool.ReadOnly && (tool.MayChangeFiles != nil || tool.PlanModeRefusal != nil):
			kept = append(kept, planModeCalls(planModeGuard(tool)))
		case tool.ReadOnly:
			kept = append(kept, planModeCalls(tool))
		}
	}
	return NewCatalog(kept)
}

// Only returns the catalog narrowed to the tools the entries cover (tools.allow entries:
// a native tool name, mcp:<server>:<tool> or mcp:<server>:*). An entry that covers no tool
// of the catalog narrows nothing more: the run asks for at most these tools. The receiver
// is unchanged.
func (c *Catalog) Only(entries []string) (*Catalog, error) {
	if c == nil {
		return nil, nil
	}
	kept := make([]Tool, 0, len(entries))
	for _, tool := range c.tools {
		if AnyEntryMatches(entries, tool.Definition) {
			kept = append(kept, tool)
		}
	}
	return NewCatalog(kept)
}

// planModeCalls marks every call of tool as a plan-mode call.
func planModeCalls(tool Tool) Tool {
	if execute := tool.Execute; execute != nil {
		tool.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
			execution.PlanMode = true
			return execute(ctx, arguments, execution)
		}
	}
	if execute := tool.ExecuteOutput; execute != nil {
		tool.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
			execution.PlanMode = true
			return execute(ctx, arguments, execution)
		}
	}
	return tool
}

// planModeGuard wraps a tool whose calls may or may not change files, so the ones that
// would are refused before the tool runs.
func planModeGuard(tool Tool) Tool {
	mayChange := tool.MayChangeFiles
	reason := func(arguments map[string]any, _ *ExecutionContext) string {
		if mayChange(arguments) {
			return PlanModeChangeRefusal
		}
		return ""
	}
	if tool.PlanModeRefusal != nil {
		reason = tool.PlanModeRefusal
		// A call the tool runs confined changes no file, so it is scheduled like a read;
		// one it refuses is refused before it starts.
		tool.MayChangeFiles = func(arguments map[string]any) bool { return reason(arguments, nil) != "" }
	}
	tool.PlanGuarded = true
	refusal := func(arguments map[string]any, execution ExecutionContext) string {
		if summary := reason(arguments, &execution); summary != "" {
			return ExecutionError(execution.CallID, StatusForbidden, PlanModeErrorCode, summary)
		}
		return ""
	}
	if execute := tool.Execute; execute != nil {
		tool.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
			if refused := refusal(arguments, execution); refused != "" {
				return refused, nil
			}
			execution.PlanGuarded = true
			return execute(ctx, arguments, execution)
		}
	}
	if execute := tool.ExecuteOutput; execute != nil {
		tool.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
			if refused := refusal(arguments, execution); refused != "" {
				return MarshalJSON(refused)
			}
			execution.PlanGuarded = true
			return execute(ctx, arguments, execution)
		}
	}
	return tool
}

// PlanModeChangeRefusal is why a plan-mode run refuses a call that could change files.
const PlanModeChangeRefusal = "Plan mode: this call could change files, so it is not run in this turn. Use a read-only call, or put the change in the plan."
