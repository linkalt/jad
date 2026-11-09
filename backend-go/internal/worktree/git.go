package worktree

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Launch is how one git command of a run starts inside the run's environment: the file that
// runs, the name it runs as (argv[0]), its arguments and its whole environment.
type Launch struct {
	File  string
	Argv0 string
	Args  []string
	Env   []string
}

// Launcher returns how to start git (the configured executable) with args in dir inside a
// run's environment (its sandbox). The worktree adds its own git variables to Env.
type Launcher func(git string, args []string, dir string) (Launch, error)

// hostGitFlags make every git command jad runs on the host ignore what a sandboxed run could
// have written into the repository's git directory: its hooks (a post-checkout hook would run
// outside the sandbox on the next worktree add) and an fsmonitor command in its config (run
// by the next status). Command-line configuration wins over every config file.
var hostGitFlags = []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}

// commitHooksFlags are hostGitFlags for the commit Finish makes when commit_hooks is on: the
// repository's hooks run for it, as the setting asks; the fsmonitor stays off.
var commitHooksFlags = []string{"-c", "core.fsmonitor=false"}

// gitCommand is one git command: where it runs, what it reads and writes, and how it starts.
type gitCommand struct {
	dir   string
	flags []string
	args  []string
	// env is added to the command's environment.
	env    []string
	stdin  io.Reader
	stdout io.Writer
	// launcher starts the command inside a run's environment; nil runs it on the host.
	launcher Launcher
	// exitCodes are exit codes that are not failures (merge-tree's 1 for a conflict); the
	// code is returned with the output.
	exitCodes []int
}

// run runs git in dir and returns its standard output without the trailing newline. A
// failure carries the first line of git's error output. The repository's hooks and its
// fsmonitor never run (hostGitFlags).
func run(ctx context.Context, options Options, dir string, arguments ...string) (string, error) {
	return runFlags(ctx, options, dir, hostGitFlags, arguments...)
}

func runFlags(ctx context.Context, options Options, dir string, flags []string, arguments ...string) (string, error) {
	out, _, err := runGit(ctx, options, gitCommand{dir: dir, flags: flags, args: arguments})
	return out, err
}

// runGit runs one git command bounded by the options' timeout. Its output is returned
// without the trailing newline unless stdout goes elsewhere.
func runGit(ctx context.Context, options Options, spec gitCommand) (string, int, error) {
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	if spec.flags == nil {
		spec.flags = hostGitFlags
	}
	arguments := append(append([]string(nil), spec.flags...), spec.args...)
	// A command never waits for a password, and the messages stay parseable.
	fixed := []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	var command *exec.Cmd
	if spec.launcher != nil {
		launch, err := spec.launcher(options.Git, arguments, spec.dir)
		if err != nil {
			return "", 0, fmt.Errorf("start git in the run's environment: %w", err)
		}
		command = exec.CommandContext(ctx, launch.File, launch.Args...)
		command.Args[0] = launch.Argv0
		command.Env = append(append(append([]string(nil), launch.Env...), fixed...), spec.env...)
	} else {
		command = exec.CommandContext(ctx, options.Git, arguments...)
		command.Env = append(append(os.Environ(), fixed...), spec.env...)
	}
	command.Dir = spec.dir
	command.Stdin = spec.stdin
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if spec.stdout != nil {
		command.Stdout = spec.stdout
	}
	err := command.Run()
	code := 0
	if err != nil {
		exitError, isExit := err.(*exec.ExitError)
		if isExit {
			code = exitError.ExitCode()
		}
		if !isExit || !containsCode(spec.exitCodes, code) {
			message := strings.TrimSpace(stderr.String())
			if line, _, found := strings.Cut(message, "\n"); found {
				message = line
			}
			if message == "" {
				return "", code, err
			}
			return "", code, fmt.Errorf("%v: %s", err, message)
		}
	}
	return strings.TrimRight(stdout.String(), "\n"), code, nil
}

func containsCode(codes []int, code int) bool {
	for _, allowed := range codes {
		if allowed == code {
			return true
		}
	}
	return false
}

// isCommitID reports whether text is a full object name, the only form a commit name read
// from a run's environment is taken in.
func isCommitID(text string) bool {
	if len(text) != 40 && len(text) != 64 {
		return false
	}
	for _, char := range text {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
