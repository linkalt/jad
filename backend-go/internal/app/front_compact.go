package app

import (
	"context"
	"errors"
	"fmt"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func (m boundMutation) FrontCompact(ctx context.Context, messageID string) (httpapi.FrontCompactionResponse, error) {
	var result httpapi.FrontCompactionResponse
	if ctx == nil {
		return result, errors.New("front-compaction context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !m.config.FrontCompactionConfig.Enabled {
		return result, &httpapi.StatusError{Code: 403, Message: "front compaction is disabled for this session"}
	}
	if m.shared == nil || m.shared.store == nil {
		return result, errors.New("session transcript store is not initialized")
	}
	m.shared.runMu.Lock()
	defer m.shared.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	m.shared.invalidateTurnStarts()
	compacted, err := m.shared.store.FrontCompactFromMessage(messageID, m.config.FrontCompactionConfig.DeleteMode)
	if err != nil {
		var conflict *transcript.FrontCompactionConflict
		if errors.As(err, &conflict) {
			return result, &httpapi.StatusError{Code: 409, Message: conflict.Error()}
		}
		return result, fmt.Errorf("front compact session transcript: %w", err)
	}
	return httpapi.FrontCompactionResponse{
		DeleteMode:  compacted.DeleteMode,
		ArchiveName: compacted.ArchiveName,
	}, nil
}
