package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

const (
	appStdioMCPEnvironment = "JAD_APP_STDIO_MCP_FIXTURE"
	// appStdioMCPGrandchildEnvironment makes the fixture start a grandchild of its own, and
	// appStdioMCPHangEnvironment makes it never speak MCP.
	appStdioMCPGrandchildEnvironment = "JAD_APP_STDIO_MCP_GRANDCHILD"
	appStdioMCPHangEnvironment       = "JAD_APP_STDIO_MCP_HANG"
	// appStdioMCPStubbornEnvironment makes the fixture ignore SIGTERM and keep running when its
	// stdin closes, so only a kill ends it and what it started.
	appStdioMCPStubbornEnvironment = "JAD_APP_STDIO_MCP_STUBBORN"
	// appGrandchildSleep is how long the grandchild sleeps; no other process sleeps this long.
	appGrandchildSleep = "600123.456"
)

// TestAppStdioMCPFixtureProcess is the stdio MCP server of the per-run server tests. Its lookup tool
// writes from-mcp.txt in the folder the server works in (or in the file target names, an absolute
// path) and says where it wrote or why it could not. It does nothing unless started as the fixture.
func TestAppStdioMCPFixtureProcess(t *testing.T) {
	if os.Getenv(appStdioMCPEnvironment) != "1" {
		return
	}
	if os.Getenv(appStdioMCPGrandchildEnvironment) == "1" {
		// A wrapper's child, as npx or sh -c leave behind; a unique sleep time names it in /proc.
		grandchild := exec.Command("sleep", appGrandchildSleep)
		grandchild.Stdin, grandchild.Stdout, grandchild.Stderr = nil, nil, nil
		if err := grandchild.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if os.Getenv(appStdioMCPHangEnvironment) == "1" {
		// A server that never answers the initialize request.
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "stdio-fixture", Version: "1.0.0"}, nil)
	server.AddTool(&mcpSDK.Tool{
		Name:        "lookup",
		Description: "writes a file where the server works",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"},"target":{"type":"string"}},"required":["word"],"additionalProperties":false}`),
	}, func(_ context.Context, request *mcpSDK.CallToolRequest) (*mcpSDK.CallToolResult, error) {
		var arguments map[string]string
		_ = json.Unmarshal(request.Params.Arguments, &arguments)
		cwd, _ := os.Getwd()
		target := arguments["target"]
		if target == "" {
			target = "from-mcp.txt"
		}
		text := "MCP wrote " + target + " in " + cwd
		if err := os.WriteFile(target, []byte(arguments["word"]), 0o644); err != nil {
			text = "MCP could not write " + target + ": " + err.Error()
		}
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: text}}}, nil
	})
	if os.Getenv(appStdioMCPStubbornEnvironment) == "1" {
		signal.Ignore(syscall.SIGTERM)
		_ = server.Run(context.Background(), &mcpSDK.StdioTransport{})
		time.Sleep(time.Hour)
	}
	if err := server.Run(context.Background(), &mcpSDK.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

// appStdioMCPServerFile is the config of the fixture server: the test binary run as the fixture,
// in cwd (empty: the session's folder).
func appStdioMCPServerFile(t *testing.T, cwd string, adjust ...func(map[string]any)) map[string]any {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	transport := map[string]any{
		"type": "stdio", "command": executable, "args": []string{"-test.run=^TestAppStdioMCPFixtureProcess$"},
		"env": map[string]string{appStdioMCPEnvironment: "1"},
	}
	if cwd != "" {
		transport["cwd"] = cwd
	}
	file := map[string]any{"schema_version": 1, "transport": transport, "timeout_ms": 10_000, "max_result_bytes": 4096}
	for _, change := range adjust {
		change(file)
	}
	return file
}

// grandchildProcesses counts the fixture's grandchildren that are alive, in any pid namespace the
// host's /proc lists. A zombie is not alive. -1 where /proc cannot be read.
func grandchildProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || !strings.Contains(string(data), appGrandchildSleep) {
			continue
		}
		if status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status")); err == nil && strings.Contains(string(status), "State:\tZ") {
			continue
		}
		count++
	}
	return count
}

// waitGrandchildren polls until the number of live grandchildren is want.
func waitGrandchildren(want int) bool {
	for range 100 {
		if grandchildProcesses() == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// newEnvTestResolver is a resolver whose environment is the process's, which the stdio servers
// inherit the variables of (PATH, HOME).
func newEnvTestResolver(t *testing.T) *Resolver {
	t.Helper()
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			environment[name] = value
		}
	}
	resolver, err := NewResolver([]sessionfs.Root{{ID: "primary", Label: "Primary", Directory: t.TempDir()}}, environment, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	return resolver
}

// fixtureProcesses counts the processes whose command line names the stdio fixture server, the
// ones in other pid namespaces included (the host's /proc lists a joined process too). -1 where
// /proc cannot be read.
func fixtureProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline")); err == nil && strings.Contains(string(data), "TestAppStdioMCPFixtureProcess") {
			count++
		}
	}
	return count
}

// waitFixtureProcesses polls until the number of fixture servers is want, and reports whether it was.
func waitFixtureProcesses(want int) bool {
	for range 100 {
		if fixtureProcesses() == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// perRunManagers counts the managers the resolver holds for isolated runs' own servers.
func perRunManagers(resolver *Resolver) int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	count := 0
	for key := range resolver.mcpManagers {
		if strings.Contains(key, "\x00run\x00") {
			count++
		}
	}
	return count
}

// delegatedMCPFixture is the isolated target fixture whose toolset also lists the stdio fixture
// server; the target's model calls its lookup tool.
func delegatedMCPFixture(t *testing.T, isolation, serverCWD string) *layoutFixture {
	t.Helper()
	f := isolatedTargetFixture(t, isolation)
	body, _ := json.Marshal(appStdioMCPServerFile(t, serverCWD))
	writeFile(t, filepath.Join(f.root, "mcp", "m", "stdio.json"), body)
	f.overwrite(t, "tools/coding/exec.json", `{"native":{"exec":{"spill":{"enabled":false}}},"mcp":{"m/stdio":{"deny_tools":[]}}}`)
	f.script = scriptFunctionCall(mcpToolNameFor("stdio", "lookup"), map[string]any{"word": "by the server"})
	return f
}

// A delegated run in a worktree calls its MCP tool on a server started in that worktree (checkout
// and overlay alike): the file the server writes is on the run's branch, the session's folder is
// unchanged, and the run's server is gone with the run.
func TestDelegatedRunsMCPServerWorksInItsWorktree(t *testing.T) {
	for _, materialize := range []string{sessionfs.SandboxIsolationMaterializeCheckout, sessionfs.SandboxIsolationMaterializeOverlay} {
		t.Run(materialize, func(t *testing.T) {
			if materialize == sessionfs.SandboxIsolationMaterializeOverlay {
				if err := native.ProbeOverlay(context.Background(), "bwrap", 30_000_000_000); err != nil {
					t.Skipf("no overlay on this host: %v", err)
				}
			}
			f := delegatedMCPFixture(t, `{"mode":"worktree","materialize":"`+materialize+`"}`, "")
			outcome := sendToReviewer(t, f)
			if outcome.Err != nil {
				t.Fatalf("outcome = %+v", outcome)
			}
			if _, err := os.Stat(filepath.Join(f.cwd, "from-mcp.txt")); !os.IsNotExist(err) {
				t.Fatalf("the run's MCP server wrote in the session's folder: %v", err)
			}
			branch := worktreeValue(outcome, "worktree_branch")
			if !strings.HasPrefix(branch, "jad/profile-") {
				t.Fatalf("worktree lines = %+v", outcome.Worktree)
			}
			if got := gitIn(t, f.cwd, "show", branch+":from-mcp.txt"); got != "by the server" {
				t.Fatalf("from-mcp.txt on %s = %q", branch, got)
			}
			if count := perRunManagers(f.service.resolver); count != 0 {
				t.Fatalf("%d managers of a finished run's servers are left", count)
			}
		})
	}
}

// A user's own run of the same profile is not isolated: its MCP server works in the session's
// folder, as before, and no server of the run's own starts.
func TestAUsersRunKeepsItsMCPServersInTheSessionFolder(t *testing.T) {
	f := delegatedMCPFixture(t, `{"mode":"worktree"}`, "")
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run?profile=review%2Freviewer", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(f.cwd, "from-mcp.txt")); err != nil || string(data) != "by the server" {
		t.Fatalf("from-mcp.txt in the session folder: %v %q", err, data)
	}
	if branches := gitIn(t, f.cwd, "branch", "--list", "jad/*"); branches != "" {
		t.Fatalf("a user's run made a branch: %s", branches)
	}
	if count := perRunManagers(f.service.resolver); count != 0 {
		t.Fatalf("%d run managers for a user's run", count)
	}
}

// A server that starts in the session's folder but not in the run's worktree (its folder is not in
// the repository) ends the delegated run: the run never falls back to the session's server, which
// would have written in the session's folder.
func TestDelegatedRunEndsWhenItsMCPServerCannotStartInTheWorktree(t *testing.T) {
	f := delegatedMCPFixture(t, `{"mode":"worktree","materialize":"checkout"}`, "mcpdir")
	if err := os.Mkdir(filepath.Join(f.cwd, "mcpdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The folder is not committed, so the worktree has none.
	outcome := sendToReviewer(t, f)
	if outcome.Err == nil {
		t.Fatalf("the run went on without its server: %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "mcpdir", "from-mcp.txt")); !os.IsNotExist(err) {
		t.Fatalf("the session's server took the call: %v", err)
	}
	if branches := gitIn(t, f.cwd, "branch", "--list", "jad/*"); branches != "" {
		t.Fatalf("a branch was left: %s", branches)
	}
	if list := gitIn(t, f.cwd, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ")-baseCheckouts(list) != 1 {
		t.Fatalf("the run's worktree was not removed:\n%s", list)
	}
	if count := perRunManagers(f.service.resolver); count != 0 {
		t.Fatalf("%d managers of a run that never started are left", count)
	}
}

// loaderFixture is a real isolated run (a delegated worktree of a new repository) and what the
// loader needs: the resolver, a session config listing the stdio fixture server and the session's
// own manager whose tool the run's calls go through.
type loaderFixture struct {
	t         *testing.T
	repo      string
	resolver  *Resolver
	config    sessionfs.SessionConfig
	opened    *native.DelegatedWorktree
	execution tools.ExecutionContext
	tool      tools.Tool
}

func newLoaderFixture(t *testing.T, materialize string, sandbox *sessionfs.SandboxSettings) *loaderFixture {
	t.Helper()
	return newLoaderFixtureWith(t, materialize, sandbox)
}

// newLoaderFixtureWith is newLoaderFixture with the fixture server's file adjusted.
func newLoaderFixtureWith(t *testing.T, materialize string, sandbox *sessionfs.SandboxSettings, adjust ...func(map[string]any)) *loaderFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitIn(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "init")
	body, _ := json.Marshal(appStdioMCPServerFile(t, "", adjust...))
	serverPath := filepath.Join(root, "stdio.json")
	if err := os.WriteFile(serverPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := newEnvTestResolver(t)
	config := sessionfs.SessionConfig{
		SettingPath: filepath.Join(root, "session.json"), Profile: "dev/coder", WorkingDir: repo, Sandbox: sandbox,
		MCPServers: []sessionfs.MCPServerReference{{ID: "stdio", ConfigPath: serverPath, Enabled: true}},
	}
	// The session's own manager lists the server in the session's folder; its tool is what the
	// run's calls reach first.
	manager, err := resolver.sharedMCPManager(config)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetRouteKey("")
	loaded, err := manager.LoadTools(context.Background(), config.MCPServers, repo)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("the session's tools = %v, %v", loaded, err)
	}
	f := &loaderFixture{t: t, repo: repo, resolver: resolver, config: config, tool: loaded[0], execution: tools.ExecutionContext{WorkspaceRoot: repo}}
	f.opened, err = native.OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree, Materialize: materialize}, &f.execution, t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// call runs the session's lookup tool as the run's tool call: with the run's execution.
func (f *loaderFixture) call(target string) string {
	f.t.Helper()
	arguments := map[string]any{"word": "x"}
	if target != "" {
		arguments["target"] = target
	}
	output, err := f.tool.ExecuteOutput(context.Background(), arguments, f.execution)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(output)
}

func (f *loaderFixture) finish() native.DelegatedWorktreeResult {
	f.t.Helper()
	return f.opened.Close(context.Background(), f.execution, "m")
}

// The loader starts the server in the run's workspace and the run's calls reach it, in a checkout
// and in an overlay: with a sandbox that lists mcp the server cannot write the session's folder,
// whatever path it is given; without one only its folder is the run's, and an absolute path still
// reaches the session's folder (cwd alone is not a boundary).
func TestRunMCPLoaderServersAreConfinedByTheSessionsSandbox(t *testing.T) {
	boxed := func(tools ...string) *sessionfs.SandboxSettings {
		return &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: tools, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}"}}
	}
	requireBwrap(t)
	for _, materialize := range []string{sessionfs.SandboxIsolationMaterializeCheckout, sessionfs.SandboxIsolationMaterializeOverlay} {
		if materialize == sessionfs.SandboxIsolationMaterializeOverlay {
			if err := native.ProbeOverlay(context.Background(), "bwrap", 30_000_000_000); err != nil {
				t.Logf("no overlay on this host: %v", err)
				continue
			}
		}
		for name, sandbox := range map[string]*sessionfs.SandboxSettings{"no sandbox": nil, "sandbox without mcp": boxed("exec"), "sandbox with mcp": boxed("exec", "mcp")} {
			t.Run(materialize+"/"+name, func(t *testing.T) {
				f := newLoaderFixture(t, materialize, sandbox)
				// The session's own server is running already; the run's are on top of it.
				baseline := fixtureProcesses()
				if err := f.opened.StartMCP(context.Background(), f.execution, f.resolver.runMCPLoader(f.config)); err != nil {
					t.Fatal(err)
				}
				if baseline >= 0 && fixtureProcesses() <= baseline {
					t.Fatalf("no server process started for the run (%d before, %d now)", baseline, fixtureProcesses())
				}
				if count := perRunManagers(f.resolver); count != 1 {
					t.Fatalf("%d managers for the run's servers, want 1", count)
				}
				if out := f.call(""); !strings.Contains(out, "MCP wrote from-mcp.txt in "+f.execution.WorkspaceRoot) {
					t.Fatalf("the server did not work in the run's workspace %s: %s", f.execution.WorkspaceRoot, out)
				}
				escape := filepath.Join(f.repo, "escaped.txt")
				out := f.call(escape)
				_, reached := os.Stat(escape)
				confined := name == "sandbox with mcp"
				if confined && (reached == nil || !strings.Contains(out, "could not write")) {
					t.Fatalf("a confined server wrote the session's folder (%v): %s", reached, out)
				}
				if !confined && reached != nil {
					t.Fatalf("an unconfined server could not write the session's folder, so this test proves nothing: %s", out)
				}
				result := f.finish()
				if count := perRunManagers(f.resolver); count != 0 {
					t.Fatalf("%d managers are left after the run", count)
				}
				if baseline >= 0 && !waitFixtureProcesses(baseline) {
					t.Fatalf("the run's server processes are still running: %d, want the session's %d", fixtureProcesses(), baseline)
				}
				branch := ""
				for _, item := range result.Information {
					if item.Key == "worktree_branch" {
						branch, _ = item.Value.(string)
					}
				}
				if got := gitIn(t, f.repo, "show", branch+":from-mcp.txt"); got != "x" {
					t.Fatalf("from-mcp.txt on %q = %q", branch, got)
				}
				// A call after the run fails; it never reaches the session's server.
				if after := f.call(""); !strings.Contains(after, "has ended") {
					t.Fatalf("a call after the run: %s", after)
				}
				if _, err := os.Stat(filepath.Join(f.repo, "from-mcp.txt")); !os.IsNotExist(err) {
					t.Fatalf("the session's folder was written after the run: %v", err)
				}
			})
		}
	}
}

// Servers of two runs are separate, a session or the resolver closing stops a run's servers, and an
// unknown set key is an error.
func TestRunMCPLoaderServersBelongToTheirRunAndTheResolver(t *testing.T) {
	first := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, nil)
	load := first.resolver.runMCPLoader(first.config)
	one, err := load(context.Background(), native.RunMCPRequest{Workspace: first.execution.WorkspaceRoot, Origin: first.repo})
	if err != nil {
		t.Fatal(err)
	}
	two, err := load(context.Background(), native.RunMCPRequest{Workspace: first.execution.WorkspaceRoot, Origin: first.repo})
	if err != nil {
		t.Fatal(err)
	}
	if count := perRunManagers(first.resolver); count != 2 || len(one.Servers) != 1 || len(two.Servers) != 1 {
		t.Fatalf("managers %d, servers %v / %v", count, one.Servers, two.Servers)
	}
	one.Close()
	one.Close()
	if count := perRunManagers(first.resolver); count != 1 {
		t.Fatalf("closing one run's servers left %d managers, want the other run's", count)
	}
	// A call to the closed run's tool fails and the other's still works.
	callTool := func(servers native.RunMCPServers) string {
		output, err := servers.Tools[0].ExecuteOutput(context.Background(), map[string]any{"word": "y", "target": filepath.Join(first.execution.WorkspaceRoot, "direct.txt")}, tools.ExecutionContext{})
		if err != nil {
			return err.Error()
		}
		return string(output)
	}
	if out := callTool(two); !strings.Contains(out, "MCP wrote") {
		t.Fatalf("the other run's server: %s", out)
	}
	if out := callTool(one); strings.Contains(out, "MCP wrote") {
		t.Fatalf("a closed run's server answered: %s", out)
	}
	if _, err := load(context.Background(), native.RunMCPRequest{Key: "no-such-set", Workspace: first.execution.WorkspaceRoot}); err == nil || !strings.Contains(err.Error(), "no-such-set") {
		t.Fatalf("an unknown set = %v", err)
	}
	// The resolver stops what a run left running.
	first.resolver.Close()
	if out := callTool(two); strings.Contains(out, "MCP wrote") {
		t.Fatalf("a server answered after the resolver closed: %s", out)
	}
	if _, err := load(context.Background(), native.RunMCPRequest{Workspace: first.execution.WorkspaceRoot}); err == nil {
		t.Fatal("a server started after the resolver closed")
	}
	first.finish()
}

// The tool the run's calls go through never reaches the run's server for a streamable HTTP server:
// the loader does not start it.
func TestRunMCPLoaderStartsOnlyStdioServers(t *testing.T) {
	f := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, nil)
	remote := filepath.Join(filepath.Dir(f.config.SettingPath), "remote.json")
	if err := os.WriteFile(remote, []byte(`{"transport":{"type":"streamable_http","url":"https://mcp.invalid/mcp"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.MCPServers = append([]sessionfs.MCPServerReference{{ID: "remote", ConfigPath: remote, Enabled: true}}, config.MCPServers...)
	servers, err := f.resolver.runMCPLoader(config)(context.Background(), native.RunMCPRequest{Workspace: f.execution.WorkspaceRoot, Origin: f.repo})
	if err != nil {
		t.Fatal(err)
	}
	defer servers.Close()
	if fmt.Sprint(servers.Servers) != "[stdio]" {
		t.Fatalf("stdio servers = %v", servers.Servers)
	}
	f.finish()
}

// An isolated sub-agent calls its MCP tool on a server started in its worktree: the file the server
// writes is on the branch the parent is told of and not in the session's folder. The same agent
// without isolation keeps working through the session's server, in the session's folder.
func TestIsolatedSubagentCallsItsMCPToolOnAServerInItsWorktree(t *testing.T) {
	for _, isolated := range []bool{true, false} {
		t.Run(fmt.Sprintf("isolated=%v", isolated), func(t *testing.T) {
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("git is not installed")
			}
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			agent := map[string]any{"model": "child.json", "tools": []string{"skill", "exec", "mcp:fixture:lookup"}, "__exec": map[string]any{}}
			if isolated {
				agent["isolation"] = "worktree"
			}
			f := buildSubagentFixtureWithMCP(t, agent, false, nil, &mcpFixture{stdio: true})
			f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"by the server"}`}
			gitIn(t, f.workspace, "init", "--quiet")
			gitIn(t, f.workspace, "add", "notes.txt")
			gitIn(t, f.workspace, "commit", "--quiet", "-m", "init")
			if body := f.run(t); !strings.Contains(body, "event: completed") {
				t.Fatalf("run did not complete:\n%s", body)
			}
			written, err := os.ReadFile(filepath.Join(f.workspace, "from-mcp.txt"))
			if !isolated {
				if err != nil || string(written) != "by the server" {
					t.Fatalf("the session's server did not write in the session's folder: %v %q", err, written)
				}
				return
			}
			if err == nil {
				t.Fatalf("the run's MCP server wrote in the session's folder: %q", written)
			}
			branch := gitIn(t, f.workspace, "branch", "--list", "jad/*", "--format=%(refname:short)")
			if !strings.HasPrefix(branch, "jad/sub_") {
				t.Fatalf("branches = %q", branch)
			}
			if got := gitIn(t, f.workspace, "show", branch+":from-mcp.txt"); got != "by the server" {
				t.Fatalf("from-mcp.txt on %s = %q", branch, got)
			}
			parent := f.provider.forModel("parent-model")
			if last := parent[len(parent)-1].Text; !strings.Contains(last, "worktree_branch: "+branch) {
				t.Fatalf("the parent was not told of the branch:\n%s", last)
			}
		})
	}
}

// A toolset agent's servers are the toolset's, not the session's: the loader reads them from the
// derived settings the key names, and starts them in the sandbox of that toolset.
func TestRunMCPLoaderStartsAToolsetsServersByKey(t *testing.T) {
	f := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, nil)
	defer f.finish()
	toolsetServers := f.config.MCPServers
	config := f.config
	config.MCPServers = nil
	config.AgentToolsets = map[string]sessionfs.SessionConfig{"extra/search": {WorkingDir: f.repo, MCPServers: toolsetServers}}
	config.NativeSubagent = &sessionfs.NativeSubagentSettings{Agents: []sessionfs.NativeSubagentAgent{{Name: "finder", Toolset: []string{"extra/search"}}}}
	load := f.resolver.runMCPLoader(config)
	request := native.RunMCPRequest{Workspace: f.execution.WorkspaceRoot, Origin: f.repo}
	if own, err := load(context.Background(), request); err != nil || len(own.Servers) != 0 {
		t.Fatalf("the session's own servers: %v, %v", own.Servers, err)
	}
	request.Key = "extra/search"
	servers, err := load(context.Background(), request)
	if err != nil || fmt.Sprint(servers.Servers) != "[stdio]" {
		t.Fatalf("the toolset's servers: %v, %v", servers.Servers, err)
	}
	defer servers.Close()
	output, err := servers.Tools[0].ExecuteOutput(context.Background(), map[string]any{"word": "t"}, tools.ExecutionContext{})
	if err != nil || !strings.Contains(string(output), "MCP wrote from-mcp.txt in "+f.execution.WorkspaceRoot) {
		t.Fatalf("the toolset's server did not work in the run's workspace: %s %v", output, err)
	}
}

// The tools of a toolset's servers ask the run's route for that toolset's key, not the session's, so
// a run's servers of one set never answer for another.
func TestToolsetMCPToolsAskTheRouteForTheirSetKey(t *testing.T) {
	f := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, nil)
	defer f.finish()
	config := f.config
	config.AgentToolsets = map[string]sessionfs.SessionConfig{"extra/search": {WorkingDir: f.repo, MCPServers: f.config.MCPServers}}
	config.NativeSubagent = &sessionfs.NativeSubagentSettings{Agents: []sessionfs.NativeSubagentAgent{{Name: "finder", Toolset: []string{"extra/search"}}}}
	loaded, err := f.resolver.loadAgentToolsetMCP(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	routed := tools.Tool{ExecuteOutput: func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
		return json.RawMessage(`"routed"`), nil
	}}
	execution := tools.ExecutionContext{MCPRoute: func(key string, definition tools.Definition, _ tools.ExecutionContext) (tools.Tool, bool, error) {
		asked = append(asked, key)
		return routed, true, nil
	}}
	output, err := loaded["extra/search"].Tools[0].ExecuteOutput(context.Background(), map[string]any{"word": "x"}, execution)
	if err != nil || string(output) != `"routed"` || fmt.Sprint(asked) != "[extra/search]" {
		t.Fatalf("output %s, %v; the route was asked for %v", output, err, asked)
	}
}

func mcpSandbox(tools []string, writes ...string) *sessionfs.SandboxSettings {
	return &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: tools, ReadPaths: []string{"/"}, WritablePaths: writes}
}

// The sandbox a run's servers start in is the effective one of the agent they start for, and only a
// toolset's own sandbox can add to it.
func TestRunServerSandbox(t *testing.T) {
	session := mcpSandbox([]string{"exec", "mcp"}, "{workspace}")
	agent := mcpSandbox([]string{"exec", "mcp"}, "{workspace}/src")
	noMCP := mcpSandbox([]string{"exec"}, "{workspace}")
	wider := mcpSandbox([]string{"exec", "mcp"}, "/", "{workspace}")
	for _, test := range []struct {
		name       string
		configured *sessionfs.SandboxSettings
		toolset    bool
		env        *tools.Environment
		want       *sessionfs.SandboxSettings
		fails      bool
	}{
		{name: "no environment, configured lists mcp", configured: session, want: session},
		{name: "no environment, configured lacks mcp", configured: noMCP},
		{name: "the agent's narrower sandbox, not the session's", configured: session, env: &tools.Environment{Sandbox: agent}, want: agent},
		{name: "an environment without mcp does not confine, as for native tools", configured: session, env: &tools.Environment{Sandbox: noMCP}},
		{name: "an environment with no sandbox", configured: session, env: &tools.Environment{}},
		{name: "toolset without a sandbox of its own", toolset: true, env: &tools.Environment{Sandbox: agent}, want: agent},
		{name: "toolset with a sandbox, agent without", toolset: true, configured: session, env: &tools.Environment{Sandbox: noMCP}, want: session},
		{name: "toolset with a sandbox, no environment", toolset: true, configured: session, want: session},
		{name: "toolset and a narrower agent", toolset: true, configured: session, env: &tools.Environment{Sandbox: agent}, want: agent},
		{name: "an agent that reaches more than its toolset's servers", toolset: true, configured: session, env: &tools.Environment{Sandbox: wider}, fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := runServerSandbox(test.configured, test.toolset, test.env)
			if test.fails {
				if err == nil || !strings.Contains(err.Error(), "does not fit") {
					t.Fatalf("= %+v, %v, want a refusal", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.want == nil && got != nil {
				t.Fatalf("= %+v, want none", got)
			}
			if test.want != nil && (got == nil || fmt.Sprint(got.WritablePaths) != fmt.Sprint(test.want.WritablePaths) || !got.Covers("mcp")) {
				t.Fatalf("= %+v, want %+v", got, test.want)
			}
		})
	}
}

// A broad parent and a narrow child ask the loader for servers of their own sandbox each, and each
// server is confined by the sandbox it was asked for: the parent's writes the run's workspace, the
// child's only its own folder, whatever the sandbox of the session. A call by the child before its
// servers exist does not reach the parent's, nor the session's.
func TestRunMCPLoaderServersFollowTheAgentsSandbox(t *testing.T) {
	requireBwrap(t)
	f := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, mcpSandbox([]string{"exec", "mcp"}, "{workspace}"))
	defer f.finish()
	workspace := f.execution.WorkspaceRoot
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := f.resolver.runMCPLoader(f.config)
	broad := &tools.Environment{Sandbox: f.config.Sandbox}
	narrow := &tools.Environment{Sandbox: mcpSandbox([]string{"exec", "mcp"}, "{workspace}/src")}
	start := func(environment *tools.Environment) native.RunMCPServers {
		servers, err := load(context.Background(), native.RunMCPRequest{Workspace: workspace, Origin: f.repo, Environment: environment})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(servers.Close)
		return servers
	}
	write := func(servers native.RunMCPServers, target string) string {
		output, err := servers.Tools[0].ExecuteOutput(context.Background(), map[string]any{"word": "x", "target": target}, tools.ExecutionContext{})
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	parent, child := start(broad), start(narrow)
	if out := write(parent, filepath.Join(workspace, "parent.txt")); !strings.Contains(out, "MCP wrote") {
		t.Fatalf("the parent's server in the run's workspace: %s", out)
	}
	if out := write(child, filepath.Join(workspace, "child.txt")); !strings.Contains(out, "could not write") {
		t.Fatalf("the narrow child's server wrote outside its folder: %s", out)
	}
	if out := write(child, filepath.Join(workspace, "src", "child.txt")); !strings.Contains(out, "MCP wrote") {
		t.Fatalf("the narrow child's server in its own folder: %s", out)
	}
	if _, err := os.Stat(filepath.Join(workspace, "child.txt")); err == nil {
		t.Fatal("child.txt was written by the confined server")
	}
	if count := perRunManagers(f.resolver); count != 2 {
		t.Fatalf("%d managers for two sandboxes, want 2", count)
	}
	// Through the run's route: the run's servers are started for the parent's execution, and the
	// session's tool called as the narrow child, for which none were, fails. It reaches neither the
	// parent's server nor the session's.
	f.execution.Environment = broad
	if err := f.opened.StartMCP(context.Background(), f.execution, load); err != nil {
		t.Fatal(err)
	}
	childExecution := f.execution
	childExecution.Environment = narrow
	output, err := f.tool.ExecuteOutput(context.Background(), map[string]any{"word": "x", "target": filepath.Join(workspace, "src", "routed.txt")}, childExecution)
	if err != nil || !strings.Contains(string(output), "was not started for this agent") {
		t.Fatalf("the child's call with no servers of its own: %s, %v", output, err)
	}
	for _, dir := range []string{workspace, f.repo} {
		if _, err := os.Stat(filepath.Join(dir, "src", "routed.txt")); err == nil {
			t.Fatalf("the child's call wrote in %s", dir)
		}
	}
	if out := f.call(""); !strings.Contains(out, "MCP wrote from-mcp.txt in "+workspace) {
		t.Fatalf("the parent's call through the route: %s", out)
	}
}

// A profile_message target of a plan-mode caller runs in the caller's sandbox in its plan-mode form
// (native callerEnvironment), and its servers start in that form: they cannot write the workspace
// the caller's normal form lets them write, only the plan-mode paths. (A server belongs to no call,
// so a {tmp} entry would be the system temp folder: the paths here are explicit.)
func TestRunMCPLoaderServersOfAPlanModeCallersTargetKeepThePlanForm(t *testing.T) {
	requireBwrap(t)
	f := newLoaderFixture(t, sessionfs.SandboxIsolationMaterializeCheckout, nil)
	defer f.finish()
	workspace := f.execution.WorkspaceRoot
	planOut := filepath.Join(t.TempDir(), "plan-out")
	if err := os.Mkdir(planOut, 0o755); err != nil {
		t.Fatal(err)
	}
	caller := mcpSandbox([]string{"exec", "mcp"}, "{workspace}")
	caller.PlanWritablePaths = []string{planOut}
	load := f.resolver.runMCPLoader(f.config)
	place := sessionfs.SandboxPlace{Workspace: workspace, CWD: workspace}
	for _, test := range []struct {
		name                   string
		sandbox                *sessionfs.SandboxSettings
		inWorkspace, inPlanOut bool
	}{{"normal form", caller, true, false}, {"plan-mode form", caller.ForPlanMode(), false, true}} {
		t.Run(test.name, func(t *testing.T) {
			target, err := confineTarget(test.sandbox, place, nil, f.config)
			if err != nil {
				t.Fatal(err)
			}
			servers, err := load(context.Background(), native.RunMCPRequest{Workspace: workspace, Origin: f.repo, Environment: runEnvironment(target)})
			if err != nil {
				t.Fatal(err)
			}
			defer servers.Close()
			for dir, want := range map[string]bool{workspace: test.inWorkspace, planOut: test.inPlanOut} {
				file := filepath.Join(dir, strings.ReplaceAll(test.name, " ", "-")+".txt")
				output, err := servers.Tools[0].ExecuteOutput(context.Background(), map[string]any{"word": "x", "target": file}, tools.ExecutionContext{})
				if err != nil {
					t.Fatal(err)
				}
				_, statErr := os.Stat(file)
				if wrote := strings.Contains(string(output), "MCP wrote"); wrote != want || (statErr == nil) != want {
					t.Fatalf("write in %s: wrote = %v, file present = %v, want %v: %s", dir, wrote, statErr == nil, want, output)
				}
			}
		})
	}
}

// A server and what it started stop when its run's servers are closed, however that happens: the
// run ends, a server never answers and the start is cancelled, or its start time runs out; a server
// of a sandbox or an overlay run too. A killed jad leaves no record (an open gap in DECISIONS.md).
func TestRunMCPLoaderStopsTheServersProcessGroup(t *testing.T) {
	boxed := mcpSandbox([]string{"exec", "mcp"}, "{workspace}")
	grandchildOf := func(file map[string]any) {
		file["transport"].(map[string]any)["env"].(map[string]string)[appStdioMCPGrandchildEnvironment] = "1"
	}
	hangs := func(file map[string]any) {
		file["transport"].(map[string]any)["env"].(map[string]string)[appStdioMCPHangEnvironment] = "1"
	}
	requireBwrap(t)
	for _, materialize := range []string{sessionfs.SandboxIsolationMaterializeCheckout, sessionfs.SandboxIsolationMaterializeOverlay} {
		if materialize == sessionfs.SandboxIsolationMaterializeOverlay {
			if err := native.ProbeOverlay(context.Background(), "bwrap", 30_000_000_000); err != nil {
				t.Logf("no overlay on this host: %v", err)
				continue
			}
		}
		for name, sandbox := range map[string]*sessionfs.SandboxSettings{"no sandbox": nil, "sandbox with mcp": boxed} {
			t.Run(materialize+"/"+name+"/run ends", func(t *testing.T) {
				f := newLoaderFixtureWith(t, materialize, sandbox, grandchildOf)
				baseline := grandchildProcesses()
				if err := f.opened.StartMCP(context.Background(), f.execution, f.resolver.runMCPLoader(f.config)); err != nil {
					t.Fatal(err)
				}
				if !waitGrandchildren(baseline + 1) {
					t.Fatalf("the run's server did not start its grandchild (%d, session's %d)", grandchildProcesses(), baseline)
				}
				f.finish()
				if !waitGrandchildren(baseline) {
					t.Fatalf("the grandchild of a run's server outlived the run: %d alive, the session's are %d", grandchildProcesses(), baseline)
				}
			})
			t.Run(materialize+"/"+name+"/run ends on a server that ignores stdin and SIGTERM", func(t *testing.T) {
				if name != "sandbox with mcp" {
					t.Skip("the slow case runs in the sandbox only")
				}
				f := newLoaderFixtureWith(t, materialize, sandbox, grandchildOf, func(file map[string]any) {
					file["transport"].(map[string]any)["env"].(map[string]string)[appStdioMCPStubbornEnvironment] = "1"
				})
				baseline, servers := grandchildProcesses(), fixtureProcesses()
				if err := f.opened.StartMCP(context.Background(), f.execution, f.resolver.runMCPLoader(f.config)); err != nil {
					t.Fatal(err)
				}
				if !waitGrandchildren(baseline + 1) {
					t.Fatal("the run's server did not start its grandchild")
				}
				began := time.Now()
				f.finish()
				// The SDK waits out its terminate delays (5s each: stdin, SIGTERM, SIGKILL) for the direct
				// child; the group is killed after.
				if took := time.Since(began); took > 15*time.Second {
					t.Fatalf("the run took %v to end", took)
				}
				if !waitGrandchildren(baseline) || !waitFixtureProcesses(servers) {
					t.Fatalf("a stubborn server or its grandchild outlived the run: %d grandchildren (before %d), %d servers (before %d)", grandchildProcesses(), baseline, fixtureProcesses(), servers)
				}
			})
			for _, ending := range []string{"cancelled", "timed out"} {
				t.Run(materialize+"/"+name+"/start "+ending, func(t *testing.T) {
					f := newLoaderFixtureWith(t, materialize, sandbox, grandchildOf)
					defer f.finish()
					// The run's own copy of the server file hangs; the session's server is up already.
					body, _ := json.Marshal(appStdioMCPServerFile(t, "", grandchildOf, hangs, func(file map[string]any) {
						if ending == "timed out" {
							file["timeout_ms"] = 600
						}
					}))
					if err := os.WriteFile(f.config.MCPServers[0].ConfigPath, body, 0o600); err != nil {
						t.Fatal(err)
					}
					baseline, servers := grandchildProcesses(), fixtureProcesses()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if ending == "cancelled" {
						time.AfterFunc(600*time.Millisecond, cancel)
					}
					began := time.Now()
					err := f.opened.StartMCP(ctx, f.execution, f.resolver.runMCPLoader(f.config))
					if err == nil {
						t.Fatal("a server that never answers started")
					}
					if time.Since(began) > 3*time.Second {
						t.Fatalf("the start took %v to give up: %v", time.Since(began), err)
					}
					if !waitGrandchildren(baseline) || !waitFixtureProcesses(servers) {
						t.Fatalf("a server that did not start left %d grandchildren (before: %d): %v", grandchildProcesses(), baseline, err)
					}
					if count := perRunManagers(f.resolver); count != 0 {
						t.Fatalf("%d managers are left after a failed start", count)
					}
				})
			}
		}
	}
}
