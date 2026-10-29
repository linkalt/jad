//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mcp

import (
	"os/exec"
	"syscall"
)

// configureStdioProcessGroup puts a stdio MCP server into its own process group, so a
// wrapper (npx, uvx, sh -c ...) and everything it starts share one group that the manager
// can signal as a whole. The SDK terminates the direct child only: it closes its stdin,
// then sends SIGTERM and finally SIGKILL to that one process (cmd.go, pipeRWC.Close).
func configureStdioProcessGroup(command *exec.Cmd) {
	if command == nil {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killStdioProcessGroup kills the whole process group of a stdio MCP server once the SDK
// has closed its session, which stops the grandchildren a wrapper left behind. It is safe
// after the direct child was reaped: a process group id stays reserved for as long as the
// group has a member, so the id of a dead leader still names its survivors and cannot have
// been handed to an unrelated process.
func killStdioProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	pid := command.Process.Pid
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
