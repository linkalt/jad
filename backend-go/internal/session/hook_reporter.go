package session

import "context"

type hookReporterKey struct{}

// WithHookReporter returns ctx carrying report, the OnHookReport of the run ctx belongs to, so
// a tool of the run (the subagent tool) can send the hook reports it causes to that run's
// stream. A nil report returns ctx unchanged.
func WithHookReporter(ctx context.Context, report func(HookReport)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, hookReporterKey{}, report)
}

// HookReporterFrom returns the hook reporter of ctx, or nil.
func HookReporterFrom(ctx context.Context) func(HookReport) {
	report, _ := ctx.Value(hookReporterKey{}).(func(HookReport))
	return report
}
