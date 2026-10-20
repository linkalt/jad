package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/tools/native"
)

// profileMessenger sends the message of the profile_message tool to another profile of
// the same session as an ordinary run of that profile: the run takes the profile's
// transcript, shows in the run registry, streams and stops like a user's message.
type profileMessenger struct {
	resolver *Resolver
}

// SetProfileRuns sets the port that starts profile_message runs. The HTTP handler exists
// only after the resolver, so the service sets it once the handler is built.
func (r *Resolver) SetProfileRuns(starter httpapi.ProfileRunStarter) {
	if starter == nil {
		r.profileRuns.Store(nil)
		return
	}
	r.profileRuns.Store(&starter)
}

func (r *Resolver) profileRunStarter() httpapi.ProfileRunStarter {
	if starter := r.profileRuns.Load(); starter != nil {
		return *starter
	}
	return nil
}

// callerMessagesPathKey carries the messages path of the transcript a run writes, so a
// tool running inside it knows which transcript the caller holds.
type callerMessagesPathKey struct{}

// Message sends one message to target and returns its final answer. The target run is the
// target's own: the caller's transcript is held by the caller's running turn and is not
// touched. The call ends when the target run ends, when ctx ends (the target is stopped
// then), or at once when the target is busy.
func (m profileMessenger) Message(ctx context.Context, ownerKey, target, message string, busyWait time.Duration) (string, error) {
	reference, caller, found := strings.Cut(ownerKey, "\x00")
	if !found {
		return "", errors.New("profile message needs the calling session key")
	}
	if target == caller {
		return "", native.ErrProfileCycle
	}
	callerMessagesPath, _ := ctx.Value(callerMessagesPathKey{}).(string)
	if callerMessagesPath == "" {
		return "", errors.New("profile message needs the calling transcript path")
	}
	starter := m.resolver.profileRunStarter()
	if starter == nil {
		return "", errors.New("profile messages are unavailable: the run registry is not set")
	}
	resolved, err := m.resolver.Resolve(ctx, reference, target, "")
	if err != nil {
		return "", err
	}
	canonical := resolved.Profile
	if canonical == caller {
		return "", native.ErrProfileCycle
	}
	// A run started by profile_message may not message further: the chain has depth one.
	if _, nested := starter.RunningProfileRunCaller(resolved.Reference, caller); nested {
		return "", native.ErrProfileCycle
	}
	// Another profile name over the caller's transcript would wait for the transcript
	// the caller holds, so a retry can never succeed; refuse it. A running target is
	// different: the start reports it as busy.
	if sameTranscript(callerMessagesPath, resolved.Runner) {
		return "", native.ErrProfileSameTranscript
	}
	var lastText atomic.Value
	run, err := starter.StartProfileRun(ctx, httpapi.ProfileRunRequest{
		Reference: resolved.Reference,
		Profile:   canonical,
		Caller:    caller,
		Message:   message,
		BusyWait:  busyWait,
		OnCommitted: func(event json.RawMessage) {
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				lastText.Store(text)
			}
		},
	})
	if err != nil {
		if errors.Is(err, httpapi.ErrSessionBusy) {
			heldBy, _ := starter.RunningProfileRunCaller(resolved.Reference, canonical)
			return "", &native.ProfileBusyError{HeldBy: heldBy}
		}
		return "", err
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		run.Stop()
		return "", ctx.Err()
	}
	switch run.Status() {
	case "completed":
	case "aborted":
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", native.ErrProfileStopped
	default:
		return "", errors.New("profile run failed")
	}
	answer := native.SubagentAnswerText(run.Final())
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
