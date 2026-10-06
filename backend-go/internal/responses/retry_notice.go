package responses

import (
	"context"
	"time"
)

// RetryWait describes a provider request that failed and is about to be retried after a
// wait. Status is the provider's HTTP status, or 0 for a connection failure.
type RetryWait struct {
	Attempt    int
	MaxRetries int
	Status     int
	Delay      time.Duration
}

// RetryNotifier is told when a run starts waiting to retry the request that opens a
// response stream (a non-nil wait) and when that wait is over (nil): the stream opened,
// the request failed for good, or the run was stopped. It is called from the goroutine
// that opens the stream and must not block.
type RetryNotifier func(*RetryWait)

type retryNotifierKey struct{}

// WithRetryNotifier returns a context whose streams report retry waits to notify. The
// notifier travels in the context because the client that retries is shared by every
// run, while the run that should show the wait is known only to the caller.
func WithRetryNotifier(ctx context.Context, notify RetryNotifier) context.Context {
	if notify == nil {
		return ctx
	}
	return context.WithValue(ctx, retryNotifierKey{}, notify)
}

// RetryNotifierFrom returns the notifier carried by ctx, or nil.
func RetryNotifierFrom(ctx context.Context) RetryNotifier {
	notify, _ := ctx.Value(retryNotifierKey{}).(RetryNotifier)
	return notify
}
