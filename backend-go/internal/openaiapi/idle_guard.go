package openaiapi

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// StreamIdleError reports a provider call that sent nothing for longer than the
// configured idle timeout. It is not a cancellation: the run was not stopped by the
// user, and it is not retried, because output may already have been shown.
type StreamIdleError struct {
	Timeout time.Duration
}

// IdleTimeout is the configured limit, for a client message that names only that number.
func (e *StreamIdleError) IdleTimeout() time.Duration { return e.Timeout }

func (e *StreamIdleError) Error() string {
	return fmt.Sprintf("provider stream was idle for %s (provider_stream.idle_timeout_ms)", e.Timeout)
}

// idleGuard cancels a child context when the provider stays silent. Reset arms it,
// Stop disarms it; work that is not the provider's silence (a slow event handler, a
// retry backoff) runs with the guard stopped. A nil guard does nothing.
type idleGuard struct {
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	fired   atomic.Bool
}

// newIdleGuard returns a child of parent and its guard. A timeout of zero or less
// returns parent itself and a nil guard.
func newIdleGuard(parent context.Context, timeout time.Duration) (context.Context, *idleGuard, context.CancelFunc) {
	if timeout <= 0 {
		return parent, nil, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	guard := &idleGuard{timeout: timeout, cancel: cancel}
	guard.timer = time.AfterFunc(timeout, func() {
		guard.fired.Store(true)
		cancel()
	})
	return ctx, guard, func() {
		guard.timer.Stop()
		cancel()
	}
}

func (g *idleGuard) Reset() {
	if g != nil {
		g.timer.Reset(g.timeout)
	}
}

func (g *idleGuard) Stop() {
	if g != nil {
		g.timer.Stop()
	}
}

// Err returns a StreamIdleError when the guard, not the caller, ended the call. A stop
// by the parent context is not idle.
func (g *idleGuard) Err(parent context.Context) *StreamIdleError {
	if g != nil && g.fired.Load() && parent.Err() == nil {
		return &StreamIdleError{Timeout: g.timeout}
	}
	return nil
}
