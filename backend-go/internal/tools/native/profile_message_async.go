package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

const asyncKindProfileMessage asyncmgr.Kind = "profile_message"

const profileMessageLabelBytes = 60

// attachProfileMessage registers the driver of kind profile_message once.
func (h *AsyncHost) attachProfileMessage() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.profileMessageSeen {
		return
	}
	h.profileMessageSeen = true
	_ = h.manager.AddDriver(&profileMessageDriver{host: h})
}

func (h *AsyncHost) profileMessageRun(id string) ProfileMessageRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.profileMessages[id]
}

func (h *AsyncHost) beginProfileMessage(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.profileStarting == nil {
		h.profileStarting = map[string]string{}
	}
	h.profileStarting[id] = ""
}

// setProfileMessageRun binds the run to its handle and returns the reason of a stop that
// came in before the run existed, empty when there was none.
func (h *AsyncHost) setProfileMessageRun(id string, run ProfileMessageRun) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.profileMessages == nil {
		h.profileMessages = map[string]ProfileMessageRun{}
	}
	h.profileMessages[id] = run
	reason := h.profileStarting[id]
	delete(h.profileStarting, id)
	return reason
}

func (h *AsyncHost) abandonProfileMessage(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.profileStarting, id)
}

// stopProfileMessage stops the run of a handle; a handle still starting is stopped as
// soon as its run exists.
func (h *AsyncHost) stopProfileMessage(id, reason string) {
	h.mu.Lock()
	run := h.profileMessages[id]
	if _, starting := h.profileStarting[id]; run == nil && starting {
		if h.profileStarting[id] == "" {
			h.profileStarting[id] = reason
			if reason == "" {
				h.profileStarting[id] = "stopped"
			}
		}
	}
	h.mu.Unlock()
	if run != nil {
		run.Stop(reason)
	}
}

func (h *AsyncHost) dropProfileMessageRun(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.profileMessages, id)
}

// runHandle sends the message as a handle of the async manager and blocks up to block_ms
// for its end. The handle outlives the call: the call ending stops the wait, never the
// message.
func (p *profileMessageTool) runHandle(ctx context.Context, callID, profile, message, owner string, plan ProfileMessagePlan, arguments map[string]any) (string, error) {
	host := p.host
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "spawn_failed", "The message id could not be created",
			tools.Information{Key: "system_error", Value: err.Error()}), nil
	}
	id := "pm_" + hex.EncodeToString(random)
	label, cut := cutUTF8(profile+": "+oneLine(message), profileMessageLabelBytes)
	if cut {
		label += "..."
	}
	host.beginProfileMessage(id)
	if err := host.Manager().Register(asyncmgr.Registration{ID: id, Owner: owner, Kind: asyncKindProfileMessage, Label: label, Queued: true}); err != nil {
		host.abandonProfileMessage(id)
		code := "spawn_failed"
		if errors.Is(err, asyncmgr.ErrCapacity) {
			code = "async_capacity_reached"
		}
		return tools.ExecutionError(callID, tools.StatusError, code, err.Error()), nil
	}
	run, err := plan.Start(ctx)
	if err != nil {
		host.abandonProfileMessage(id)
		manager := host.Manager()
		_, _ = manager.Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: boundedReason(err.Error())})
		manager.MarkRead(owner, id)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return p.renderFailure(callID, profile, err), nil
	}
	if reason := host.setProfileMessageRun(id, run); reason != "" {
		run.Stop(reason)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		select {
		case <-run.Started():
			host.Manager().MarkRunning(id)
		case <-run.Done():
		}
		<-run.Done()
		outcome := run.Outcome()
		host.dropProfileMessageRun(id)
		// The signal is queued before ended closes, so a call released by it finds it.
		_, _ = host.Manager().Complete(id, p.asyncResult(profile, outcome))
	}()

	var blockMs *int64
	if value, ok := safeInteger(arguments["block_ms"]); ok {
		blockMs = &value
	}
	timer := time.NewTimer(host.Config().BlockDuration(blockMs))
	defer timer.Stop()
	select {
	case <-ended:
		host.Manager().MarkRead(owner, id)
		return p.renderOutcome(callID, profile, run.Outcome())
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-ended:
		host.Manager().MarkRead(owner, id)
		return p.renderOutcome(callID, profile, run.Outcome())
	default:
	}
	state, note := "running", "is still working on it"
	if run.Queued() {
		select {
		case <-run.Started():
		default:
			state, note = "queued", "is busy, so the message waits its turn"
		}
	}
	text, err := tools.RenderToolResult(tools.ResultOptions{
		ID:     callID,
		Status: tools.StatusOK,
		Summary: fmt.Sprintf("Profile %s %s (message %s). Do not send the message again: use the async tool with id %s to wait for the answer (action wait), look at it (status) or stop it",
			profile, note, id, id),
		Information: []tools.Information{
			{Key: "run", Value: id},
			{Key: "state", Value: state},
		},
	})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The result could not be rendered"), nil
	}
	return text, nil
}

// profileMessageAsyncResult maps how a message ended to the unified result. The answer, or
// whatever final text the target left, is kept in the record, so it is delivered once even
// after the target moved on to another caller's task. With no text the summary is the
// failure itself, so a wait explains it.
func (p *profileMessageTool) asyncResult(profile string, outcome ProfileMessageOutcome) asyncmgr.Result {
	text, cut := cutUTF8(strings.TrimSpace(outcome.Text), int64(p.settings.ResultMaxBytes))
	result := asyncmgr.Result{Summary: text, Detail: text, Cut: cut, Bytes: int64(len(text)), Trace: outcome.Trace, Reason: outcome.Reason}
	if outcome.Err == nil {
		result.State, result.OK = asyncmgr.StateDone, true
		return result
	}
	code, message := p.failure(profile, outcome.Err)
	result.Code = code
	switch {
	case errors.Is(outcome.Err, ErrProfileStopped):
		result.State = asyncmgr.StateStopped
	case errors.Is(outcome.Err, ErrProfileTimeout):
		result.State = asyncmgr.StateTimedOut
	default:
		result.State = asyncmgr.StateFailed
	}
	if result.Reason == "" || result.State == asyncmgr.StateFailed {
		result.Reason = boundedReason(message)
	}
	if text == "" {
		result.Summary, result.Detail = message, message
	}
	return result
}

// profileMessageDriver is the async driver of kind profile_message: it stops a queued or
// running message. The message reports its own end.
type profileMessageDriver struct{ host *AsyncHost }

func (d *profileMessageDriver) Kind() asyncmgr.Kind { return asyncKindProfileMessage }

func (d *profileMessageDriver) Stop(id, reason string) {
	d.host.stopProfileMessage(id, reason)
}

func (d *profileMessageDriver) Detail(id string, maxBytes int) string {
	run := d.host.profileMessageRun(id)
	if run == nil {
		return ""
	}
	text, _ := cutUTF8(run.Status(), int64(maxBytes))
	return text
}
