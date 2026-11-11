package native

import (
	"reflect"
	"strings"
	"testing"
)

// R0 inventory of the unified agent run design (docs/design_unified_agent_runs.md, section 3.3 and
// phase R0): every field of the three structs a caller fills to start a run today, with where the
// design puts it. A field added, removed or renamed fails this test, so R1 (CallerContext) and R5
// (one admission) account for it instead of dropping it. The destinations:
//
//   - caller: a CallerContext field of section 3.3 (or a context value it replaces);
//   - spec: RunSpec (template, input, seed) of section 3.4;
//   - admission: the one admission of section 3.5 (caps, queue settings, fail fast);
//   - registry: the run tree registry and its manifest of section 3.7 (records, handles, results);
//   - starter: work StartRun does itself in section 3.4 (launcher, worktree, catalog);
//   - open: the design does not place it; R1 must decide (see backend-go/DECISIONS.md).
type runInventoryField struct {
	name, destination string
}

var subagentRequestInventory = []runInventoryField{
	{"Agent", "spec"},
	{"Task", "spec"},
	{"CallID", "open"}, // names the run folder (<call id>-<hex>), a store-layout contract (3.8)
	{"Tools", "starter"},
	{"Execution", "caller"},
	{"OnTranscript", "registry"},
	{"Seed", "spec"},
	{"CallerValues", "caller"}, // cost meter, hook reporter, caller run id (T-003), chain values
	{"Caller", "caller"},       // R1: tools.CallerContext, carried beside the fields above; read from R2
}

// runAdmissionInventory is R5a's one admission input: the fields subagentStart carried one by one
// (session, depth, rootRun and the five limits), with the destinations R0 gave them.
var runAdmissionInventory = []runInventoryField{
	{"session", "registry"},
	{"parent", "registry"}, // T-007: the caller's node in the shared run tree (ExecutionContext.RunNode)
	{"depth", "caller"},
	{"rootRun", "caller"},
	{"chainDepth", "caller"}, // R6: CallerContext.RunDepth, the depth across both kinds of run
	{"maxDepth", "admission"},
	{"maxPerRoot", "admission"},
	{"maxPerSession", "admission"},
	{"limit", "admission"},
	{"maxQueued", "admission"},
}

var subagentStartInventory = []runInventoryField{
	{"launcher", "starter"},
	{"request", "spec"},
	{"owner", "registry"},
	{"handleOwner", "caller"},
	{"admission", "admission"}, // R5a: runAdmission, listed below
	{"parentID", "caller"},
	{"lineage", "registry"}, // R5c: run lineage across both kinds (run_lineage.go)
	{"changes", "registry"},
	{"worktree", "starter"},
	{"runMCP", "starter"}, // isolation P6, added after the design was written
	{"changedMax", "registry"},
	{"keep", "registry"},
	{"resultMax", "registry"},
	{"external", "registry"},
	{"async", "registry"},
	{"asyncHost", "registry"},
	{"childAsync", "registry"},
	{"writerReserved", "admission"},
	{"note", "registry"},
	{"configNote", "registry"},
	{"config", "registry"},
	{"resumedFrom", "registry"},
	{"sandbox", "caller"},
	{"caller", "caller"},
	{"resumedConfig", "registry"},
	{"seed", "spec"},
}

var profileMessageRequestInventory = []runInventoryField{
	{"OwnerKey", "caller"},
	{"Session", "spec"},
	{"Target", "spec"},
	{"Message", "spec"},
	{"Model", "caller"},
	{"ContextTrigger", "open"}, // T-003, added after the design was written
	{"QueueWait", "admission"},
	{"Timeout", "admission"},
	{"MaxQueued", "admission"},
	{"MaxRunsPerRoot", "admission"}, // T-007: the caller's root cap the message's run joins the tree with
	{"Blocking", "admission"},
	{"Environment", "caller"},
	{"CallerPlace", "open"}, // the caller's placeholders for D20 narrowing; not in the 3.3 table
	{"Sandbox", "caller"},
	{"Caller", "caller"}, // R1: tools.CallerContext, carried beside the fields above; read from R5
}

func TestUnifiedRunInventoryCoversEveryStartField(t *testing.T) {
	known := map[string]bool{"caller": true, "spec": true, "admission": true, "registry": true, "starter": true, "open": true}
	for _, tc := range []struct {
		name      string
		value     any
		inventory []runInventoryField
	}{
		{"SubagentRequest", SubagentRequest{}, subagentRequestInventory},
		{"subagentStart", subagentStart{}, subagentStartInventory},
		{"runAdmission", runAdmission{}, runAdmissionInventory},
		{"ProfileMessageRequest", ProfileMessageRequest{}, profileMessageRequestInventory},
	} {
		kind := reflect.TypeOf(tc.value)
		var fields, listed []string
		for index := range kind.NumField() {
			fields = append(fields, kind.Field(index).Name)
		}
		for _, field := range tc.inventory {
			if !known[field.destination] {
				t.Errorf("%s.%s: unknown destination %q", tc.name, field.name, field.destination)
			}
			listed = append(listed, field.name)
		}
		if strings.Join(fields, ",") != strings.Join(listed, ",") {
			t.Errorf("%s fields changed; update the R0 inventory and say where each new field goes:\n have %v\n list %v", tc.name, fields, listed)
		}
	}
}
