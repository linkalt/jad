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

// Run starts one child with a fresh transcript and returns its final answer. It is the
// subagent tool's adapter onto Resolver.StartRun (design section 3.4, phase R2): it turns the
// request into an ephemeral RunSpec over the calling session's configuration, and the result back
// into the tool's. The manager that calls it keeps the slot, the timeout, the worktree, the
// run's MCP servers, the panic boundary and the run's end; StartRun runs in its goroutine.
func (l subagentLauncher) Run(ctx context.Context, request native.SubagentRequest) (native.SubagentResult, error) {
	spec, err := ephemeralRunSpec(l.parent, request)
	if err != nil {
		return native.SubagentResult{}, err
	}
	handle, err := l.resolver.StartRun(ctx, spec)
	result := handle.Result()
	return native.SubagentResult{Text: result.Text, Rounds: result.Rounds, Tokens: result.Tokens}, err
}

// ephemeralRunSpec is the RunSpec of one subagent request: the agent over the calling session's
// configuration as its template, the task as its input, and every other field of the request
// as it is. Encoding a string cannot fail, so moving the encoding ahead of the store (the
// launcher encoded after it) changes nothing visible.
func ephemeralRunSpec(parent sessionfs.SessionConfig, request native.SubagentRequest) (RunSpec, error) {
	input, err := json.Marshal(request.Task)
	if err != nil {
		return RunSpec{}, fmt.Errorf("encode sub-agent task: %w", err)
	}
	return RunSpec{
		Kind:         RunEphemeral,
		Template:     RunTemplate{Base: parent, Agent: request.Agent},
		Input:        input,
		Caller:       request.Caller,
		Seed:         request.Seed,
		CallID:       request.CallID,
		Tools:        request.Tools,
		Execution:    request.Execution,
		CallerValues: request.CallerValues,
		OnTranscript: request.OnTranscript,
	}, nil
}

// subagentStopHook reports the end of one ephemeral run to the calling session's
// subagent_stop commands. Like run_end, it runs on a context the stop cannot cancel, so a child that was
// stopped or timed out still reports. Its reports go to the server log and to onReport, the
// calling run's reporter; a background child that ends after that run reaches its log only,
// since the ended run's stream has no reader left.
func (r *Resolver) subagentStopHook(ctx context.Context, spec RunSpec, outcome RunResult, runErr error, onReport func(session.HookReport)) {
	hooks := spec.Template.Base.Hooks
	if !hooks.Enabled || len(hooks.SubagentStop) == 0 {
		return
	}
	event := native.RunEvent{
		Event:  native.HookEventSubagentStop,
		Agent:  spec.Template.Agent.Name,
		CallID: spec.CallID,
		Rounds: outcome.Rounds,
		Tokens: outcome.Tokens,
		Answer: outcome.Text,
		// The caller's commands, for the end of a run whose model may not be the caller's: they hide
		// that model's key too (T-009).
		ModelKeyEnvs: spec.Execution.ModelKeyEnvs,
	}
	switch {
	case runErr == nil:
		event.Outcome = native.RunOutcomeOK
	case errors.Is(runErr, context.Canceled):
		event.Outcome = native.RunOutcomeCancelled
	default:
		event.Outcome, event.Error = native.RunOutcomeError, runErr.Error()
	}
	reference, _, _ := strings.Cut(spec.Execution.SessionKey, "\x00")
	fireSessionHook(context.WithoutCancel(ctx), r.logger, reference, spec.Template.Base, event, onReport)
}

// compactionFor is the automatic compaction of one child. The agent's own trigger and
// switch win; what it leaves out comes from the session's compaction settings, so a
// cheap model can compact at a different size than the master.
func compactionFor(base sessionfs.SessionConfig, agent sessionfs.NativeSubagentAgent) *session.AutomaticCompactionSettings {
	compaction := base.Compaction
	enabled := compaction.AutoEnabled
	if agent.CompactionTriggerTokens > 0 {
		enabled = true
		compaction.TriggerTokens = agent.CompactionTriggerTokens
	}
	if agent.HasAutoCompaction {
		enabled = agent.AutoCompaction
	}
	return &session.AutomaticCompactionSettings{
		Enabled:       enabled,
		TriggerTokens: compaction.TriggerTokens,
		TurnType:      compaction.TurnType,
		KeepTurns:     compaction.KeepTurns,
		KeepToolPairs: compaction.KeepToolPairs,
		BackupMode:    compaction.BackupMode,
		AppendLimit:   compaction.AppendLimit,
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
func (r *Resolver) stateContextFor(base sessionfs.SessionConfig, agent sessionfs.NativeSubagentAgent) ([]session.StateContextPrompt, error) {
	if !agent.StateContext {
		return nil, nil
	}
	shared, release, err := r.sharedStore(base)
	if err != nil {
		return nil, fmt.Errorf("read the session state context: %w", err)
	}
	defer release()
	entries, err := boundRunner{resolver: r, shared: shared, config: base}.stateContextEntries()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	newest := entries[len(entries)-1]
	return []session.StateContextPrompt{{Role: newest.Role, Text: newest.Text}}, nil
}

// childConfig is the template's base (the calling session's configuration) with the agent's
// model and instructions. It stays the run's configuration: the design's section 3.2 order is
// not built, because a toolset agent's run and its tools read four fields with different values
// (agent_config_fields_test.go; DECISIONS.md, "Unified agent runs: completion assessment").
func childConfig(template RunTemplate) (sessionfs.SessionConfig, error) {
	config := template.Base
	if template.Agent.ModelPath != "" {
		config.ModelPath = template.Agent.ModelPath
		config.ReasoningEffort = template.Agent.ReasoningEffort
	}
	// The parent's prompt is not the child's: it gets its own instructions and
	// documents, or none.
	config.Model.PromptText = template.Agent.PromptText
	loaded, err := config.WithModel()
	if err != nil {
		return sessionfs.SessionConfig{}, fmt.Errorf("load the model of agent %s: %w", template.Agent.Name, err)
	}
	return loaded, nil
}

// openEphemeralStore creates the child's transcript under the agent storage directory, in a
// directory of its own so two children never share history.
func (r *Resolver) openEphemeralStore(config sessionfs.SessionConfig, spec RunSpec) (*transcript.TurnStore, string, error) {
	if config.NativeSubagent == nil {
		return nil, "", errors.New("tools.native.subagent is not configured")
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory name: %w", err)
	}
	name := unsafeDirectoryCharacters.ReplaceAllString(spec.CallID, "_")
	if name == "" {
		name = "call"
	}
	directory := filepath.Join(config.NativeSubagent.StorageDir, name+"-"+hex.EncodeToString(random[:]))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, "", fmt.Errorf("create sub-agent directory: %w", err)
	}
	// A resumed run starts from a copy of its source transcript. The copy is made into this
	// run's own folder before the journal opens it, and the source is only read.
	if spec.Seed != "" {
		if err := copyFileExclusive(spec.Seed, filepath.Join(directory, "messages.jsonl")); err != nil {
			return nil, "", fmt.Errorf("copy the transcript of the resumed run: %w", err)
		}
	}
	journal, err := transcript.NewJournal(filepath.Join(directory, "messages.jsonl"), transcript.DurabilityMode(config.Persistence.Durability))
	if err != nil {
		return nil, "", fmt.Errorf("create sub-agent transcript: %w", err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(directory, "staging"), transcript.TurnStoreOptions{
		CompactionCachePath: filepath.Join(directory, "compaction-cache.json"),
		Logger:              r.logger,
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
