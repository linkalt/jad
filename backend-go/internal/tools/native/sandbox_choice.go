package native

import (
	"fmt"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A call of subagent or profile_message may pick, with its sandbox argument, one of the sandbox
// templates its tool's sandbox_choice lists (docs/isolation-sandbox-design.md §15). A choice
// only narrows: the run it starts is confined by the chosen template and by everything that
// confined it before (the agent's or profile's own sandbox, the session's, the caller's run),
// and a choice that would widen any of them is refused, never cut.

// sandboxUnavailableCode is the error code of a refused sandbox argument.
const sandboxUnavailableCode = "sandbox_unavailable"

// sandboxChoiceText is the part of a tool description that names the sandboxes a call may pick,
// each with its template's description; empty when the tool offers none.
func sandboxChoiceText(choices []sessionfs.SandboxChoice) string {
	if len(choices) == 0 {
		return ""
	}
	parts := make([]string, len(choices))
	for index, choice := range choices {
		parts[index] = choice.Name
		if choice.Description != "" {
			parts[index] += " (" + choice.Description + ")"
		}
	}
	return " Sandboxes for sandbox: " + strings.Join(parts, "; ") + "."
}

// sandboxProblem is a refused sandbox argument: message for the model, and widens (the path or
// setting that would widen the run) when the choice was refused for that.
type sandboxProblem struct {
	message, widens string
}

// sandboxUnavailable is the error result of a refused sandbox argument; an unknown name lists
// the choices, a widening one names what it would widen.
func sandboxUnavailable(callID string, problem *sandboxProblem, choices []sessionfs.SandboxChoice) string {
	var information []tools.Information
	if problem.widens != "" {
		information = append(information, tools.Information{Key: "widens", Value: problem.widens})
	} else {
		information = append(information, tools.Information{Key: "available", Value: strings.Join(sessionfs.SandboxChoiceNames(choices), ", ")})
	}
	return tools.ExecutionError(callID, tools.StatusInvalidInput, sandboxUnavailableCode, problem.message, information...)
}

// lookupSandboxChoice is the choice named name among choices; what offers them is "sub-agents"
// or "profile messages", for the message when there is none.
func lookupSandboxChoice(name string, choices []sessionfs.SandboxChoice, what string) (sessionfs.SandboxChoice, *sandboxProblem) {
	if len(choices) == 0 {
		return sessionfs.SandboxChoice{}, &sandboxProblem{message: "This session offers no sandbox choice for " + what}
	}
	choice, found := sessionfs.FindSandboxChoice(choices, name)
	if !found {
		return sessionfs.SandboxChoice{}, &sandboxProblem{message: fmt.Sprintf("Sandbox %s is not one this session offers", name)}
	}
	return choice, nil
}

// widensProblem is the problem of a choice that NarrowSandbox refused.
func widensProblem(name string, err error) *sandboxProblem {
	widens := strings.TrimPrefix(err.Error(), sessionfs.SandboxWidensPrefix)
	return &sandboxProblem{message: fmt.Sprintf("Sandbox %s reaches more than the run would otherwise get, so it cannot be used here: %s", name, widens), widens: widens}
}

// withCallSandbox is agent as a run in sandbox choice runs it: its sandbox narrowed by the
// choice (the agent's own, already narrowed by the session's; with none, the choice alone,
// which childEnvironment then narrows by the calling run's), and, when the template sets
// isolation.mode worktree, in a worktree of its own (base from the template). A choice cannot
// take an isolated agent out of its worktree.
func withCallSandbox(agent sessionfs.NativeSubagentAgent, choice sessionfs.SandboxChoice) (sessionfs.NativeSubagentAgent, *sandboxProblem) {
	switch choice.Isolation.Mode {
	case sessionfs.SandboxIsolationNone:
		if agent.Isolated() {
			return agent, &sandboxProblem{
				message: fmt.Sprintf("Sandbox %s sets isolation.mode none, and agent %s works in a git worktree of its own", choice.Name, agent.Name),
				widens:  "isolation.mode none",
			}
		}
	case sessionfs.SandboxIsolationWorktree:
		// An agent that cannot change files has nothing a worktree would keep apart.
		if agent.ChangesFiles() {
			agent.Isolation = sessionfs.SubagentIsolationWorktree
			agent.IsolationBase = choice.Isolation.Base
			agent.IsolationMaterialize = choice.Isolation.Materialize
			agent.IsolationShare = choice.Isolation.SharePaths
		}
	}
	if choice.Sandbox != nil {
		narrowed, err := sessionfs.NarrowSandbox(agent.Sandbox, choice.Sandbox, nil, nil)
		if err != nil {
			return agent, widensProblem(choice.Name, err)
		}
		agent.Sandbox = narrowed
	}
	return agent, nil
}

// callSandboxName is the sandbox a call runs in: its sandbox argument, or, for a resume that
// names none, the one the source run was started with (D22). Empty runs the agent as it is.
func (s *subagentTool) callSandboxName(arguments map[string]any) string {
	if name, _ := arguments["sandbox"].(string); name != "" {
		return name
	}
	if resume, _ := arguments["resume"].(string); resume != "" {
		if run := s.manager.byID(resume); run != nil {
			return run.view().sandbox
		}
	}
	return ""
}

// chooseSandbox is agent for a call that runs in sandbox name: the agent narrowed to that
// environment, whose catalog childCatalog builds for the choice. An empty name keeps the agent
// as it is.
func (s *subagentTool) chooseSandbox(agent subagentAgent, name string) (subagentAgent, *sandboxProblem) {
	if name == "" {
		return agent, nil
	}
	choice, problem := lookupSandboxChoice(name, s.settings.SandboxChoices, "sub-agents")
	if problem != nil {
		return agent, problem
	}
	config, problem := withCallSandbox(agent.config, choice)
	if problem != nil {
		return agent, problem
	}
	// A worktree is asked for, and withCallSandbox keeps an agent out of it when it has no native tool
	// that changes files. That is right for a reader, but not for an agent that changes files through
	// MCP tools alone: its servers cannot be moved into a worktree here, so it would run in the
	// session's own folder. Say so instead of dropping what was asked for.
	if choice.Isolation.Mode == sessionfs.SandboxIsolationWorktree && !config.Isolated() && agent.mcpWrites() {
		return agent, &sandboxProblem{message: fmt.Sprintf("Sandbox %s sets isolation.mode worktree, but agent %s changes files only through MCP tools, which cannot be moved into a worktree, so it cannot run in one", choice.Name, agent.config.Name)}
	}
	return s.choiceAgent(agent, config), nil
}

// choiceAgent is agent running with config, the agent narrowed by a sandbox choice. Its catalogs
// are the agent's own set (childCatalog), keyed by the choice's name: the exec and tool_smith
// descriptions tell the chosen environment, and an agent the choice put in a worktree reads how
// git works there. They are built on the first call that picks the choice and kept until the
// agent's inputs change (bindMCP), so a choice nobody makes costs nothing (D23).
func (s *subagentTool) choiceAgent(agent subagentAgent, config sessionfs.NativeSubagentAgent) subagentAgent {
	variant := agent
	variant.config = config
	variant.background = backgroundSafe(s.settings, variant, s.async)
	return variant
}

// sandboxText is the line an async summary carries for a run started in a sandbox choice.
func sandboxText(run subagentRunView) string {
	if run.sandbox == "" {
		return ""
	}
	return "sandbox: " + run.sandbox
}
