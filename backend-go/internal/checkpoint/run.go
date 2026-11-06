package checkpoint

import (
	"context"
	"encoding/json"
)

// CallChangesWorkspace reports whether a call can change files or start something
// that can; reads never take a checkpoint. source is the tool's source ("native"
// or "mcp"; empty for an unknown name). Every exec call counts, a read_only one too:
// a checkpoint is the undo of a run, so it is taken whenever a program runs. The calls
// a tool itself says may change files are counted by Catalog.CallMayChangeFiles.
func CallChangesWorkspace(call json.RawMessage, source string, includeMCPTools bool) bool {
	if source == "mcp" {
		return includeMCPTools
	}
	var fields struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	return fields.Name == "exec"
}

// Run is the checkpoint of one run: taken at most once, closed once. It is not
// safe for concurrent use; a run drives it from one goroutine.
type Run struct {
	service     *Service
	turnID      string
	sessionTurn int64
	id          string
	attempted   bool
}

// NewRun returns the hooks of one run, or nil when checkpoints are off.
func NewRun(service *Service, turnID string, sessionTurn int64) *Run {
	if service == nil || !service.Enabled() {
		return nil
	}
	return &Run{service: service, turnID: turnID, sessionTurn: sessionTurn}
}

func (r *Run) take(ctx context.Context) {
	if r.attempted {
		return
	}
	r.attempted = true
	r.id = r.service.Begin(ctx, r.turnID, r.sessionTurn)
}

// Start takes the checkpoint before the first provider request when the setting
// is run_start.
func (r *Run) Start(ctx context.Context) {
	if r != nil && r.service.SnapshotOn() == "run_start" {
		r.take(ctx)
	}
}

// BeforeCalls runs before the calls of one tool round; for first_mutation it
// takes the checkpoint at the first changing call.
func (r *Run) BeforeCalls(ctx context.Context, changesWorkspace bool) {
	if r != nil && changesWorkspace && r.service.SnapshotOn() == "first_mutation" {
		r.take(ctx)
	}
}

// Finish records the end of the run, whatever its outcome.
func (r *Run) Finish() {
	if r != nil && r.id != "" {
		r.service.Finish(r.id)
	}
}

// IncludeMCPTools reports whether a call to an MCP tool counts as a change.
func (r *Run) IncludeMCPTools() bool {
	return r != nil && r.service.IncludeMCPTools()
}
