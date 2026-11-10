package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

var unsafeDirectoryCharacters = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// subagentLauncher runs the children of the subagent tool. It holds the calling
// session's configuration, so a child that names no model of its own uses the
// parent's model, credential and retry settings.
type subagentLauncher struct {
	resolver *Resolver
	parent   sessionfs.SessionConfig
}

// subagentLauncherFor returns the launcher of one session, or nil when the session
// does not configure the subagent tool.
func (r *Resolver) subagentLauncherFor(config sessionfs.SessionConfig) native.SubagentLauncher {
	if config.NativeSubagent == nil {
		return nil
	}
	return subagentLauncher{resolver: r, parent: config}
}

// Run starts one child with a fresh transcript and returns its final answer. The
// child is an ordinary session.Runner run: its own store, its own model client and
// the catalog the tool chose, with the question channel and the change listener of the call
// that started it (request.Execution). It takes no checkpoint of its own: the calling run's
// checkpoint is taken before a call that may change files and covers what the child writes, and
// at every depth: the tool judges a child that may nest by the agents it can reach, so the call at
// the top takes it for a grandchild's writes too. It compacts its own transcript when the agent's trigger is reached, and sees the session's newest
// state-context block when the agent asks for it.
func (l subagentLauncher) Run(ctx context.Context, request native.SubagentRequest) (native.SubagentResult, error) {
	config, err := l.childConfig(request)
	if err != nil {
		return native.SubagentResult{}, err
	}
	apiKey, err := l.resolver.providerAPIKey(config)
	if err != nil {
		return native.SubagentResult{}, err
	}
	store, dir, err := l.openStore(config, request)
	if err != nil {
		return native.SubagentResult{}, err
	}
	if request.OnTranscript != nil {
		request.OnTranscript(dir)
	}
	streamer, _ := l.resolver.runStreamer(config, apiKey)
	runner, err := session.NewRunner(store, streamer, session.ModelSettings{
		Model:       config.Model.Name,
		Request:     config.Model.Request,
		HostedTools: config.Model.HostedTools,
		PromptRole:  config.Model.PromptRole,
		PromptText:  config.Model.PromptText,
		// The children of one agent share a key: they share the agent's prompt and tools.
		PromptCacheKey: promptCacheKey(config.Model, l.parent.MessagesPath+"\x00"+request.Agent.Name),
	})
	if err != nil {
		return native.SubagentResult{}, fmt.Errorf("create sub-agent runner: %w", err)
	}
	input, err := json.Marshal(request.Task)
	if err != nil {
		return native.SubagentResult{}, fmt.Errorf("encode sub-agent task: %w", err)
	}

	stateContext, err := l.stateContextFor(request.Agent)
	if err != nil {
		return native.SubagentResult{}, err
	}
	// The child's spend adds into the calling run's meter, so the caller's max_run_cost_usd
	// and budget count it (the child's own context hangs off the sub-agent manager).
	// Its subagent_stop reports, and those of its own children, go to the calling run's
	// stream.
	var onReport func(session.HookReport)
	if request.CallerValues != nil {
		ctx = withCallerRun(ctx, request.CallerValues)
		if callerMeter := session.CostMeterFrom(request.CallerValues); callerMeter != nil && session.CostMeterFrom(ctx) == nil {
			ctx = session.WithCostMeter(ctx, callerMeter)
		}
		if onReport = session.HookReporterFrom(request.CallerValues); onReport != nil && session.HookReporterFrom(ctx) == nil {
			ctx = session.WithHookReporter(ctx, onReport)
		}
	}
	ctx, sweepChildren := native.WithChildGroups(ctx)
	defer sweepChildren()
	var rounds, tokens atomic.Int64
	// lastText is the newest assistant text of any round. A child that is cut off has
	// no final answer, but what it wrote last is the best partial work it has.
	var lastText atomic.Value
	result, runErr := runner.Run(ctx, input, session.RunOptions{
		ToolCatalog:          request.Tools,
		ImageResizer:         newImageResizer(config.ImageResize),
		ImageStore:           newImageStore(config.ImageStore),
		ToolExecution:        request.Execution,
		MaxToolRounds:        int(request.Agent.MaxToolRounds),
		MaxRunTokens:         request.Agent.MaxRunTokens,
		MaxParallelToolCalls: int(l.parent.MaxParallelToolCalls),
		ToolCallScheduling:   l.parent.ToolCallScheduling,
		LeafBudget:           l.resolver.leafBudget,
		StateContext:         stateContext,
		AutomaticCompaction:  l.compactionFor(request.Agent),
		CompactHistory:       compactWhileWorking(store),
		OnResponseCommitted: func(event json.RawMessage, _ transcript.CommitResult) {
			rounds.Add(1)
			tokens.Add(session.ResponseTokens(event))
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				lastText.Store(text)
			}
		},
	})
	outcome := native.SubagentResult{Rounds: int(rounds.Load()), Tokens: tokens.Load()}
	if runErr != nil {
		outcome.Text, _ = lastText.Load().(string)
	} else {
		outcome.Text = native.SubagentAnswerText(result.Event)
	}
	// A background child may end after the caller's last round, which would have recorded
	// its spend in the caller's budget.
	if callerMeter := session.CostMeterFrom(ctx); callerMeter != nil {
		if err := callerMeter.RecordUnrecordedSpend(time.Now()); err != nil && l.resolver.logger != nil {
			l.resolver.logger.Warn("record a sub-agent's spend in the budget", "agent", request.Agent.Name, "error", err)
		}
	}
	l.subagentStopHook(ctx, request, outcome, runErr, onReport)
	return outcome, runErr
}

// subagentStopHook reports the end of one child run to the parent session's subagent_stop
// commands. Like run_end, it runs on a context the stop cannot cancel, so a child that was
// stopped or timed out still reports. Its reports go to the server log and to onReport, the
// calling run's reporter; a background child that ends after that run reaches its log only,
// since the ended run's stream has no reader left.
func (l subagentLauncher) subagentStopHook(ctx context.Context, request native.SubagentRequest, outcome native.SubagentResult, runErr error, onReport func(session.HookReport)) {
	hooks := l.parent.Hooks
	if !hooks.Enabled || len(hooks.SubagentStop) == 0 {
		return
	}
	event := native.RunEvent{
		Event:  native.HookEventSubagentStop,
		Agent:  request.Agent.Name,
		CallID: request.CallID,
		Rounds: outcome.Rounds,
		Tokens: outcome.Tokens,
		Answer: outcome.Text,
	}
	switch {
	case runErr == nil:
		event.Outcome = native.RunOutcomeOK
	case errors.Is(runErr, context.Canceled):
		event.Outcome = native.RunOutcomeCancelled
	default:
		event.Outcome, event.Error = native.RunOutcomeError, runErr.Error()
	}
	reference, _, _ := strings.Cut(request.Execution.SessionKey, "\x00")
	fireSessionHook(context.WithoutCancel(ctx), l.resolver.logger, reference, l.parent, event, onReport)
}

// compactionFor is the automatic compaction of one child. The agent's own trigger and
// switch win; what it leaves out comes from the session's compaction settings, so a
// cheap model can compact at a different size than the master.
func (l subagentLauncher) compactionFor(agent sessionfs.NativeSubagentAgent) *session.AutomaticCompactionSettings {
	compaction := l.parent.Compaction
	enabled := compaction.AutoEnabled
	if agent.CompactionTriggerTokens > 0 {
		enabled = true
		compaction.TriggerTokens = agent.CompactionTriggerTokens
	}
	if agent.HasAutoCompaction {
		enabled = agent.AutoCompaction
	}
	return &session.AutomaticCompactionSettings{
		Enabled:        enabled,
		TriggerTokens:  compaction.TriggerTokens,
		TurnType:       compaction.TurnType,
		KeepTurns:      compaction.KeepTurns,
		KeepToolPairs:  compaction.KeepToolPairs,
		BackupMode:     compaction.BackupMode,
		BackupMinTurns: compaction.BackupMinTurns,
	}
}

// compactWhileWorking compacts only a round that asked for tools. After a final answer
// the child is done, and compacting would empty its messages.jsonl, which a restart
// reads to restore the run's answer.
func compactWhileWorking(store *transcript.TurnStore) func(transcript.CompactionOptions) (transcript.CompactionResult, error) {
	return func(options transcript.CompactionOptions) (transcript.CompactionResult, error) {
		if options.PreserveResponseID == "" {
			return transcript.CompactionResult{}, nil
		}
		return store.CompactHistory(options)
	}
}

// stateContextFor returns the session's newest stored state-context block for an agent
// that asks for it, placed before the child's history so it is sent in every request,
// compaction or not. A child has no stores of its own, so it never collects a block.
func (l subagentLauncher) stateContextFor(agent sessionfs.NativeSubagentAgent) ([]session.StateContextPrompt, error) {
	if !agent.StateContext {
		return nil, nil
	}
	shared, release, err := l.resolver.sharedStore(l.parent)
	if err != nil {
		return nil, fmt.Errorf("read the session state context: %w", err)
	}
	defer release()
	entries, err := boundRunner{resolver: l.resolver, shared: shared, config: l.parent}.stateContextEntries()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	newest := entries[len(entries)-1]
	return []session.StateContextPrompt{{Role: newest.Role, Text: newest.Text}}, nil
}

// childConfig is the parent's configuration with the agent's model and instructions.
func (l subagentLauncher) childConfig(request native.SubagentRequest) (sessionfs.SessionConfig, error) {
	config := l.parent
	if request.Agent.ModelPath != "" {
		config.ModelPath = request.Agent.ModelPath
		config.ReasoningEffort = request.Agent.ReasoningEffort
	}
	// The parent's prompt is not the child's: it gets its own instructions and
	// documents, or none.
	config.Model.PromptText = request.Agent.PromptText
	loaded, err := config.WithModel()
	if err != nil {
		return sessionfs.SessionConfig{}, fmt.Errorf("load the model of agent %s: %w", request.Agent.Name, err)
	}
	return loaded, nil
}

// openStore creates the child's transcript under the agent storage directory, in a
// directory of its own so two children never share history.
func (l subagentLauncher) openStore(config sessionfs.SessionConfig, request native.SubagentRequest) (*transcript.TurnStore, string, error) {
	if config.NativeSubagent == nil {
		return nil, "", errors.New("tools.native.subagent is not configured")
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory name: %w", err)
	}
	name := unsafeDirectoryCharacters.ReplaceAllString(request.CallID, "_")
	if name == "" {
		name = "call"
	}
	directory := filepath.Join(config.NativeSubagent.StorageDir, name+"-"+hex.EncodeToString(random[:]))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory: %w", err)
	}
	// A resumed run starts from a copy of its source transcript. The copy is made into this
	// run's own folder before the journal opens it, and the source is only read.
	if request.Seed != "" {
		if err := copyFileExclusive(request.Seed, filepath.Join(directory, "messages.jsonl")); err != nil {
			return nil, "", fmt.Errorf("copy the transcript of the resumed run: %w", err)
		}
	}
	journal, err := transcript.NewJournal(filepath.Join(directory, "messages.jsonl"), transcript.DurabilityMode(config.Persistence.Durability))
	if err != nil {
		return nil, "", fmt.Errorf("create sub-agent transcript: %w", err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(directory, "staging"), transcript.TurnStoreOptions{
		CompactionCachePath: filepath.Join(directory, "compaction-cache.json"),
		Logger:              l.resolver.logger,
		MaxItemBytes:        config.Persistence.MaxItemBytes,
		ResponseMetadata:    transcript.ResponseMetadataMode(config.Persistence.ResponseMetadata),
	})
	if err != nil {
		return nil, "", fmt.Errorf("create sub-agent turn store: %w", err)
	}
	return store, directory, nil
}

// copyFileExclusive copies src to dst, creating dst and failing if it already exists. The
// source is read only, and a copy never overwrites a file of another run. It streams, so a
// long transcript is not held in memory whole. As before, dst gets no fsync of its own; in the
// turn and strict durability modes the journal's next append syncs the file.
func copyFileExclusive(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// withCallerRun puts the calling run's id (callerRunKey) from the calling tool call's values
// onto ctx, so a profile_message made by a sub-agent, or by a sub-agent of a sub-agent (each
// level copies from the context of the call that started it), is keyed to the run that started
// the tree: a context_trigger acts once per caller run for the whole tree. Only a value is
// copied. ctx keeps its own cancellation and deadline (the sub-agent manager's, so stopping,
// timing out and resetting the child work as before), and an id already on ctx is kept.
func withCallerRun(ctx, callerValues context.Context) context.Context {
	if ctx.Value(callerRunKey{}) != nil {
		return ctx
	}
	if run, _ := callerValues.Value(callerRunKey{}).(uint64); run != 0 {
		return context.WithValue(ctx, callerRunKey{}, run)
	}
	return ctx
}
