package app

import (
	"errors"
	"slices"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// A profile_message target runs in its own session, but never outside the caller's sandbox
// (docs/isolation-decisions.md D20): its sandbox is narrowed by the caller's, compared with the
// paths each session fills in ({workspace} of the caller is not the target's), a target with
// no sandbox takes the caller's with the caller's paths, and a target whose own sandbox would
// reach more is refused. The narrowing is applied to the runner of the target run, after the
// target's settings are loaded, so a cached config of the target is never the narrowed one.
//
// Not covered: the target's stdio MCP servers are its session's (D10), and a remote target
// runs on its own server, outside any sandbox of this one.

// confineTarget is target narrowed by the caller's sandbox, then by the sandbox template the
// call picked (chooseTargetSandbox); caller nil and choice nil leave it as it is.
func confineTarget(caller *sessionfs.SandboxSettings, callerPlace sessionfs.SandboxPlace, choice *sessionfs.SandboxChoice, target sessionfs.SessionConfig) (sessionfs.SessionConfig, error) {
	target, err := confineByCaller(caller, callerPlace, target)
	if err != nil || choice == nil {
		return target, err
	}
	return chooseTargetSandbox(choice, target)
}

// choiceWidensError is a sandbox choice that would widen the target's run.
type choiceWidensError struct {
	choice string
	err    error
}

func (e *choiceWidensError) Error() string { return e.err.Error() }

// chooseTargetSandbox is target with the sandbox template choice narrowing its sandbox (the
// target's own, already narrowed by the caller's): the template's placeholders stand for the
// target's places, as the run fills them in. Its isolation.mode worktree makes the message's
// run work in a worktree of its own; none cannot take a target out of its worktree.
func chooseTargetSandbox(choice *sessionfs.SandboxChoice, target sessionfs.SessionConfig) (sessionfs.SessionConfig, error) {
	switch choice.Isolation.Mode {
	case sessionfs.SandboxIsolationNone:
		if target.Isolation.Mode == sessionfs.SandboxIsolationWorktree {
			return sessionfs.SessionConfig{}, &choiceWidensError{choice: choice.Name, err: errors.New(sessionfs.SandboxWidensPrefix + "isolation.mode none, and the profile runs a message in a git worktree of its own")}
		}
	case sessionfs.SandboxIsolationWorktree:
		target.Isolation = choice.Isolation
	}
	if choice.Sandbox == nil {
		return target, nil
	}
	place := sessionfs.SandboxPlace{Workspace: target.WorkingDir, CWD: target.WorkingDir, SessionDir: target.StateDir}
	narrowed, err := sessionfs.NarrowSandbox(target.Sandbox, choice.Sandbox, place.Expander(), place.Expander())
	if err != nil {
		return sessionfs.SessionConfig{}, &choiceWidensError{choice: choice.Name, err: err}
	}
	// The template's read-only paths are the caller's session's; the target's own session
	// file stays read-only too, as for a target that inherits the caller's sandbox.
	if target.SettingPath != "" && !slices.Contains(narrowed.ReadOnlyPaths, target.SettingPath) {
		copied := *narrowed
		copied.ReadOnlyPaths = append(append([]string(nil), narrowed.ReadOnlyPaths...), target.SettingPath)
		narrowed = &copied
	}
	return withRunSandbox(target, narrowed), nil
}

// confineByCaller is target narrowed by the caller's sandbox; caller nil leaves it as it is.
func confineByCaller(caller *sessionfs.SandboxSettings, callerPlace sessionfs.SandboxPlace, target sessionfs.SessionConfig) (sessionfs.SessionConfig, error) {
	if caller == nil {
		return target, nil
	}
	callerExpand := callerPlace.Expander()
	var confined *sessionfs.SandboxSettings
	if target.Sandbox == nil {
		confined = sessionfs.InheritSandbox(caller, callerExpand)
		// The caller's read-only paths are its session's; the target's own session file is
		// kept read-only too (the catalogs and the state folder are the layout's, shared).
		if target.SettingPath != "" && !slices.Contains(confined.ReadOnlyPaths, target.SettingPath) {
			confined.ReadOnlyPaths = append(append([]string(nil), confined.ReadOnlyPaths...), target.SettingPath)
		}
	} else {
		targetPlace := sessionfs.SandboxPlace{Workspace: target.WorkingDir, CWD: target.WorkingDir, SessionDir: target.StateDir}
		var err error
		if confined, err = sessionfs.NarrowSandbox(caller, target.Sandbox, callerExpand, targetPlace.Expander()); err != nil {
			return sessionfs.SessionConfig{}, err
		}
	}
	return withRunSandbox(target, confined), nil
}

// withRunSandbox is config with sandbox in place of its own, handed to the tools it covers
// as the loader does, on copies of their settings so the loaded config is not changed.
func withRunSandbox(config sessionfs.SessionConfig, sandbox *sessionfs.SandboxSettings) sessionfs.SessionConfig {
	covered := func(tool string) *sessionfs.SandboxSettings {
		if sandbox.Covers(tool) {
			return sandbox
		}
		return nil
	}
	config.Sandbox = sandbox
	if config.NativeExec != nil {
		copied := *config.NativeExec
		copied.Child.Sandbox = covered("exec")
		config.NativeExec = &copied
	}
	if config.NativeToolSmith != nil {
		copied := *config.NativeToolSmith
		copied.Child.Sandbox = covered("tool_smith")
		config.NativeToolSmith = &copied
	}
	if config.NativeLSP != nil {
		copied := *config.NativeLSP
		copied.Sandbox = covered("lsp")
		config.NativeLSP = &copied
	}
	config.Hooks.Sandbox = covered("hooks")
	return config
}

// targetConfiner is the Confine hook of a profile_message run: the target's runner with its
// config narrowed by the caller's sandbox and the call's sandbox choice. A runner it cannot
// narrow fails the start.
func targetConfiner(caller *sessionfs.SandboxSettings, callerPlace sessionfs.SandboxPlace, choice *sessionfs.SandboxChoice) func(httpapi.RunSession) (httpapi.RunSession, error) {
	return func(runner httpapi.RunSession) (httpapi.RunSession, error) {
		bound, ok := runner.(boundRunner)
		if !ok {
			return nil, errors.New("the target's run cannot be confined to the caller's sandbox")
		}
		config, err := confineTarget(caller, callerPlace, choice, bound.config)
		if err != nil {
			return nil, profileSandboxError(err)
		}
		bound.config = config
		return bound, nil
	}
}

// profileSandboxError is the native error of a target that confineTarget refused: a sandbox
// choice that would widen the run names the choice.
func profileSandboxError(err error) *native.ProfileSandboxError {
	refused := &native.ProfileSandboxError{Message: err.Error()}
	var choice *choiceWidensError
	if errors.As(err, &choice) {
		refused.Choice = choice.choice
	}
	return refused
}
