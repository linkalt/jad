package native

import (
	"context"
	"encoding/json"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

const (
	HookEventRunStart = "run_start"
	HookEventRunEnd   = "run_end"
	// HookEventPreCompact fires before a compaction attempt; its commands are reported to
	// the server log and cannot stop or change the compaction.
	HookEventPreCompact = "pre_compact"

	CompactTriggerAutomatic = "automatic"
	CompactTriggerManual    = "manual"

	RunOutcomeOK        = "ok"
	RunOutcomeError     = "error"
	RunOutcomeCancelled = "cancelled"
)

// RunEvent is the JSON a run_start, run_end or pre_compact command reads on stdin. Outcome
// and Error are set for run_end only; Trigger for pre_compact only.
type RunEvent struct {
	Event         string `json:"event"`
	Session       string `json:"session"`
	Profile       string `json:"profile"`
	TurnID        string `json:"turn_id,omitempty"`
	Trigger       string `json:"trigger,omitempty"`
	WorkspaceRoot string `json:"workspace_root"`
	Outcome       string `json:"outcome,omitempty"`
	Error         string `json:"error,omitempty"`
	ErrorCut      bool   `json:"error_cut,omitempty"`
}

// FireRunEvent runs the run_start, run_end or pre_compact commands in order and returns one report
// section per command that has something to say. The reports are for the server log: they
// never reach the model or the transcript, and a command never changes the run's result.
func FireRunEvent(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv string, event RunEvent) []string {
	if !hooks.RunEventsActive() {
		return nil
	}
	var list []sessionfs.RunEventHook
	switch event.Event {
	case HookEventRunStart:
		list = hooks.RunStart
	case HookEventRunEnd:
		list = hooks.RunEnd
	case HookEventPreCompact:
		list = hooks.PreCompact
	}
	if len(list) == 0 {
		return nil
	}
	event.Error, event.ErrorCut = cutUTF8(event.Error, hooks.MaxPayloadBytes/2)
	payload, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	var reports []string
	for _, hook := range list {
		section := runHookChild(ctx, hooks, apiKeyEnv, event.WorkspaceRoot, hookChild{
			name:          hook.Name,
			setting:       "hooks." + event.Event + "[" + hook.Name + "].argv",
			argv:          hook.Argv,
			cwd:           event.WorkspaceRoot,
			stdin:         string(payload),
			onFailureOnly: hook.OnFailureOnly,
		})
		if section != "" {
			reports = append(reports, section)
		}
	}
	return reports
}
