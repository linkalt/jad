//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package mcp

import "os/exec"

// Windows has no process group here (user decision: unix only, Windows behaviour
// unchanged), so a stdio MCP server keeps starting and stopping exactly as before: the
// SDK signals the direct child, and nothing else.
func configureStdioProcessGroup(*exec.Cmd) {}

func killStdioProcessGroup(*exec.Cmd) {}
