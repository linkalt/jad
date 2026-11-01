package app

import (
	"context"
	"errors"
	"net/http"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessiongate"
)

// ErrRunActive is what ResetProfile returns while a run of the profile is going. It carries
// the status the HTTP reset route answers with.
var ErrRunActive = &httpapi.StatusError{Code: http.StatusConflict, Message: "cannot reset a profile while a run is active"}

// ErrGateHeld is what ResetProfile returns while another mutation of the profile holds the gate.
var ErrGateHeld = &httpapi.StatusError{Code: http.StatusConflict, Message: "session profile is already changing"}

// SetRunActive sets the check that tells whether a run of a session profile is still running.
// The HTTP handler owns the run registry, so the service sets it once the handler is built.
func (r *Resolver) SetRunActive(active func(reference, profile string) bool) {
	r.runActive.Store(&active)
}

// SessionGate returns the mutation gate and session locks the HTTP handler shares with this
// resolver, so every path that changes a profile is refused by the same gate.
func (r *Resolver) SessionGate() *sessiongate.Gate {
	return r.gate
}

// ResetProfile resets the transcript of one profile: the mutation gate, the session lock, the
// active-run check and the process lock, then the reset and the session's children. The HTTP
// route and the native profile tool both call it, so the refusal is the same. It refuses at
// once rather than waiting for a run or a change in progress.
func (r *Resolver) ResetProfile(ctx context.Context, reference, profile string) error {
	resolved, err := r.ResolveMaintenance(ctx, reference, profile)
	if err != nil {
		return err
	}
	if resolved.Session == nil {
		return errors.New("session maintenance provider is not initialized")
	}
	key := sessionKey(resolved.Reference, resolved.Profile)
	// The gate is taken before the per-session lock.
	if !r.gate.Begin(key) {
		return ErrGateHeld
	}
	// The gate is cleared before the locks are given back.
	var unlock, processUnlock func()
	defer func() {
		r.gate.End(key)
		if processUnlock != nil {
			processUnlock()
		}
		if unlock != nil {
			unlock()
		}
	}()
	unlock = r.gate.Lock(reference)
	// A run holds the transcript lock until it ends, so the check has to come before waiting
	// for that lock: after it, the run is over and the reset would wipe the answer it just
	// wrote. The session lock above serializes this check with a run start.
	if active := r.runActive.Load(); active != nil && (*active)(resolved.Reference, resolved.Profile) {
		return ErrRunActive
	}
	processUnlock, err = r.lockSessionProcess(ctx, reference, profile)
	if err != nil {
		return err
	}
	if err := resolved.Session.Reset(ctx); err != nil {
		return err
	}
	// The history the handles were started from is gone, so their results have no use and
	// would only keep costing.
	r.ResetSessionChildren(resolved.Reference, resolved.Profile)
	return nil
}

// lockSessionProcess takes the cross-process session lock for a maintenance action.
func (r *Resolver) lockSessionProcess(ctx context.Context, reference, profile string) (func(), error) {
	return r.LockSession(ctx, reference, profile, "")
}
