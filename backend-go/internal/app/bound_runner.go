package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// boundRunner is a session resolved from its settings. Its model, tools and
// credential are prepared inside Run, once the user input is durable.
type boundRunner struct {
	resolver       *Resolver
	shared         *sharedSession
	config         sessionfs.SessionConfig
	reference      string
	processManager native.ProcessManager
	taskStore      *native.TaskStore
	memoryStore    *native.MemoryStore
}

func (r boundRunner) Run(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	if err := r.shared.runMu.LockCtx(ctx); err != nil {
		return session.RunResult{}, err
	}
	defer r.shared.runMu.Unlock()
	nextSessionTurn := func() (int64, error) {
		index, err := scanTranscriptIndex(r.shared, r.config)
		if err != nil {
			return 0, fmt.Errorf("read prior usage: %w", err)
		}
		if index.usage.TurnCount >= maxSafeUsageInteger {
			return 0, errors.New("session turn counter is exhausted")
		}
		return index.usage.TurnCount + 1, nil
	}
	// Once the run lock is held, the user input is stored before anything that can
	// still fail (an unreadable model file, an unreachable tool server, a missing
	// credential): the message is not lost, and the next run repairs and continues
	// from it. A run that gives up while waiting for the lock stores nothing.
	persisted, err := session.PersistInputWithImages(r.shared.store, input, nextSessionTurn, newImageStore(r.config.ImageStore))
	if err != nil {
		return session.RunResult{}, err
	}
	if options.OnInputAccepted != nil {
		options.OnInputAccepted(*persisted)
	}
	prepared, err := r.resolver.prepareRun(ctx, r.shared, r.config)
	if err != nil {
		return session.RunResult{}, err
	}
	// From here on the configuration includes the model.
	r.config = prepared.config
	r.processManager, r.taskStore, r.memoryStore = prepared.processManager, prepared.taskStore, prepared.memoryStore
	options.PersistedInput = persisted
	options.ToolCatalog = prepared.toolCatalog
	options.ImageResizer = newImageResizer(r.config.ImageResize)
	options.ImageStore = newImageStore(r.config.ImageStore)
	options.MaxToolRounds = int(r.config.MaxToolRounds)
	options.MaxRunTokens = r.config.MaxRunTokens
	options.MaxParallelToolCalls = int(r.config.MaxParallelToolCalls)
	options.ToolCallScheduling = r.config.ToolCallScheduling
	options.Approval = &tools.ApprovalOptions{
		TimeoutMS:     r.config.Approval.TimeoutMS,
		ExtendMS:      r.config.Approval.ExtendMS,
		MaxTimeoutMS:  r.config.Approval.MaxTimeoutMS,
		HeadlessAllow: r.config.Approval.Headless == sessionfs.ApprovalHeadlessAllow,
		PreviewBytes:  r.config.Approval.PreviewBytes,
	}
	if r.config.Checkpoint.Enabled {
		options.Checkpoint = checkpoint.ForSession(r.config, r.resolver.logger)
	}
	options.ToolExecution.WorkspaceRoot = r.config.WorkingDir
	options.ToolExecution.SessionDir = r.config.StateDir
	options.ToolExecution.SessionKey = sessionKey(r.reference, r.config.Profile)
	options.AutomaticCompaction = &session.AutomaticCompactionSettings{
		Enabled:       r.config.Compaction.AutoEnabled,
		TriggerTokens: r.config.Compaction.TriggerTokens,
		TurnType:      r.config.Compaction.TurnType,
		KeepTurns:     r.config.Compaction.KeepTurns,
		KeepToolPairs: r.config.Compaction.KeepToolPairs,
		BackupMode:    r.config.Compaction.BackupMode,
	}
	options.NextSessionTurn = nextSessionTurn
	options.CompactHistory = func(compaction transcript.CompactionOptions) (transcript.CompactionResult, error) {
		r.fireRunHook(ctx, native.RunEvent{Event: native.HookEventPreCompact, TurnID: options.TurnID, Trigger: native.CompactTriggerAutomatic}, options.OnHookReport)
		return r.shared.compactPreservingUsage(r.config, compaction)
	}
	options.RefreshStateContext = func(context.Context) ([]session.StateContextPrompt, error) {
		snapshot, include, err := collectStateContextSnapshot(r.config, stateContextSources{
			process: r.processManager, task: r.taskStore, memory: r.memoryStore,
			subagent: r.resolver.sharedSubagentManager(r.config), owner: sessionKey(r.reference, r.config.Profile),
		}, "compaction")
		if err != nil {
			return nil, fmt.Errorf("collect state context after automatic compaction: %w", err)
		}
		if include {
			if _, err := r.shared.store.AppendStateContext(snapshot, r.config.StateContext.Role); err != nil {
				return nil, fmt.Errorf("persist state context after automatic compaction: %w", err)
			}
		}
		return r.stateContextBlocks()
	}
	options.StateContext = nil
	stateContext, err := r.currentStateContext()
	if err != nil {
		return session.RunResult{}, err
	}
	options.StateContext = stateContext
	r.fireRunHook(ctx, native.RunEvent{Event: native.HookEventRunStart, TurnID: options.TurnID}, options.OnHookReport)
	finish := r.runEndHook(ctx, options.OnHookReport)
	result, err := prepared.runner.Run(ctx, input, options)
	finish(result.TurnID, err)
	return result, err
}

// currentStateContext returns the stored blocks whose trigger is still configured. With
// none, the session_start trigger writes one, so the model sees the stores from the
// first run on and again after a compaction that found nothing to report. Then the
// run_start trigger writes a block if the stores differ from the latest one.
func (r boundRunner) currentStateContext() ([]session.StateContextPrompt, error) {
	settings := r.config.StateContext
	if !settings.Enabled {
		return nil, nil
	}
	entries, err := r.stateContextEntries()
	if err != nil {
		return nil, err
	}
	sources := stateContextSources{
		process: r.processManager, task: r.taskStore, memory: r.memoryStore,
		subagent: r.resolver.sharedSubagentManager(r.config), owner: sessionKey(r.reference, r.config.Profile),
	}
	changed := false
	if _, configured := settings.Triggers[sessionfs.StateContextTriggerSessionStart]; configured && len(entries) == 0 {
		started, include, err := collectStateContextSnapshot(r.config, sources, sessionfs.StateContextTriggerSessionStart)
		if err != nil {
			return nil, fmt.Errorf("collect state context at session start: %w", err)
		}
		if include {
			if _, err := r.shared.store.AppendStateContext(started, settings.Role); err != nil {
				return nil, fmt.Errorf("persist state context at session start: %w", err)
			}
			changed = true
		}
	}
	if _, configured := settings.Triggers[sessionfs.StateContextTriggerRunStart]; configured {
		if changed {
			if entries, err = r.stateContextEntries(); err != nil {
				return nil, err
			}
		}
		var latest *transcript.StateContextEntry
		if len(entries) > 0 {
			latest = &entries[len(entries)-1]
		}
		snapshot, write, err := collectRunStartStateContext(r.config, sources, latest)
		if err != nil {
			return nil, fmt.Errorf("collect state context at run start: %w", err)
		}
		if write {
			if _, err := r.shared.store.AppendStateContext(snapshot, settings.Role); err != nil {
				return nil, fmt.Errorf("persist state context at run start: %w", err)
			}
			changed = true
		}
	}
	if !changed {
		return promptsFromStateContextEntries(entries), nil
	}
	return r.stateContextBlocks()
}

// stateContextEntries reads the stored blocks in write order and drops those of a trigger
// that is no longer configured.
func (r boundRunner) stateContextEntries() ([]transcript.StateContextEntry, error) {
	settings := r.config.StateContext
	if !settings.Enabled {
		return nil, nil
	}
	stored, err := r.shared.store.ReadStateContextEntries()
	if err != nil {
		return nil, fmt.Errorf("read state context: %w", err)
	}
	var entries []transcript.StateContextEntry
	for _, entry := range stored {
		if _, configured := settings.Triggers[entry.Trigger]; configured {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (r boundRunner) stateContextBlocks() ([]session.StateContextPrompt, error) {
	entries, err := r.stateContextEntries()
	if err != nil {
		return nil, err
	}
	return promptsFromStateContextEntries(entries), nil
}

func promptsFromStateContextEntries(entries []transcript.StateContextEntry) []session.StateContextPrompt {
	var blocks []session.StateContextPrompt
	for _, entry := range entries {
		blocks = append(blocks, session.StateContextPrompt{Role: entry.Role, Text: entry.Text, After: entry.After})
	}
	return blocks
}

// ReadRunUsageSummary reads the session's usage from inside a run, where the
// run already holds runMu (it serializes against compaction and reset), so it
// must not be called from outside Run callbacks.
func (r boundRunner) ReadRunUsageSummary(ctx context.Context) (webui.UsageSummary, error) {
	var empty webui.UsageSummary
	if ctx == nil {
		return empty, errors.New("usage summary context is required")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if r.shared == nil || r.shared.store == nil || r.config.MessagesPath == "" {
		return empty, errors.New("bound session runner is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	index, err := scanTranscriptIndex(r.shared, r.config)
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return index.usage, nil
}

// RecordRunError keeps a failed run in the session's run-error cache, outside
// messages.jsonl. The cap is the session's persistence.run_error_cache_max_entries.
func (r boundRunner) RecordRunError(entry transcript.RunErrorEntry) error {
	if r.shared == nil || r.shared.store == nil {
		return errors.New("bound session runner is not initialized")
	}
	return r.shared.store.RecordRunError(entry, r.config.Persistence.RunErrorCacheMaxEntries)
}
