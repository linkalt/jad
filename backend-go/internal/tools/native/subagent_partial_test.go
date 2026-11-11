package native

import (
	"context"
	"strings"
	"testing"
	"time"
)

// cutOffLauncher reports its transcript directory, then waits to be cut off and
// returns the last text it wrote, as the real launcher does.
func cutOffLauncher(text, dir string) *fakeLauncher {
	return &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if request.OnTranscript != nil {
			request.OnTranscript(dir)
		}
		<-ctx.Done()
		return SubagentResult{Text: text, Rounds: 3, Tokens: 700}, ctx.Err()
	}}
}

func TestSubagentTimeoutSignalCarriesThePartialTextAndTheTranscript(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 60
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agent), cutOffLauncher("found two files so far", "/tmp/kids/call-1"), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":5}`))

	output := asyncWait(t, catalog)
	for _, want := range []string{id, "timed_out", `"cut":false`, "/tmp/kids/call-1/messages.jsonl", "found two files so far"} {
		if want == `"cut":false` {
			continue
		}
		if !strings.Contains(output, want) {
			t.Errorf("timeout signal lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, `"ok":true`) {
		t.Errorf("a timed out run is ok:\n%s", output)
	}
}

func TestSubagentStopSignalCarriesThePartialTextAndTheTranscript(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), cutOffLauncher("half a list", "/tmp/kids/call-2"), readOnlyTool("read"))
	id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":5}`))
	output := asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`)
	for _, want := range []string{id, "stopped", "/tmp/kids/call-2/messages.jsonl", "half a list"} {
		if !strings.Contains(output, want) {
			t.Errorf("stop signal lacks %q:\n%s", want, output)
		}
	}
}

func TestSubagentCutOffWithoutTextStillNamesTheTranscript(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 60
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agent), cutOffLauncher("", "/tmp/kids/call-3"), readOnlyTool("read"))
	runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":5}`))

	output := asyncWait(t, catalog)
	if !strings.Contains(output, "/tmp/kids/call-3/messages.jsonl") || !strings.Contains(output, "timed_out") {
		t.Errorf("no transcript path:\n%s", output)
	}
	if strings.Contains(output, `"label":"slow: x","summary"`) {
		t.Errorf("a run with no text must not carry a summary:\n%s", output)
	}
}

func TestSubagentPartialTextIsCutToTheSummaryLimit(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 60
	settings := subagentSettings(agent)
	settings.ResultMaxBytes = 10
	catalog, _ := buildSubagentCatalog(t, settings, cutOffLauncher(strings.Repeat("x", 100), "/tmp/kids/call-4"), readOnlyTool("read"))
	runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":5}`))

	output := asyncWait(t, catalog)
	if !strings.Contains(output, `"cut":true`) || strings.Contains(output, strings.Repeat("x", 11)) {
		t.Errorf("partial text was not cut to 10 bytes:\n%s", output)
	}
}

func TestSubagentRunKnowsItsTranscriptBeforeTheLauncherReturns(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		request.OnTranscript("/tmp/kids/early")
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SubagentResult{}, ctx.Err()
	}}
	manager := NewSubagentManager()
	defer manager.Close()
	run, err := manager.start(subagentStart{
		launcher: launcher, owner: "o", request: SubagentRequest{Agent: agentNamed("explore"), Task: "x"},
		admission: runAdmission{limit: 1, maxQueued: 1}, keep: 5, resultMax: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; run.view().dir == ""; i++ {
		if i > 2000 {
			t.Fatal("the run never learned its transcript directory")
		}
		time.Sleep(time.Millisecond)
	}
	if got := run.view().dir; got != "/tmp/kids/early" {
		t.Fatalf("dir = %q", got)
	}
}
