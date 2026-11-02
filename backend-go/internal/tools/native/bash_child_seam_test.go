package native

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// installBashSeams replaces the pipe and command hooks for one test. Tests that call it
// must not run in parallel: the hooks are package globals.
func installBashSeams(t *testing.T, pipe func() (*os.File, *os.File, error), command func(configured, resolved string, args ...string) *exec.Cmd) {
	t.Helper()
	previousPipe, previousCommand := newBashPipe, newBashCommand
	newBashPipe, newBashCommand = pipe, command
	t.Cleanup(func() {
		newBashPipe, newBashCommand = previousPipe, previousCommand
	})
}

func seamRequest(t *testing.T) bashChildRequest {
	t.Helper()
	return bashChildRequest{
		file:        "/fake/bash",
		args:        []string{"-c", "echo hi"},
		cwd:         t.TempDir(),
		environment: []string{"PATH=/usr/bin:/bin"},
	}
}

// A command whose Err is set makes exec.Cmd.Start fail before it forks, so the fake is the
// only thing that runs and no process is created.
func TestRunBashCommandSpawnErrorFromFakeCommandStartsNoProcess(t *testing.T) {
	spawnErr := errors.New("fake spawn refused")
	var built *exec.Cmd
	installBashSeams(t, os.Pipe, func(configured, resolved string, args ...string) *exec.Cmd {
		if configured != "/fake/bash" || resolved != "/fake/bash" || len(args) != 2 {
			t.Errorf("fake command got configured=%q resolved=%q args=%q", configured, resolved, args)
		}
		built = &exec.Cmd{Path: resolved, Args: append([]string{configured}, args...), Err: spawnErr}
		return built
	})

	child := runBashCommand(context.Background(), seamRequest(t))

	if !errors.Is(child.spawnError, spawnErr) {
		t.Fatalf("spawnError = %v, want %v", child.spawnError, spawnErr)
	}
	if built == nil || built.Process != nil {
		t.Fatalf("fake command process = %v, want a command that never started", built)
	}
	if child.hasExitCode || child.timedOut || child.leftRunning {
		t.Fatalf("spawn failure reported a run: %+v", child)
	}
}

// A stdout pipe failure happens before any command is built, so the command hook must not run.
func TestRunBashCommandStdoutPipeErrorBuildsNoCommand(t *testing.T) {
	pipeErr := errors.New("fake pipe refused")
	commandCalls := 0
	installBashSeams(t, func() (*os.File, *os.File, error) {
		return nil, nil, pipeErr
	}, func(configured, resolved string, args ...string) *exec.Cmd {
		commandCalls++
		return &exec.Cmd{Err: errors.New("unreachable")}
	})

	child := runBashCommand(context.Background(), seamRequest(t))

	if !errors.Is(child.spawnError, pipeErr) {
		t.Fatalf("spawnError = %v, want %v", child.spawnError, pipeErr)
	}
	if commandCalls != 0 {
		t.Fatalf("command hook ran %d times after a pipe failure, want 0", commandCalls)
	}
}

// A stderr pipe failure must close the stdout pipe it already opened, then fail without a command.
func TestRunBashCommandStderrPipeErrorClosesStdoutAndBuildsNoCommand(t *testing.T) {
	pipeErr := errors.New("fake stderr pipe refused")
	pipeCalls := 0
	commandCalls := 0
	installBashSeams(t, func() (*os.File, *os.File, error) {
		pipeCalls++
		if pipeCalls == 2 {
			return nil, nil, pipeErr
		}
		return os.Pipe()
	}, func(configured, resolved string, args ...string) *exec.Cmd {
		commandCalls++
		return &exec.Cmd{Err: errors.New("unreachable")}
	})

	child := runBashCommand(context.Background(), seamRequest(t))

	if !errors.Is(child.spawnError, pipeErr) {
		t.Fatalf("spawnError = %v, want %v", child.spawnError, pipeErr)
	}
	if commandCalls != 0 || pipeCalls != 2 {
		t.Fatalf("pipe calls = %d, command calls = %d; want 2 and 0", pipeCalls, commandCalls)
	}
}
