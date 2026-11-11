package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// R5 pre-change characterisation of admission when several limits are hit at once. The cases seed
// the run tree and the manager's counts directly (the nodes below the root and in the session,
// running, queued, closed), so they do not depend on how a start passes its limits to the manager,
// and record the exact result text, so a refactor of admission (R5a, the shared run tree of T-007)
// must keep both which limit wins and its wording. Golden: testdata/admission_order.txt
// (JAD_UPDATE_RUN_BASELINE=1 rewrites it).

const admissionRoot = "sub_0000root"

// admissionSeedSession executes the seeded root and the runs below it, so they count against the
// root cap only, as the separate per-root count the golden was recorded with did.
const admissionSeedSession = "seed\x00other"

type admissionSeed struct {
	root, session, running, queued int
	closed                         bool
}

// apply seeds m and returns the node of the seeded top-level run, which a nested call carries as
// its RunNode. seed.root counts the top-level run itself, as the per-root count did: the runs
// below it are seed.root-1.
func (seed admissionSeed) apply(m *SubagentManager, session string) string {
	tree := m.runTree()
	reserve := func(parent, session string) *RunNode {
		node, err := tree.Reserve(RunReservation{Parent: parent, Session: session, Kind: RunNodeSubagent, State: RunNodeRunning})
		if err != nil {
			panic(err)
		}
		return node
	}
	root := reserve("", admissionSeedSession)
	for i := 1; i < seed.root; i++ {
		reserve(root.ID(), admissionSeedSession)
	}
	for range seed.session {
		reserve("", session)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running, m.queued = seed.running, seed.queued
	return root.ID()
}

func admissionSettings() sessionfs.NativeSubagentSettings {
	settings := nestingSettings(nestingAgent("leaf", "read"))
	settings.MaxDepth, settings.MaxRunsPerRoot, settings.MaxRunsPerSession = 2, 2, 3
	settings.MaxConcurrent, settings.MaxQueued = 1, 2
	return settings
}

// admissionOutcome is the result text of a refusal, or how a start that was admitted went.
func admissionOutcome(m *SubagentManager, before admissionSeed, out string) string {
	if strings.Contains(out, `"status":"error"`) || strings.Contains(out, `"status":"forbidden"`) || strings.Contains(out, `"status":"invalid_input"`) {
		return out
	}
	m.mu.Lock()
	queued := m.queued
	m.mu.Unlock()
	if queued > before.queued {
		return "admitted: queued"
	}
	return "admitted: holds a slot"
}

func TestAdmissionOrderOfCompetingLimitsIsUnchanged(t *testing.T) {
	full := admissionSeed{root: 3, session: 3, running: 1, queued: 2}
	local := []struct {
		name  string
		depth int
		seed  admissionSeed
	}{
		{"nested: depth, both caps, no slot and a full queue", 2, full},
		{"nested: both caps, no slot and a full queue", 1, full},
		{"nested: root cap and no slot", 1, admissionSeed{root: 3, running: 1}},
		{"nested: session cap and no slot", 1, admissionSeed{session: 3, running: 1}},
		{"nested: root cap at its bound (root plus two)", 1, admissionSeed{root: 2}},
		{"nested: no slot, queue room", 1, admissionSeed{running: 1}},
		{"nested: closed with every limit hit", 1, admissionSeed{root: 3, session: 3, running: 1, queued: 2, closed: true}},
		{"top level: both caps hit, a free slot", 0, admissionSeed{root: 3, session: 3}},
		{"top level: both caps hit, no slot, queue room", 0, admissionSeed{root: 3, session: 3, running: 1}},
		{"top level: no slot and a full queue", 0, admissionSeed{running: 1, queued: 2}},
		{"top level: closed with no slot and a full queue", 0, admissionSeed{running: 1, queued: 2, closed: true}},
	}
	var report strings.Builder
	for _, tc := range local {
		catalog, manager, _ := nestingFixture(t, admissionSettings(), map[string]nestedBehaviour{
			"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
				return SubagentResult{Text: "leaf"}, nil
			},
		})
		execution := tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, AgentDepth: tc.depth}
		root := tc.seed.apply(manager, testSessionKey)
		if tc.depth > 0 {
			execution.RootRun, execution.RunNode = admissionRoot, root
		}
		if tc.seed.closed {
			_ = manager.Close()
		}
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_a", "name": "subagent", "arguments": `{"task":"t","agent":"leaf","block_ms":0}`})
		raw, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
		if err != nil {
			t.Fatal(err)
		}
		var item struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&report, "local %s:\n  %s\n", tc.name, admissionOutcome(manager, tc.seed, item.Output))
		_ = manager.Close()
	}

	// A remote start is admitted by the depth and the caps alone: it takes no slot or queue place.
	remote := []struct {
		name  string
		depth int
		seed  admissionSeed
	}{
		{"nested: both caps, no slot and a full queue", 1, full},
		{"nested: session cap only", 1, admissionSeed{session: 3}},
		{"nested: no slot and a full queue", 1, admissionSeed{running: 1, queued: 2}},
		{"nested: closed with both caps hit", 1, admissionSeed{root: 3, session: 3, closed: true}},
		{"top level: both caps, no slot and a full queue", 0, full},
	}
	for _, tc := range remote {
		f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) {
			s.MaxDepth, s.MaxRunsPerRoot, s.MaxRunsPerSession, s.MaxConcurrent, s.MaxQueued = 2, 2, 3, 1, 2
		})
		execution := remoteCaller
		execution.AgentDepth = tc.depth
		root := tc.seed.apply(f.manager, remoteCaller.SessionKey)
		if tc.depth > 0 {
			execution.RootRun, execution.RunNode = admissionRoot, root
		}
		if tc.seed.closed {
			_ = f.manager.Close()
		}
		// The peer ends a run it accepts at once (onSubagentStart runs under the backend's lock).
		backend := f.peer.backend
		// It records the session's count while the run is going: a nested remote run holds a place
		// in it, a top-level one none.
		var counted atomic.Int64
		counted.Store(-1)
		backend.onSubagentStart = func() {
			counted.Store(int64(f.manager.sessionRuns(remoteCaller.SessionKey)))
			backend.runs[len(backend.runs)-1].finish(remoteapi.RunResult{State: "done", Text: "ok"})
		}
		out := f.start(t, remoteAgent, 5000, execution)
		outcome := out
		if strings.Contains(out, "finished") {
			outcome = fmt.Sprintf("admitted: reached the peer, the session counted %d runs while it ran (seeded %d)", counted.Load(), tc.seed.session)
		}
		fmt.Fprintf(&report, "remote %s:\n  %s\n", tc.name, outcome)
	}
	compareChildCatalogGolden(t, "admission_order.txt", report.String())
}
