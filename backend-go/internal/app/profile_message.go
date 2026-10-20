package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
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
func (m profileMessenger) Message(ctx context.Context, ownerKey, target, message string, busyWait time.Duration, compact bool) (string, error) {
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
	var beforeRun func(context.Context)
	if compact {
		runID, _ := ctx.Value(callerRunKey{}).(uint64)
		beforeRun = func(ctx context.Context) {
			if runID != 0 && !m.resolver.callerRuns.claim(runID, resolved.Reference+"\x00"+canonical) {
				return
			}
			compactTarget := m.resolver.compactProfile
			if compactTarget == nil {
				compactTarget = m.resolver.compactTarget
			}
			if err := compactTarget(ctx, resolved.Reference, canonical); err != nil {
				m.resolver.logger.Warn("profile_message compaction failed; sending the message anyway", "session", resolved.Reference, "profile", canonical, "error", err)
			}
		}
	}
	var lastText atomic.Value
	run, err := starter.StartProfileRun(ctx, httpapi.ProfileRunRequest{
		Reference: resolved.Reference,
		Profile:   canonical,
		Caller:    caller,
		Message:   message,
		BusyWait:  busyWait,
		BeforeRun: beforeRun,
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
		return "", &native.ProfileFailedError{Message: run.Failure()}
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

// compactTarget compacts the target's transcript with the target's own compaction
// settings. The caller holds the target's transcript, so no run or mutation can interleave.
func (r *Resolver) compactTarget(ctx context.Context, reference, profile string) error {
	resolved, err := r.ResolveMaintenance(ctx, reference, profile)
	if err != nil {
		return err
	}
	if resolved.Session == nil {
		return errors.New("session maintenance provider is not initialized")
	}
	_, err = resolved.Session.Compact(ctx)
	return err
}

// callerRunKey carries the id of the caller's run (one boundRunner.Run), so the
// messenger knows which targets that run already compacted.
type callerRunKey struct{}

// callerRunTargets remembers, per caller run, the targets profile_message already
// compacted. A run's entry is dropped when the run ends.
type callerRunTargets struct {
	mu   sync.Mutex
	runs map[uint64]map[string]struct{}
}

// claim reports whether target is not yet compacted in run, and marks it.
func (c *callerRunTargets) claim(run uint64, target string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, done := c.runs[run][target]; done {
		return false
	}
	if c.runs == nil {
		c.runs = map[uint64]map[string]struct{}{}
	}
	if c.runs[run] == nil {
		c.runs[run] = map[string]struct{}{}
	}
	c.runs[run][target] = struct{}{}
	return true
}

func (c *callerRunTargets) forget(run uint64) {
	c.mu.Lock()
	delete(c.runs, run)
	c.mu.Unlock()
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
