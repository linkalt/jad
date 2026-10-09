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
	return writerFixtureWith(t, approval, nil, nil)
}

// writerFixtureWith is writerFixture plus files that exist before the run and extra
// session settings (for example tool_call_scheduling).
func writerFixtureWith(t *testing.T, approval bool, files map[string]string, session map[string]any) subagentFixture {
	t.Helper()
	return writerFixtureWithPaths(t, approval, files, session, nil)
}

// writerFixtureWithPaths is writerFixtureWith plus the agent's paths (nil for none).
func writerFixtureWithPaths(t *testing.T, approval bool, files map[string]string, session, paths map[string]any) subagentFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	write := map[string]any{"max_bytes": 4096}
	if approval {
		write["approval"] = "ask"
	}
	setting := map[string]any{
		"model": "child.json", "tools": []string{"read", "write"},
		"__write": write, "__checkpoint": `{"enabled":true}`,
		"__subagent_settings": map[string]any{"allow_write": true},
	}
	if session != nil {
		setting["__session"] = session
	}
	if paths != nil {
		setting["paths"] = paths
	}
	f := newSubagentFixtureWithFiles(t, setting, files)
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

func (f subagentFixture) postBody(t *testing.T, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo"+path, strings.NewReader(body))
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
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (f subagentFixture) read(name string) string {
	data, err := os.ReadFile(filepath.Join(f.workspace, name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func TestSubagentWriterChangesAreUndoneByRestoringTheParentRunsCheckpoint(t *testing.T) {
	f := writerFixtureWith(t, false, map[string]string{"made.txt": "original"}, nil)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if f.read("made.txt") != "by the child" {
		t.Fatalf("made.txt = %q: the child did not overwrite it", f.read("made.txt"))
	}
	records := f.checkpointRecords(t)
	if len(records) != 1 || records[0]["state"] != "complete" {
		t.Fatalf("checkpoints = %v", records)
	}
	id := fmt.Sprint(records[0]["id"])
	// The plan names the file the child changed, and a dry run touches nothing.
	status, body := f.postBody(t, "/checkpoints/"+id+"/restore", `{"dry_run":true}`)
	if status != http.StatusOK || !strings.Contains(body, "made.txt") {
		t.Fatalf("dry run status = %d, body = %s; the plan must name the child's file", status, body)
	}
	if f.read("made.txt") != "by the child" {
		t.Fatal("a dry run changed the file")
	}
	if status, body := f.postBody(t, "/checkpoints/"+id+"/restore", `{}`); status != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", status, body)
	}
	if f.read("made.txt") != "original" {
		t.Fatalf("after the restore made.txt = %q, want the original content back", f.read("made.txt"))
	}
}

func TestSubagentWriterCreatedFileIsRemovedByTheRestore(t *testing.T) {
	f := writerFixture(t, false)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if f.read("made.txt") != "by the child" {
		t.Fatalf("made.txt = %q", f.read("made.txt"))
	}
	records := f.checkpointRecords(t)
	if len(records) != 1 {
		t.Fatalf("checkpoints = %v", records)
	}
	if status, body := f.postBody(t, "/checkpoints/"+fmt.Sprint(records[0]["id"])+"/restore", `{}`); status != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the file the child created is still there after the restore (err %v)", err)
	}
}

// answerEveryQuestion streams a run and answers each approval question as it arrives,
// and returns the questions in the order they were asked. It fails if two questions are
// ever open at once: the broker holds one, so a second would be refused.
func (f subagentFixture) answerEveryQuestion(t *testing.T, decision string) []map[string]any {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"ask the explorer","supports_interaction":true}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var asked []map[string]any
	var event string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			return asked
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") || event != "interaction" {
			continue
		}
		var data struct {
			Pending map[string]any `json:"pending"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data) != nil || data.Pending == nil {
			continue
		}
		asked = append(asked, data.Pending)
		f.answer(t, fmt.Sprint(data.Pending["id"]), decision)
	}
	t.Fatal("the run did not finish in time")
	return asked
}

func TestSubagentTwoWritersAskingAtOnceAreAskedOneAtATime(t *testing.T) {
	for _, scheduling := range []string{"ordered", "concurrent"} {
		t.Run(scheduling, func(t *testing.T) {
			f := writerFixtureWith(t, true, nil, map[string]any{"tool_call_scheduling": scheduling, "max_parallel_tool_calls": 4})
			f.provider.parentCalls = 2
			f.provider.childWritesByTask = true
			asked := f.answerEveryQuestion(t, "accept")
			if len(asked) != 2 {
				t.Fatalf("questions asked = %d, want 2 (one per writer)", len(asked))
			}
			calls := map[string]bool{}
			for _, question := range asked {
				if question["tool"] != "write" || !strings.Contains(fmt.Sprint(question["title"]), "(agent explore)") {
					t.Errorf("question = %v", question)
				}
				calls[fmt.Sprint(question["tool_call_id"])] = true
			}
			if len(calls) != 2 {
				t.Errorf("the two questions share a call id: %v", calls)
			}
			// Both writes happened: a second question refused for being a second would
			// have blocked its child's write.
			for _, name := range []string{"w0.txt", "w1.txt"} {
				if got := f.read(name); !strings.HasPrefix(got, "child ") {
					t.Errorf("%s = %q; the write was blocked", name, got)
				}
			}
			if records := f.checkpointRecords(t); len(records) != 1 {
				t.Errorf("checkpoints = %d, want one for the whole run", len(records))
			}
		})
	}
}

func TestSubagentWriterParkedOnAQuestionEndsCleanlyWhenTheRunIsStopped(t *testing.T) {
	f := writerFixture(t, true)
	reader, closer, pending := f.startAndWaitForQuestion(t)
	defer closer.Close()
	if status, body := f.postBody(t, "/run/stop", ``); status != http.StatusOK {
		t.Fatalf("stop status = %d, body = %s", status, body)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the stream did not end after the stop; the question %v is still holding the run", pending["id"])
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err == nil {
		t.Fatal("the child wrote after the run was stopped")
	}
	// The stopped run closes its checkpoint and the child's run is not left going.
	deadline := time.Now().Add(5 * time.Second)
	for {
		records := f.checkpointRecords(t)
		if len(records) == 1 && records[0]["state"] == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoints after the stop = %v, want one complete", records)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, body := f.get(t, "/run"); status == http.StatusOK && strings.Contains(body, `"status":"running"`) {
		t.Fatalf("the run is still going: %s", body)
	}
}

// A writer child with paths: its write inside the allowed folder happens, and the same
// write outside it is refused by the real write tool with a result the child sees.
func TestSubagentWriterPathsLimitWhereTheChildMayWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		target    string
		wantFile  bool
		wantError string
	}{
		"inside allow":  {target: "src/made.txt", wantFile: true},
		"outside allow": {target: "docs/made.txt", wantError: "path_outside_agent_paths"},
		"denied inside": {target: "src/secret/made.txt", wantError: "path_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			f := writerFixtureWithPaths(t, false, map[string]string{"src/keep.txt": "k", "docs/keep.txt": "k", "src/secret/keep.txt": "k"}, nil,
				map[string]any{"allow": []string{"src"}, "deny": []string{"src/secret"}})
			f.provider.childFirstCall = [2]string{"write", `{"path":"` + tc.target + `","content":"by the child"}`}
			if body := f.run(t); !strings.Contains(body, "event: completed") {
				t.Fatalf("run did not complete:\n%s", body)
			}
			_, err := os.Stat(filepath.Join(f.workspace, tc.target))
			if wrote := err == nil; wrote != tc.wantFile {
				t.Fatalf("file %s written = %v, want %v", tc.target, wrote, tc.wantFile)
			}
			if tc.wantError != "" {
				seen := false
				for _, request := range f.provider.forModel("child-model") {
					if strings.Contains(request.Text, tc.wantError) {
						seen = true
					}
				}
				if !seen {
					t.Fatalf("the child never got %s in a tool result", tc.wantError)
				}
			}
		})
	}
}

// The parent sees which files a writer child changed, taken from the real write tool.
func TestSubagentWriterResultListsTheFilesItChanged(t *testing.T) {
	f := writerFixture(t, false)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if !strings.Contains(last, "written_files: made.txt (created)") {
		t.Fatalf("the parent's tool result does not list the file the child wrote:\n%s", last)
	}
}

func TestSubagentWriterThatFailedToWriteListsNone(t *testing.T) {
	f := writerFixtureWithPaths(t, false, map[string]string{"src/keep.txt": "k"}, nil, map[string]any{"allow": []string{"src"}})
	f.provider.childFirstCall = [2]string{"write", `{"path":"docs/made.txt","content":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if !strings.Contains(last, "written_files: none (changes made by bash or exec are not listed)") {
		t.Fatalf("a refused write must not be listed as a change:\n%s", last)
	}
}
