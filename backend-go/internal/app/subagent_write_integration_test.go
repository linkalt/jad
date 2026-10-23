package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writerFixture is the subagent fixture with a write tool, a checkpoint and one agent
// that lists write.
func writerFixture(t *testing.T) subagentFixture {
	t.Helper()
	return writerFixtureWith(t, nil, nil)
}

// writerFixtureWith is writerFixture plus files that exist before the run and extra
// session settings (for example tool_call_scheduling).
func writerFixtureWith(t *testing.T, files map[string]string, session map[string]any) subagentFixture {
	t.Helper()
	return writerFixtureWithPaths(t, files, session, nil)
}

// writerFixtureWithPaths is writerFixtureWith plus the agent's paths (nil for none).
func writerFixtureWithPaths(t *testing.T, files map[string]string, session, paths map[string]any) subagentFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	write := map[string]any{"max_bytes": 4096}
	setting := map[string]any{
		"model": "child.json", "tools": []string{"read", "write"},
		"__write": write, "__checkpoint": `{"enabled":true}`,
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
	f := writerFixture(t)
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
	})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if records := f.checkpointRecords(t); len(records) != 0 {
		t.Fatalf("a read-only child took a checkpoint: %v", records)
	}
}

func TestSubagentWriterNeverHandsOffEvenWhenAskedTo(t *testing.T) {
	f := writerFixture(t)
	f.provider.handoffOnly = true // the parent asks for block_ms 0
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

func TestSubagentWriterWritesAtOnceWithNothingToAsk(t *testing.T) {
	f := writerFixture(t)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v; the child's write must run with no gate", data, err)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 || strings.Contains(child[len(child)-1].Text, "approval") {
		t.Fatalf("the child's write was gated")
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
	f := writerFixtureWith(t, map[string]string{"made.txt": "original"}, nil)
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
	f := writerFixture(t)
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
			f := writerFixtureWithPaths(t, map[string]string{"src/keep.txt": "k", "docs/keep.txt": "k", "src/secret/keep.txt": "k"}, nil,
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
	f := writerFixture(t)
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
	f := writerFixtureWithPaths(t, map[string]string{"src/keep.txt": "k"}, nil, map[string]any{"allow": []string{"src"}})
	f.provider.childFirstCall = [2]string{"write", `{"path":"docs/made.txt","content":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	last := parent[len(parent)-1].Text
	if !strings.Contains(last, "written_files: none (changes made by bash, exec or process are not listed)") {
		t.Fatalf("a refused write must not be listed as a change:\n%s", last)
	}
}
