package native

import "context"

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

// External returns the run with the id, or false when this manager does not hold it.
func (m *SubagentManager) External(id string) (*ExternalRun, bool) {
	run := m.byID(id)
	if run == nil {
		return nil, false
	}
	return &ExternalRun{run: run}, true
}

// Done closes when the run has ended.
func (e *ExternalRun) Done() <-chan struct{} { return e.run.done }

// Stop asks the run to end; it does not wait.
func (e *ExternalRun) Stop(reason string) { e.run.stopWith(reason) }

// Outcome is the final state (done, failed, stopped, timed_out or lost), the answer or the
// partial text the child left, and the reason. It is meaningful once Done is closed.
func (e *ExternalRun) Outcome() (state, text, reason string) {
	view := e.run.view()
	return view.state, view.result.Text, view.reason
}
