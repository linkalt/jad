package app

import (
	"context"
	"errors"
	"net/http"

	"example.com/agent-harness/backend-go/internal/checkpoint"
	"example.com/agent-harness/backend-go/internal/httpapi"
)

// ListCheckpoints lists the session's checkpoints, newest first. Reading needs no
// git, so a missing binary does not hide the list.
func (m boundMutation) ListCheckpoints(ctx context.Context) (httpapi.CheckpointListing, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.CheckpointListing{}, err
	}
	service := checkpoint.ForSession(m.config, nil)
	return httpapi.CheckpointListing{Enabled: service.Enabled(), Checkpoints: service.List()}, nil
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
