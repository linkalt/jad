package tools

import (
	"context"
	"encoding/json"
)

type InteractionDecision string

const (
	InteractionAccept  InteractionDecision = "accept"
	InteractionUpdate  InteractionDecision = "update"
	InteractionDecline InteractionDecision = "decline"
)

type InteractionOutcome string

const (
	InteractionAnswered  InteractionOutcome = "answered"
	InteractionTimedOut  InteractionOutcome = "timed_out"
	InteractionCancelled InteractionOutcome = "cancelled"
)

type InteractionSource string

const (
	InteractionSourceUser    InteractionSource = "user"
	InteractionSourceTimeout InteractionSource = "timeout"
)

type InteractionIdentity struct {
	Tool       string
	ToolCallID string
}

type InteractionLimits struct {
	MaxDraftBytes int64 `json:"max_draft_bytes"`
	MaxDraftDepth int64 `json:"max_draft_depth"`
}

// InteractionKind says what a question is for. A draft is a proposal the user
// may edit; an approval is a yes or no on running one call and cannot be edited.
type InteractionKind string

const (
	// InteractionKindDraft is the zero value, so a question that sets no kind is
	// a draft and clients that predate approvals keep working.
	InteractionKindDraft    InteractionKind = ""
	InteractionKindApproval InteractionKind = "approval"
)

type InteractionQuestion struct {
	Kind         InteractionKind   `json:"kind,omitempty"`
	Draft        json.RawMessage   `json:"draft"`
	Title        string            `json:"title,omitempty"`
	TimeoutMS    int64             `json:"timeout_ms"`
	ExtendMS     int64             `json:"extend_ms"`
	MaxTimeoutMS int64             `json:"max_timeout_ms"`
	Limits       InteractionLimits `json:"limits"`
}

type InteractionAnswer struct {
	Decision InteractionDecision `json:"decision"`
	Draft    json.RawMessage     `json:"draft,omitempty"`
	Reason   *string             `json:"reason,omitempty"`
	Outcome  InteractionOutcome  `json:"outcome"`
	Source   InteractionSource   `json:"source,omitempty"`
}

type PendingInteraction struct {
	ID           string          `json:"id"`
	Kind         InteractionKind `json:"kind,omitempty"`
	Tool         string          `json:"tool"`
	ToolCallID   string          `json:"tool_call_id"`
	Draft        json.RawMessage `json:"draft"`
	Title        string          `json:"title,omitempty"`
	CreatedAt    string          `json:"created_at"`
	ExpiresAt    string          `json:"expires_at"`
	MaxExpiresAt string          `json:"max_expires_at"`
	ExtendMS     int64           `json:"extend_ms"`
}

// Clone returns a copy that shares no memory with the receiver.
func (pending PendingInteraction) Clone() PendingInteraction {
	pending.Draft = append([]byte(nil), pending.Draft...)
	return pending
}

// InteractionEnd says why a question left the run. It is published once, with
// the event that clears the question, so a client that only sees the question
// vanish does not have to guess whether it was answered here, answered in
// another window, timed out or cut off by the run ending. Decision is set only
// for an answer: a timeout or a stop is not a decision anybody made.
type InteractionEnd struct {
	InteractionID string              `json:"interaction_id"`
	Kind          string              `json:"kind"`
	Tool          string              `json:"tool"`
	Outcome       InteractionOutcome  `json:"outcome"`
	Decision      InteractionDecision `json:"decision,omitempty"`
}

type InteractionReply struct {
	Decision InteractionDecision `json:"decision"`
	Draft    json.RawMessage     `json:"draft,omitempty"`
	Reason   *string             `json:"reason,omitempty"`
}

type InteractionStatus string

const (
	InteractionStatusAnswered InteractionStatus = "answered"
	InteractionStatusUnknown  InteractionStatus = "unknown"
	InteractionStatusExpired  InteractionStatus = "expired"
)

type InteractionExtendStatus string

const (
	InteractionExtendExtended InteractionExtendStatus = "extended"
	InteractionExtendUnknown  InteractionExtendStatus = "unknown"
	InteractionExtendExpired  InteractionExtendStatus = "expired"
	InteractionExtendAtMax    InteractionExtendStatus = "at_max"
)

type InteractionHandler func(context.Context, InteractionIdentity, InteractionQuestion) (InteractionAnswer, error)

type InteractionResolvedHandler func(InteractionIdentity, InteractionAnswer, json.RawMessage) error
