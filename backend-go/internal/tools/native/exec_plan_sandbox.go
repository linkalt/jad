package native

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// An exec call that says read_only, and every exec call of a plan-mode run, starts in a
// sandbox that keeps the project read-only (docs/isolation-decisions.md D2, D3): the plan-mode
// form of the sandbox that covers exec, or, when none does, the fallback the settings carry
// (NativeBashSettings.PlanSandbox), wherever it starts on this host. read_only counts, and plan
// mode runs exec, only where such a sandbox holds the call.

// planSandboxProbe starts a trial child in a sandbox; tests replace it.
var planSandboxProbe = ProbeSandbox

// planSandboxProbes holds one trial start per fallback sandbox (its JSON without the
// session's read-only paths, which change no trial start), made the first time a call needs
// it and kept for the life of the process.
var planSandboxProbes sync.Map

type planSandboxProbeResult struct {
	once sync.Once
	ok   bool
}

// planSandboxWorks reports whether the fallback sandbox starts on this host. The first answer
// is kept; a host that cannot start it is logged once.
func planSandboxWorks(sandbox *sessionfs.SandboxSettings) bool {
	if !sandbox.EnforcesPlanMode() {
		return false
	}
	probed := *sandbox.ForPlanMode()
	probed.ReadOnlyPaths = nil
	key, err := json.Marshal(probed)
	if err != nil {
		return false
	}
	entry, _ := planSandboxProbes.LoadOrStore(string(key), &planSandboxProbeResult{})
	result := entry.(*planSandboxProbeResult)
	result.once.Do(func() {
		err := planSandboxProbe(context.Background(), &probed, os.TempDir())
		result.ok = err == nil
		if err != nil {
			slog.Default().Warn("exec read_only and plan-mode calls cannot be confined on this host, so read_only does not count and plan mode refuses exec code",
				"backend", probed.Backend, "executable", probed.Executable, "error", err)
		}
	})
	return result.ok
}

// childCallSandbox is callSandbox for a tool whose settings may carry a fallback for its
// plan-mode calls: a call of a plan-mode run, or a read_only call, that no sandbox covers starts
// in settings.PlanSandbox where that works on this host.
func childCallSandbox(tool string, settings sessionfs.NativeBashSettings, execution tools.ExecutionContext) *sessionfs.SandboxSettings {
	sandbox := callSandbox(tool, settings.Sandbox, execution)
	if sandbox == nil && execution.PlanMode && planSandboxWorks(settings.PlanSandbox) {
		return settings.PlanSandbox.ForPlanMode()
	}
	return sandbox
}

// execReadOnlyEnforced reports whether an exec call of these settings that starts in the
// plan-mode environment cannot write the project: the sandbox that covers exec is bwrap or
// seatbelt, or none covers it and the fallback starts on this host. The call's own run may
// start in another sandbox (a sub-agent's, a profile_message caller's); execPlanModeRefusal
// checks that one before a plan-mode call runs.
func execReadOnlyEnforced(settings sessionfs.NativeExecSettings) bool {
	if settings.Child.Sandbox != nil {
		return settings.ReadOnlyEnforced()
	}
	return planSandboxWorks(settings.Child.PlanSandbox)
}

// execPlanModeRefusal is exec's Tool.PlanModeRefusal: in plan mode every exec call starts in
// the plan-mode environment, so one that runs code is refused only where that environment
// does not keep the project read-only (no bwrap or seatbelt works on this host, or exec runs
// in a command sandbox, which jad cannot narrow). A call without code only lists helpers.
func execPlanModeRefusal(settings sessionfs.NativeExecSettings) func(map[string]any, *tools.ExecutionContext) string {
	return func(arguments map[string]any, execution *tools.ExecutionContext) string {
		if !execGiven(arguments, "code") {
			return ""
		}
		var sandbox *sessionfs.SandboxSettings
		if execution == nil {
			if execReadOnlyEnforced(settings) {
				return ""
			}
			sandbox = settings.Child.Sandbox
		} else {
			call := *execution
			call.PlanMode = true
			if sandbox = childCallSandbox("exec", settings.Child, call); sandbox.EnforcesPlanMode() {
				return ""
			}
		}
		if sandbox != nil && sandbox.Backend == sessionfs.SandboxBackendCommand {
			return "Plan mode: exec cannot run code in this turn, because exec runs in a command sandbox, which jad cannot make read-only. Plan with the other tools, and put in the plan what you would run."
		}
		return "Plan mode: exec cannot run code on this host, because no bwrap or seatbelt sandbox works here to keep the call from writing the project. Plan with the other tools, and put in the plan what you would run."
	}
}
