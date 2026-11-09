package native

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// WithPostEditHooks returns tool unchanged unless hooks has checks to run. Otherwise it
// returns a copy whose Execute runs the matching checks after the call changed a file
// and appends their report to the result. Only a call that changed a file runs a check:
// a failed call, a rejected path and an unchanged file do not. The check never changes
// the status of the call; its outcome is text for the model.
func WithPostEditHooks(tool tools.Tool, hooks sessionfs.HooksSettings, apiKeyEnv string) tools.Tool {
	if !hooks.PostEditActive() {
		return tool
	}
	inner := tool.Execute
	tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		// apply_patch changes several files in one call; each changed (not deleted) file is
		// checked once, in the order it was changed.
		var changed []string
		outer := execution.OnFileChange
		execution.OnFileChange = func(path, action string) {
			if action != "deleted" && !slices.Contains(changed, path) {
				changed = append(changed, path)
			}
			if outer != nil {
				outer(path, action)
			}
		}
		result, err := inner(ctx, arguments, execution)
		if err != nil || len(changed) == 0 {
			return result, err
		}
		var reports []string
		for _, path := range changed {
			if report := runPostEditHooks(ctx, hooks, apiKeyEnv, execution, path); report != "" {
				reports = append(reports, report)
			}
		}
		if len(reports) == 0 {
			return result, nil
		}
		return tools.AppendContent(result, tools.Content{Text: strings.Join(reports, "\n")})
	}
	return tool
}

// runPostEditHooks runs every check whose extension matches the file and returns their
// report, or "" when none matched or every matching check passed with on_failure_only.
func runPostEditHooks(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv string, execution tools.ExecutionContext, changed string) string {
	file := changed
	if !filepath.IsAbs(file) {
		file = filepath.Join(execution.WorkspaceRoot, filepath.FromSlash(changed))
	}
	extension := filepath.Ext(file)
	var sections []string
	for _, hook := range hooks.PostEdit {
		if !hookMatchesExtension(hook, extension) {
			continue
		}
		if section := runPostEditHook(ctx, hook, hooks, apiKeyEnv, execution, file); section != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return "post-edit checks for " + changed + "\n" + strings.Join(sections, "\n")
}

func hookMatchesExtension(hook sessionfs.PostEditHook, extension string) bool {
	for _, wanted := range hook.Ext {
		if strings.EqualFold(wanted, extension) {
			return true
		}
	}
	return false
}

func runPostEditHook(ctx context.Context, hook sessionfs.PostEditHook, hooks sessionfs.HooksSettings, apiKeyEnv string, execution tools.ExecutionContext, file string) string {
	workspaceRoot := execution.WorkspaceRoot
	cwd, found := hookWorkingDirectory(hook, workspaceRoot, file)
	if !found {
		marker := strings.TrimPrefix(hook.Cwd, sessionfs.HookCwdNearestPrefix)
		if !sessionfs.PathWithin(workspaceRoot, file) {
			return "[" + hook.Name + "] skipped: the file is outside the workspace, so nearest:" + marker + " has nowhere to look"
		}
		return "[" + hook.Name + "] skipped: no " + marker + " between the file and the workspace root"
	}
	return runHookChild(ctx, hooks, apiKeyEnv, workspaceRoot, hookChild{
		name:          hook.Name,
		setting:       "hooks.post_edit[" + hook.Name + "].argv",
		argv:          hookArguments(hook, cwd, file),
		cwd:           cwd,
		watched:       file,
		onFailureOnly: hook.OnFailureOnly,
		call:          &execution,
	})
}

// hookChild is one command a hook starts: what to run, where, and what to report about it.
type hookChild struct {
	name    string
	setting string
	argv    []string
	cwd     string
	stdin   string
	// watched is a file whose change by the command is reported; empty watches nothing.
	watched       string
	onFailureOnly bool
	// rawStdout keeps the long lines of the standard output whole, for a command whose
	// output is read as JSON; the output is still cut to hooks.max_output_bytes.
	rawStdout bool
	// call is the tool call the command runs for, nil for a run event. A command of a call
	// starts in that call's sandbox: in a plan-mode run (or for a read_only exec call) the
	// sandbox's plan-mode form, with the call's own temp folder.
	call *tools.ExecutionContext
}

// runHookChild runs one hook command under the hooks' own child-process settings and
// returns its section of the report, or "" for a passing on_failure_only command.
func runHookChild(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv, workspaceRoot string, hook hookChild) string {
	section, _ := runHookChildPassed(ctx, hooks, apiKeyEnv, workspaceRoot, hook)
	return section
}

// runHookChildPassed is runHookChild that also says whether the command exited 0. A command
// that could not start, was stopped, timed out or was killed by a signal did not pass.
func runHookChildPassed(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv, workspaceRoot string, hook hookChild) (string, bool) {
	section, passed, _ := runHookChildOutput(ctx, hooks, apiKeyEnv, workspaceRoot, hook)
	return section, passed
}

// runHookChildOutput is runHookChildPassed that also returns the command's standard output,
// as cut to hooks.max_output_bytes, for a command whose output is its answer.
func runHookChildOutput(ctx context.Context, hooks sessionfs.HooksSettings, apiKeyEnv, workspaceRoot string, hook hookChild) (string, bool, bashStreamOutput) {
	before := ""
	if hook.watched != "" {
		before = fileDigest(hook.watched)
	}
	cwd := hook.cwd
	maximum := int(hooks.MaxOutputBytes)
	head, tail := maximum*3/4, maximum-maximum*3/4
	maxLineChars := int(hooks.MaxLineChars)
	if hook.rawStdout {
		maxLineChars = maximum
	}
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: head, tailBytes: tail},
		bashStreamLimits{headBytes: head, tailBytes: tail},
		maxLineChars,
		hooks.StripANSI,
	)
	sandbox, tmp := hooks.Sandbox, ""
	if hook.call != nil {
		sandbox = callSandbox(sandbox, *hook.call)
		var err error
		if tmp, err = callTempDir(sandbox, *hook.call); err != nil {
			return "[" + hook.name + "] could not start: " + err.Error() + " (setting: " + hook.setting + ")", false, bashStreamOutput{}
		}
	}
	child := runBashCommand(ctx, bashChildRequest{
		file:                     hook.argv[0],
		args:                     hook.argv[1:],
		stdin:                    hook.stdin,
		cwd:                      cwd,
		environment:              buildNativeBashEnvironment(os.Environ(), sessionfs.NativeBashSettings{Env: hooks.Env, EnvDeny: hooks.EnvDeny}, apiKeyEnv),
		timeout:                  time.Duration(hooks.TimeoutMS) * time.Millisecond,
		killGrace:                time.Duration(hooks.KillGraceMs) * time.Millisecond,
		killDeadline:             time.Duration(hooks.KillDeadlineMs) * time.Millisecond,
		drainAfterExit:           time.Duration(hooks.DrainAfterExitMs) * time.Millisecond,
		killProcessGroupWhenDone: hooks.KillProcessGroup == "always",
		collector:                collector,
		sandbox:                  sandbox,
		workspace:                workspaceRoot,
		tmp:                      tmp,
	})
	label := "[" + hook.name + "] "
	timing := fmt.Sprintf("%d ms, in %s", child.durationMs, displayPath(workspaceRoot, cwd))
	switch {
	case child.spawnError != nil:
		return label + "could not start: " + child.spawnError.Error() + " (setting: " + hook.setting + ")", false, bashStreamOutput{}
	case child.aborted:
		return label + "stopped before it finished", false, bashStreamOutput{}
	case child.timedOut:
		return label + fmt.Sprintf("timed out after %d ms (setting: hooks.timeout_ms)", hooks.TimeoutMS) + hookOutputBlock(child.output, maximum), false, bashStreamOutput{}
	}
	changedNote := ""
	if hook.watched != "" && fileDigest(hook.watched) != before {
		changedNote = "; the check changed the file, read it again before the next edit"
	}
	if child.hasExitCode && child.exitCode == 0 {
		if hook.onFailureOnly && changedNote == "" {
			return "", true, child.output.stdout
		}
		return label + "passed (" + timing + ")" + changedNote + hookOutputBlock(child.output, maximum), true, child.output.stdout
	}
	status := "failed"
	if child.signal != "" {
		status += ", signal " + child.signal
	} else if child.hasExitCode {
		status += fmt.Sprintf(", exit %d", child.exitCode)
	}
	return label + status + " (" + timing + ")" + changedNote + hookOutputBlock(child.output, maximum), false, bashStreamOutput{}
}

// hookWorkingDirectory resolves the check's directory. found is false only for a
// nearest:<marker> setting whose marker lies nowhere from the file up to the workspace root.
func hookWorkingDirectory(hook sessionfs.PostEditHook, workspaceRoot, file string) (string, bool) {
	directory := filepath.Dir(file)
	switch {
	case hook.Cwd == sessionfs.HookCwdWorkspace:
		return workspaceRoot, true
	case hook.Cwd == sessionfs.HookCwdFileDir:
		return directory, true
	}
	marker := strings.TrimPrefix(hook.Cwd, sessionfs.HookCwdNearestPrefix)
	for sessionfs.PathWithin(workspaceRoot, directory) {
		if _, err := os.Stat(filepath.Join(directory, marker)); err == nil {
			return directory, true
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", false
}

// hookArguments replaces the placeholders of every argument after the program.
func hookArguments(hook sessionfs.PostEditHook, cwd, file string) []string {
	directory := filepath.Dir(file)
	relative := func(target string) string {
		value, err := filepath.Rel(cwd, target)
		if err != nil {
			return target
		}
		return filepath.ToSlash(value)
	}
	replacer := strings.NewReplacer("{file}", file, "{dir}", directory, "{rel}", relative(file), "{reldir}", relative(directory))
	argv := make([]string, len(hook.Argv))
	argv[0] = hook.Argv[0]
	for index := 1; index < len(hook.Argv); index++ {
		argv[index] = replacer.Replace(hook.Argv[index])
	}
	return argv
}

// hookOutputBlock is the check's output on lines of its own. The collector already cut it
// to a head and a tail within the output budget; truncated adds the setting that sets it.
func hookOutputBlock(output bashRenderedOutput, maximum int) string {
	text := strings.TrimRight(output.text, " \t\r\n")
	if text == "" {
		return ""
	}
	if output.truncated {
		text += fmt.Sprintf("\n[output cut to about %d bytes (hooks.max_output_bytes)]", maximum)
	}
	return "\n" + text
}

// fileDigest identifies a file's content; an unreadable or missing file has the empty digest.
func fileDigest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return string(sum[:])
}

// withConfiguredHooks applies the session's post-edit checks to tool. The checks run
// under the hooks' own child-process settings and need no other tool.
func withConfiguredHooks(config sessionfs.SessionConfig, tool tools.Tool) tools.Tool {
	return WithPostEditHooks(tool, config.Hooks, config.Model.APIKeyEnv)
}
