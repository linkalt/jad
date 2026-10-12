package native

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// WithPostEditHooks returns tool unchanged unless hooks has checks to run. Otherwise it
// returns a copy whose Execute runs the matching checks after the call changed a file
// and appends their report to the result. Only a call that changed a file runs a check:
// a failed call, a rejected path and an unchanged file do not. The check never changes
// the status of the call; its outcome is text for the model.
func WithPostEditHooks(tool tools.Tool, hooks sessionfs.HooksSettings, apiKeyEnv string) tools.Tool {
	if !hooks.Active() {
		return tool
	}
	inner := tool.Execute
	tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
		var changed string
		outer := execution.OnFileChange
		execution.OnFileChange = func(path, action string) {
			changed = path
			if outer != nil {
				outer(path, action)
			}
		}
		result, err := inner(ctx, arguments, execution)
		if err != nil || changed == "" {
			return result, err
		}
		report := runPostEditHooks(ctx, hooks, apiKeyEnv, execution, changed)
		if report == "" {
			return result, nil
		}
		return tools.AppendContent(result, tools.Content{Text: report})
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
		if section := runPostEditHook(ctx, hook, hooks, apiKeyEnv, execution.WorkspaceRoot, file); section != "" {
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

func runPostEditHook(ctx context.Context, hook sessionfs.PostEditHook, hooks sessionfs.HooksSettings, apiKeyEnv, workspaceRoot, file string) string {
	cwd, found := hookWorkingDirectory(hook, workspaceRoot, file)
	if !found {
		marker := strings.TrimPrefix(hook.Cwd, sessionfs.HookCwdNearestPrefix)
		if !isWithin(workspaceRoot, file) {
			return "[" + hook.Name + "] skipped: the file is outside the workspace, so nearest:" + marker + " has nowhere to look"
		}
		return "[" + hook.Name + "] skipped: no " + marker + " between the file and the workspace root"
	}
	before := fileDigest(file)
	maximum := int(hooks.MaxOutputBytes)
	head, tail := maximum*3/4, maximum-maximum*3/4
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: head, tailBytes: tail},
		bashStreamLimits{headBytes: head, tailBytes: tail},
		int(hooks.MaxLineChars),
		hooks.StripANSI,
	)
	argv := hookArguments(hook, cwd, file)
	child := runBashCommand(ctx, bashChildRequest{
		file:                     argv[0],
		args:                     argv[1:],
		cwd:                      cwd,
		environment:              buildNativeBashEnvironment(os.Environ(), sessionfs.NativeBashSettings{Env: hooks.Env, EnvDeny: hooks.EnvDeny}, apiKeyEnv),
		timeout:                  time.Duration(hooks.TimeoutMS) * time.Millisecond,
		killGrace:                time.Duration(hooks.KillGraceMs) * time.Millisecond,
		killProcessGroupWhenDone: hooks.KillProcessGroup == "always",
		collector:                collector,
	})
	label := "[" + hook.Name + "] "
	timing := fmt.Sprintf("%d ms, in %s", child.durationMs, displayPath(workspaceRoot, cwd))
	switch {
	case child.spawnError != nil:
		return label + "could not start: " + child.spawnError.Error() + " (setting: hooks.post_edit[" + hook.Name + "].argv)"
	case child.aborted:
		return label + "stopped before it finished"
	case child.timedOut:
		return label + fmt.Sprintf("timed out after %d ms (setting: hooks.timeout_ms)", hooks.TimeoutMS) + hookOutputBlock(child.output, maximum)
	}
	changedNote := ""
	if after := fileDigest(file); before != after {
		changedNote = "; the check changed the file, read it again before the next edit"
	}
	if child.hasExitCode && child.exitCode == 0 {
		if hook.OnFailureOnly && changedNote == "" {
			return ""
		}
		return label + "passed (" + timing + ")" + changedNote + hookOutputBlock(child.output, maximum)
	}
	status := "failed"
	if child.signal != "" {
		status += ", signal " + child.signal
	} else if child.hasExitCode {
		status += fmt.Sprintf(", exit %d", child.exitCode)
	}
	return label + status + " (" + timing + ")" + changedNote + hookOutputBlock(child.output, maximum)
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
	for isWithin(workspaceRoot, directory) {
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
