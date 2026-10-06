package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

// providerStep is one scripted provider response: a function call, a final
// message, or an HTTP failure.
type providerStep struct {
	call    string // JSON function_call item
	message string
	status  int
}

func writeCall(callID, path, content string) providerStep {
	arguments, _ := json.Marshal(map[string]string{"path": path, "content": content})
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": callID, "name": "write", "arguments": string(arguments)})
	return providerStep{call: string(item)}
}

func readCall(callID, path string) providerStep {
	arguments, _ := json.Marshal(map[string]string{"path": path})
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": callID, "name": "read", "arguments": string(arguments)})
	return providerStep{call: string(item)}
}

type checkpointFixtureOptions struct {
	steps      []providerStep
	checkpoint string // JSON of the checkpoint node; empty omits it
	approval   bool   // write asks for approval
	nested     bool   // the session directory sits inside the workspace
}

type checkpointFixture struct {
	t          *testing.T
	workspace  string
	sessionDir string
	api        *httptest.Server
	service    *Service
	providerN  atomic.Int32
}

func newCheckpointFixture(t *testing.T, options checkpointFixtureOptions) *checkpointFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "ws")
	sessionsDir := filepath.Join(root, "sessions")
	if options.nested {
		sessionsDir = filepath.Join(workspace, ".jad", "sessions")
	}
	sessionDir := filepath.Join(sessionsDir, "demo")
	for _, dir := range []string{workspace, sessionDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}
	writeTool, err := native.NewWriteTool(sessionfs.NativeWriteSettings{Policy: policy, MaxBytes: 4096}, native.NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	readTool, err := native.NewReadTool(sessionfs.NativeReadSettings{Policy: policy, MaxBytes: 4096, MaxFileBytes: 4096, DefaultLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"schema_version": 1, "tools": []any{readTool.Definition, writeTool.Definition}})
	if err := os.WriteFile(filepath.Join(root, "tools.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := &checkpointFixture{t: t, workspace: workspace, sessionDir: sessionDir}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(fixture.providerN.Add(1)) - 1
		if index >= len(options.steps) {
			http.Error(w, "unexpected extra provider round", http.StatusBadRequest)
			return
		}
		step := options.steps[index]
		if step.status != 0 {
			http.Error(w, "scripted failure", step.status)
			return
		}
		var events []string
		if step.call != "" {
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + step.call + `}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","status":"completed","model":"gpt-test","output":[]}}`, index),
			}
		} else {
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + step.message + `"}]}}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","status":"completed","model":"gpt-test","output":[]}}`, index),
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range append(events, "[DONE]") {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)

	if err := os.WriteFile(filepath.Join(root, "setting.json"), []byte(fmt.Sprintf(`{"session_roots":[{"id":"local","path":%q}]}`, sessionsDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSetting := `{"max_bytes":4096`
	if options.approval {
		writeSetting += `,"approval":"ask"`
	}
	writeSetting += `}`
	setting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":4,"tool_definition_paths":[%q],"tools":{"native":{"read":{},"write":%s}}`,
		workspace, filepath.Join(root, "tools.json"), writeSetting)
	if options.checkpoint != "" {
		setting += `,"checkpoint":` + options.checkpoint
	}
	setting += `}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	model := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(model), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  root,
		Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fixture.service = service
	fixture.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		fixture.api.Close()
	})
	return fixture
}

func (f *checkpointFixture) request(method, path, body string, authorized bool) (int, string) {
	f.t.Helper()
	request, err := http.NewRequest(method, f.api.URL+"/api/sessions/local:demo"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if authorized {
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

// runToEnd runs one turn and reports "completed" or "error" from the stream.
func (f *checkpointFixture) runToEnd() string {
	f.t.Helper()
	status, body := f.request(http.MethodPost, "/run", `{"input":"go"}`, true)
	if status != http.StatusOK {
		f.t.Fatalf("run status = %d, body = %s", status, body)
	}
	if strings.Contains(body, "event: error") {
		return "error"
	}
	return "completed"
}

type listedCheckpoint map[string]any

func (f *checkpointFixture) checkpoints() (bool, []listedCheckpoint) {
	f.t.Helper()
	status, body := f.request(http.MethodGet, "/checkpoints", "", true)
	if status != http.StatusOK {
		f.t.Fatalf("list status = %d, body = %s", status, body)
	}
	var listing struct {
		Enabled     bool               `json:"enabled"`
		Checkpoints []listedCheckpoint `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		f.t.Fatalf("list body %q: %v", body, err)
	}
	return listing.Enabled, listing.Checkpoints
}

func (f *checkpointFixture) file(name string) string {
	data, err := os.ReadFile(filepath.Join(f.workspace, name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func (f *checkpointFixture) put(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.workspace, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestCheckpointsAreOffByDefaultAWriteRunsAndNothingIsRecorded(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{steps: []providerStep{writeCall("w1", "a.txt", "hello"), {message: "done"}}})
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	if f.file("a.txt") != "hello" {
		t.Fatalf("a.txt = %q", f.file("a.txt"))
	}
	enabled, records := f.checkpoints()
	if enabled || len(records) != 0 {
		t.Fatalf("enabled=%v records=%v", enabled, records)
	}
	staging := filepath.Join(f.sessionDir, "staging")
	if exists(filepath.Join(staging, "checkpoints.json")) || exists(filepath.Join(staging, "checkpoints.git")) {
		t.Fatal("an off checkpoint created files")
	}
}

func TestARunThatWritesTakesOneCheckpointAndTheUserCanListDryRunAndRestoreIt(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), writeCall("w2", "b.txt", "second"), {message: "done"}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	if f.file("a.txt") != "agent" {
		t.Fatalf("a.txt = %q", f.file("a.txt"))
	}
	enabled, records := f.checkpoints()
	if !enabled || len(records) != 1 {
		t.Fatalf("enabled=%v records=%v: one checkpoint is expected for the whole run, not one per write", enabled, records)
	}
	if records[0]["state"] != "complete" || records[0]["kind"] != "run" {
		t.Fatalf("record = %v", records[0])
	}
	id := records[0]["id"].(string)

	status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{"dry_run":true}`, true)
	if status != http.StatusOK {
		t.Fatalf("dry run status = %d body = %s", status, body)
	}
	var planned struct {
		Restore struct {
			Restored []string `json:"restored"`
			Deleted  []string `json:"deleted"`
			DryRun   bool     `json:"dry_run"`
		} `json:"restore"`
	}
	if err := json.Unmarshal([]byte(body), &planned); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(planned.Restore.Restored) != "[a.txt]" || fmt.Sprint(planned.Restore.Deleted) != "[b.txt]" || !planned.Restore.DryRun {
		t.Fatalf("plan = %+v", planned.Restore)
	}
	if f.file("a.txt") != "agent" {
		t.Fatal("a dry run changed a file")
	}

	if status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{}`, true); status != http.StatusOK {
		t.Fatalf("restore status = %d body = %s", status, body)
	}
	if f.file("a.txt") != "original" || exists(filepath.Join(f.workspace, "b.txt")) {
		t.Fatalf("after restore a.txt = %q, b.txt exists = %v", f.file("a.txt"), exists(filepath.Join(f.workspace, "b.txt")))
	}
	_, after := f.checkpoints()
	if len(after) != 2 || after[0]["kind"] != "pre_restore" {
		t.Fatalf("records after restore = %v: the restore must be recorded so it can be undone", after)
	}
}

func TestAReadOnlyRunTakesNoCheckpoint(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{readCall("r1", "a.txt"), {message: "done"}},
	})
	f.put("a.txt", "x")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	if _, records := f.checkpoints(); len(records) != 0 {
		t.Fatalf("records = %v", records)
	}
	if exists(filepath.Join(f.sessionDir, "staging", "checkpoints.git")) {
		t.Fatal("a read-only run created the private repository")
	}
}

func TestARunThatFailsAfterChangingFilesStillRecordsItsEndState(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {status: http.StatusBadRequest}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "error" {
		t.Fatalf("run = %s, want error", got)
	}
	_, records := f.checkpoints()
	if len(records) != 1 || records[0]["state"] != "complete" || records[0]["after_tree"] == nil {
		t.Fatalf("records = %v: the end of a failed run must be recorded", records)
	}
	status, body := f.request(http.MethodPost, "/checkpoints/"+records[0]["id"].(string)+"/restore", `{}`, true)
	if status != http.StatusOK || f.file("a.txt") != "original" {
		t.Fatalf("restore status = %d body = %s a.txt = %q", status, body, f.file("a.txt"))
	}
}

func TestASnapshotThatCannotBeTakenDoesNotStopTheToolCallAndIsRecorded(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true,"git_binary":"/nonexistent/git"}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "hello"), {message: "done"}},
	})
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	if f.file("a.txt") != "hello" {
		t.Fatalf("a.txt = %q: the write must still run", f.file("a.txt"))
	}
	_, records := f.checkpoints()
	if len(records) != 1 || records[0]["state"] != "failed" || !strings.Contains(fmt.Sprint(records[0]["warning"]), "cannot run") {
		t.Fatalf("records = %v", records)
	}
	if status, _ := f.request(http.MethodPost, "/checkpoints/"+records[0]["id"].(string)+"/restore", `{}`, true); status != http.StatusConflict {
		t.Fatalf("restore status = %d, want 409", status)
	}
}

func TestASessionDirectoryInsideTheWorkspaceIsNeverPartOfASnapshotOrARestore(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		nested:     true,
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	messagesBefore, err := os.ReadFile(filepath.Join(f.sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, records := f.checkpoints()
	id := records[0]["id"].(string)
	status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{"dry_run":true}`, true)
	if status != http.StatusOK {
		t.Fatalf("dry run status = %d body = %s", status, body)
	}
	var planned struct {
		Restore struct {
			Restored  []string `json:"restored"`
			Deleted   []string `json:"deleted"`
			Conflicts []string `json:"conflicts"`
		} `json:"restore"`
	}
	if err := json.Unmarshal([]byte(body), &planned); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(append(append(planned.Restore.Restored, planned.Restore.Deleted...), planned.Restore.Conflicts...)); got != "[a.txt]" {
		t.Fatalf("planned paths = %s, want only a.txt", got)
	}
	if status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{}`, true); status != http.StatusOK {
		t.Fatalf("restore status = %d body = %s", status, body)
	}
	messagesAfter, _ := os.ReadFile(filepath.Join(f.sessionDir, "messages.jsonl"))
	if string(messagesAfter) != string(messagesBefore) {
		t.Fatal("the transcript was rolled back by the restore")
	}
}

func TestRestoreIsRefusedWith409WhileARunIsActiveAndTheFileIsLeftAlone(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		approval:   true,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"go","supports_interaction":true}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var pendingID string
	deadline := time.Now().Add(10 * time.Second)
	var event string
	for pendingID == "" && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the approval question: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") && event == "interaction" {
			var data struct {
				Pending *struct {
					ID string `json:"id"`
				} `json:"pending"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data) == nil && data.Pending != nil {
				pendingID = data.Pending.ID
			}
		}
	}
	if pendingID == "" {
		t.Fatal("no approval question arrived")
	}
	// The run is parked on the approval; its checkpoint was taken before the question.
	_, records := f.checkpoints()
	if len(records) != 1 || records[0]["state"] != "open" {
		t.Fatalf("records = %v, want one open checkpoint", records)
	}
	status, body := f.request(http.MethodPost, "/checkpoints/"+records[0]["id"].(string)+"/restore", `{"mode":"full"}`, true)
	if status != http.StatusConflict || !strings.Contains(body, "run is active") {
		t.Fatalf("restore during a run: status = %d body = %s", status, body)
	}
	if f.file("a.txt") != "original" {
		t.Fatalf("a.txt = %q: the parked write must not have run", f.file("a.txt"))
	}
	if status, body := f.request(http.MethodPost, "/run/interaction", fmt.Sprintf(`{"interaction_id":%q,"decision":"accept"}`, pendingID), true); status != http.StatusOK {
		t.Fatalf("answer status = %d body = %s", status, body)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	_, after := f.checkpoints()
	if len(after) != 1 || after[0]["state"] != "complete" {
		t.Fatalf("records after the run = %v", after)
	}
}

func TestTheCheckpointRoutesAnswer404And400And401(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{checkpoint: `{"enabled":true}`})
	if status, _ := f.request(http.MethodPost, "/checkpoints/ckpt-nope/restore", `{}`, true); status != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", status)
	}
	for _, body := range []string{`{"mode":"everything"}`, `{"dry_run":"yes"}`, `{"surprise":1}`, `[]`, ``, `{`} {
		if status, response := f.request(http.MethodPost, "/checkpoints/ckpt-nope/restore", body, true); status != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400 (%s)", body, status, response)
		}
	}
	if status, _ := f.request(http.MethodGet, "/checkpoints", "", false); status != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", status)
	}
	if status, _ := f.request(http.MethodPost, "/checkpoints", `{}`, true); status != http.StatusNotFound {
		t.Fatalf("wrong method status = %d, want 404", status)
	}
}

// leaveOpen rewrites the stored records the way a server killed mid-run leaves them.
func (f *checkpointFixture) leaveOpen(kinds ...string) {
	f.t.Helper()
	path := filepath.Join(f.sessionDir, "staging", "checkpoints.json")
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var document struct {
		SchemaVersion int              `json:"schema_version"`
		Checkpoints   []map[string]any `json:"checkpoints"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		f.t.Fatal(err)
	}
	for _, record := range document.Checkpoints {
		for _, kind := range kinds {
			if record["kind"] == kind {
				record["state"] = "open"
				delete(record, "after_tree")
				delete(record, "finished_at")
				delete(record, "warning")
			}
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestARunCheckpointLeftOpenByAStoppedServerIsListedAsInterruptedAndAFullRestoreStillWorks(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	f.leaveOpen("run", "pre_restore")
	path := filepath.Join(f.sessionDir, "staging", "checkpoints.json")
	stored, _ := os.ReadFile(path)

	_, records := f.checkpoints()
	if len(records) != 1 || records[0]["state"] != "interrupted" {
		t.Fatalf("records = %v, want one interrupted checkpoint", records)
	}
	if records[0]["warning"] != "The run ended without recording its end (the server stopped or crashed)." || records[0]["before_tree"] == nil {
		t.Fatalf("record = %v, want the warning and the saved state kept", records[0])
	}
	if after, _ := os.ReadFile(path); string(after) != string(stored) {
		t.Fatal("listing wrote the record back")
	}

	id := records[0]["id"].(string)
	status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{"mode":"safe"}`, true)
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || status != http.StatusConflict || !strings.Contains(refusal.Error, `use mode "full"`) {
		t.Fatalf("safe restore: status = %d body = %s", status, body)
	}
	if status, body := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{"mode":"full"}`, true); status != http.StatusOK {
		t.Fatalf("full restore: status = %d body = %s", status, body)
	}
	if f.file("a.txt") != "original" {
		t.Fatalf("a.txt = %q, want the original", f.file("a.txt"))
	}
}

func TestAPreRestoreCheckpointLeftOpenIsReportedTheSameWayAndFinishedRecordsAreUntouched(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	_, records := f.checkpoints()
	if status, body := f.request(http.MethodPost, "/checkpoints/"+records[0]["id"].(string)+"/restore", `{}`, true); status != http.StatusOK {
		t.Fatalf("restore: status = %d body = %s", status, body)
	}
	pairs := func() string {
		_, listed := f.checkpoints()
		var out []string
		for _, record := range listed {
			out = append(out, fmt.Sprintf("%v/%v", record["kind"], record["state"]))
		}
		return strings.Join(out, " ")
	}
	if got := pairs(); got != "pre_restore/complete run/complete" {
		t.Fatalf("before = %q", got)
	}
	f.leaveOpen("pre_restore")
	if got := pairs(); got != "pre_restore/interrupted run/complete" {
		t.Fatalf("after = %q", got)
	}
}

func TestACheckpointIsOpenNotInterruptedWhileItsRunIsLive(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		approval:   true,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"go","supports_interaction":true}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var pendingID, event string
	deadline := time.Now().Add(10 * time.Second)
	for pendingID == "" && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the approval question: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") && event == "interaction" {
			var data struct {
				Pending *struct {
					ID string `json:"id"`
				} `json:"pending"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data) == nil && data.Pending != nil {
				pendingID = data.Pending.ID
			}
		}
	}
	if pendingID == "" {
		t.Fatal("no approval question arrived")
	}
	if _, records := f.checkpoints(); len(records) != 1 || records[0]["state"] != "open" {
		t.Fatalf("records = %v, want one open checkpoint while the run is live", records)
	}
	if status, body := f.request(http.MethodPost, "/run/interaction", fmt.Sprintf(`{"interaction_id":%q,"decision":"accept"}`, pendingID), true); status != http.StatusOK {
		t.Fatalf("answer status = %d body = %s", status, body)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if _, records := f.checkpoints(); len(records) != 1 || records[0]["state"] != "complete" {
		t.Fatalf("records after the run = %v", records)
	}
}

// slowGit writes a git wrapper that waits while flag exists, so a restore can be
// held open long enough to look at it; without the flag it is plain git.
func slowGit(t *testing.T, dir, flag string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	script := filepath.Join(dir, "slow-git.sh")
	body := fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then sleep 0.12; fi\nexec %q \"$@\"\n", flag, gitPath)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func (f *checkpointFixture) restoring() bool {
	f.t.Helper()
	status, body := f.request(http.MethodGet, "/checkpoints", "", true)
	if status != http.StatusOK {
		f.t.Fatalf("list status = %d, body = %s", status, body)
	}
	var listing struct {
		Restoring *bool `json:"restoring"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil || listing.Restoring == nil {
		f.t.Fatalf("list body has no restoring field: %q (%v)", body, err)
	}
	return *listing.Restoring
}

func TestTheListSaysRestoringWhileARealRestoreRunsAndNotForADryRun(t *testing.T) {
	scratch := t.TempDir()
	flag := filepath.Join(scratch, "slow.flag")
	git := slowGit(t, scratch, flag)
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: fmt.Sprintf(`{"enabled":true,"git_binary":%q}`, git),
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	f.put("a.txt", "original")
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	_, records := f.checkpoints()
	id := records[0]["id"].(string)
	if f.restoring() {
		t.Fatal("restoring before the restore started")
	}

	if err := os.WriteFile(flag, []byte("slow"), 0o600); err != nil {
		t.Fatal(err)
	}
	dryDone := make(chan int, 1)
	go func() {
		status, _ := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{"dry_run":true}`, true)
		dryDone <- status
	}()
	time.Sleep(60 * time.Millisecond)
	if f.restoring() {
		t.Fatal("a dry run was reported as a restore")
	}
	if status := <-dryDone; status != http.StatusOK {
		t.Fatalf("dry run status = %d", status)
	}

	restoreDone := make(chan int, 1)
	go func() {
		status, _ := f.request(http.MethodPost, "/checkpoints/"+id+"/restore", `{}`, true)
		restoreDone <- status
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !f.restoring() {
		if time.Now().After(deadline) {
			t.Fatal("the list never reported the restore")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if f.file("a.txt") != "agent" {
		t.Fatalf("the restore had already finished: a.txt = %q", f.file("a.txt"))
	}
	// A second restore sent now is not asserted: TypeScript refuses it with 409 but
	// Go queues it behind the first (a recorded, separate difference).
	if status := <-restoreDone; status != http.StatusOK {
		t.Fatalf("restore status = %d", status)
	}
	if f.file("a.txt") != "original" {
		t.Fatalf("a.txt = %q", f.file("a.txt"))
	}
	if f.restoring() {
		t.Fatal("restoring stayed true after the restore ended")
	}
}

func TestARefusedRestoreDoesNotLeaveTheListReportingRestoring(t *testing.T) {
	f := newCheckpointFixture(t, checkpointFixtureOptions{
		checkpoint: `{"enabled":true}`,
		steps:      []providerStep{writeCall("w1", "a.txt", "agent"), {message: "done"}},
	})
	if got := f.runToEnd(); got != "completed" {
		t.Fatalf("run = %s", got)
	}
	if status, _ := f.request(http.MethodPost, "/checkpoints/ckpt-nope/restore", `{}`, true); status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	if f.restoring() {
		t.Fatal("a refused restore left restoring true")
	}
}
