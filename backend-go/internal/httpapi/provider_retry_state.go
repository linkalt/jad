package httpapi

import (
	"time"

	"jad/backend-go/internal/responses"
)

// providerRetryWait is the wire form of a retry the run is waiting on, the `waiting`
// member of the `provider_retry` event. It matches the TypeScript backend's
// ProviderRetryWait: Status is null for a connection failure, and ResumeAt is when the
// wait ends, so a client can count down without knowing the server's clock.
type providerRetryWait struct {
	Attempt    int    `json:"attempt"`
	MaxRetries int    `json:"max_retries"`
	Status     *int   `json:"status"`
	WaitMS     int64  `json:"wait_ms"`
	ResumeAt   string `json:"resume_at"`
}

func newProviderRetryWait(wait *responses.RetryWait, now time.Time) *providerRetryWait {
	if wait == nil {
		return nil
	}
	var status *int
	if wait.Status > 0 {
		value := wait.Status
		status = &value
	}
	return &providerRetryWait{
		Attempt:    wait.Attempt,
		MaxRetries: wait.MaxRetries,
		Status:     status,
		WaitMS:     wait.Delay.Milliseconds(),
		ResumeAt:   now.Add(wait.Delay).UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// setProviderRetry publishes that the run is waiting to retry a provider request, or
// that the wait is over (nil). It is run state, not transcript: nothing is written for
// it, and a finished run keeps no wait.
func (r *activeRun) setProviderRetry(wait *responses.RetryWait) {
	if r == nil {
		return
	}
	next := newProviderRetryWait(wait, time.Now())
	r.mu.Lock()
	if r.status != "running" || (next == nil && r.providerRetry == nil) {
		r.mu.Unlock()
		return
	}
	r.providerRetry = next
	event, err := makeSSEEvent("provider_retry", map[string]any{"turn_id": r.turnID, "waiting": next})
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
