package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// readerChoice is a sandbox template that writes only scratch; writerChoice one that writes
// the workspace and has the network, which a session without network does not.
func readerChoice() sessionfs.SandboxChoice {
	reader := bwrapSandbox()
	reader.Template = "reader"
	reader.WritablePaths, reader.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	return sessionfs.SandboxChoice{Name: "reader", Description: "Reader: writes nothing but scratch", Sandbox: reader}
}

func networkChoice() sessionfs.SandboxChoice {
	wide := bwrapSandbox()
	wide.Template, wide.Network = "online", true
	return sessionfs.SandboxChoice{Name: "online", Sandbox: wide}
}

func choiceSettings(agents ...sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentSettings {
	settings := writeSettings(agents...)
	settings.SandboxChoices = []sessionfs.SandboxChoice{readerChoice(), networkChoice(), {Name: "isolate", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree, Base: sessionfs.SandboxIsolationBaseWorking}}, {Name: "in-place", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationNone}}}
	settings.SandboxChoice = sessionfs.SandboxChoiceNames(settings.SandboxChoices)
	return settings
}

// lastRequest is the launcher's last request.
func lastRequest(t *testing.T, launcher *fakeLauncher) SubagentRequest {
	t.Helper()
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.requests) == 0 {
		t.Fatal("no run started")
	}
	return launcher.requests[len(launcher.requests)-1]
}

// With sandbox_choice set the tool offers the sandbox argument and lists the choices with their
// descriptions; without it, the schema has no sandbox argument.
func TestSandboxChoiceOffersTheArgument(t *testing.T) {
	_, tool := buildSubagentCatalog(t, choiceSettings(writerAgent("fixer", "read", "exec")), &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	if !strings.Contains(string(tool.Definition.InputSchema), `"sandbox"`) {
		t.Fatalf("schema has no sandbox: %s", tool.Definition.InputSchema)
	}
	if !strings.Contains(tool.Definition.Description, " Sandboxes for sandbox: reader (Reader: writes nothing but scratch); online; isolate; in-place.") {
		t.Fatalf("description:\n%s", tool.Definition.Description)
	}
	_, plain := buildSubagentCatalog(t, writeSettings(writerAgent("fixer", "read", "exec")), &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	if strings.Contains(string(plain.Definition.InputSchema), `"sandbox"`) || strings.Contains(plain.Definition.Description, "Sandboxes for sandbox") {
		t.Fatalf("sandbox_choice off still offers it: %s", plain.Definition.InputSchema)
	}
}

// A chosen template confines the run: the child runs in it, narrowed by the calling run's
// environment, its exec description tells it, and its result names the choice.
func TestSandboxChoiceConfinesTheRun(t *testing.T) {
	launcher := &fakeLauncher{}
	session := bwrapSandbox()
	session.PlanWritablePaths = []string{"{tmp}"}
	stub := tools.Tool{Definition: tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "exec." + sandboxDescription(session)}}
	parent := withExecutors(t, readOnlyTool("read"), stub)
	tool, err := newSubagentToolDescribed(choiceSettings(writerAgent("fixer", "read", "exec")), parent, map[string]string{execToolID: "exec."}, nil, launcher, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append([]tools.Tool(nil), parent...), tool))
	if err != nil {
		t.Fatal(err)
	}
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": `{"agent":"fixer","task":"x","sandbox":"reader"}`})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, Environment: &tools.Environment{Sandbox: session}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sandbox: reader") {
		t.Fatalf("the result does not name the choice: %s", raw)
	}
	request := lastRequest(t, launcher)
	environment := request.Execution.Environment
	if environment == nil || environment.Sandbox == nil || strings.Join(environment.Sandbox.WritablePaths, ",") != "{tmp}" || environment.Sandbox.Template != "reader" {
		t.Fatalf("child environment = %+v", environment)
	}
	if !request.Agent.SandboxReader() {
		t.Fatal("the child under reader is not a reader")
	}
	if got := childDescriptionOf(t, launcher, "fixer", execToolID); got != "exec."+sandboxDescription(environment.Sandbox) {
		t.Fatalf("exec description = %q", got)
	}
	// The agent's own catalog is not changed by the choice: a plain call reads the session's.
	subagentCall(t, catalog, `{"agent":"fixer","task":"y"}`)
	for _, definition := range lastRequest(t, launcher).Tools.Definitions() {
		if definition.ID == execToolID && definition.Description != "exec."+sandboxDescription(session) {
			t.Fatalf("plain call exec description = %q", definition.Description)
		}
	}
}

// Unknown names, widening choices, a remote agent and a choice that takes an isolated agent out
// of its worktree are refused with sandbox_unavailable before any run starts.
func TestSandboxChoiceRefusals(t *testing.T) {
	launcher := &fakeLauncher{}
	isolated := isolatedAgent("isolated")
	catalog, _ := buildSubagentCatalog(t, choiceSettings(writerAgent("fixer", "read", "exec"), isolated), launcher, readOnlyTool("read"), writeTool("exec"))
	noNetwork := bwrapSandbox()
	for name, check := range map[string]struct {
		arguments string
		env       *tools.Environment
		want      []string
	}{
		"unknown":   {`{"agent":"fixer","task":"x","sandbox":"nope"}`, nil, []string{"error_code: sandbox_unavailable", "Sandbox nope is not one this session offers", "available: reader, online, isolate, in-place"}},
		"widens":    {`{"agent":"fixer","task":"x","sandbox":"online"}`, &tools.Environment{Sandbox: noNetwork}, []string{"error_code: sandbox_unavailable", "widens: network is on"}},
		"remote":    {`{"agent":"remote:box/s/c/p/a","task":"x","sandbox":"reader"}`, nil, []string{"error_code: sandbox_unavailable", "remote's own environment"}},
		"isolation": {`{"agent":"isolated","task":"x","sandbox":"in-place"}`, nil, []string{"error_code: sandbox_unavailable", "widens: isolation.mode none"}},
	} {
		t.Run(name, func(t *testing.T) {
			call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": check.arguments})
			raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, Environment: check.env})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range check.want {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("output lacks %q:\n%s", want, raw)
				}
			}
		})
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("a refused call started %d runs", len(launcher.requests))
	}
}

// Scheduling follows the choice: a writer called in reader is a reader call; in a choice
// that isolates it, too; with no choice or an unknown one, it is a writer.
func TestSandboxChoiceDecidesWhetherTheCallChangesFiles(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, choiceSettings(writerAgent("fixer", "read", "exec")), &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	for arguments, want := range map[string]bool{
		`{"agent":"fixer","task":"x"}`:                     true,
		`{"agent":"fixer","task":"x","sandbox":"reader"}`:  false,
		`{"agent":"fixer","task":"x","sandbox":"isolate"}`: false,
		`{"agent":"fixer","task":"x","sandbox":"online"}`:  true,
		`{"agent":"fixer","task":"x","sandbox":"nope"}`:    true,
	} {
		call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": arguments})
		if got := catalog.CallMayChangeFiles(call); got != want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", arguments, got, want)
		}
	}
}

// A choice that sets isolation.mode worktree puts a writer in a worktree of its own, with the
// template's base, and its exec reads how git works there.
func TestSandboxChoiceIsolatesTheRun(t *testing.T) {
	agent := writerAgent("fixer", "read", "exec")
	got, problem := withCallSandbox(agent, sessionfs.SandboxChoice{Name: "isolate", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree, Base: sessionfs.SandboxIsolationBaseWorking}})
	if problem != nil || !got.Isolated() || got.IsolationBase != sessionfs.SandboxIsolationBaseWorking {
		t.Fatalf("agent = %+v, problem %+v", got, problem)
	}
	reader := agentNamed("looker")
	reader.Tools, reader.HasTools = []string{"read"}, true
	if got, _ := withCallSandbox(reader, sessionfs.SandboxChoice{Name: "isolate", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}}); got.Isolated() {
		t.Fatal("an agent that changes no file was put in a worktree")
	}
	pool := withExecutors(t, tools.Tool{Definition: tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "exec."}})
	described := describeAgentIsolation(describeAgentIsolation(pool))
	if strings.Count(described[0].Definition.Description, isolationDescription) != 1 {
		t.Fatalf("isolation sentence added twice: %q", described[0].Definition.Description)
	}
}

// The catalogs of a choice are built once per (agent, choice) and reused, also from parallel
// calls; another choice gets its own.
func TestSandboxChoiceCatalogsAreBuiltOnce(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, choiceSettings(writerAgent("fixer", "read", "exec")), launcher, readOnlyTool("read"), writeTool("exec"))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subagentCallAs(t, catalog, testSessionKey, `{"agent":"fixer","task":"x","sandbox":"reader"}`)
		}()
	}
	wg.Wait()
	subagentCall(t, catalog, `{"agent":"fixer","task":"x","sandbox":"online"}`)
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if len(launcher.requests) != 5 {
		t.Fatalf("runs = %d", len(launcher.requests))
	}
	first := launcher.requests[0].Tools
	for _, request := range launcher.requests[1:4] {
		if request.Tools != first {
			t.Fatal("a second call with the same choice built its catalog again")
		}
	}
	if launcher.requests[4].Tools == first {
		t.Fatal("another choice reused the reader catalog")
	}
}

// A resume that names no sandbox runs in the source run's choice, which the run record keeps
// (also across a restart); one that names a choice changing isolation is refused.
func TestSandboxChoiceIsKeptOnResume(t *testing.T) {
	root := t.TempDir()
	launcher := transcribingLauncher(t, root)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	settings := choiceSettings(writerAgent("fixer", "read", "exec"))
	settings.StorageDir, settings.PersistRuns, settings.KeepFinished = root, true, 10
	parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
	tool, err := NewSubagentToolWithManager(settings, parent, launcher, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	id := startedRunID(t, catalog, manager, testSessionKey, `{"agent":"fixer","task":"first","sandbox":"reader"}`)
	if manifest := manager.byID(id).manifest(); manifest.Sandbox != "reader" {
		t.Fatalf("manifest sandbox = %q", manifest.Sandbox)
	}
	out := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(out, "sandbox: reader") {
		t.Fatalf("resume output:\n%s", out)
	}
	if request := lastRequest(t, launcher); request.Execution.Environment == nil || strings.Join(request.Execution.Environment.Sandbox.WritablePaths, ",") != "{tmp}" {
		t.Fatalf("the resume did not run in reader: %+v", request.Execution.Environment)
	}
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": `{"resume":"` + id + `","task":"more"}`})
	if catalog.CallMayChangeFiles(call) {
		t.Fatal("a resume of a reader run is scheduled as a writer")
	}
	refused := resumeCall(t, catalog, testSessionKey, `{"resume":"`+id+`","task":"more","sandbox":"isolate"}`)
	if !strings.Contains(refused, "error_code: sandbox_unavailable") || !strings.Contains(refused, "widens: isolation.mode") {
		t.Fatalf("isolation change on resume:\n%s", refused)
	}
	// A restarted manager reads the choice back from the manifest.
	restarted := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = restarted.Close() })
	if run := restarted.byID(id); run == nil || run.view().sandbox != "reader" {
		t.Fatal("the restored run lost its sandbox choice")
	}
}

// A resume whose recorded choice the session no longer offers is refused, not run in the
// agent's default environment.
func TestSandboxChoiceResumeOfAChoiceNoLongerOffered(t *testing.T) {
	root := t.TempDir()
	launcher := transcribingLauncher(t, root)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	build := func(settings sessionfs.NativeSubagentSettings) *tools.Catalog {
		settings.StorageDir, settings.PersistRuns, settings.KeepFinished = root, true, 10
		parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
		tool, err := NewSubagentToolWithManager(settings, parent, launcher, nil, manager)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := tools.NewCatalog(append(parent, tool))
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	}
	id := startedRunID(t, build(choiceSettings(writerAgent("fixer", "read", "exec"))), manager, testSessionKey, `{"agent":"fixer","task":"first","sandbox":"reader"}`)
	runs := len(launcher.requests)
	out := resumeCall(t, build(writeSettings(writerAgent("fixer", "read", "exec"))), testSessionKey, `{"resume":"`+id+`","task":"more"}`)
	if !strings.Contains(out, "error_code: sandbox_unavailable") || !strings.Contains(out, "ran in sandbox reader") {
		t.Fatalf("output:\n%s", out)
	}
	if len(launcher.requests) != runs {
		t.Fatal("the refused resume started a run")
	}
}

func profileMessageChoiceSettings() sessionfs.NativeProfileMessageSettings {
	settings := profileMessageSettings()
	settings.SandboxChoices = []sessionfs.SandboxChoice{readerChoice()}
	settings.SandboxChoice = []string{"reader"}
	return settings
}

// profile_message offers sandbox only with sandbox_choice, hands the resolved choice to the
// messenger, refuses an unknown name or a remote profile before anything is sent, and maps a
// choice the target refused to sandbox_unavailable.
func TestProfileMessageSandboxChoice(t *testing.T) {
	tool, err := NewProfileMessageTool(profileMessageChoiceSettings(), profileMessageTargets, &fakeMessenger{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tool.Definition.InputSchema), `"sandbox"`) || !strings.Contains(tool.Definition.Description, "Sandboxes for sandbox: reader (Reader: writes nothing but scratch).") {
		t.Fatalf("definition: %s\n%s", tool.Definition.InputSchema, tool.Definition.Description)
	}

	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	callProfileMessage(t, profileMessageChoiceSettings(), messenger, `{"profile":"reviewer","message":"check","sandbox":"reader"}`)
	if messenger.request.Sandbox == nil || messenger.request.Sandbox.Name != "reader" || messenger.request.Sandbox.Sandbox == nil {
		t.Fatalf("request sandbox = %+v", messenger.request.Sandbox)
	}

	for name, check := range map[string]struct{ arguments, want string }{
		"unknown":  {`{"profile":"reviewer","message":"check","sandbox":"nope"}`, "available: reader"},
		"remote":   {`{"profile":"remote:box/s/c/p","message":"check","sandbox":"reader"}`, "remote's own environment"},
		"disabled": {`{"profile":"reviewer","message":"check","sandbox":"reader"}`, "error_code: sandbox_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			settings := profileMessageChoiceSettings()
			if name == "disabled" {
				settings = profileMessageSettings()
				// The schema has no sandbox then; the call is refused before the tool runs.
				check.want = "Invalid arguments for profile_message"
			}
			messenger := &fakeMessenger{}
			out := callProfileMessage(t, settings, messenger, check.arguments)
			if !strings.Contains(out, check.want) || messenger.message != "" {
				t.Fatalf("output (sent %q):\n%s", messenger.message, out)
			}
		})
	}

	refusing := &fakeMessenger{prepareErr: &ProfileSandboxError{Message: sessionfs.SandboxWidensPrefix + "network is on", Choice: "reader"}}
	out := callProfileMessage(t, profileMessageChoiceSettings(), refusing, `{"profile":"reviewer","message":"check","sandbox":"reader"}`)
	if !strings.Contains(out, "error_code: sandbox_unavailable") || !strings.Contains(out, "widens: network is on") || !strings.Contains(out, "Sandbox reader reaches more") {
		t.Fatalf("refused choice:\n%s", out)
	}
	widens := &fakeMessenger{prepareErr: &ProfileSandboxError{Message: "widens: network"}}
	if out := callProfileMessage(t, profileMessageChoiceSettings(), widens, `{"profile":"reviewer","message":"check"}`); !strings.Contains(out, "error_code: sandbox_widens") {
		t.Fatalf("the caller's own narrowing:\n%s", out)
	}
}

// An async summary names the run's sandbox choice after its worktree line, with no blank line
// when there is none; a run without a choice has no such line.
func TestSandboxChoiceInTheAsyncSummary(t *testing.T) {
	run := subagentRunView{state: subagentRunDone, result: SubagentResult{Text: "the answer"}, resultMax: 1024, sandbox: "reader"}
	if got := subagentAsyncResult(run).Summary; got != "sandbox: reader\nthe answer" {
		t.Fatalf("summary = %q", got)
	}
	run.sandbox = ""
	if got := subagentAsyncResult(run).Summary; got != "the answer" {
		t.Fatalf("summary without a choice = %q", got)
	}
}

// An agent with a sandbox of its own is narrowed by the choice: a choice inside it is taken,
// one that reaches more is refused naming what it widens.
func TestSandboxChoiceNarrowsTheAgentsOwnSandbox(t *testing.T) {
	agent := writerAgent("fixer", "read", "exec")
	agent.Sandbox = bwrapSandbox()
	agent.Sandbox.PlanWritablePaths = []string{"{tmp}"}
	got, problem := withCallSandbox(agent, readerChoice())
	if problem != nil || strings.Join(got.Sandbox.WritablePaths, ",") != "{tmp}" || got.Sandbox.Template != "reader" {
		t.Fatalf("reader: %+v %+v", got.Sandbox, problem)
	}
	if _, problem := withCallSandbox(agent, networkChoice()); problem == nil || !strings.HasPrefix(problem.widens, "network is on") {
		t.Fatalf("online: %+v", problem)
	}
}

// A choice whose template does not cover exec leaves exec unconfined, so a writer called in it
// is still a writer, however narrow its paths.
func TestSandboxChoiceNotCoveringExecKeepsAWriter(t *testing.T) {
	settings := choiceSettings(writerAgent("fixer", "read", "exec"))
	uncovered := readerChoice()
	uncovered.Name = "uncovered"
	uncovered.Sandbox.Tools = []string{"tool_smith"}
	settings.SandboxChoices = append(settings.SandboxChoices, uncovered)
	settings.SandboxChoice = sessionfs.SandboxChoiceNames(settings.SandboxChoices)
	catalog, _ := buildSubagentCatalog(t, settings, &fakeLauncher{}, readOnlyTool("read"), writeTool("exec"))
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": `{"agent":"fixer","task":"x","sandbox":"uncovered"}`})
	if !catalog.CallMayChangeFiles(call) {
		t.Fatal("a writer in a sandbox that does not cover exec is scheduled as a reader")
	}
}
