package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// batchCall is a call of the runner with runs: each item is a run target and its arguments.
func batchCall(items ...string) json.RawMessage {
	return runnerCall("runner", `{"runs":[`+strings.Join(items, ",")+`]}`)
}

func item(target string) string { return `{"run":"` + target + `"}` }

func nameOfCall(t *testing.T, call json.RawMessage) string {
	t.Helper()
	var fields struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(call, &fields); err != nil {
		t.Fatal(err)
	}
	return fields.Name
}

func TestResolveCallsReturnsTheTargetsOfABatchInTheOrderSent(t *testing.T) {
	catalog := delegateCatalog(t)
	calls, batch := catalog.ResolveCalls(batchCall(item("write"), item("mcp:docs:fetch"), item("read"), item("write")))
	if !batch || len(calls) != 4 {
		t.Fatalf("batch = %v with %d calls, want a batch of 4", batch, len(calls))
	}
	var names []string
	for _, call := range calls {
		names = append(names, nameOfCall(t, call))
		var fields map[string]string
		_ = json.Unmarshal(call, &fields)
		if fields["call_id"] != "c1" || fields["type"] != "function_call" {
			t.Errorf("a resolved item lost the outer call_id: %s", call)
		}
	}
	if got, want := strings.Join(names, ","), "write,mcp_docs_fetch_0123456789,read,write"; got != want {
		t.Fatalf("resolved %s, want %s", got, want)
	}
	// A target that resolves to nothing runs nothing and adds no call; the others still count.
	calls, batch = catalog.ResolveCalls(batchCall(item("exec"), item("nothing"), item("read"), item("runner")))
	if !batch || len(calls) != 1 || nameOfCall(t, calls[0]) != "read" {
		t.Fatalf("a batch with unresolvable targets resolved to %d calls", len(calls))
	}
	// A batch none of whose targets resolve stays one call, judged as the runner.
	whole := batchCall(item("exec"), item("nothing"))
	calls, batch = catalog.ResolveCalls(whole)
	if !batch || len(calls) != 1 || string(calls[0]) != string(whole) {
		t.Fatalf("an unresolvable batch = %v %s, want the call itself", batch, calls)
	}
	// A single call and any other call are not batches, and resolve as ResolveCall does.
	for _, call := range []json.RawMessage{runnerCall("runner", `{"run":"write"}`), runnerCall("runner", `{}`), runnerCall("write", `{}`), runnerCall("runner", `{"runs":[]}`), runnerCall("runner", `{"runs":[{"run":"write"}],"x":1`), json.RawMessage(`[]`)} {
		calls, batch := catalog.ResolveCalls(call)
		if batch || len(calls) != 1 || string(calls[0]) != string(catalog.ResolveCall(call)) {
			t.Errorf("%s: batch %v, calls %s; want the single resolution", call, batch, calls)
		}
	}
	var nilCatalog *Catalog
	if calls, batch := nilCatalog.ResolveCalls(whole); batch || len(calls) != 1 {
		t.Errorf("a nil catalog resolved %v %v", calls, batch)
	}
	// A batch is not a single run for ResolveCall.
	if got := catalog.ResolveCall(batchCall(item("write"))); string(got) != string(batchCall(item("write"))) {
		t.Errorf("ResolveCall rewrote a batch: %s", got)
	}
}

// A batch is judged as all its targets together: it can change state, change files, need the
// user's decision when any target does, and it is guarded in plan mode only when every target is.
func TestJudgmentsOfABatchAggregateItsTargets(t *testing.T) {
	catalog := delegateCatalog(t)
	maybe := func(write string) string {
		return `{"run":"maybe","arguments":{"write":` + write + `}}`
	}
	for name, tc := range map[string]struct {
		call                              json.RawMessage
		changes, files, decision, guarded bool
	}{
		"read and read":         {batchCall(item("read"), item("read")), false, false, false, false},
		"read and write":        {batchCall(item("read"), item("write")), true, false, false, false},
		"write last":            {batchCall(item("read"), item("read"), item("write")), true, false, false, false},
		"an MCP target":         {batchCall(item("read"), item("mcp:docs:fetch")), true, false, false, false},
		"may change files":      {batchCall(item("read"), maybe("true")), true, true, false, false},
		"may not change files":  {batchCall(item("read"), maybe("false")), false, false, false, false},
		"a leaf is no leaf":     {batchCall(item("lsp"), item("read")), false, false, false, false},
		"only leaves":           {batchCall(item("lsp"), item("lsp")), false, false, false, false},
		"a user decision":       {batchCall(item("read"), item("decide")), false, false, true, false},
		"all guarded":           {batchCall(item("guarded"), item("guarded")), false, false, false, true},
		"one guarded":           {batchCall(item("guarded"), item("read")), false, false, false, false},
		"native targets only":   {batchCall(item("exec"), item("request_draft")), false, false, false, false},
		"native beside a write": {batchCall(item("exec"), item("write")), true, false, false, false},
	} {
		if got := catalog.ChangesState(tc.call); got != tc.changes {
			t.Errorf("%s: ChangesState = %v, want %v", name, got, tc.changes)
		}
		if got := catalog.CallMayChangeFiles(tc.call); got != tc.files {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", name, got, tc.files)
		}
		if got := catalog.IsUserDecisionCall(tc.call); got != tc.decision {
			t.Errorf("%s: IsUserDecisionCall = %v, want %v", name, got, tc.decision)
		}
		if name != "all guarded" && name != "one guarded" {
			continue
		}
		if got := catalog.IsPlanGuardedCall(tc.call); got != tc.guarded {
			t.Errorf("%s: IsPlanGuardedCall = %v, want %v", name, got, tc.guarded)
		}
	}
	// A leaf run alone is a leaf (it holds the token for the call); a batch is never one, because the
	// runner takes one token for each leaf item itself.
	leafRun := runnerCall("runner", `{"run":"lsp"}`)
	if !catalog.IsLeafCall(leafRun) || catalog.IsLeafCall(batchCall(item("lsp"))) || catalog.IsLeafCall(batchCall(item("lsp"), item("lsp"))) {
		t.Error("a single run of a leaf is a leaf and a batch never is")
	}
	// A batch of plain targets is guarded in plan mode as the runner would be: not guarded.
	if catalog.IsPlanGuardedCall(batchCall(item("exec"))) {
		t.Error("an unresolvable batch is guarded")
	}
}

func TestSourceOfABatchIsTheCommonSourceOrNone(t *testing.T) {
	catalog := delegateCatalog(t)
	for name, tc := range map[string]struct {
		call json.RawMessage
		want string
	}{
		"one source":       {batchCall(item("read"), item("write")), "extension"},
		"mcp only":         {batchCall(item("mcp:docs:fetch"), item("mcp:docs:fetch")), "mcp"},
		"mixed sources":    {batchCall(item("read"), item("mcp:docs:fetch")), ""},
		"unresolvable":     {batchCall(item("exec")), "native"},
		"single run":       {runnerCall("runner", `{"run":"mcp:docs:fetch"}`), "mcp"},
		"unresolved mixed": {batchCall(item("exec"), item("mcp:docs:fetch")), "mcp"},
	} {
		if got := catalog.SourceOf(tc.call); got != tc.want {
			t.Errorf("%s: SourceOf = %q, want %q", name, got, tc.want)
		}
	}
}
