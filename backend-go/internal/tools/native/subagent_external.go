package native

import (
	"context"

	"jad/backend-go/internal/tools"
)

type externalRunKey struct{}

// WithExternalRun marks a subagent call as started on behalf of the remote API: the run is
// not an async handle (nobody in this process collects it), the call waits for its end, and
// observe gets the run as soon as it exists.
func WithExternalRun(ctx context.Context, observe func(*ExternalRun)) context.Context {
	return context.WithValue(ctx, externalRunKey{}, observe)
}

func externalObserver(ctx context.Context) func(*ExternalRun) {
	observe, _ := ctx.Value(externalRunKey{}).(func(*ExternalRun))
	return observe
}

// ExternalRun is a read-only view of one sub-agent run for a caller that is not a tool call
// (the remote API): it waits for the end, reads the outcome and stops the run.
type ExternalRun struct {
	run *subagentRun
}

// Identity names the run for as long as the manager can hold it. Unlike ID, it is never drawn again.
func (e *ExternalRun) Identity() any { return e.run }

// ID is the run's id on this host ("sub_…").
func (e *ExternalRun) ID() string { return e.run.id }

// Done closes when the run has ended.
func (e *ExternalRun) Done() <-chan struct{} { return e.run.done }

// Stop asks the run to end; it does not wait.
func (e *ExternalRun) Stop(reason string) { e.run.stopWith(reason) }

// Outcome is the final state (done, failed, stopped, timed_out or lost), the answer or the
// partial text the child left, and the reason. It is meaningful once Done is closed. The text is
// whole: the sub-agent's own result_max_bytes does not cut it here, because the remote API
// bounds what it serves itself and hands the rest out in parts.
func (e *ExternalRun) Outcome() (state, text, reason string) {
	view := e.run.view()
	return view.state, view.fullText(), view.reason
}

// fullText is the answer of a run kept whole (an external run), else the cut one.
func (v subagentRunView) fullText() string {
	if v.full != "" {
		return v.full
	}
	return v.result.Text
}

// ResumeFacts is the run's resume facts: the run it continues from ("" for none) and the config
// note of a resume ("" when there is none).
func (e *ExternalRun) ResumeFacts() (resumedFrom, configNote string) {
	view := e.run.view()
	return view.resumedFrom, view.configNote
}

// WorktreeInformation are the worktree_* lines of a run that worked in a git worktree of its
// own; empty for another run. Meaningful once Done is closed.
func (e *ExternalRun) WorktreeInformation() []tools.Information {
	return worktreeInformation(e.run.view())
}
