package checkpoint

import (
	"encoding/json"

	"example.com/agent-harness/backend-go/internal/tools"
)

// CallChangesWorkspace reports whether a call can change files or start something
// that can; reads never take a checkpoint. source is the tool's source ("native"
// or "mcp"; empty for an unknown name).
func CallChangesWorkspace(call json.RawMessage, source string, includeMCPTools bool) bool {
	if source == "mcp" {
		return includeMCPTools
	}
	var fields struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(call, &fields) != nil {
		return false
	}
	switch fields.Name {
	case "write", "edit", "bash":
		return true
	case "process":
		var text string
		if json.Unmarshal(fields.Arguments, &text) != nil {
			return false
		}
		var args map[string]any
		if json.Unmarshal([]byte(text), &args) != nil || args == nil {
			return false
		}
		return tools.CallNeedsApproval("process", args)
	}
	return false
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

func (r *Run) take() {
	if r.attempted {
		return
	}
	r.attempted = true
	r.id = r.service.Begin(r.turnID, r.sessionTurn)
}

// Start takes the checkpoint before the first provider request when the setting
// is run_start.
func (r *Run) Start() {
	if r != nil && r.service.SnapshotOn() == "run_start" {
		r.take()
	}
}

// BeforeCalls runs before the calls of one tool round; for first_mutation it
// takes the checkpoint at the first changing call.
func (r *Run) BeforeCalls(changesWorkspace bool) {
	if r != nil && changesWorkspace && r.service.SnapshotOn() == "first_mutation" {
		r.take()
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
