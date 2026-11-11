package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

// RunKind says which kind of run a RunSpec starts (design section 3.4).
type RunKind int

const (
	// RunEphemeral is a run with a fresh transcript folder of its own: a sub-agent.
	RunEphemeral RunKind = 1
	// RunPersistent is a run of a profile over its own transcript started for a caller: a
	// profile_message to a profile of this server, local or sent through the remote API (R5b).
	// A run a user starts over HTTP does not go through StartRun.
	RunPersistent RunKind = 2
)

// RunTemplate is what an ephemeral run is started from: an agent definition over the
// configuration of the session whose catalog holds the subagent tool, which childConfig copies.
// The design's one loader for agents and profiles and its section 3.2 configuration order are
// not built, by decision: an agent's run and its tools keep two configurations (DECISIONS.md,
// "Unified agent runs: completion assessment").
type RunTemplate struct {
	Base  sessionfs.SessionConfig
	Agent sessionfs.NativeSubagentAgent
}

// RunSpec is one run to start (design section 3.4, phase R2). Template, Kind, Input, Caller and
// Seed are the design's fields. CallID, Tools, Execution, CallerValues and OnTranscript are
// what the subagent tool and its manager decide for an ephemeral run, and Profile is a persistent
// run's whole start: StartRun takes them as given. The catalog stays with the subagent tool and
// admission with the manager, the run tree and the run registry, by decision (DECISIONS.md,
// "Unified agent runs: completion assessment"), so these fields stay.
type RunSpec struct {
	Kind     RunKind
	Template RunTemplate
	// Input is the run's first user input, a JSON string.
	Input json.RawMessage
	// Caller is the calling run (R1). It is filled but not read today: Execution and
	// CallerValues are the source of the run's scope and of the caller's meter and reporter.
	// Kept reserved for U1; if U1 is declined, its removal (with SubagentRequest.Caller) is a
	// separate R7 cleanup.
	Caller tools.CallerContext
	// Seed, when set, is the transcript a resumed run starts from a copy of.
	Seed string
	// CallID names the run's folder and is reported by subagent_stop.
	CallID string
	// Tools is the run's catalog, built by the subagent tool.
	Tools *tools.Catalog
	// Execution is the run's own scope as the manager set it: depth, handle owner, root run,
	// environment, and the worktree when the run has one.
	Execution tools.ExecutionContext
	// CallerValues is the calling tool call's context, kept for its values only.
	CallerValues context.Context
	// OnTranscript, when set, is called once with the run's folder as soon as it exists.
	OnTranscript func(dir string)
	// Profile is a persistent run's whole start as the run registry takes it (R5b): the target
	// profile, the message, the chain, the hops, the wait and the callbacks of the caller. It is
	// not mapped onto Template, Input and Caller.
	Profile httpapi.ProfileRunRequest
}

// RunResult is what a finished run reports: its final answer (or, when it failed or was cut
// off, the newest text it wrote), the provider responses it used and their tokens.
type RunResult struct {
	Text   string
	Rounds int
	Tokens int64
}

// RunHandle is a run StartRun started (design section 3.4). An ephemeral run has ended by the
// time StartRun returns, and its handle holds the result. A persistent run goes on after
// StartRun returns, and its handle is the run as the run registry holds it.
type RunHandle struct {
	result  RunResult
	profile *httpapi.ProfileRun
}

// Result is an ephemeral run's result: its final answer, or, when it failed or was cut off, the
// newest text it wrote. It is the zero result for a nil handle and for a persistent run.
func (h *RunHandle) Result() RunResult {
	if h == nil {
		return RunResult{}
	}
	return h.result
}

// ProfileRun is a persistent run as the run registry holds it; nil for an ephemeral run.
func (h *RunHandle) ProfileRun() *httpapi.ProfileRun {
	if h == nil {
		return nil
	}
	return h.profile
}

// errProfileRunsUnset is the refusal of a persistent start before the HTTP handler gave the
// resolver its run registry (SetProfileRuns).
var errProfileRunsUnset = errors.New("profile messages are unavailable: the run registry is not set")

// StartRun starts spec and returns its handle.
//
// An ephemeral run runs in the calling goroutine to its end: the sub-agent manager that calls it
// through the launcher keeps admission, the slot, the timeout, the worktree, the run's MCP
// servers, the recovery of a panic and the run's end, in their order, so StartRun neither admits
// nor detaches it. Its handle carries the result also when err is set (a run that was cut off
// reports the text it wrote).
//
// A persistent run is handed to the run registry (R5b) as it was before StartRun took it: the
// registry admits it against the target's transcript (wait or fail fast by Profile.Wait), runs
// it on its own goroutine and ends it; the profile message queue, its wait-for graph and the
// chain check stay with the caller. err is the registry's, unchanged (httpapi.ErrSessionBusy for
// a busy target that is not waited for).
func (r *Resolver) StartRun(ctx context.Context, spec RunSpec) (*RunHandle, error) {
	switch spec.Kind {
	case RunEphemeral:
		result, err := r.runEphemeral(ctx, spec)
		return &RunHandle{result: result}, err
	case RunPersistent:
		starter := r.profileRunStarter()
		if starter == nil {
			return nil, errProfileRunsUnset
		}
		run, err := starter.StartProfileRun(ctx, spec.Profile)
		if err != nil {
			return nil, err
		}
		return &RunHandle{profile: run}, nil
	}
	return nil, fmt.Errorf("start run: run kind %d is not started by StartRun", spec.Kind)
}

// runEphemeral runs one child with a fresh transcript. The child is an ordinary session.Runner
// run: its own store, its own model client and the catalog the tool chose, with the question
// channel and the change listener of the call that started it (spec.Execution). It takes no
// checkpoint of its own: the calling run's checkpoint is taken before a call that may change files
// and covers what the child writes, and at every depth: the tool judges a child that may nest by
// the agents it can reach, so the call at the top takes it for a grandchild's writes too. It
// compacts its own transcript when the agent's trigger is reached, and sees the session's newest
// state-context block when the agent asks for it.
func (r *Resolver) runEphemeral(ctx context.Context, spec RunSpec) (RunResult, error) {
	base, agent := spec.Template.Base, spec.Template.Agent
	config, err := childConfig(spec.Template)
	if err != nil {
		return RunResult{}, err
	}
	apiKey, err := r.providerAPIKey(config)
	if err != nil {
		return RunResult{}, err
	}
	// The child's tools were built with the owner's model; its calls also hide the keys of the
	// models that may answer the child (the agent's, or the one its call chose, and the
	// model_fallback ones), beside its callers' (T-009), and so does its subagent_stop hook.
	// JoinDeny copies, so the caller's list is not changed.
	spec.Execution.ModelKeyEnvs = tools.JoinDeny(spec.Execution.ModelKeyEnvs, runModelKeys(config))
	store, dir, err := r.openEphemeralStore(config, spec)
	if err != nil {
		return RunResult{}, err
	}
	if spec.OnTranscript != nil {
		spec.OnTranscript(dir)
	}
	streamer, _ := r.runStreamer(config, apiKey)
	runner, err := session.NewRunner(store, streamer, session.ModelSettings{
		Model:       config.Model.Name,
		Request:     config.Model.Request,
		HostedTools: config.Model.HostedTools,
		PromptRole:  config.Model.PromptRole,
		PromptText:  config.Model.PromptText,
		// The children of one agent share a key: they share the agent's prompt and tools.
		PromptCacheKey: promptCacheKey(config.Model, base.MessagesPath+"\x00"+agent.Name),
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("create sub-agent runner: %w", err)
	}

	stateContext, err := r.stateContextFor(base, agent)
	if err != nil {
		return RunResult{}, err
	}
	// The child's spend adds into the calling run's meter, so the caller's max_run_cost_usd
	// and budget count it (the child's own context hangs off the sub-agent manager).
	// Its subagent_stop reports, and those of its own children, go to the calling run's
	// stream.
	var onReport func(session.HookReport)
	if spec.CallerValues != nil {
		if callerMeter := session.CostMeterFrom(spec.CallerValues); callerMeter != nil && session.CostMeterFrom(ctx) == nil {
			ctx = session.WithCostMeter(ctx, callerMeter)
		}
		if onReport = session.HookReporterFrom(spec.CallerValues); onReport != nil && session.HookReporterFrom(ctx) == nil {
			ctx = session.WithHookReporter(ctx, onReport)
		}
	}
	ctx, sweepChildren := native.WithChildGroups(ctx)
	defer sweepChildren()
	var rounds, tokens atomic.Int64
	// lastText is the newest assistant text of any round. A child that is cut off has
	// no final answer, but what it wrote last is the best partial work it has.
	var lastText atomic.Value
	result, runErr := runner.Run(ctx, spec.Input, session.RunOptions{
		ToolCatalog:          spec.Tools,
		ImageResizer:         newImageResizer(config.ImageResize),
		ImageStore:           newImageStore(config.ImageStore),
		ToolExecution:        spec.Execution,
		MaxToolRounds:        int(agent.MaxToolRounds),
		MaxRunTokens:         agent.MaxRunTokens,
		MaxParallelToolCalls: int(base.MaxParallelToolCalls),
		ToolCallScheduling:   base.ToolCallScheduling,
		LeafBudget:           r.leafBudget,
		StateContext:         stateContext,
		AutomaticCompaction:  compactionFor(base, agent),
		CompactHistory:       compactWhileWorking(store),
		OnResponseCommitted: func(event json.RawMessage, _ transcript.CommitResult) {
			rounds.Add(1)
			tokens.Add(session.ResponseTokens(event))
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				lastText.Store(text)
			}
		},
	})
	outcome := RunResult{Rounds: int(rounds.Load()), Tokens: tokens.Load()}
	if runErr != nil {
		outcome.Text, _ = lastText.Load().(string)
	} else {
		outcome.Text = native.SubagentAnswerText(result.Event)
	}
	// A background child may end after the caller's last round, which would have recorded
	// its spend in the caller's budget.
	if callerMeter := session.CostMeterFrom(ctx); callerMeter != nil {
		if err := callerMeter.RecordUnrecordedSpend(time.Now()); err != nil && r.logger != nil {
			r.logger.Warn("record a sub-agent's spend in the budget", "agent", agent.Name, "error", err)
		}
	}
	r.subagentStopHook(ctx, spec, outcome, runErr, onReport)
	return outcome, runErr
}
