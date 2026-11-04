package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

func (r *Resolver) ResolveMaintenance(ctx context.Context, reference, profile string) (httpapi.ResolvedMutation, error) {
	if ctx == nil {
		return httpapi.ResolvedMutation{}, errors.New("session maintenance context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.ResolvedMutation{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.ResolvedMutation{}, mapSessionError(err)
	}
	// Settings only: the TypeScript reset and front-compact handlers open the
	// session without reading model.json, so a broken model file must not block
	// them. Compact validates the model itself, as TypeScript's compact() does.
	config, err := sessionfs.LoadSessionSettings(location, profile, "")
	if err != nil {
		return httpapi.ResolvedMutation{}, mapSessionError(err)
	}
	// Each mutation leases the store again (boundMutation.hold); this open only reports
	// a store that cannot be used before the mutation starts.
	shared, release, err := r.sharedStore(config)
	if err != nil {
		return httpapi.ResolvedMutation{}, err
	}
	release()
	processManager := r.sharedProcessManager(config)
	taskStore := r.sharedTaskStore(config)
	memoryStore := r.sharedMemoryStore(config)
	return httpapi.ResolvedMutation{
		Reference: location.Reference,
		Profile:   config.Profile,
		Session: boundMutation{
			resolver: r, shared: shared, config: config, processManager: processManager, taskStore: taskStore, memoryStore: memoryStore,
			asyncManager: r.sessionAsyncManager(config), subagentManager: r.sharedSubagentManager(config), owner: sessionKey(location.Reference, config.Profile),
			reference: location.Reference, logger: r.logger,
		},
	}, nil
}

type boundMutation struct {
	// resolver hands out the cached store for each operation; see hold.
	resolver        *Resolver
	shared          *sharedSession
	config          sessionfs.SessionConfig
	processManager  native.ProcessManager
	taskStore       *native.TaskStore
	memoryStore     *native.MemoryStore
	subagentManager *native.SubagentManager
	asyncManager    *asyncmgr.Manager
	// owner is the session key of the sub-agent runs this session started.
	owner     string
	reference string
	logger    *slog.Logger
}

// hold leases the transcript store cached for the session now and points m at it for
// the rest of the operation; see boundRunner.hold. A mutation without a resolver (a
// test) keeps the store it was given.
func (m *boundMutation) hold() (func(), error) {
	if m.resolver == nil {
		return noRelease, nil
	}
	shared, release, err := m.resolver.sharedStore(m.config)
	if err != nil {
		return noRelease, err
	}
	m.shared = shared
	return release, nil
}

func (m boundMutation) Compact(ctx context.Context) (httpapi.CompactionResponse, error) {
	var result httpapi.CompactionResponse
	if ctx == nil {
		return result, errors.New("session compaction context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
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
	// TypeScript compact() loads the full configuration, so an unusable
	// model.json fails the request with a 422 before the transcript is touched.
	if _, err := m.config.WithModel(); err != nil {
		return result, mapSessionError(err)
	}
	m.shared.invalidateTurnStarts()
	hooked := fireSessionHook(ctx, m.logger, m.reference, m.config, native.RunEvent{Event: native.HookEventPreCompact, Trigger: native.CompactTriggerManual}, nil)
	if hooked.BlockedBy != "" {
		blocked := native.HookBlockedError{Event: native.HookEventPreCompact, Hook: hooked.BlockedBy}
		return result, &httpapi.StatusError{Code: http.StatusConflict, Message: blocked.ClientMessage()}
	}
	compacted, err := m.shared.compactPreservingUsage(m.config, transcript.CompactionOptions{
		TurnType:       m.config.Compaction.TurnType,
		KeepTurns:      m.config.Compaction.KeepTurns,
		KeepToolPairs:  m.config.Compaction.KeepToolPairs,
		BackupMode:     m.config.Compaction.BackupMode,
		BackupMinTurns: m.config.Compaction.BackupMinTurns,
	})
	if err != nil {
		return result, fmt.Errorf("compact session transcript: %w", err)
	}
	if compacted.Compacted {
		snapshot, include, err := collectStateContextSnapshot(m.config, stateContextSources{
			process: m.processManager, task: m.taskStore, memory: m.memoryStore,
			subagent: m.subagentManager, owner: m.owner, async: m.asyncManager,
		}, "compaction")
		if err != nil {
			return result, fmt.Errorf("collect state context after compaction: %w", err)
		}
		if include {
			if _, err := m.shared.store.AppendStateContext(snapshot, m.config.StateContext.Role); err != nil {
				return result, fmt.Errorf("persist state context after compaction: %w", err)
			}
		}
	}
	index, err := scanTranscriptIndex(m.shared, m.config)
	if err != nil {
		return result, fmt.Errorf("read usage after compaction: %w", err)
	}
	usage, err := json.Marshal(index.usage)
	if err != nil {
		return result, fmt.Errorf("encode usage after compaction: %w", err)
	}
	return httpapi.CompactionResponse{
		Compacted:      compacted.Compacted,
		CompactedItems: compacted.CompactedItems,
		RetainedItems:  compacted.RetainedItems,
		Usage:          usage,
	}, nil
}

func (m boundMutation) Reset(ctx context.Context) error {
	if ctx == nil {
		return errors.New("session reset context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := m.hold()
	if err != nil {
		return err
	}
	defer release()
	if m.shared == nil || m.shared.store == nil {
		return errors.New("session transcript store is not initialized")
	}
	if err := m.shared.runMu.LockCtx(ctx); err != nil {
		return err
	}
	defer m.shared.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.shared.invalidateTurnStarts()
	return m.shared.store.Reset()
}
