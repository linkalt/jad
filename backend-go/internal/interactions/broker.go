package interactions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/tools"
)

// Broker owns at most one user decision for a run and publishes pending state
// through the listener supplied by the run registry.
type Broker struct {
	mu       sync.Mutex
	entry    *pendingEntry
	onChange func(*tools.PendingInteraction)
	now      func() time.Time
}

type pendingEntry struct {
	pending    tools.PendingInteraction
	limits     tools.InteractionLimits
	expiresAt  time.Time
	maxExpires time.Time
	timer      *time.Timer
	settled    chan tools.InteractionAnswer
}

func NewBroker(onChange func(*tools.PendingInteraction)) *Broker {
	return &Broker{onChange: onChange, now: time.Now}
}

func (b *Broker) Pending() *tools.PendingInteraction {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entry == nil {
		return nil
	}
	pending := clonePending(b.entry.pending)
	return &pending
}

func (b *Broker) Limits() *tools.InteractionLimits {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entry == nil {
		return nil
	}
	limits := b.entry.limits
	return &limits
}

// Request publishes a question and waits for its answer, timeout, or run stop.
func (b *Broker) Request(ctx context.Context, identity tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
	if b == nil {
		return tools.InteractionAnswer{}, errors.New("interaction broker is not initialized")
	}
	if ctx == nil {
		return tools.InteractionAnswer{}, errors.New("interaction context is required")
	}
	if err := ctx.Err(); err != nil {
		return cancelledAnswer("The run was stopped before the draft was answered."), nil
	}
	if identity.Tool == "" || identity.ToolCallID == "" {
		return tools.InteractionAnswer{}, errors.New("interaction tool identity is required")
	}

	timeout := time.Duration(maxInt64(1, question.TimeoutMS)) * time.Millisecond
	maxTimeoutMS := maxInt64(question.TimeoutMS, question.MaxTimeoutMS)
	if maxTimeoutMS < 1 {
		maxTimeoutMS = 1
	}
	extendMS := maxInt64(0, question.ExtendMS)
	id, err := newInteractionID()
	if err != nil {
		return tools.InteractionAnswer{}, err
	}
	createdAt := b.now().UTC().Truncate(time.Millisecond)
	expiresAt := createdAt.Add(timeout)
	maxExpiresAt := createdAt.Add(time.Duration(maxTimeoutMS) * time.Millisecond)
	pending := tools.PendingInteraction{
		ID:           id,
		Tool:         identity.Tool,
		ToolCallID:   identity.ToolCallID,
		Draft:        append([]byte(nil), question.Draft...),
		Title:        question.Title,
		CreatedAt:    interactionTimestamp(createdAt),
		ExpiresAt:    interactionTimestamp(expiresAt),
		MaxExpiresAt: interactionTimestamp(maxExpiresAt),
		ExtendMS:     extendMS,
	}
	entry := &pendingEntry{
		pending:    pending,
		limits:     question.Limits,
		expiresAt:  expiresAt,
		maxExpires: maxExpiresAt,
		settled:    make(chan tools.InteractionAnswer, 1),
	}

	b.mu.Lock()
	if b.entry != nil {
		b.mu.Unlock()
		return tools.InteractionAnswer{}, errors.New("an interaction is already pending for this run")
	}
	b.entry = entry
	entry.timer = time.AfterFunc(maxDuration(0, expiresAt.Sub(b.now())), func() { b.timeout(entry) })
	b.mu.Unlock()
	b.notify(&pending)

	select {
	case answer := <-entry.settled:
		return answer, nil
	case <-ctx.Done():
		b.settle(entry, cancelledAnswer("The run was stopped before the draft was answered."), true)
		return <-entry.settled, nil
	}
}

func (b *Broker) Reply(id string, reply tools.InteractionReply) tools.InteractionStatus {
	if b == nil {
		return tools.InteractionStatusUnknown
	}
	b.mu.Lock()
	entry := b.entry
	if entry == nil || entry.pending.ID != id {
		b.mu.Unlock()
		return tools.InteractionStatusUnknown
	}
	if !b.now().Before(entry.expiresAt) {
		answer := timedOutAnswer(entry)
		b.settleLocked(entry, answer)
		b.mu.Unlock()
		b.notify(nil)
		return tools.InteractionStatusExpired
	}
	b.settleLocked(entry, tools.InteractionAnswer{
		Decision: reply.Decision,
		Draft:    append([]byte(nil), reply.Draft...),
		Reason:   cloneString(reply.Reason),
		Outcome:  tools.InteractionAnswered,
		Source:   tools.InteractionSourceUser,
	})
	b.mu.Unlock()
	b.notify(nil)
	return tools.InteractionStatusAnswered
}

func (b *Broker) Extend(id string) tools.InteractionExtendStatus {
	if b == nil {
		return tools.InteractionExtendUnknown
	}
	b.mu.Lock()
	entry := b.entry
	if entry == nil || entry.pending.ID != id {
		b.mu.Unlock()
		return tools.InteractionExtendUnknown
	}
	now := b.now()
	if !now.Before(entry.expiresAt) {
		b.settleLocked(entry, timedOutAnswer(entry))
		b.mu.Unlock()
		b.notify(nil)
		return tools.InteractionExtendExpired
	}
	if !entry.expiresAt.Before(entry.maxExpires) {
		b.mu.Unlock()
		return tools.InteractionExtendAtMax
	}
	next := entry.expiresAt.Add(time.Duration(entry.pending.ExtendMS) * time.Millisecond)
	if next.After(entry.maxExpires) {
		next = entry.maxExpires
	}
	entry.expiresAt = next
	entry.pending.ExpiresAt = interactionTimestamp(next)
	entry.timer.Reset(maxDuration(0, next.Sub(now)))
	pending := clonePending(entry.pending)
	b.mu.Unlock()
	b.notify(&pending)
	return tools.InteractionExtendExtended
}

// Cancel resolves a pending decision without inventing a user answer.
func (b *Broker) Cancel(reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	entry := b.entry
	if entry == nil {
		b.mu.Unlock()
		return
	}
	b.settleLocked(entry, cancelledAnswer(reason))
	b.mu.Unlock()
	b.notify(nil)
}

func (b *Broker) timeout(entry *pendingEntry) {
	b.mu.Lock()
	if b.entry != entry {
		b.mu.Unlock()
		return
	}
	now := b.now()
	if now.Before(entry.expiresAt) {
		entry.timer.Reset(entry.expiresAt.Sub(now))
		b.mu.Unlock()
		return
	}
	b.settleLocked(entry, timedOutAnswer(entry))
	b.mu.Unlock()
	b.notify(nil)
}

func (b *Broker) settle(entry *pendingEntry, answer tools.InteractionAnswer, notify bool) bool {
	b.mu.Lock()
	if b.entry != entry {
		b.mu.Unlock()
		return false
	}
	b.settleLocked(entry, answer)
	b.mu.Unlock()
	if notify {
		b.notify(nil)
	}
	return true
}

func (b *Broker) settleLocked(entry *pendingEntry, answer tools.InteractionAnswer) {
	if b.entry != entry {
		return
	}
	b.entry = nil
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.settled <- answer
}

func (b *Broker) notify(pending *tools.PendingInteraction) {
	if b.onChange == nil {
		return
	}
	if pending == nil {
		b.onChange(nil)
		return
	}
	copy := clonePending(*pending)
	b.onChange(&copy)
}

func timedOutAnswer(entry *pendingEntry) tools.InteractionAnswer {
	waited := entry.expiresAt.Sub(parseInteractionTimestamp(entry.pending.CreatedAt)).Milliseconds()
	if waited < 0 {
		waited = 0
	}
	reason := fmt.Sprintf("No answer within %s; the draft was not confirmed.", formatWait(waited))
	return tools.InteractionAnswer{
		Decision: tools.InteractionDecline,
		Outcome:  tools.InteractionTimedOut,
		Reason:   &reason,
		Source:   tools.InteractionSourceTimeout,
	}
}

func cancelledAnswer(reason string) tools.InteractionAnswer {
	return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionCancelled, Reason: &reason}
}

func formatWait(milliseconds int64) string {
	seconds := int64(math.Round(float64(milliseconds) / 1000))
	if seconds < 1 {
		seconds = 1
	}
	if seconds%60 == 0 {
		minutes := seconds / 60
		if minutes == 1 {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	if seconds == 1 {
		return "a second"
	}
	return fmt.Sprintf("%d seconds", seconds)
}

func newInteractionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create interaction ID: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded), nil
}

func interactionTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseInteractionTimestamp(value string) time.Time {
	parsed, _ := time.Parse("2006-01-02T15:04:05.000Z", value)
	return parsed
}

func clonePending(pending tools.PendingInteraction) tools.PendingInteraction {
	pending.Draft = append([]byte(nil), pending.Draft...)
	return pending
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}
