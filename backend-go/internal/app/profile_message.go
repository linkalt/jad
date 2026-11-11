package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

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
		return nil, errProfileRunsUnset
	}
	resolved, err := m.resolver.Resolve(ctx, targetReference, target, request.Model)
	if err != nil {
		// Only a target that resolves without the model is refused for the model itself.
		var status *httpapi.StatusError
		if request.Model != "" && errors.As(err, &status) && (status.Code == http.StatusBadRequest || status.Code == http.StatusUnprocessableEntity) {
			if _, plain := m.resolver.Resolve(ctx, targetReference, target, ""); plain == nil {
				return nil, &native.ProfileModelError{Model: request.Model, Message: status.Message}
			}
		}
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
	plan := &profileMessagePlan{
		resolver:  m.resolver,
		starter:   starter,
		key:       resolved.Reference + "\x00" + canonical,
		reference: resolved.Reference,
		target:    canonical,
		caller:    caller,
		callerRef: callerRef,
		chain:     chain,
		message:   request.Message,
		model:     request.Model,
		timeout:   request.Timeout,
		queueWait: request.QueueWait,
		maxQueued: request.MaxQueued,
		blocking:  request.Blocking,
		// The run joins the caller's run tree below the caller's node, under the caller's root
		// cap; it executes in the target's session, under the target's own session cap.
		parentNode:    request.Caller.RunNode,
		maxPerRoot:    request.MaxRunsPerRoot,
		maxPerSession: targetMaxRunsPerSession(resolved.Runner),
		hops:          request.Caller.RemoteHops,
		depth:         request.Caller.RunDepth() + 1,
		deny:          request.Caller.Deny,
		modelKeys:     request.Caller.ModelKeyEnvs,
		// The target's run works under the caller's lineage and the caller's session key, so a
		// stop, reset or delete of the caller reaches what that run leaves going.
		lineage: native.ExtendRunLineage(native.RunLineageOf(ctx), request.OwnerKey),

		callerMessages: callerMessagesPath,
	}
	if bound, ok := resolved.Runner.(boundRunner); ok {
		plan.targetMessages = bound.config.MessagesPath
	}
	var callerSandbox *sessionfs.SandboxSettings
	if request.Environment != nil {
		callerSandbox = request.Environment.Sandbox
	}
	if callerSandbox != nil || request.Sandbox != nil {
		// Refused here, before a handle or a queue place is taken, when the target's own
		// sandbox would reach more than the caller's, or the call's sandbox choice more than
		// the target's; the run applies both again to the config it starts with.
		bound, ok := resolved.Runner.(boundRunner)
		if !ok {
			return nil, errors.New("profile message needs the target's settings to confine it to the caller's sandbox")
		}
		if _, err := confineTarget(callerSandbox, request.CallerPlace, request.Sandbox, bound.config); err != nil {
			return nil, profileSandboxError(err)
		}
		plan.confine = targetConfiner(callerSandbox, request.CallerPlace, request.Sandbox)
	}
	plan.beforeRun = m.contextStep(plan, request.ContextTrigger)
	return plan, nil
}

// targetMaxRunsPerSession is the max_runs_per_session of the session a message's run executes
// in, the target's own (the section's default when the target has none, or when its settings are
// not known), as a sub-agent run is counted under its executing session's own cap (T-007).
func targetMaxRunsPerSession(runner httpapi.RunSession) int {
	var subagent *sessionfs.NativeSubagentSettings
	if bound, ok := runner.(boundRunner); ok {
		subagent = bound.config.NativeSubagent
	}
	return int(subagent.RunMaxRunsPerSession())
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
	if err := maintenanceReady(resolved); err != nil {
		return err
	}
	_, err = resolved.Session.Compact(ctx)
	return err
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
