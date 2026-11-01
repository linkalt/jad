package app

import (
	"context"
	"net/http"

	"jad/backend-go/internal/httpapi"
)

// ErrCompactGateHeld is what CompactProfile returns while another mutation holds the gate. It
// keeps the compact route's own 409 text.
var ErrCompactGateHeld = &httpapi.StatusError{Code: http.StatusConflict, Message: "session profile or compaction is already changing"}

// CompactProfile compacts one profile's transcript. The HTTP compact route and the native
// profile tool both call it, so they take the same locks in the same order: the mutation gate
// (a second mutation is refused at once), the session lock, then the profile's transcript lock,
// which waits for another process's run. An in-process run holds the transcript too, so the
// compaction waits for it, as DECISIONS.md records for compaction during a live run.
func (r *Resolver) CompactProfile(ctx context.Context, reference, profile string) (httpapi.CompactionResponse, error) {
	var result httpapi.CompactionResponse
	resolved, err := r.ResolveMaintenance(ctx, reference, profile)
	if err != nil {
		return result, err
	}
	if err := maintenanceReady(resolved); err != nil {
		return result, err
	}
	key := sessionKey(resolved.Reference, resolved.Profile)
	if !r.gate.Begin(key) {
		return result, ErrCompactGateHeld
	}
	// The gate is cleared before the locks are given back, as ResetProfile does.
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
	unlock = r.gate.Lock(resolved.Reference)
	processUnlock, err = r.lockSessionProcess(ctx, reference, profile)
	if err != nil {
		return result, err
	}
	return resolved.Session.Compact(ctx)
}
