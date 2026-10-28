package httpapi

import (
	"log/slog"
	"runtime/debug"
	"slices"
	"time"

	"jad/backend-go/internal/logging"
)

// AsyncState is what a run stream tells a client about the session's async handles:
// counts and a bounded list of live ids, never summaries.
type AsyncState struct {
	Active int      `json:"active"`
	Unread int      `json:"unread"`
	IDs    []string `json:"ids"`
}

// AsyncStateSource is an optional resolver adapter. WatchSessionAsync calls notify (which
// must not block) whenever the session's handle counts may have changed, and returns a
// reader for the current state and a stop function.
type AsyncStateSource interface {
	WatchSessionAsync(reference, profile string, notify func()) (read func() AsyncState, stop func())
}

func (s AsyncState) equal(other AsyncState) bool {
	return s.Active == other.Active && s.Unread == other.Unread && slices.Equal(s.IDs, other.IDs)
}

// watchAsyncState publishes the session's async state on the run's stream while the run
// lasts: once at the start when there is anything to report, then on change, at most once
// per interval. The returned function stops the watcher and waits for it.
func (h *Handler) watchAsyncState(run *activeRun, reference, profile string) func() {
	source, ok := h.resolver.(AsyncStateSource)
	if !ok {
		return func() {}
	}
	wake := make(chan struct{}, 1)
	read, stopWatch := source.WatchSessionAsync(reference, profile, func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if read == nil {
		stopWatch()
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() {
			if recovered := recover(); recovered != nil {
				logging.Or(h.logger).Error("async state watcher panicked", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
			}
		}()
		interval := h.toolStreamInterval
		publish := func() {
			state := read()
			if state.IDs == nil {
				state.IDs = []string{}
			}
			run.setAsyncState(state)
		}
		publish()
		for {
			select {
			case <-done:
				return
			case <-wake:
			}
			publish()
			if interval <= 0 {
				continue
			}
			timer := time.NewTimer(interval)
			select {
			case <-done:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		stopWatch()
	}
}

// setAsyncState publishes the state when it differs from the last one published. A state
// with nothing to report is not published first, so a run of a session without handles
// emits no event. It is run state, not transcript.
func (r *activeRun) setAsyncState(state AsyncState) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.status != "running" || (r.asyncState == nil && state.Active == 0 && state.Unread == 0) ||
		(r.asyncState != nil && r.asyncState.equal(state)) {
		r.mu.Unlock()
		return
	}
	r.asyncState = &state
	event, err := makeSSEEvent("async_state", asyncStatePayload(r.turnID, state))
	if err != nil {
		r.mu.Unlock()
		return
	}
	for queue := range r.subscribers {
		if !queue.push(event, false) {
			delete(r.subscribers, queue)
		}
	}
	r.mu.Unlock()
}

func asyncStatePayload(turnID string, state AsyncState) map[string]any {
	return map[string]any{"turn_id": turnID, "active": state.Active, "unread": state.Unread, "ids": state.IDs}
}
