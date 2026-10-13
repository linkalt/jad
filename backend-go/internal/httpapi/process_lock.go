package httpapi

import "context"

func (h *Handler) lockSessionProcess(ctx context.Context, reference, profile, modelPath string) (func(), error) {
	locker, ok := h.resolver.(SessionProcessLocker)
	if !ok {
		return func() {}, nil
	}
	release, err := locker.LockSession(ctx, reference, profile, modelPath)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return func() {}, nil
	}
	return release, nil
}

func (h *Handler) lockSessionSettingsProcess(ctx context.Context, reference string) (func(), error) {
	locker, ok := h.resolver.(SessionProcessLocker)
	if !ok {
		return func() {}, nil
	}
	release, err := locker.LockSessionSettings(ctx, reference)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return func() {}, nil
	}
	return release, nil
}

// lockSession takes the in-process lock of one session. It is keyed by the canonical
// reference, so a request that spells the session differently still excludes the others.
// A reference that does not resolve keeps its own key: the request fails later anyway.
func (h *Handler) lockSession(reference string) func() {
	if references, ok := h.resolver.(SessionReferences); ok {
		if canonical, err := references.CanonicalSessionReference(reference); err == nil {
			reference = canonical
		}
	}
	return h.sessionLocks.lock(reference)
}
