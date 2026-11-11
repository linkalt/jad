package native

import (
	"strings"
	"testing"
	"time"
)

// R5 pre-change characterisation: a top-level run that waits for a slot is not bounded by its
// agent's timeout_ms. The timeout starts once the slot is taken (SubagentManager.execute), so
// a queued run waits as long as the slot stays held, and then still gets its whole timeout.
// Only a stop, the caller's cancel (a run without a handle) or Close ends the wait. A merged
// admission that bounds or refuses this wait changes this test on purpose.
func TestSubagentSlotWaitIsNotBoundedByTheAgentsTimeout(t *testing.T) {
	release := make(chan struct{})
	quick := agentNamed("quick")
	quick.TimeoutMS = 150
	settings := subagentSettings(agentNamed("holder"), quick)
	settings.MaxConcurrent = 1
	catalog, _ := buildSubagentCatalog(t, settings, blockedLauncher(release, nil), readOnlyTool("read"))
	if out := subagentCall(t, catalog, `{"agent":"holder","task":"hold","block_ms":0}`); !strings.Contains(out, "state: running") {
		t.Fatalf("holder:\n%s", out)
	}
	queued := runIDOf(t, subagentCall(t, catalog, `{"agent":"quick","task":"wait","block_ms":0}`))
	time.Sleep(3 * time.Duration(quick.TimeoutMS) * time.Millisecond)
	if out := asyncCall(t, catalog, `{"action":"status","id":"`+queued+`"}`); !strings.Contains(out, "queued") || strings.Contains(out, "timed_out") {
		t.Fatalf("after three times its timeout_ms in the queue the run must still be queued:\n%s", out)
	}
	close(release)
	eventually(t, "the queued run to finish with its answer", func() bool {
		return strings.Contains(asyncCall(t, catalog, `{"action":"status","id":"`+queued+`"}`), "late answer")
	})
}
