package app

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// runPlan is runInput for a plan-mode run.
func (f subagentFixture) runPlan(t *testing.T) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"input": "ask the explorer", "plan": true})
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buffer)
		body.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plan run status = %d, body=%s", response.StatusCode, body.String())
	}
	return body.String()
}

// nestedMCPAgents are the two agents of the nesting scenarios: a reader that may nest (explore), and a
// writer whose list names only the stdio MCP server's tool, whose lookup writes a file in the folder it
// works in (writer).
func nestedMCPAgents() map[string]any {
	return map[string]any{
		"max_depth": 2,
		"agents": map[string]any{
			"explore": map[string]any{"model": "openai/child", "tools": []string{"skill", "subagent"}},
			"writer":  map[string]any{"model": "openai/child", "tools": []string{"mcp:fixture:lookup"}},
		},
	}
}

func nestedMCPFixture(t *testing.T, checkpoint bool) subagentFixture {
	t.Helper()
	setting := map[string]any{"model": "child.json", "__subagent_settings": nestedMCPAgents()}
	if checkpoint {
		setting["__checkpoint"] = `{"enabled":true}`
	}
	f := buildSubagentFixtureWithMCP(t, setting, false, nil, &mcpFixture{stdio: true})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"look around","agent":"explore"}`}
	f.provider.childFirstCall = [2]string{"subagent", `{"task":"write through the server","agent":"writer"}`}
	f.provider.leafFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"by the server"}`}
	return f
}

func (f subagentFixture) wroteThroughMCP() bool {
	_, err := os.Stat(filepath.Join(f.workspace, "from-mcp.txt"))
	return err == nil
}

// A plan-mode run that starts a child whose tools list names only an MCP tool that writes is refused
// before the child runs: no request reaches the child's model and the server writes nothing.
func TestPlanModeRunRefusesAnMCPOnlyChild(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"mcp:fixture:lookup"}}, false, nil, &mcpFixture{stdio: true})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"by the server"}`}
	if body := f.runPlan(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "plan_mode") {
		t.Fatalf("the parent was not told plan mode refused the call:\n%s", last)
	}
	if child := f.provider.forModel("child-model"); len(child) != 0 || f.wroteThroughMCP() {
		t.Fatalf("a child ran below a plan-mode run: %d request(s), wrote %v", len(child), f.wroteThroughMCP())
	}
}

// A plan-mode run lets a reader child nest, and the grandchild that lists an MCP write tool is
// refused with the child's own call: plan mode reaches the second level through the real launcher.
func TestPlanModeRunRefusesAnMCPWriterGrandchildOfAReaderChild(t *testing.T) {
	f := nestedMCPFixture(t, false)
	if body := f.runPlan(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	// The reader child ran (its call, then its answer after the refusal); no grandchild did.
	if len(child) != 2 || !strings.Contains(child[1].Text, "plan_mode") {
		t.Fatalf("child requests = %d; the grandchild was not refused with plan_mode:\n%v", len(child), child)
	}
	if f.wroteThroughMCP() {
		t.Fatal("the MCP server wrote in the session's folder below a plan-mode run")
	}
}

// The legitimate read-only child still runs in plan mode when a writer agent exists.
func TestPlanModeRunStillRunsAReaderChild(t *testing.T) {
	f := nestedMCPFixture(t, false)
	f.provider.childFirstCall = [2]string{}
	if body := f.runPlan(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "the answer is 42") {
		t.Fatalf("the reader child did not answer in plan mode:\n%s", last)
	}
}

// Outside plan mode a reader child that starts an MCP-writing grandchild takes the parent's
// checkpoint: nothing below it has one, so the call at the top must, and what the server wrote is there
// to restore.
func TestReaderChildThatStartsAnMCPWriterTakesTheParentsCheckpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := nestedMCPFixture(t, true)
	if body := f.runInput(t, "ask the explorer"); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if !f.wroteThroughMCP() {
		t.Fatal("the grandchild's MCP write did not happen")
	}
	if records := f.checkpointRecords(t); len(records) != 1 || records[0]["state"] != "complete" {
		t.Fatalf("checkpoints = %v; a reader child that nests a writer must take exactly one", records)
	}
}

// Two reader children of one response that each start an MCP writer are writers for scheduling: the
// run takes them in order, one after the other, through the real tool batch and launcher. Their
// requests to the model never overlap, and the one checkpoint covers both.
func TestSiblingReaderChildrenThatStartMCPWritersRunOneAfterTheOther(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := nestedMCPFixture(t, true)
	f.provider.parentSiblings = []string{`{"task":"look around A","agent":"explore"}`, `{"task":"look around B","agent":"explore"}`}
	f.provider.childDelay = 120 * time.Millisecond
	if body := f.runInput(t, "ask the explorers"); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	// The explorers are the children that hold the subagent tool: each makes a first request, which
	// starts its writer, and a last one with the writer's answer. Run in order they alternate; run side
	// by side the two first requests come before either last one.
	var rounds []string
	for _, request := range f.provider.forModel("child-model") {
		if !slices.Contains(request.Tools, "subagent") {
			continue
		}
		if strings.Contains(request.Text, "function_call_output") {
			rounds = append(rounds, "last")
		} else {
			rounds = append(rounds, "first")
		}
	}
	if got := strings.Join(rounds, ","); got != "first,last,first,last" {
		t.Fatalf("explorer requests = %s, want first,last,first,last: the sibling calls overlapped", got)
	}
	if !f.wroteThroughMCP() {
		t.Fatal("the grandchildren's MCP write did not happen")
	}
	if records := f.checkpointRecords(t); len(records) != 1 || records[0]["state"] != "complete" {
		t.Fatalf("checkpoints = %v, want the one of the response", records)
	}
}

// The agent list the model reads marks the agents the tool schedules as writers: the MCP-only agent and
// the reader that may reach it, which only the resolver's second description (after the MCP tools are
// bound) can know.
func TestAgentListTheModelReadsMarksMCPAndNestingWriters(t *testing.T) {
	f := nestedMCPFixture(t, false)
	f.provider.childFirstCall, f.provider.leafFirstCall = [2]string{}, [2]string{}
	if body := f.runInput(t, "ask the explorer"); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	if len(parent) == 0 {
		t.Fatal("the parent made no request")
	}
	description := parent[0].Descriptions["subagent"]
	for _, want := range []string{"- writer [can change files; runs to its end]", "- explore [can change files; runs to its end]"} {
		if !strings.Contains(description, want) {
			t.Errorf("the subagent description lacks %q:\n%s", want, description)
		}
	}
}
