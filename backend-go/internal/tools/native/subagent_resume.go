package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/transcript"
)

// subagentResumeFile is the transcript a resumed run starts from, the file its copy is made of.
const subagentResumeFile = "messages.jsonl"

// resumeSource is the finished run a resume continues from, checked before a new run is made.
type resumeSource struct {
	run   *subagentRun
	view  subagentRunView
	dir   string
	agent string
}

// resumeProblem is a refusal of a resume: the error code and the text the model reads.
type resumeProblem struct {
	code, message string
}

// checkResumeSource looks up the run to resume and refuses the ones a resume may not continue.
// A run another session started is reported as unknown, so an id cannot be probed. A local
// resume (caller "") must be the session's own run; a remote resume (caller "remote:<token id>")
// must be a run that the same token started, and a local run has no caller so it is never
// resumed remotely.
func (s *subagentTool) checkResumeSource(id, owner, caller, agent string) (resumeSource, *resumeProblem) {
	run := s.manager.byID(id)
	if run == nil || !resumeMayUse(run, owner, caller) {
		return resumeSource{}, &resumeProblem{"unknown_run", fmt.Sprintf("No sub-agent run %s in this session", id)}
	}
	view := run.view()
	switch view.state {
	case subagentRunDone, subagentRunFailed, subagentRunStopped, subagentRunTimedOut:
	case subagentRunQueued, subagentRunRunning:
		return resumeSource{}, &resumeProblem{"resume_active", fmt.Sprintf("Run %s is still %s: wait for it (async) before resuming it", id, view.state)}
	default:
		return resumeSource{}, &resumeProblem{"resume_lost", fmt.Sprintf("Run %s was lost when the server stopped, so it cannot be resumed", id)}
	}
	if agent != "" && agent != view.agent {
		return resumeSource{}, &resumeProblem{"resume_agent_mismatch", fmt.Sprintf("Run %s used agent %s; a resume must name that agent or omit agent", id, view.agent)}
	}
	if view.dir == "" {
		return resumeSource{}, &resumeProblem{"unknown_run", fmt.Sprintf("The transcript of run %s is gone: it cannot be resumed", id)}
	}
	return resumeSource{run: run, view: view, dir: view.dir, agent: view.agent}, nil
}

// resumeMayUse is the identity check of a resume: the owner for a local call, the caller for a
// remote one.
func resumeMayUse(run *subagentRun, owner, caller string) bool {
	if caller == "" {
		return run.owner == owner && run.caller == ""
	}
	return run.caller == caller
}

// checkResumeTranscript reads the source transcript and refuses one that is gone or whose last
// tool call was never answered: a child started from it would begin with an open call.
func checkResumeTranscript(id string, source resumeSource) (string, *resumeProblem) {
	path := filepath.Join(source.dir, subagentResumeFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", &resumeProblem{"unknown_run", fmt.Sprintf("The transcript of run %s is gone: it cannot be resumed", id)}
	}
	if transcript.HasUnansweredToolCall(data) {
		return "", &resumeProblem{"resume_unanswered_call", fmt.Sprintf("Run %s stopped in a tool call that never got its answer, so it cannot be resumed", id)}
	}
	return path, nil
}

// startResume starts a new run that continues from a finished run. The source run and its folder
// are not changed: the new run gets its own id and a copy of the transcript. A local call and a
// remote one (the caller on its context, see WithRemoteCaller) share this core; only the identity
// check differs, and the new run records the caller it was started for.
func (s *subagentTool) startResume(ctx context.Context, callID, id, task string, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
	caller := remoteCallerOf(ctx)
	agentName, _ := arguments["agent"].(string)
	source, problem := s.checkResumeSource(id, execution.SessionKey, caller, agentName)
	if problem != nil {
		return refuseResume(callID, problem)
	}
	seed, problem := checkResumeTranscript(id, source)
	if problem != nil {
		return refuseResume(callID, problem)
	}
	branch := ""
	if record := source.view.worktree; record != nil && record.Changed {
		branch = record.Branch
	}
	return s.startRunFrom(ctx, callID, task, arguments, execution, resumeStart{from: id, seed: seed, agent: source.agent, config: source.run.config, branch: branch})
}

// resumeStart is what a run that continues another carries: the source run's id, its transcript
// path, and the agent it must use. branch is the source run's worktree branch, when it
// left one: a resume of an agent with isolation worktree starts its worktree there.
type resumeStart struct {
	from, seed, agent string
	config            string
	branch            string
}

// withAgent returns the call's arguments with agent set to name, so a resume that omits agent
// runs the source run's agent.
func withAgent(arguments map[string]any, name string) map[string]any {
	copied := make(map[string]any, len(arguments)+1)
	for key, value := range arguments {
		copied[key] = value
	}
	copied["agent"] = name
	return copied
}

// joinNotes joins the notes a result carries, skipping the empty ones.
func joinNotes(notes ...string) string {
	var kept []string
	for _, note := range notes {
		if note != "" {
			kept = append(kept, note)
		}
	}
	return strings.Join(kept, " ")
}

// configNoteFor is the config note of a resume: the source run's fingerprint against the new run's.
func configNoteFor(from resumeStart, config string) string {
	if from.from == "" {
		return ""
	}
	return configChangedNote(from.config, config)
}

// refuseResume is the error a refused resume returns: every refusal is invalid input, so the
// model can correct the call.
func refuseResume(callID string, problem *resumeProblem) (string, error) {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, problem.code, problem.message), nil
}
