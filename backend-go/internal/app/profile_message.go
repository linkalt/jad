package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

// profileMessenger runs the message of the profile_message tool as one run of another
// profile of the same session, in that profile's own transcript.
type profileMessenger struct {
	resolver *Resolver
}

type profileChainKey struct{}

// profileRunFailureText is what the run-error cache keeps, like the HTTP run's generic line.
const profileRunFailureText = "The run failed. Response details were withheld; check server logs for diagnostics."

type runErrorRecorder interface {
	RecordRunError(transcript.RunErrorEntry) error
}

// Message sends one message to target and returns its final answer. Only the target's
// transcript is locked: the caller's own is held by the caller's running turn.
func (m profileMessenger) Message(ctx context.Context, ownerKey, target, message string) (string, error) {
	reference, caller, found := strings.Cut(ownerKey, "\x00")
	if !found {
		return "", errors.New("profile message needs the calling session key")
	}
	if ctx.Value(profileChainKey{}) != nil || target == caller {
		return "", native.ErrProfileCycle
	}
	resolved, err := m.resolver.Resolve(ctx, reference, target, "")
	if err != nil {
		return "", err
	}
	canonical := resolved.Profile
	if canonical == caller {
		return "", native.ErrProfileCycle
	}
	// The same transcript path under another profile name shares one store, whose
	// run mutex the caller holds; waiting for it would never end.
	if bound, ok := resolved.Runner.(boundRunner); ok {
		if !bound.shared.runMu.TryLock() {
			return "", native.ErrProfileSameTranscript
		}
		bound.shared.runMu.Unlock()
	}
	release, err := m.resolver.TryLockSession(ctx, reference, canonical, "")
	if err != nil {
		if errors.Is(err, httpapi.ErrSessionBusy) {
			return "", native.ErrProfileBusy
		}
		return "", err
	}
	defer release()
	resolved, err = m.resolver.Resolve(ctx, reference, canonical, "")
	if err != nil {
		return "", err
	}
	input, err := json.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("encode profile message: %w", err)
	}
	turnID, err := newProfileTurnID()
	if err != nil {
		return "", err
	}
	var accepted *session.PersistedInput
	chainCtx := context.WithValue(ctx, profileChainKey{}, []string{caller, canonical})
	result, err := resolved.Runner.Run(chainCtx, input, session.RunOptions{
		TurnID:          turnID,
		OnInputAccepted: func(input session.PersistedInput) { accepted = &input },
		ToolExecution: tools.ExecutionContext{
			RequestApproval: func(context.Context, tools.ApprovalCall) tools.ApprovalVerdict {
				return tools.ApprovalVerdict{Allowed: true}
			},
		},
	})
	if err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) {
			m.resolver.recordProfileRunFailure(resolved.Runner, accepted, turnID)
		}
		return "", err
	}
	return native.SubagentAnswerText(result.Event), nil
}

func (r *Resolver) recordProfileRunFailure(runner httpapi.RunSession, accepted *session.PersistedInput, turnID string) {
	recorder, ok := runner.(runErrorRecorder)
	if !ok || accepted == nil {
		return
	}
	entry := transcript.RunErrorEntry{
		Turn:    accepted.SessionTurn,
		TurnID:  turnID,
		At:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Message: profileRunFailureText,
	}
	if accepted.HasMessage {
		entry.MessageID = fmt.Sprintf("session-user-o%d", accepted.MessageOffset)
	}
	if err := recorder.RecordRunError(entry); err != nil && r.logger != nil {
		r.logger.Warn("failed to record run error", slog.Any("error", err))
	}
}

func newProfileTurnID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create profile turn id: %w", err)
	}
	return "profile-" + hex.EncodeToString(raw[:]), nil
}
