package httpapi

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/webui"
)

// messageStreamEmitter coalesces append-only Web UI snapshots while preserving
// ordering around structural changes. The latest cumulative message is retained
// so publishMessage can emit one equivalent frontend delta when the window ends.
type messageStreamEmitter struct {
	mu           sync.Mutex
	interval     time.Duration
	publish      func(webui.Message) error
	pending      *webui.Message
	pendingIndex int
	lastEmitted  *webui.Message
	lastEmitAt   time.Time
	timer        *time.Timer
}

func newMessageStreamEmitter(interval time.Duration, publish func(webui.Message) error) *messageStreamEmitter {
	return &messageStreamEmitter{interval: interval, publish: publish}
}

func (e *messageStreamEmitter) push(message webui.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.interval <= 0 {
		if err := e.flushPendingLocked(); err != nil {
			return err
		}
		return e.emitLocked(message)
	}

	previous := e.pending
	if previous == nil {
		previous = e.lastEmitted
	}
	partIndex, appendOnly := appendOnlyPartIndex(previous, message)
	if !appendOnly {
		if err := e.flushPendingLocked(); err != nil {
			return err
		}
		return e.emitLocked(message)
	}

	if e.pending != nil {
		if e.pendingIndex != partIndex {
			if err := e.flushPendingLocked(); err != nil {
				return err
			}
			return e.emitLocked(message)
		}
		latest := cloneStreamMessage(message)
		e.pending = &latest
		e.scheduleLocked()
		return nil
	}

	if e.timer == nil && (e.lastEmitAt.IsZero() || time.Since(e.lastEmitAt) >= e.interval) {
		return e.emitLocked(message)
	}
	latest := cloneStreamMessage(message)
	e.pending = &latest
	e.pendingIndex = partIndex
	e.scheduleLocked()
	return nil
}

func (e *messageStreamEmitter) flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopTimerLocked()
	return e.flushPendingLocked()
}

// discard drops a coalesced snapshot that a terminal full message supersedes.
func (e *messageStreamEmitter) discard() {
	e.mu.Lock()
	e.stopTimerLocked()
	e.pending = nil
	e.mu.Unlock()
}

func (e *messageStreamEmitter) scheduleLocked() {
	if e.timer != nil {
		return
	}
	wait := e.interval - time.Since(e.lastEmitAt)
	if e.lastEmitAt.IsZero() || wait < 0 {
		wait = 0
	}
	e.timer = time.AfterFunc(wait, func() {
		e.mu.Lock()
		e.timer = nil
		err := e.flushPendingLocked()
		e.mu.Unlock()
		if err != nil {
			log.Printf("[web-ui-run-cache] coalesced stream dispatch failed: %v", err)
		}
	})
}

func (e *messageStreamEmitter) flushPendingLocked() error {
	e.stopTimerLocked()
	pending := e.pending
	e.pending = nil
	if pending == nil {
		return nil
	}
	return e.emitLocked(*pending)
}

func (e *messageStreamEmitter) emitLocked(message webui.Message) error {
	latest := cloneStreamMessage(message)
	if e.publish != nil {
		if err := e.publish(latest); err != nil {
			return err
		}
	}
	e.lastEmitted = &latest
	e.lastEmitAt = time.Now()
	return nil
}

func (e *messageStreamEmitter) stopTimerLocked() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
}

func appendOnlyPartIndex(previous *webui.Message, next webui.Message) (int, bool) {
	if previous == nil {
		return 0, false
	}
	updates := webui.Increments(*previous, next)
	if len(updates) != 1 || updates[0].Name != "ui.delta" {
		return 0, false
	}
	data, ok := updates[0].Data.(map[string]any)
	if !ok {
		return 0, false
	}
	index, ok := data["index"].(int)
	if !ok {
		return 0, false
	}
	appendText, ok := data["append"].(string)
	return index, ok && appendText != ""
}

func cloneStreamMessage(message webui.Message) webui.Message {
	message.Parts = append([]webui.Part(nil), message.Parts...)
	for index := range message.Parts {
		if message.Parts[index].Arguments != nil {
			value := *message.Parts[index].Arguments
			message.Parts[index].Arguments = &value
		}
		if message.Parts[index].Truncated != nil {
			value := *message.Parts[index].Truncated
			message.Parts[index].Truncated = &value
		}
	}
	message.Steps = append([]webui.Step(nil), message.Steps...)
	if message.Cursor != nil {
		value := *message.Cursor
		message.Cursor = &value
	}
	if message.Pending != nil {
		value := *message.Pending
		value.Draft = append(json.RawMessage(nil), message.Pending.Draft...)
		message.Pending = &value
	}
	return message
}
