package app

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// R0 baseline of the unified agent run design (docs/design_unified_agent_runs.md): golden
// transcripts and tool definitions of a nested sub-agent tree, a profile_message chain, and a
// remote sub-agent start and resume, taken before StartRun, the template loader and the shared
// registry change how these runs are started. Every later phase (R1 to R5) must leave these files
// byte for byte equal. Only values that differ between two runs of the same scenario are masked:
// the temporary folders, the random suffix of a run folder, the remote run ids, the input batch ids
// and the content tokens of tool results.
//
// Set JAD_UPDATE_RUN_BASELINE=1 to rewrite the files; a rewrite is a contract change and needs a
// decision of its own.

const runBaselineDir = "testdata/run_baseline"

// runBaselineMasker replaces the values that change between two runs of one scenario.
type runBaselineMasker struct {
	paths []string // longest first
	names []string
}

// runBaselineRules mask the random values the paths do not cover.
var runBaselineRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	// A temporary folder of the test that no fixture field names (the layout root of the skills).
	{regexp.MustCompile(`(?:/[^/"\\\s]+)*/TestRunBaseline[A-Za-z]+[0-9]+/[0-9]+`), "<tmp>"},
	// A run folder is <call id or remote_agent>-<random hex>.
	{regexp.MustCompile(`\b((?:call-|remote_)[A-Za-z0-9_-]*?)-[0-9a-f]{8,}\b`), "${1}-<hex>"},
}

// runBaselineNumbered are random values masked as a placeholder numbered by first appearance in one
// masked text, so equal values stay equal (a resumed run keeps its source's batch ids).
var runBaselineNumbered = []struct {
	prefix string
	value  *regexp.Regexp
	name   string
}{
	{`"__agent_session_input_batch_id":"`, regexp.MustCompile(`"__agent_session_input_batch_id":"[0-9a-f]{32}`), "batch"},
	{"token=", regexp.MustCompile(`token=r_[A-Za-z0-9_-]{22}`), "token"},
	{"", regexp.MustCompile(`\bsub_[0-9a-f]{8}\b`), "run"},
	{"", regexp.MustCompile(`\brr_[0-9a-f]{8,}\b`), "remote-run"},
}

func newRunBaselineMasker(paths map[string]string) *runBaselineMasker {
	m := &runBaselineMasker{}
	for path := range paths {
		if path != "" {
			m.paths = append(m.paths, path)
		}
	}
	sort.Slice(m.paths, func(i, j int) bool { return len(m.paths[i]) > len(m.paths[j]) })
	for _, path := range m.paths {
		m.names = append(m.names, paths[path])
	}
	return m
}

func (m *runBaselineMasker) mask(text string) string {
	for index, path := range m.paths {
		escaped, _ := json.Marshal(path)
		text = strings.ReplaceAll(text, strings.Trim(string(escaped), `"`), m.names[index])
		text = strings.ReplaceAll(text, path, m.names[index])
	}
	for _, rule := range runBaselineRules {
		text = rule.pattern.ReplaceAllString(text, rule.replace)
	}
	for _, rule := range runBaselineNumbered {
		seen := map[string]int{}
		text = rule.value.ReplaceAllStringFunc(text, func(value string) string {
			if _, ok := seen[value]; !ok {
				seen[value] = len(seen) + 1
			}
			return fmt.Sprintf("%s<%s-%d>", rule.prefix, rule.name, seen[value])
		})
	}
	return text
}

// compareRunBaseline compares got with the golden file name, or rewrites it when asked.
func compareRunBaseline(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(runBaselineDir, name)
	if os.Getenv("JAD_UPDATE_RUN_BASELINE") == "1" {
		if err := os.MkdirAll(runBaselineDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (set JAD_UPDATE_RUN_BASELINE=1 to write it)", path, err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the R0 baseline (%d bytes, want %d):\n--- got ---\n%s\n--- want ---\n%s", path, len(got), len(want), got, want)
	}
}

// runTranscripts returns every messages.jsonl under dir, keyed by its masked folder path relative
// to dir, joined in key order with a header line per file.
func runTranscripts(t *testing.T, dir string, m *runBaselineMasker) string {
	t.Helper()
	// Folders whose names mask alike (a run and its resume) are told apart by their content.
	files := map[string][]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "messages.jsonl" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		key := m.mask(filepath.ToSlash(rel))
		files[key] = append(files[key], m.mask(string(data)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		sort.Strings(files[key])
		for index, data := range files[key] {
			fmt.Fprintf(&out, "=== %s #%d\n%s", key, index+1, data)
			if !strings.HasSuffix(data, "\n") {
				out.WriteString("\n")
			}
		}
	}
	return out.String()
}

// toolDefinitions lists the tools array of every provider request in order, one line per tool,
// so a change of a tool set, its order or a description shows as a diff.
func toolDefinitions(requests []providerRequest, m *runBaselineMasker) string {
	var out strings.Builder
	for index, request := range requests {
		fmt.Fprintf(&out, "=== request %d model %s\n", index+1, request.Model)
		for _, tool := range request.RawTools {
			out.WriteString(m.mask(tool))
			out.WriteString("\n")
		}
	}
	return out.String()
}

func (p *subagentProvider) all() []providerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerRequest(nil), p.requests...)
}

func subagentFixtureMasker(f subagentFixture) *runBaselineMasker {
	return newRunBaselineMasker(map[string]string{
		f.workspace: "<workspace>", f.stateDir: "<state>", f.toolDir: "<tools>", f.modelsDir: "<models>",
		f.sessionDir: "<session>", f.apiURL: "<api>",
	})
}

// A three-level ephemeral tree: the session's run starts explore, explore starts explore again, and
// the grandchild (at max_depth 2, so its catalog has no subagent tool) reads the notes skill.
func TestRunBaselineNestedSubagentTree(t *testing.T) {
	f := buildSubagentFixture(t, map[string]any{"model": "child.json", "__subagent_settings": map[string]any{
		"max_depth": 2,
		"agents": map[string]any{
			"explore": map[string]any{"model": "openai/child", "description": "Finds facts.", "tools": []string{"skill", "subagent"}},
		},
	}}, false, nil)
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"look around","agent":"explore"}`}
	f.provider.childFirstCall = [2]string{"subagent", `{"task":"look deeper","agent":"explore"}`}
	f.provider.leafFirstCall = [2]string{"skill", notesCall}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if child := f.provider.forModel("child-model"); len(child) != 4 {
		t.Fatalf("child requests = %d, want 4 (two levels, two rounds each)", len(child))
	}
	m := subagentFixtureMasker(f)
	compareRunBaseline(t, "nested_tree_session.jsonl", m.mask(strings.Join(readLines(t, filepath.Join(f.stateDir, "messages.jsonl")), "\n")+"\n"))
	compareRunBaseline(t, "nested_tree_children.txt", runTranscripts(t, f.toolDir, m))
	compareRunBaseline(t, "nested_tree_tools.txt", toolDefinitions(f.provider.all(), m))
}

// A remote sub-agent start, then a resume of that run: the resumed run's folder starts from a copy
// of the source transcript (SubagentRequest.Seed) and the source is left alone.
func TestRunBaselineRemoteSubagentStartAndResume(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "first", "request_id": "req-baseline-1"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	source, _ := body["id"].(string)
	first := server.waitDone(t, source)
	status, body = server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "more", "resume": source, "request_id": "req-baseline-2"})
	if status != 202 {
		t.Fatalf("resume: %d %v", status, body)
	}
	resumedID, _ := body["id"].(string)
	resumed := server.waitDone(t, resumedID)
	if first["state"] != "done" || resumed["state"] != "done" || resumed["resumed_from"] != source {
		t.Fatalf("source %v, resumed %v", first, resumed)
	}
	m := newRunBaselineMasker(map[string]string{
		f.workspace: "<workspace>", f.stateDir: "<state>", f.toolDir: "<tools>", f.modelsDir: "<models>",
		f.sessionDir: "<session>", source: "<source-id>", resumedID: "<resumed-id>",
	})
	var out strings.Builder
	for _, run := range []map[string]any{first, resumed} {
		keys := make([]string, 0, len(run))
		for key := range run {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintf(&out, "=== status keys %s\n", strings.Join(keys, ","))
		for _, key := range []string{"state", "text", "resumed_from"} {
			if value, ok := run[key]; ok {
				encoded, _ := json.Marshal(value)
				fmt.Fprintf(&out, "%s: %s\n", key, m.mask(string(encoded)))
			}
		}
	}
	compareRunBaseline(t, "remote_resume_status.txt", out.String())
	compareRunBaseline(t, "remote_resume_children.txt", runTranscripts(t, f.toolDir, m))
}

// A profile_message chain dev/coder -> review/reviewer, where the reviewer messages dev/coder back
// and is refused as a cycle: both persistent transcripts as written.
func TestRunBaselineProfileMessageChain(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{}},"mcp":{}}`)
	coderCall := scriptFunctionCall("profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
	reviewerCall := scriptFunctionCall("profile_message", map[string]any{"profile": "dev/coder", "message": "back to you"})
	f.script = func(model, body string) string {
		if model == "model-large" {
			return reviewerCall(model, body)
		}
		return coderCall(model, body)
	}
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	state := filepath.Join(f.cwd, ".jad", "state", "demo")
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	coder, err := os.ReadFile(filepath.Join(state, "dev", "coder", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := os.ReadFile(filepath.Join(state, "review", "reviewer", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewer), "profile_cycle") {
		t.Fatalf("the reviewer's call back to dev/coder was not refused as a cycle:\n%s", reviewer)
	}
	compareRunBaseline(t, "profile_chain_coder.jsonl", m.mask(string(coder)))
	compareRunBaseline(t, "profile_chain_reviewer.jsonl", m.mask(string(reviewer)))
}

// layoutCall is one function_call output item of the layout fixture's provider (f.script).
func layoutCall(callID, name string, arguments map[string]any) string {
	encoded, _ := json.Marshal(arguments)
	item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": callID, "name": name, "arguments": string(encoded)})
	return string(item)
}

// requestTools lists the tools array of every captured request body in order, one line per tool.
func requestTools(t *testing.T, bodies []string, m *runBaselineMasker) string {
	t.Helper()
	var out strings.Builder
	for index, body := range bodies {
		var request struct {
			Model string            `json:"model"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "=== request %d model %s\n", index+1, request.Model)
		for _, tool := range request.Tools {
			out.WriteString(m.mask(string(tool)))
			out.WriteString("\n")
		}
	}
	return out.String()
}

func (f *layoutFixture) capturedBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// A local resume through the subagent tool's resume argument: a first session run starts explore, a
// second run resumes that run by its id. The resumed run's folder starts from a copy of the source.
func TestRunBaselineLocalSubagentResume(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	first := subagentStep{"subagent", `{"task":"first","agent":"explore"}`}
	f.provider.mu.Lock()
	f.provider.parentSteps = []subagentStep{first}
	f.provider.mu.Unlock()
	if body := f.runInput(t, "start one"); !strings.Contains(body, "event: completed") {
		t.Fatalf("first run did not complete:\n%s", body)
	}
	// A finished blocking call does not name its run id; the run's manifest (<id>.json) does.
	manifests, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "sub_*.json"))
	if err != nil || len(manifests) != 1 {
		all, _ := filepath.Glob(filepath.Join(f.toolDir, "*", "*"))
		t.Fatalf("manifests = %v, %v; tool dir holds %v", manifests, err, all)
	}
	source := strings.TrimSuffix(filepath.Base(manifests[0]), ".json")
	f.provider.mu.Lock()
	f.provider.parentSteps = []subagentStep{first, {"subagent", `{"task":"more","agent":"explore","resume":"` + source + `"}`}}
	f.provider.mu.Unlock()
	if body := f.runInput(t, "resume it"); !strings.Contains(body, "event: completed") {
		t.Fatalf("second run did not complete:\n%s", body)
	}
	session := strings.Join(readLines(t, filepath.Join(f.stateDir, "messages.jsonl")), "\n") + "\n"
	if !strings.Contains(session, "resumed_from") {
		t.Fatalf("the second call did not resume %s:\n%s", source, session)
	}
	m := subagentFixtureMasker(f)
	compareRunBaseline(t, "local_resume_session.jsonl", m.mask(session))
	compareRunBaseline(t, "local_resume_children.txt", runTranscripts(t, f.toolDir, m))
}

// A remote sub-agent started by the caller's subagent tool (agent remote:<name>/...): the caller's
// tool definitions and transcript, and the child transcript on the serving JAD.
func TestRunBaselineRemoteSubagentFromTheCaller(t *testing.T) {
	server, serverFixture := e2eServer(t)
	f := newLayoutFixture(t)
	entry := fmt.Sprintf(`{"kind":"remote","remote":{"url":%q,"token_env":"JAD_REMOTE_E2E","timeout_ms":5000,"poll_ms":300,"lost_after_ms":1500,"retries":1,"default_session":"demo"}}`, server.url)
	writeFile(t, filepath.Join(f.root, ".jad", "remotes", "peer.json"), []byte(entry))
	f.overwrite(t, "tools/coding/none.json", `{"native":{"async":{},"subagent":{"agents":{"explore":{"description":"Finds facts."}},"remotes":["peer"]}},"mcp":{}}`)
	f.captureBodies = true
	f.script = func(_, body string) string {
		if strings.Contains(body, "function_call_output") {
			return ""
		}
		return layoutCall("call-remote", "subagent", map[string]any{"agent": "remote:peer/demo/dev/coder/explore", "task": "find the answer"})
	}
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"ask the remote"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	coder, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(coder), "the answer is 42") {
		t.Fatalf("the remote child's answer did not reach the caller:\n%s", coder)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>", server.url: "<server>",
		serverFixture.workspace: "<server-workspace>", serverFixture.toolDir: "<server-tools>"})
	compareRunBaseline(t, "remote_caller_coder.jsonl", m.mask(string(coder)))
	compareRunBaseline(t, "remote_caller_tools.txt", requestTools(t, f.capturedBodies(), m))
	compareRunBaseline(t, "remote_caller_server_children.txt", runTranscripts(t, serverFixture.toolDir, m))
}

// A persistent target reached by profile_message starts an ephemeral run: dev/coder messages
// review/reviewer, whose run starts explore, which reads the notes skill. Every transcript in the
// session's state folder, and the tool definitions of every request. The child's tools pin the depth:
// before R6 the target run counted from 0 again and its child, at depth 1, was offered subagent;
// since R6 (design §3.5) the target runs at depth 1 and its child at depth 2, the default max_depth,
// so the child has no subagent tool. That is the only change to the R0 goldens; the target run
// itself is still a session's own run at sub-agent depth 0 (transcripts unchanged).
func TestRunBaselineProfileMessageTargetStartsASubagent(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"profile_message":{},"skill":{},"async":{},"subagent":{"agents":{"explore":{"description":"Finds facts.","tools":["skill","subagent"]}}}},"mcp":{}}`)
	f.captureBodies = true
	f.script = func(model, body string) string {
		switch {
		case strings.Contains(body, "function_call_output"):
			return ""
		case model == "model-small":
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review"})
		case strings.Contains(body, "please review"):
			return layoutCall("call-sub", "subagent", map[string]any{"agent": "explore", "task": "look around"})
		default:
			return layoutCall("call-read", "skill", map[string]any{"name": "notes"})
		}
	}
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	transcripts := runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
	if strings.Count(transcripts, "=== ") != 3 || !strings.Contains(transcripts, "the secret is 42") {
		t.Fatalf("want the coder, reviewer and explore transcripts, the child having read the notes:\n%s", transcripts)
	}
	compareRunBaseline(t, "profile_target_subagent_transcripts.txt", transcripts)
	compareRunBaseline(t, "profile_target_subagent_tools.txt", requestTools(t, f.capturedBodies(), m))
}

// The agents of sample/subagents as a session loads them through agents_dir, in a session with the
// sample coding toolset's natives (exec, async, subagent) and the extension tool triager names: the
// tools and the input (prompt and task) of each agent's first request, which R3 and R4 must keep.
func TestRunBaselineSampleAgentDefinitions(t *testing.T) {
	samples, err := filepath.Abs(filepath.Join("..", "..", "..", "sample"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		events := messageEvents("done", 9)
		if strings.Contains(body, "run the samples") {
			switch strings.Count(body, `"function_call_output"`) {
			case 0:
				events = functionCallEvents("call-explore", "subagent", `{"agent":"explore","task":"find the entry point"}`)
			case 1:
				events = functionCallEvents("call-triager", "subagent", `{"agent":"triager","task":"is this a bug?"}`)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
		}
	}))
	t.Cleanup(provider.Close)
	workspace := t.TempDir()
	native := filepath.Join(samples, "tools", "native")
	layout := layouttest.Write(t, layouttest.Spec{
		SessionID: "demo", CWD: workspace, ProfileModels: []string{"openai/parent"},
		ToolDefinitionPaths: []string{filepath.Join(native, "exec.json"), filepath.Join(native, "async.json"), filepath.Join(native, "subagent.json"), filepath.Join(samples, "tools", "extension", "decide_systemone.json")},
		Native: map[string]any{
			"exec":     map[string]any{"spill": map[string]any{"enabled": false}},
			"async":    map[string]any{},
			"subagent": map[string]any{"agents_dir": []string{filepath.Join(samples, "subagents")}},
		},
		Extension: map[string]any{"decide_systemone": map[string]any{}},
		Settings:  map[string]any{"max_tool_rounds": 4},
		Models: map[string]map[string]any{"openai/parent": {
			"provider": "openai", "model": "parent-model", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	})
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: layout.Root, RootSettingPath: layout.RootSetting,
		Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"}})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	t.Cleanup(func() { service.Close(); api.Close() })
	if body := (subagentFixture{apiURL: api.URL}).runInput(t, "run the samples"); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	mu.Lock()
	captured := append([]string(nil), bodies...)
	mu.Unlock()
	if len(captured) != 5 {
		t.Fatalf("provider requests = %d, want 5 (three of the session, one per agent)", len(captured))
	}
	m := newRunBaselineMasker(map[string]string{workspace: "<workspace>", layout.Root: "<root>", samples: "<sample>", provider.URL: "<provider>"})
	var out strings.Builder
	for index, body := range captured {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "run the samples") {
			if index == 0 {
				fmt.Fprintf(&out, "=== session request %d tools\n", index+1)
				for _, tool := range request.Tools {
					out.WriteString(m.mask(string(tool)) + "\n")
				}
			}
			continue
		}
		fmt.Fprintf(&out, "=== agent request %d tools\n", index+1)
		for _, tool := range request.Tools {
			out.WriteString(m.mask(string(tool)) + "\n")
		}
		fmt.Fprintf(&out, "=== agent request %d input\n", index+1)
		for _, item := range request.Input {
			out.WriteString(m.mask(string(item)) + "\n")
		}
	}
	compareRunBaseline(t, "sample_agent_definitions.txt", out.String())
}

// A sub-agent call with a sandbox choice (tools.native.subagent.sandbox_choice): the parent's
// subagent tool offers the choice, the child runs in the chosen template (the command backend, so
// no bwrap is needed to list the tools), and the parent is told the run's sandbox. R4 moves the
// per-choice catalog (choiceAgent) to prepareRun, so the tools the child is sent under a choice
// are pinned here before it does.
func TestRunBaselineSubagentSandboxChoice(t *testing.T) {
	reader := map[string]any{"schema_version": 1, "description": "Reader: writes nothing but scratch", "enabled": true, "backend": "command", "command": []string{"env"},
		"tools": []string{"exec"}, "write": map[string]any{"paths": []string{"{tmp}"}, "plan_paths": []string{"{tmp}"}}}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "exec"}, "__exec": map[string]any{},
		"__subagent_settings": map[string]any{"sandbox_choice": []string{"reader"}},
		"__sandbox_templates": map[string]map[string]any{"reader": reader},
	})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"look around","sandbox":"reader"}`}
	f.provider.childFirstCall = [2]string{"skill", notesCall}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if child := f.provider.forModel("child-model"); len(child) != 2 {
		t.Fatalf("child requests = %d, want 2", len(child))
	}
	m := subagentFixtureMasker(f)
	compareRunBaseline(t, "sandbox_choice_session.jsonl", m.mask(strings.Join(readLines(t, filepath.Join(f.stateDir, "messages.jsonl")), "\n")+"\n"))
	compareRunBaseline(t, "sandbox_choice_tools.txt", toolDefinitions(f.provider.all(), m))
}
