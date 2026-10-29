package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// The sub-agent tool call runs on its own goroutine so the run it starts can be
// observed, and nothing above that goroutine recovers: a panic there must fail the
// call, as it does on the batch path, instead of ending the process.
func TestCallSubagentToolRecoversAPanic(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	output := callSubagentTool(func() json.RawMessage { panic("handler exploded") })
	if output != nil {
		t.Fatalf("output = %s, want none after a panic", output)
	}
	if !strings.Contains(logged.String(), "handler exploded") {
		t.Fatalf("the recovered panic left no log line: %q", logged.String())
	}
	// The caller reports the panic as the ordinary "no run started" refusal.
	if err := subagentStartFailure(output); err == nil || !strings.Contains(err.Error(), "did not start a run") {
		t.Fatalf("subagentStartFailure(nil) = %v, want the ordinary refusal", err)
	}
}

// The boundary must not change what a call returns when nothing panics.
func TestCallSubagentToolReturnsTheCallOutput(t *testing.T) {
	want := `{"type":"function_call_output","call_id":"remote_a","output":"ok"}`
	if got := callSubagentTool(func() json.RawMessage { return json.RawMessage(want) }); string(got) != want {
		t.Fatalf("output = %s, want %s", got, want)
	}
}
