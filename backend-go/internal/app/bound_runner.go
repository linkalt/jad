package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// boundRunner is a session resolved from its settings. Its model, tools and
// credential are prepared inside Run, once the user input is durable.
type boundRunner struct {
	resolver     *Resolver
	shared       *sharedSession
	config       sessionfs.SessionConfig
	reference    string
	taskStore    *native.TaskStore
	recordsStore *native.RecordsStore
	// delegation is set for a run a profile_message started (targetRunner); nil for a
	// user's run.
	delegation *profileDelegation
}

// hold leases the transcript store cached for the session now and points r at it for
// the rest of the operation. The store the runner was resolved with may have been
// dropped as idle since (CatalogConfig.StoreIdle), and only the cached store may write
// the transcript. A runner without a resolver (a test) keeps the store it was given.
func (r *boundRunner) hold() (func(), error) {
	if r.resolver == nil {
		return noRelease, nil
	}
	shared, release, err := r.resolver.sharedStore(r.config)
	if err != nil {
		return noRelease, err
	}
	r.shared = shared
	return release, nil
}

func (r boundRunner) Run(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	release, err := r.hold()
	if err != nil {
		return session.RunResult{}, err
	}
	defer release()
	if err := r.shared.runMu.LockCtx(ctx); err != nil {
		return session.RunResult{}, err
	}
	defer r.shared.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.RunResult{}, err
	}
	if r.config.MaxRunDurationMS <= 0 {
		return r.runLocked(ctx, input, options)
	}
	// The limit counts from here: a run waiting for the lock behind another has not
	// started. When it ends the run, the run fails with a message naming the limit.
	limit := time.Duration(r.config.MaxRunDurationMS) * time.Millisecond
	limitErr := &session.RunDurationLimitError{Limit: limit}
	runCtx, cancel := context.WithTimeoutCause(ctx, limit, limitErr)
	defer cancel()
	result, err := r.runLocked(runCtx, input, options)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(runCtx), limitErr) {
		return result, limitErr
	}
	return result, err
}

// runLocked is a run once the session's run lock is held.
func (r boundRunner) runLocked(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	var err error
	// Listing and reading a session never create its transcript directory; the first
	// write of a run does, before the input is appended to the transcript.
	if err := os.MkdirAll(filepath.Dir(r.config.MessagesPath), 0o755); err != nil {
		return session.RunResult{}, fmt.Errorf("create transcript directory: %w", err)
	}
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
	// The prompt_submit commands run first: one that blocks leaves nothing stored.
	if options.ExecutePlan {
		if input, err = executePlanInput(input, r.config.PlanMode); err != nil {
			return session.RunResult{}, err
		}
	}
	// A spent budget refuses the run before prompt_submit runs and before the input is
	// stored, so nothing is left to repair.
	budget := r.resolver.sessionBudget(r.config)
	if err := budget.Check(time.Now()); err != nil {
		return session.RunResult{}, err
	}
	if input, err = r.sessionStartInput(ctx, input, options.TurnID, nextSessionTurn, options.OnHookReport); err != nil {
		return session.RunResult{}, err
	}
	if input, err = r.promptSubmitInput(ctx, input, options.TurnID, options.OnHookReport); err != nil {
		return session.RunResult{}, err
	}
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
	r.taskStore, r.recordsStore = prepared.taskStore, prepared.recordsStore
	options.PersistedInput = persisted
	options.ToolCatalog = prepared.toolCatalog
	if options.Plan {
		if options.ToolCatalog, err = options.ToolCatalog.PlanMode(r.config.PlanMode.AllowTools); err != nil {
			return session.RunResult{}, fmt.Errorf("prepare the plan-mode tools: %w", err)
		}
		options.PlanPrompt = planModePrompt(r.config.PlanMode)
	}
	if note := r.resourceChangesNote(); note.Text != "" {
		options.Notes = append(options.Notes, note)
	}
	if options.Tools != nil {
		if options.ToolCatalog, err = options.ToolCatalog.Only(options.Tools); err != nil {
			return session.RunResult{}, fmt.Errorf("narrow the run's tools: %w", err)
		}
	}
	options.ImageResizer = newImageResizer(r.config.ImageResize)
	options.ImageStore = newImageStore(r.config.ImageStore)
	options.MaxToolRounds = int(r.config.MaxToolRounds)
	options.MaxRunTokens = r.config.MaxRunTokens
	options.MaxRunCostUSD = r.config.MaxRunCostUSD
	options.Budget = budget
	options.MaxParallelToolCalls = int(r.config.MaxParallelToolCalls)
	options.ToolCallScheduling = r.config.ToolCallScheduling
	options.LeafBudget = r.resolver.leafBudget
	if r.config.Checkpoint.Enabled {
		options.Checkpoint = checkpoint.ForSession(r.config, r.resolver.logger)
	}
	options.ToolExecution.WorkspaceRoot = r.config.WorkingDir
	options.ToolExecution.SessionDir = r.config.StateDir
	options.ToolExecution.SessionKey = sessionKey(r.reference, r.config.Profile)
	options.ToolExecution.Environment = runEnvironment(r.config)
	if r.delegation != nil && r.delegation.isolation.Mode == sessionfs.SandboxIsolationWorktree {
		// A delegated run of a profile with isolation worktree works in a git worktree of its
		// own (docs/isolation-sandbox-design.md §9.7). It writes no file of the session, so
		// it sets no checkpoint. The worktree is finished once the run and everything it
		// started have ended (the deferred sweep below runs first).
		opened, err := r.openDelegatedWorktree(ctx, &options)
		if err != nil {
			return session.RunResult{}, err
		}
		defer opened.close(ctx)
		if r.resolver != nil {
			// The run's MCP tools call servers started in the worktree, not the session's. A
			// server that does not start ends the run (the deferred close finishes the worktree).
			if err := opened.worktree.StartMCP(ctx, options.ToolExecution, r.resolver.runMCPLoader(r.config)); err != nil {
				return session.RunResult{}, mapSessionError(err)
			}
		}
	}
	options.AutomaticCompaction = &session.AutomaticCompactionSettings{
		Enabled:        r.config.Compaction.AutoEnabled,
		TriggerTokens:  r.config.Compaction.TriggerTokens,
		TurnType:       r.config.Compaction.TurnType,
		KeepTurns:      r.config.Compaction.KeepTurns,
		KeepToolPairs:  r.config.Compaction.KeepToolPairs,
		BackupMode:     r.config.Compaction.BackupMode,
		BackupMinTurns: r.config.Compaction.BackupMinTurns,
		Summary:        compactionSummarySettings(r.config),
		OnSummaryError: compactionSummaryLogger(r.resolver.logger, r.reference, r.config.Profile, native.CompactTriggerAutomatic),
		Overflow:       overflowSettings(r.config, r.resolver.logger, r.reference),
	}
	options.NextSessionTurn = nextSessionTurn
	options.CompactHistory = func(compaction transcript.CompactionOptions) (transcript.CompactionResult, error) {
		hooked := fireSessionHook(ctx, r.resolver.logger, r.reference, r.config, native.RunEvent{Event: native.HookEventPreCompact, TurnID: options.TurnID, Trigger: native.CompactTriggerAutomatic}, options.OnHookReport)
		if hooked.BlockedBy != "" {
			// A blocked automatic compaction is skipped, and the run goes on uncompacted.
			return transcript.CompactionResult{}, nil
		}
		return r.shared.compactPreservingUsage(r.config, compaction)
	}
	options.RefreshStateContext = func(context.Context) ([]session.StateContextPrompt, error) {
		snapshot, include, err := collectStateContextSnapshot(r.config, stateContextSources{
			task: r.taskStore, records: r.recordsStore,
			subagent: r.resolver.sharedSubagentManager(r.config), owner: sessionKey(r.reference, r.config.Profile),
			async: r.resolver.sessionAsyncManager(r.config),
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
	r.applyStopHooks(&options)
	r.applyNotificationHooks(ctx, &options)
	finish := r.runEndHook(ctx, options.OnHookReport)
	callerRun := r.resolver.callerRunSeq.Add(1)
	r.resolver.callerRuns.begin(callerRun)
	defer r.resolver.callerRuns.forget(callerRun)
	runCtx, sweepChildren := native.WithChildGroups(context.WithValue(context.WithValue(ctx, callerMessagesPathKey{}, r.config.MessagesPath), callerRunKey{}, callerRun))
	defer sweepChildren()
	result, err := prepared.runner.Run(runCtx, input, options)
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
		task: r.taskStore, records: r.recordsStore,
		subagent: r.resolver.sharedSubagentManager(r.config), owner: sessionKey(r.reference, r.config.Profile),
		async: r.resolver.sessionAsyncManager(r.config),
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
	release, err := r.hold()
	if err != nil {
		return empty, err
	}
	defer release()
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
	release, err := r.hold()
	if err != nil {
		return err
	}
	defer release()
	if r.shared == nil || r.shared.store == nil {
		return errors.New("bound session runner is not initialized")
	}
	return r.shared.store.RecordRunError(entry, r.config.Persistence.RunErrorCacheMaxEntries)
}

// executePlanInput is the input of a run that carries out a plan: plan_mode.execute_prompt,
// then the person's own text, when there is any, on a new paragraph. The run route lets
// only a text input (or none) through with execute_plan.
func executePlanInput(input json.RawMessage, settings sessionfs.PlanModeSettings) (json.RawMessage, error) {
	prompt := settings.ExecutePrompt
	if prompt == "" {
		prompt = sessionfs.DefaultPlanModeExecutePrompt
	}
	var text string
	if len(input) > 0 {
		if err := json.Unmarshal(input, &text); err != nil {
			return nil, errors.New("execute_plan takes a text input")
		}
	}
	if text = strings.TrimSpace(text); text != "" {
		prompt = strings.TrimRight(prompt, "\n") + "\n\n" + text
	}
	return json.Marshal(prompt)
}

// planModePrompt is the message a plan-mode run sends after the history. A configuration
// built without the loader (zero settings) gets the defaults.
func planModePrompt(settings sessionfs.PlanModeSettings) session.StateContextPrompt {
	defaults := sessionfs.DefaultPlanModeSettings()
	prompt := session.StateContextPrompt{Role: settings.Role, Text: settings.Instructions}
	if prompt.Text == "" {
		prompt.Text = defaults.Instructions
	}
	if prompt.Role == "" {
		prompt.Role = defaults.Role
	}
	return prompt
}

// resourceChangesNote tells the run what the profile's MCP servers reported changed since
// its last run (resources it watches, resource lists); each change is told once. It is sent
// in the model's prompt role and is not stored.
func (r boundRunner) resourceChangesNote() session.StateContextPrompt {
	if len(r.config.MCPServers) == 0 {
		return session.StateContextPrompt{}
	}
	manager, err := r.resolver.sharedMCPManager(r.config)
	if err != nil {
		return session.StateContextPrompt{}
	}
	text := mcpclient.ResourceChangesNote(manager.TakeResourceChanges())
	if text == "" {
		return session.StateContextPrompt{}
	}
	role := r.config.Model.PromptRole
	if role == "" {
		role = "developer"
	}
	return session.StateContextPrompt{Role: role, Text: text}
}

// runEnvironment is the environment a run of config starts its calls in: the session's sandbox
// (narrowed by a profile_message caller's, when one started the run). Nil without a sandbox.
func runEnvironment(config sessionfs.SessionConfig) *tools.Environment {
	if config.Sandbox == nil {
		return nil
	}
	return &tools.Environment{Sandbox: config.Sandbox}
}
