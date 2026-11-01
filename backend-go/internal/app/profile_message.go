package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
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

// Prepare checks a message to the target profile and returns the plan that sends it. Every
// check that can never succeed on a retry (cycle, chain, same transcript) fails here, before
// the caller reserves a handle or any run starts. The target run is the target's own: the
// caller's transcript is held by the caller's running turn and is not touched.
func (m profileMessenger) Prepare(ctx context.Context, request native.ProfileMessageRequest) (native.ProfileMessagePlan, error) {
	reference, caller, found := strings.Cut(request.OwnerKey, "\x00")
	if !found {
		return nil, errors.New("profile message needs the calling session key")
	}
	callerRef := reference
	if location, err := sessionfs.Resolve(m.resolver.roots, reference); err == nil {
		callerRef = location.Reference
	}
	target := request.Target
	targetReference := reference
	if request.Session != "" {
		var err error
		if targetReference, err = m.otherSession(request.Session, target); err != nil {
			return nil, err
		}
	}
	if request.Session == "" && target == caller {
		return nil, native.ErrProfileCycle
	}
	callerMessagesPath, _ := ctx.Value(callerMessagesPathKey{}).(string)
	if callerMessagesPath == "" && native.IsChildCall(ctx) {
		// A sub-agent runs on a context the manager owns, so it carries no transcript path.
		// Its caller is the session's profile, whose transcript the root run holds.
		callerRunner, err := m.resolver.Resolve(ctx, reference, caller, "")
		if err != nil {
			return nil, err
		}
		if bound, ok := callerRunner.Runner.(boundRunner); ok {
			callerMessagesPath = bound.config.MessagesPath
		}
	}
	if callerMessagesPath == "" {
		return nil, errors.New("profile message needs the calling transcript path")
	}
	starter := m.resolver.profileRunStarter()
	if starter == nil {
		return nil, errors.New("profile messages are unavailable: the run registry is not set")
	}
	resolved, err := m.resolver.Resolve(ctx, targetReference, target, "")
	if err != nil {
		return nil, err
	}
	canonical := resolved.Profile
	if resolved.Reference == callerRef && canonical == caller {
		return nil, native.ErrProfileCycle
	}
	// The caller's chain is the one its running run recorded when profile_message started
	// it, or just the caller when a user started it. The target may not be in it, and the
	// chain with the target added may not pass max_chain.
	// Entries carry their session, since profile names repeat across sessions.
	callerChain := []string{chainNode(callerRef, caller)}
	if recorded, nested := starter.RunningProfileRunChain(callerRef, caller); nested && len(recorded) > 0 {
		callerChain = recorded
	}
	node := chainNode(resolved.Reference, canonical)
	if slices.Contains(callerChain, node) {
		return nil, native.ErrProfileCycle
	}
	maxChain := native.ProfileChainMax(ctx)
	if maxChain < 1 {
		maxChain = sessionfs.DefaultProfileMessageMaxChain
	}
	chain := append(append([]string(nil), callerChain...), node)
	if len(chain) > maxChain {
		return nil, native.ErrProfileChainTooLong
	}
	// Another profile name over the caller's transcript would wait for the transcript
	// the caller holds, so a retry can never succeed; refuse it. A running target is
	// different: the start reports it as busy.
	if sameTranscript(callerMessagesPath, resolved.Runner) {
		return nil, native.ErrProfileSameTranscript
	}
	callerRun, _ := ctx.Value(callerRunKey{}).(uint64)
	plan := &profileMessagePlan{
		resolver:     m.resolver,
		starter:      starter,
		key:          resolved.Reference + "\x00" + canonical,
		reference:    resolved.Reference,
		target:       canonical,
		caller:       caller,
		callerRef:    callerRef,
		chain:        chain,
		message:      request.Message,
		timeout:      request.Timeout,
		queueWait:    request.QueueWait,
		maxQueued:    request.MaxQueued,
		blocking:     request.Blocking,
		hops:         native.RemoteHopsOf(ctx),
		systemPrompt: request.SystemPrompt,

		callerMessages: callerMessagesPath,
	}
	if bound, ok := resolved.Runner.(boundRunner); ok {
		plan.targetMessages = bound.config.MessagesPath
	}
	if request.Compact {
		plan.beforeRun = func(ctx context.Context) {
			if callerRun != 0 && !m.resolver.callerRuns.claim(callerRun, plan.key) {
				return
			}
			compactTarget := m.resolver.compactProfile
			if compactTarget == nil {
				compactTarget = m.resolver.compactTarget
			}
			if err := compactTarget(ctx, plan.reference, plan.target); err != nil {
				m.resolver.logger.Warn("profile_message compaction failed; sending the message anyway", "session", plan.reference, "profile", plan.target, "error", err)
			}
		}
	}
	return plan, nil
}

// chainNode names a profile of a session in a message chain or wait edge.
func chainNode(reference, profile string) string { return reference + "\x00" + profile }

// otherSession is the reference of an existing catalog-layout session that has the profile.
// A session or profile that does not exist is native.ErrProfileUnknown, whatever the reason,
// so it reads like one the settings do not allow.
func (m profileMessenger) otherSession(session, profile string) (string, error) {
	location, err := sessionfs.Resolve(m.resolver.roots, session)
	if err != nil || !location.UsesLayout() {
		return "", native.ErrProfileUnknown
	}
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil || !slices.Contains(info.Profiles, profile) {
		return "", native.ErrProfileUnknown
	}
	return location.Reference, nil
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

// RemoteDirectory implements native.RemoteDirectories: the view of a remotes folder whose
// clients and follower goroutines the resolver owns and closes.
func (m profileMessenger) RemoteDirectory(dir string) *remote.Directory {
	return m.resolver.remotes.Directory(dir)
}
