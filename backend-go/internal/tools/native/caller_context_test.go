package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// Phase R1 of docs/design_unified_agent_runs.md: SubagentRequest and ProfileMessageRequest carry
// a CallerContext. These tests pin that it is the calling run's scope at the time of the call,
// never the started run's own (depth, handle owner, worktree, narrowed environment), and that
// the fields the runs read today are unchanged beside it.

// callerScope is a top-level caller with a sandbox, a path scope, a hop count and both hooks.
func callerScope(environment *tools.Environment) tools.ExecutionContext {
	return tools.ExecutionContext{
		WorkspaceRoot: "/work",
		SessionDir:    "/sess",
		SessionKey:    testSessionKey,
		Environment:   environment,
		PathScope:     &tools.PathScope{Allow: []string{"/work/src"}},
		RemoteHops:    1,
		OnFileChange:  func(string, string) {},
		RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{}, nil
		},
	}
}

func TestSubagentRequestCarriesTheCallersContext(t *testing.T) {
	launcher := &fakeLauncher{}
	session := bwrapSandbox()
	session.PlanWritablePaths = []string{"{tmp}"}
	stub := tools.Tool{Definition: tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "exec." + sandboxDescription(session)}}
	parent := withExecutors(t, readOnlyTool("read"), stub)
	settings := choiceSettings(writerAgent("fixer", "read", "exec"))
	settings.ModelChoice = true
	settings.ProfileModels = map[string]string{"openai/a": "/models/a.json"}
	tool, err := newSubagentToolDescribed(settings, parent, map[string]string{execToolID: "exec."}, nil, launcher, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append([]tools.Tool(nil), parent...), tool))
	if err != nil {
		t.Fatal(err)
	}
	execution := callerScope(&tools.Environment{Sandbox: session})
	ctx := WithRemoteCaller(context.Background(), "remote:token-1")
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "subagent", "arguments": `{"agent":"fixer","task":"x","sandbox":"reader","model":"openai/a"}`})
	raw, err := catalog.ExecuteFunctionCall(ctx, call, execution)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Agent fixer finished") {
		t.Fatalf("the run did not finish: %s", raw)
	}
	request := lastRequest(t, launcher)
	caller := request.Caller
	if caller.Workspace != "/work" || caller.SessionDir != "/sess" || caller.SessionKey != testSessionKey || caller.RemoteHops != 1 {
		t.Fatalf("caller roots = %+v", caller)
	}
	if caller.Depth != 0 || caller.RootRun != "" || caller.HandleOwner != "" || caller.AgentName != "" {
		t.Fatalf("the caller is the session's own run, got depth %d root %q owner %q agent %q", caller.Depth, caller.RootRun, caller.HandleOwner, caller.AgentName)
	}
	if caller.RemoteCaller != "remote:token-1" || caller.ModelChoice != "openai/a" || caller.SandboxChoice != "reader" {
		t.Fatalf("call-site fields: remote %q model %q sandbox %q", caller.RemoteCaller, caller.ModelChoice, caller.SandboxChoice)
	}
	if caller.Environment != execution.Environment || caller.PathScope != execution.PathScope {
		t.Fatal("the caller's environment or path scope is not the calling run's own")
	}
	if caller.OnFileChange == nil || caller.RequestInteraction == nil {
		t.Fatal("the caller's change listener or question channel was dropped")
	}
	// The child's own scope is on Execution, as before, and differs from the caller's.
	if request.Execution.Environment == caller.Environment || request.Execution.Environment.Sandbox.Template != "reader" {
		t.Fatalf("the child's environment is not the narrowed choice: %+v", request.Execution.Environment)
	}
	if request.Execution.AgentDepth != 1 || !strings.HasPrefix(request.Execution.ProcessOwner, "sub_") || request.Execution.AgentName != "fixer" {
		t.Fatalf("the child's own scope changed: depth %d owner %q agent %q", request.Execution.AgentDepth, request.Execution.ProcessOwner, request.Execution.AgentName)
	}
	if request.Agent.Model != "openai/a" {
		t.Fatalf("the agent's model = %q", request.Agent.Model)
	}
}

// A run started inside a child names that child as its caller: the caller's depth, root run and
// handle owner are the child's, one level above the run's own.
func TestNestedSubagentRequestNamesTheChildAsCaller(t *testing.T) {
	requests := make(chan SubagentRequest, 2)
	catalog, _, _ := nestingFixture(t, nestingSettings(nestingAgent("boss", "read", "subagent"), nestingAgent("leaf", "read")),
		map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				requests <- request
				childCall(t, ctx, request, "subagent", `{"task":"go on","agent":"leaf"}`)
				return SubagentResult{Text: "boss done"}, nil
			},
			"leaf": func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
				requests <- request
				return SubagentResult{Text: "leaf"}, nil
			},
		})
	if out := subagentCall(t, catalog, `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
		t.Fatalf("boss: %s", out)
	}
	boss, leaf := <-requests, <-requests
	if boss.Caller.Depth != 0 || boss.Caller.HandleOwner != "" || boss.Caller.AgentName != "" {
		t.Fatalf("boss's caller = %+v", boss.Caller)
	}
	if leaf.Caller.Depth != 1 || leaf.Caller.AgentName != "boss" ||
		leaf.Caller.HandleOwner != boss.Execution.ProcessOwner || leaf.Caller.RootRun != boss.Execution.RootRun {
		t.Fatalf("leaf's caller = depth %d agent %q owner %q root %q; boss runs as owner %q root %q",
			leaf.Caller.Depth, leaf.Caller.AgentName, leaf.Caller.HandleOwner, leaf.Caller.RootRun, boss.Execution.ProcessOwner, boss.Execution.RootRun)
	}
	if leaf.Execution.AgentDepth != 2 || leaf.Execution.ProcessOwner == leaf.Caller.HandleOwner {
		t.Fatalf("leaf's own scope: depth %d owner %q", leaf.Execution.AgentDepth, leaf.Execution.ProcessOwner)
	}
}

// The worktree a run opens moves only the run's own Execution: the caller context keeps the
// caller's workspace, so it is not a snapshot of the child that goes stale or leaks back.
func TestIsolatedRunsCallerKeepsTheCallersWorkspace(t *testing.T) {
	repo := gitRepository(t)
	child := &worktreeChild{executions: make(chan tools.ExecutionContext, 1), files: map[string]string{"a.txt": "changed\n"}}
	settings := writeSettings(isolatedAgent("fixer"))
	settings.StorageDir = t.TempDir()
	launcher := &fakeLauncher{run: child.run}
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("exec"))
	worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"fix it","block_ms":3000}`)
	execution := <-child.executions
	caller := lastRequest(t, launcher).Caller
	if execution.WorkspaceRoot == repo || execution.WorkspaceOrigin != repo {
		t.Fatalf("the child did not run in a worktree: root %q origin %q", execution.WorkspaceRoot, execution.WorkspaceOrigin)
	}
	if caller.Workspace != repo || caller.WorkspaceOrigin != "" {
		t.Fatalf("the caller context follows the child's worktree: workspace %q origin %q", caller.Workspace, caller.WorkspaceOrigin)
	}
}

func TestProfileMessageRequestCarriesTheCallersContext(t *testing.T) {
	settings := profileMessageSettings()
	settings.ModelChoice = true
	settings.SandboxChoices = []sessionfs.SandboxChoice{readerChoice()}
	settings.SandboxChoice = sessionfs.SandboxChoiceNames(settings.SandboxChoices)
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	tool, err := NewProfileMessageTool(settings, profileMessageTargets, messenger, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	// A child in a plan-mode run: the request's Environment is the plan-mode form, the caller's
	// is the run's own.
	execution := callerScope(&tools.Environment{Sandbox: bwrapSandbox()})
	execution.PlanMode = true
	execution.AgentName, execution.AgentDepth, execution.RootRun, execution.ProcessOwner = "asker", 1, "sub_00000001", "sub_00000002"
	ctx := WithRemoteCaller(context.Background(), "remote:token-2")
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "m", "name": "profile_message",
		"arguments": `{"profile":"reviewer","message":"hi","model":" openai/large@high ","sandbox":"reader"}`})
	if _, err := catalog.ExecuteFunctionCall(ctx, call, execution); err != nil {
		t.Fatal(err)
	}
	request := messenger.request
	caller := request.Caller
	if caller.SessionKey != testSessionKey || caller.Workspace != "/work" || caller.SessionDir != "/sess" || !caller.PlanMode || caller.RemoteHops != 1 {
		t.Fatalf("caller = %+v", caller)
	}
	if caller.Depth != 1 || caller.AgentName != "asker" || caller.RootRun != "sub_00000001" || caller.HandleOwner != "sub_00000002" {
		t.Fatalf("the caller's place in the tree = depth %d agent %q root %q owner %q", caller.Depth, caller.AgentName, caller.RootRun, caller.HandleOwner)
	}
	if caller.RemoteCaller != "remote:token-2" || caller.ModelChoice != "openai/large@high" || caller.SandboxChoice != "reader" {
		t.Fatalf("call-site fields: remote %q model %q sandbox %q", caller.RemoteCaller, caller.ModelChoice, caller.SandboxChoice)
	}
	if caller.Environment != execution.Environment || caller.PathScope != execution.PathScope {
		t.Fatal("the caller's environment or path scope is not the calling run's own")
	}
	// The fields the messenger reads are what they were.
	if request.OwnerKey != testSessionKey || request.Model != "openai/large@high" || request.Sandbox == nil || request.Sandbox.Name != "reader" {
		t.Fatalf("request = owner %q model %q sandbox %+v", request.OwnerKey, request.Model, request.Sandbox)
	}
	if request.Environment == execution.Environment || request.Environment.Sandbox == execution.Environment.Sandbox {
		t.Fatal("the request's environment is no longer the plan-mode form of the caller's")
	}
	if request.CallerPlace.Workspace != "/work" || request.Timeout != 5*time.Second {
		t.Fatalf("request place %+v timeout %v", request.CallerPlace, request.Timeout)
	}
}
