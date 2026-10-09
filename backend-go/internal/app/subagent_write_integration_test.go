package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writerFixture is the subagent fixture with a write tool, a checkpoint and one agent
// that lists write. approval makes the write tool ask the user.
func writerFixture(t *testing.T, approval bool) subagentFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	write := map[string]any{"max_bytes": 4096}
	if approval {
		write["approval"] = "ask"
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"read", "write"},
		"__write": write, "__checkpoint": `{"enabled":true}`,
		"__subagent_settings": map[string]any{"allow_write": true},
	})
	f.provider.childFirstCall = [2]string{"write", `{"path":"made.txt","content":"by the child"}`}
	return f
}

func (f subagentFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.apiURL+"/api/sessions/local:demo"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (f subagentFixture) checkpointRecords(t *testing.T) []map[string]any {
	t.Helper()
	status, body := f.get(t, "/checkpoints")
	if status != http.StatusOK {
		t.Fatalf("checkpoints status = %d, body = %s", status, body)
	}
	var listing struct {
		Checkpoints []map[string]any `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("checkpoints body %q: %v", body, err)
	}
	return listing.Checkpoints
}

func TestSubagentWriterChangesAFileAndTheCheckpointCoversIt(t *testing.T) {
	f := writerFixture(t, false)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt"))
	if err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v; the child's write did not happen", data, err)
	}
	records := f.checkpointRecords(t)
	if len(records) != 1 || records[0]["state"] != "complete" {
		t.Fatalf("checkpoints = %v; a writer child's run must take exactly one", records)
	}
	// The parent saw the child's answer, and the child ran exactly once.
	parent := f.provider.forModel("parent-model")
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "Agent explore finished") {
		t.Errorf("parent did not get the child's answer:\n%s", last)
	}
	if child := f.provider.forModel("child-model"); len(child) != 2 {
		t.Errorf("child requests = %d, want 2 (the write, then the answer)", len(child))
	}
}

func TestSubagentReaderRunTakesNoCheckpointEvenWithWriterSupportOn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"read"},
		"__write": map[string]any{"max_bytes": 4096}, "__checkpoint": `{"enabled":true}`,
		"__subagent_settings": map[string]any{"allow_write": true},
	})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if records := f.checkpointRecords(t); len(records) != 0 {
		t.Fatalf("a read-only child took a checkpoint: %v", records)
	}
}

func TestSubagentWriterNeverHandsOffEvenWhenAskedTo(t *testing.T) {
	f := writerFixture(t, false)
	f.provider.handoffOnly = true // the parent asks for wait_ms 0
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if strings.Contains(last, "still running") || !strings.Contains(last, "Agent explore finished") {
		t.Fatalf("a writer was handed off or did not finish inline:\n%s", last)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err != nil {
		t.Fatalf("the writer's file is missing: %v", err)
	}
}

// pendingApproval starts a run with interaction support and returns the stream, the
// question's id, and its tool, title and draft.
func (f subagentFixture) startAndWaitForQuestion(t *testing.T) (*bufio.Reader, io.Closer, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"ask the explorer","supports_interaction":true}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	deadline := time.Now().Add(10 * time.Second)
	var event string
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			response.Body.Close()
			t.Fatalf("stream ended before the approval question: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") && event == "interaction" {
			var data struct {
				Pending map[string]any `json:"pending"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data) == nil && data.Pending != nil {
				return reader, response.Body, data.Pending
			}
		}
	}
	response.Body.Close()
	t.Fatal("no approval question arrived")
	return nil, nil, nil
}

func (f subagentFixture) answer(t *testing.T, id, decision string) {
	t.Helper()
	body := fmt.Sprintf(`{"interaction_id":%q,"decision":%q,"reason":"not now"}`, id, decision)
	request, _ := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run/interaction", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("answer status = %d, body = %s", response.StatusCode, data)
	}
}

func TestSubagentWriterAsksTheUserAndTheQuestionNamesTheAgent(t *testing.T) {
	f := writerFixture(t, true)
	reader, closer, pending := f.startAndWaitForQuestion(t)
	defer closer.Close()
	if pending["tool"] != "write" || !strings.Contains(fmt.Sprint(pending["title"]), "(agent explore)") || !strings.Contains(fmt.Sprint(pending["tool_call_id"]), "/") {
		t.Fatalf("question = %v; want the write tool, the agent's name in the title and a child call id", pending)
	}
	if !strings.Contains(string(mustJSON(t, pending["draft"])), `"agent":"explore"`) {
		t.Fatalf("the draft does not name the agent: %v", pending["draft"])
	}
	// The run is parked on the question: the child's write has not happened, and the
	// checkpoint was already taken before the child started.
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err == nil {
		t.Fatal("the child wrote before the user answered")
	}
	if records := f.checkpointRecords(t); len(records) != 1 || records[0]["state"] != "open" {
		t.Fatalf("checkpoints while parked = %v, want one open", records)
	}
	f.answer(t, fmt.Sprint(pending["id"]), "accept")
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v after the user accepted", data, err)
	}
}

func TestSubagentWriterDeniedByTheUserDoesNotWrite(t *testing.T) {
	f := writerFixture(t, true)
	reader, closer, pending := f.startAndWaitForQuestion(t)
	defer closer.Close()
	f.answer(t, fmt.Sprint(pending["id"]), "decline")
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err == nil {
		t.Fatal("the child wrote after the user declined")
	}
	// The child saw the refusal as an ordinary blocked result and the run went on.
	child := f.provider.forModel("child-model")
	if len(child) < 2 || !strings.Contains(child[len(child)-1].Text, "approval_denied") {
		t.Fatalf("the child never saw the refusal: %d requests", len(child))
	}
}

func TestSubagentWriterWithNobodyToAskIsBlockedLikeTheParent(t *testing.T) {
	f := writerFixture(t, true)
	// The run does not support interaction, so there is nobody to ask.
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err == nil {
		t.Fatal("a child wrote with approval required and nobody to ask")
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 || !strings.Contains(child[len(child)-1].Text, "approval_unavailable") {
		t.Fatalf("the child did not see approval_unavailable")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
