package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// SetProfileRuns sets the registry that profile_message runs are listed in. The HTTP
// handler exists only after the resolver, so the service sets it once the handler is built.
func (r *Resolver) SetProfileRuns(tracker httpapi.ProfileRunTracker) {
	if tracker == nil {
		r.profileRuns.Store(nil)
		return
	}
	r.profileRuns.Store(&tracker)
}

func (r *Resolver) profileRunTracker() httpapi.ProfileRunTracker {
	if tracker := r.profileRuns.Load(); tracker != nil {
		return *tracker
	}
	return nil
}

type profileChainKey struct{}

// callerMessagesPathKey carries the messages path of the transcript a run writes, so a
// tool running inside it knows which transcript the caller holds.
type callerMessagesPathKey struct{}

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
	callerMessagesPath, _ := ctx.Value(callerMessagesPathKey{}).(string)
	if callerMessagesPath == "" {
		return "", errors.New("profile message needs the calling transcript path")
	}
	resolved, err := m.resolver.Resolve(ctx, reference, target, "")
	if err != nil {
		return "", err
	}
	canonical := resolved.Profile
	if canonical == caller {
		return "", native.ErrProfileCycle
	}
	// Another profile name over the caller's transcript would wait for the run mutex
	// the caller holds, so a retry can never succeed; refuse it. A running target is
	// different: TryLockSession reports it as busy.
	if sameTranscript(callerMessagesPath, resolved.Runner) {
		return "", native.ErrProfileSameTranscript
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
	runCtx := ctx
	var stopCtx context.Context
	status := "failed"
	if tracker := m.resolver.profileRunTracker(); tracker != nil {
		var finish func(string)
		stopCtx, finish, err = tracker.BeginProfileRun(resolved.Reference, canonical, turnID, caller)
		if err != nil {
			var statusErr *httpapi.StatusError
			if errors.As(err, &statusErr) && (statusErr.Code == http.StatusConflict || statusErr.Code == http.StatusTooManyRequests) {
				return "", native.ErrProfileBusy
			}
			return "", err
		}
		defer func() { finish(status) }()
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(stopCtx, cancel)()
	}
	var accepted *session.PersistedInput
	var lastText atomic.Value
	chainCtx := context.WithValue(runCtx, profileChainKey{}, []string{caller, canonical})
	result, err := resolved.Runner.Run(chainCtx, input, session.RunOptions{
		TurnID:          turnID,
		OnInputAccepted: func(input session.PersistedInput) { accepted = &input },
		OnResponseCommitted: func(event json.RawMessage, _ transcript.CommitResult) {
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				lastText.Store(text)
			}
		},
		ToolExecution: tools.ExecutionContext{
			RequestApproval: func(context.Context, tools.ApprovalCall) tools.ApprovalVerdict {
				return tools.ApprovalVerdict{Allowed: true}
			},
		},
	})
	if err != nil {
		if stopCtx != nil && stopCtx.Err() != nil && ctx.Err() == nil {
			status = "aborted"
			return "", native.ErrProfileStopped
		}
		if ctx.Err() != nil {
			status = "aborted"
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			m.resolver.recordProfileRunFailure(resolved.Runner, accepted, turnID)
		}
		return "", err
	}
	status = "completed"
	answer := native.SubagentAnswerText(result.Event)
	if strings.TrimSpace(answer) == "" {
		answer, _ = lastText.Load().(string)
	}
	if strings.TrimSpace(answer) == "" {
		return "", native.ErrProfileNoAnswer
	}
	return answer, nil
}

// sameTranscript reports whether target runs over the transcript at callerMessagesPath.
// Paths are compared as the same file (cleaned and symlink-resolved); the store cache
// itself is keyed by the unresolved cleaned path.
func sameTranscript(callerMessagesPath string, target httpapi.RunSession) bool {
	bound, ok := target.(boundRunner)
	return ok && callerMessagesPath != "" && canonicalPath(bound.config.MessagesPath) == canonicalPath(callerMessagesPath)
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(parent, filepath.Base(path))
	}
	return path
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
