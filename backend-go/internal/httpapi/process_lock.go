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
