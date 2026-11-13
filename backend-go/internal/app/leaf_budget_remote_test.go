package app

import (
	"testing"

	"jad/backend-go/internal/tools"
)

// remoteDeadlockFixture is a client session with the budget at one, whose profile_message can
// message the remote peer named by peer.json. The server is a second service with a token
// table, the same scaffolding the remote end-to-end tests use. The parent's first response
// calls profile_message on the remote profile, so the call runs in the parent's batch.
func remoteDeadlockFixture(t *testing.T) *layoutFixture {
	t.Helper()
	server, _ := e2eServer(t)
	f := newLayoutFixtureWithBudget(t, nil, tools.NewLeafBudget(1))
	f.bindRemote(t, "peer", server.url)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{"remotes":["peer"]}},"mcp":{}}`)
	f.captureBodies = true
	f.script = scriptFunctionCall("profile_message", map[string]any{"profile": "remote:peer/demo/dev/coder", "message": "what is the answer", "block_ms": 0})
	return f
}

// TestDeadlockRemoteProfileMessageFinishesWithBudgetOne: the parent's batch runs a remote
// profile_message call; the remote run needs no local leaf token, and the call returns.
// The remote leaves run on the other server, which has its own budget, so the local wrapper
// holds nothing the remote run waits for. Setting LeafBudget on profile_message does not
// deadlock this path: it is a completion check only, and the structural guard is the opt-in check.
func TestDeadlockRemoteProfileMessageFinishesWithBudgetOne(t *testing.T) {
	f := remoteDeadlockFixture(t)
	requireParentReceivedProfileMessageOutput(t, f, postRunWithDeadlockTimeout(t, f.api.URL), "answered")
}
