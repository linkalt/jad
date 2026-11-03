package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

// StateContext fires the user trigger with a prompt typed by the user.
func (m boundMutation) StateContext(ctx context.Context, prompt string, preview bool) (httpapi.StateContextResponse, error) {
	var result httpapi.StateContextResponse
	if ctx == nil {
		return result, errors.New("state context request context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	settings := m.config.StateContext
	trigger, configured := settings.Triggers[sessionfs.StateContextTriggerUser]
	if !settings.Enabled || !configured {
		return result, &httpapi.StatusError{Code: http.StatusForbidden, Message: "the user state context trigger is not configured for this session"}
	}
	prompt = strings.TrimSpace(prompt)
	if int64(len(prompt)) > trigger.MaxPromptBytes {
		return result, &httpapi.StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("prompt is larger than statecontext.triggers.user.max_prompt_bytes (%d)", trigger.MaxPromptBytes)}
	}
	release, err := m.hold()
	if err != nil {
		return result, err
	}
	defer release()
	if m.shared == nil || m.shared.store == nil {
		return result, errors.New("session transcript store is not initialized")
	}
	if err := m.shared.runMu.LockCtx(ctx); err != nil {
		return result, err
	}
	defer m.shared.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	snapshot, include, err := collectStateContextSnapshotWithPrompt(m.config, stateContextSources{
		process: m.processManager, task: m.taskStore, memory: m.memoryStore,
		subagent: m.subagentManager, owner: m.owner, async: m.asyncManager,
	}, sessionfs.StateContextTriggerUser, prompt)
	if err != nil {
		return result, fmt.Errorf("collect state context for the user trigger: %w", err)
	}
	if !include {
		return httpapi.StateContextResponse{Preview: preview}, nil
	}
	result = httpapi.StateContextResponse{Preview: preview, Role: settings.Role, Text: snapshot.Text, Bytes: len(snapshot.Text)}
	if preview {
		return result, nil
	}
	if _, err := m.shared.store.AppendStateContext(snapshot, settings.Role); err != nil {
		return httpapi.StateContextResponse{}, fmt.Errorf("persist state context for the user trigger: %w", err)
	}
	result.Written = true
	return result, nil
}
