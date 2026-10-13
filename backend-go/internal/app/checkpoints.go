package app

import (
	"context"
	"errors"
	"net/http"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/httpapi"
)

// ListCheckpoints lists the session's checkpoints, newest first. Reading needs no
// git, so a missing binary does not hide the list. A record left open while no run
// is live is reported as interrupted.
func (m boundMutation) ListCheckpoints(ctx context.Context, runIsLive bool) (httpapi.CheckpointListing, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.CheckpointListing{}, err
	}
	service := checkpoint.ForSession(m.config, nil)
	return httpapi.CheckpointListing{Enabled: service.Enabled(), Checkpoints: service.Report(runIsLive)}, nil
}

// RestoreCheckpoint reverts what a checkpoint's run changed. It holds the same
// guard compaction and reset use, so no run starts while files are rewritten.
func (m boundMutation) RestoreCheckpoint(ctx context.Context, id string, request httpapi.CheckpointRestoreRequest) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.shared == nil {
		return nil, errors.New("session transcript store is not initialized")
	}
	m.shared.runMu.Lock()
	defer m.shared.runMu.Unlock()
	result, err := checkpoint.ForSession(m.config, nil).Restore(id, checkpoint.RestoreOptions{DryRun: request.DryRun, Mode: request.Mode})
	if err != nil {
		var refusal *checkpoint.Error
		if errors.As(err, &refusal) {
			status := http.StatusUnprocessableEntity
			switch refusal.Code {
			case checkpoint.CodeNotFound:
				status = http.StatusNotFound
			case checkpoint.CodeConflict:
				status = http.StatusConflict
			}
			return nil, &httpapi.StatusError{Code: status, Message: refusal.Message}
		}
		return nil, err
	}
	return result, nil
}
