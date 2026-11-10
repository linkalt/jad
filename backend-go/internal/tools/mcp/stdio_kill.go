package mcp

import (
	"context"
	"os/exec"
	"sync"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioKill kills a stdio server's process group as soon as a connect that is still going is
// cancelled or runs out of time. Without it the SDK would close the server's stdin and wait out
// its terminate delay (5s) before it signals the direct child, so a server that never answers
// would hold the run (and its worktree) for that long, and its grandchildren until the group is
// killed afterwards.
type stdioKill struct {
	command *exec.Cmd
	mu      sync.Mutex
	started bool
	killed  bool
}

// kill kills the group when the process has started; one that starts later is killed at once.
func (k *stdioKill) kill() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.killed = true
	if k.started {
		killStdioProcessGroup(k.command)
	}
}

// transport wraps inner, which starts k.command: the kill applies from the moment it has started.
func (k *stdioKill) transport(inner mcpSDK.Transport) mcpSDK.Transport {
	return &stdioKillTransport{inner: inner, kill: k}
}

type stdioKillTransport struct {
	inner mcpSDK.Transport
	kill  *stdioKill
}

func (t *stdioKillTransport) Connect(ctx context.Context) (mcpSDK.Connection, error) {
	connection, err := t.inner.Connect(ctx)
	t.kill.mu.Lock()
	defer t.kill.mu.Unlock()
	t.kill.started = true
	if t.kill.killed {
		killStdioProcessGroup(t.kill.command)
	}
	return connection, err
}
