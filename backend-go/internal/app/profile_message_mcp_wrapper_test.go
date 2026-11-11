package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// What a profile_message run narrowed by the caller's sandbox does to the target's own stdio MCP
// servers. Before the fix (Template T0, bug record: confined profile_message run rewraps the
// target's shared stdio MCP servers) the narrowed run built its catalog on the target's shared
// manager with the narrowed wrapper, so the message restarted the target's server inside the
// caller's sandbox and the target's next own run restarted it once more (starts 1 -> 2 -> 3).
// Now the narrowed run gets servers of its own narrowing (runMCPManager): the target's server is
// started once and never again, and each message's server starts in the caller's sandbox and stops
// with the message, which left nothing going (mcpScopeHold), so a later message narrowed the same
// way starts its own.
func TestConfinedProfileMessageRunLeavesTheTargetsSharedStdioServerAlone(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh for the command sandbox")
	}
	f := newLayoutFixture(t)
	logs := t.TempDir()
	starts, wrapped := filepath.Join(logs, "starts"), filepath.Join(logs, "wrapped")
	writeConfinedMCPTarget(t, f, starts)

	count := func(path string) int {
		data, _ := os.ReadFile(path)
		return strings.Count(string(data), "\n")
	}
	ownRun := func() {
		t.Helper()
		if status, body := f.do(t, "POST", "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
			t.Fatalf("own run: %d %s", status, body)
		}
	}
	message := func() {
		t.Helper()
		messenger := profileMessenger{resolver: f.service.resolver}
		ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
		defer cancel()
		plan, err := messenger.Prepare(ctx, native.ProfileMessageRequest{
			OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "look",
			Environment: &tools.Environment{Sandbox: loggingSandbox(wrapped, "caller", logs)}, CallerPlace: sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd},
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := plan.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-run.Done():
		case <-ctx.Done():
			t.Fatal("the message did not end")
		}
		if outcome := run.Outcome(); outcome.Err != nil {
			t.Fatalf("message outcome = %+v", outcome)
		}
	}

	// Control: two own runs share one start of the server.
	ownRun()
	ownRun()
	if got := count(starts); got != 1 || count(wrapped) != 0 {
		t.Fatalf("after the target's two own runs: %d starts, %d in the caller's sandbox; want 1, 0", got, count(wrapped))
	}
	message()
	if got := count(starts); got != 2 || count(wrapped) != 1 {
		t.Fatalf("after the narrowed message run: %d starts, %d in the caller's sandbox; want 2, 1 (one server of the narrowing, the target's left running)", got, count(wrapped))
	}
	if got := confinedManagers(f.service.resolver); got != 0 {
		t.Fatalf("%d managers of narrowings left after the message ended with nothing going, want 0", got)
	}
	ownRun()
	if got := count(starts); got != 2 || count(wrapped) != 1 {
		t.Fatalf("after the target's next own run: %d starts, %d in the caller's sandbox; want 2, 1 (its shared server was not restarted)", got, count(wrapped))
	}
	message()
	if got := count(starts); got != 3 || count(wrapped) != 2 {
		t.Fatalf("after a second message narrowed the same way: %d starts, %d in the caller's sandbox; want 3, 2 (a server of its own, the target's still not restarted)", got, count(wrapped))
	}
	if got := confinedManagers(f.service.resolver); got != 0 {
		t.Fatalf("%d managers of narrowings left after the second message, want 0", got)
	}
}

// confinedManagers counts the managers of narrowings the resolver holds (holdMCPScope).
func confinedManagers(resolver *Resolver) int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	n := 0
	for key := range resolver.mcpManagers {
		if strings.Contains(key, "\x00confined\x00") {
			n++
		}
	}
	return n
}

// writeConfinedMCPTarget gives review/reviewer the fixture stdio server m/stdio, which logs each
// start to starts.
func writeConfinedMCPTarget(t *testing.T, f *layoutFixture, starts string) {
	t.Helper()
	body, _ := json.Marshal(appStdioMCPServerFile(t, "", func(file map[string]any) {
		transport := file["transport"].(map[string]any)
		transport["env"] = map[string]string{appStdioMCPEnvironment: "1", appStdioMCPStartLogEnvironment: starts}
	}))
	writeFile(t, filepath.Join(f.root, "mcp", "m", "stdio.json"), body)
	writeFile(t, filepath.Join(f.root, "tools", "review", "stdio.json"), []byte(`{"native":{},"mcp":{"m/stdio":{}}}`))
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"review/stdio"}`)
}

// loggingSandbox is a command sandbox covering mcp that appends a line to log for every start it
// wraps (its command line) and starts the server with tag (appStdioMCPTagEnvironment). extra is a writable path.
func loggingSandbox(log, tag, extra string) *sessionfs.SandboxSettings {
	return &sessionfs.SandboxSettings{
		Backend: sessionfs.SandboxBackendCommand, Executable: "sh", Tools: []string{"exec", "mcp"},
		Command:   []string{"sh", "-c", `echo wrapped "$@" >> "$0"; shift; ` + appStdioMCPTagEnvironment + `=` + tag + ` exec "$@"`, log, "sandbox"},
		ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{tmp}", extra},
	}
}

// The tools a run lists keep answering from the servers they were listed from, whatever another
// run of the same profile prepares in between: the target's own run (not narrowed) and two
// message runs narrowed by different callers each list the m/stdio tool, and every one of them,
// called after all three prepared, is answered by its own start of the server, in its own
// sandbox. Before the fix a narrowed prepare rewrapped the shared manager, which closed the
// connection the earlier listing called (the target's tool failed) and moved later calls into
// whichever sandbox prepared last.
func TestConfinedRunsAndTheTargetsOwnRunKeepTheirOwnServers(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh for the command sandbox")
	}
	f := newLayoutFixture(t)
	logs := t.TempDir()
	starts, wrapped := filepath.Join(logs, "starts"), filepath.Join(logs, "wrapped")
	writeConfinedMCPTarget(t, f, starts)
	resolver := f.service.resolver
	place := sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	holds := map[string]*mcpScopeHold{}
	lookup := func(runner boundRunner) func() string {
		t.Helper()
		hold, err := resolver.holdMCPScope(runner.config, runner.mcpScope())
		if err != nil {
			t.Fatal(err)
		}
		if hold != nil {
			holds[runner.mcpScope()] = hold
		}
		prepared, err := resolver.prepareRun(ctx, runner.shared, runner.config, hold)
		if err != nil {
			t.Fatal(err)
		}
		name := mcpToolNameFor("stdio", "lookup")
		if !prepared.toolCatalog.Has(name) {
			t.Fatalf("no lookup tool in %v", prepared.toolCatalog.Definitions())
		}
		arguments, _ := json.Marshal(map[string]string{"word": "w", "target": filepath.Join(logs, "out.txt")})
		call := functionCallItem(name, string(arguments))
		return func() string {
			t.Helper()
			output, err := prepared.toolCatalog.ExecuteFunctionCall(ctx, json.RawMessage(call), tools.ExecutionContext{})
			if err != nil {
				return "error: " + err.Error()
			}
			return string(output)
		}
	}
	scopes := map[string]string{}
	narrowed := func(tag string) boundRunner {
		t.Helper()
		confine := targetRunner(targetConfiner(loggingSandbox(wrapped, tag, filepath.Join(logs, tag)), place, nil), "dev/coder", "look", nil, nil, nil, nil)
		runner, err := confine(boundRunnerOf(t, f, "review/reviewer"))
		if err != nil {
			t.Fatal(err)
		}
		bound := runner.(boundRunner)
		if bound.mcpScope() == "" {
			t.Fatalf("a caller sandbox covering mcp gave the run no MCP scope")
		}
		scopes[tag] = bound.mcpScope()
		return bound
	}

	own := lookup(boundRunnerOf(t, f, "review/reviewer"))
	first := lookup(narrowed("a"))
	second := lookup(narrowed("b"))
	for _, check := range []struct {
		name, tag string
		call      func() string
	}{{"the target's own", "", own}, {"the first caller's", "[a] ", first}, {"the second caller's", "[b] ", second}, {"the target's own, again", "", own}, {"the first caller's, again", "[a] ", first}} {
		got := check.call()
		if !strings.Contains(got, "MCP wrote") || strings.Contains(got, "[a]") != (check.tag == "[a] ") || strings.Contains(got, "[b]") != (check.tag == "[b] ") {
			t.Fatalf("%s tool answered %s, want its own server (tag %q)", check.name, got, check.tag)
		}
	}
	data, _ := os.ReadFile(starts)
	if got := strings.Count(string(data), "\n"); got != 3 {
		t.Fatalf("%d starts, want 3 (the target's, and one per narrowing)", got)
	}

	// Cleanup: a narrowing's manager closes with the release of its last hold, and the others
	// stay; a session delete closes what is still held, and the late release then closes nothing.
	if got := confinedManagers(resolver); got != 2 {
		t.Fatalf("%d managers of narrowings, want 2", got)
	}
	holds[scopes["a"]].release(true)
	if got := confinedManagers(resolver); got != 1 {
		t.Fatalf("%d managers of narrowings after the first's release, want 1", got)
	}
	if got := first(); strings.Contains(got, "MCP wrote") {
		t.Fatalf("a released narrowing's server still answered: %s", got)
	}
	if got := second(); !strings.Contains(got, "[b] MCP wrote") {
		t.Fatalf("the other narrowing's server stopped with the first's release: %s", got)
	}
	if got := own(); !strings.Contains(got, "MCP wrote") || strings.Contains(got, "[a]") || strings.Contains(got, "[b]") {
		t.Fatalf("the target's own server stopped with a narrowing's release: %s", got)
	}
	resolver.releaseSessionChildren(boundRunnerOf(t, f, "review/reviewer").config.SettingPath)
	if got := confinedManagers(resolver); got != 0 {
		t.Fatalf("%d managers of narrowings left after the session's release, want 0", got)
	}
	if got := second(); strings.Contains(got, "MCP wrote") {
		t.Fatalf("a narrowing's server still answered after the session's release: %s", got)
	}
	holds[scopes["b"]].release(true)
	resolver.mu.Lock()
	refs := len(resolver.mcpScopeRefs)
	resolver.mu.Unlock()
	if refs != 0 {
		t.Fatalf("%d hold counts left after every hold was released", refs)
	}
}

// Which runs get a scope of their own: only a narrowing that changes where the stdio servers
// start. No caller sandbox, or one that does not cover mcp over a target without a sandbox,
// keeps the profile's shared servers; so does a run the remote API started (no confine).
func TestProfileMessageRunGetsAnMCPScopeOnlyWhenTheNarrowingMovesItsServers(t *testing.T) {
	f := newLayoutFixture(t)
	place := sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd}
	scope := func(confine func(httpapi.RunSession) (httpapi.RunSession, error)) string {
		t.Helper()
		runner, err := targetRunner(confine, "dev/coder", "look", nil, nil, nil, nil)(boundRunnerOf(t, f, "review/reviewer"))
		if err != nil {
			t.Fatal(err)
		}
		return runner.(boundRunner).mcpScope()
	}
	if got := scope(nil); got != "" {
		t.Fatalf("no confinement: scope %q", got)
	}
	execOnly := loggingSandbox("/dev/null", "x", t.TempDir())
	execOnly.Tools = []string{"exec"}
	if got := scope(targetConfiner(execOnly, place, nil)); got != "" {
		t.Fatalf("a caller sandbox without mcp: scope %q", got)
	}
	covering := loggingSandbox("/dev/null", "x", t.TempDir())
	first, second := scope(targetConfiner(covering, place, nil)), scope(targetConfiner(covering, place, nil))
	if first == "" || first != second {
		t.Fatalf("a caller sandbox covering mcp: scopes %q and %q, want one, the same for two runs narrowed alike", first, second)
	}
	// An isolated target's narrowed run stops the servers it listed from (runLocked): no other
	// run, isolated or not, may share them.
	isolated := func() string {
		t.Helper()
		target := boundRunnerOf(t, f, "review/reviewer")
		target.config.Isolation.Mode = sessionfs.SandboxIsolationWorktree
		runner, err := targetRunner(targetConfiner(covering, place, nil), "dev/coder", "look", nil, nil, nil, nil)(target)
		if err != nil {
			t.Fatal(err)
		}
		return runner.(boundRunner).mcpScope()
	}
	if a, b := isolated(), isolated(); a == "" || a == b || a == first || b == first {
		t.Fatalf("isolated runs narrowed alike: scopes %q and %q (not isolated: %q), want each its own", a, b, first)
	}
	if got := boundRunnerOf(t, f, "review/reviewer").mcpScope(); got != "" {
		t.Fatalf("a user's run: scope %q", got)
	}
	// A target whose own sandbox covers mcp, left as it is by the confinement, keeps its servers.
	own := boundRunnerOf(t, f, "review/reviewer")
	own.config.Sandbox = loggingSandbox("/dev/null", "own", t.TempDir())
	unchanged := func(runner httpapi.RunSession) (httpapi.RunSession, error) { return runner, nil }
	runner, err := targetRunner(unchanged, "dev/coder", "look", nil, nil, nil, nil)(own)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.(boundRunner).mcpScope(); got != "" {
		t.Fatalf("a target's own mcp sandbox, not narrowed: scope %q", got)
	}
}

// The pin of the sampler overwrite by a message's model choice is replaced by
// TestProfileMessageModelChoiceSamplesOnlyForItsOwnRunAndItsChildren (T-008, mcp_sampling_run_test.go).

// Template T0: the MCP servers of an agent's toolset have no sampler (loadAgentToolsetMCP never
// sets one), so a toolset server's sampling request is not answered by any model, while the same
// server listed by the session is answered by the session's model.
func TestAgentToolsetMCPServersHaveNoSampler(t *testing.T) {
	body, _ := json.Marshal(appStdioMCPServerFile(t, "", func(file map[string]any) {
		file["transport"].(map[string]any)["env"] = map[string]string{appStdioMCPEnvironment: "1", appStdioMCPAskEnvironment: "1"}
		file["sampling"] = map[string]any{"enabled": true}
	}))
	root := t.TempDir()
	serverPath := filepath.Join(root, "stdio.json")
	writeFile(t, serverPath, body)
	servers := []sessionfs.MCPServerReference{{ID: "stdio", ConfigPath: serverPath, Enabled: true}}
	resolver := newEnvTestResolver(t)
	config := sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "session.json"), Profile: "dev/coder", WorkingDir: root,
		AgentToolsets:  map[string]sessionfs.SessionConfig{"extra/search": {WorkingDir: root, MCPServers: servers}},
		NativeSubagent: &sessionfs.NativeSubagentSettings{Agents: []sessionfs.NativeSubagentAgent{{Name: "finder", Toolset: []string{"extra/search"}}}},
	}
	loaded, err := resolver.loadAgentToolsetMCP(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	call := func() string {
		t.Helper()
		for _, tool := range loaded["extra/search"].Tools {
			if tool.Definition.Name == mcpToolNameFor("stdio", "ask") {
				output, err := tool.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{})
				if err != nil {
					t.Fatal(err)
				}
				return string(output)
			}
		}
		t.Fatalf("no ask tool among the toolset's tools: %+v", loaded)
		return ""
	}
	if got := call(); strings.Contains(got, "sampled by") || !strings.Contains(got, `\"status\":\"error\"`) {
		t.Fatalf("a toolset server's sampling request = %s, want it refused", got)
	}
	// Control: the refusal is the missing sampler; with one the same server is answered.
	manager, err := resolver.mcpManagerFor(filepath.Clean(config.SettingPath) + "\x00" + config.Profile + "\x00toolset\x00extra/search")
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSampler(func(context.Context, mcpclient.SamplingRequest) (string, string, error) {
		return "hi", "stub-model", nil
	})
	if got := call(); !strings.Contains(got, "sampled by stub-model") {
		t.Fatalf("with a sampler set: %s", got)
	}
}
