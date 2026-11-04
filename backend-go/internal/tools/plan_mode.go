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
// that would change files is refused with status forbidden and error_code plan_mode.
// An allow entry keeps a tool whole, with no such refusal. The receiver is unchanged.
func (c *Catalog) PlanMode(allow []string) (*Catalog, error) {
	if c == nil {
		return nil, nil
	}
	kept := make([]Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		switch {
		case AnyEntryMatches(allow, tool.Definition):
			kept = append(kept, tool)
		case tool.ReadOnly && tool.MayChangeFiles != nil:
			kept = append(kept, planModeGuard(tool))
		case tool.ReadOnly:
			kept = append(kept, tool)
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

// planModeGuard wraps a tool whose calls may or may not change files, so the ones that
// would are refused before the tool runs.
func planModeGuard(tool Tool) Tool {
	mayChange := tool.MayChangeFiles
	refusal := func(execution ExecutionContext) string {
		return ExecutionError(execution.CallID, StatusForbidden, PlanModeErrorCode,
			"Plan mode: this call could change files, so it is not run in this turn. Use a read-only call, or put the change in the plan.")
	}
	if execute := tool.Execute; execute != nil {
		tool.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
			if mayChange(arguments) {
				return refusal(execution), nil
			}
			return execute(ctx, arguments, execution)
		}
	}
	if execute := tool.ExecuteOutput; execute != nil {
		tool.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
			if mayChange(arguments) {
				return MarshalJSON(refusal(execution))
			}
			return execute(ctx, arguments, execution)
		}
	}
	return tool
}
