package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
)

// addCallerProfile adds a second profile, dev/caller, to a session that is already built and
// running. It writes the profile file and its toolset, and lists the profile in the session's
// scope, which the server reads on each request. The caller has no tools.lazy and no subagent.
func addCallerProfile(t *testing.T, f subagentFixture) string {
	t.Helper()
	layoutRoot := filepath.Dir(filepath.Dir(f.modelsDir))
	profile := map[string]any{"models": []string{"openai/parent"}, "model_default": "openai/parent", "toolset": "coding/caller"}
	writeJSON(t, filepath.Join(layoutRoot, "profiles", "dev", "caller.json"), profile)
	writeJSON(t, filepath.Join(layoutRoot, "tools", "coding", "caller.json"), map[string]any{"native": map[string]any{}})
	var session map[string]any
	raw, err := os.ReadFile(f.sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	// The session may have no scope of its own: then the server offers every profile, and the caller
	// is already visible. Only a scope that lists profiles needs the caller added to it.
	if scope, ok := session["scope"].(map[string]any); ok {
		scope["profiles"] = []string{"dev/coder", "dev/caller"}
	}
	writeJSON(t, f.sessionFile, session)
	// The caller's state folder is the sibling of the target's: <state>/demo/<category>/<name>.
	return filepath.Join(filepath.Dir(f.stateDir), "caller")
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}

// A remote subagent call on a target profile with tools.lazy: the child's request lists the lazy MCP
// tool only after its load, the target's own lazy_tools.json holds it, and the caller profile's state
// folder is never created.
func TestRemoteSubagentGatesTheTargetsLazyToolFromItsOwnStateFolder(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	profileTools := map[string]any{"allow": []string{"subagent", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "description": "Finds facts.", "tools": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{"load_tool", `{"names":["` + name + `"]}`}
	callerState := addCallerProfile(t, f)
	server := startRemoteServer(t, f, tokenTable(tokenRow("peer", remoteTestSecret, fullGrant)))
	status, body := server.call(t, "POST", remoteapi.Prefix+"/subagents", remoteTestSecret, map[string]any{
		"session": "demo", "profile": "dev/coder", "agent": "explore", "task": "load the lookup tool", "request_id": "req-lazy-00001"})
	if status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	if done := server.waitDone(t, body["id"].(string)); done["state"] != "done" {
		t.Fatalf("outcome: %v", done)
	}

	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests, want the load and the answer", len(child))
	}
	if slices.Contains(child[0].Tools, name) {
		t.Fatalf("the lazy MCP tool was in the request before the load: %v", child[0].Tools)
	}
	if !slices.Contains(child[1].Tools, name) {
		t.Fatalf("the loaded MCP tool was absent from the next request: %v", child[1].Tools)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); !slices.Contains(loaded, "mcp:fixture:lookup") {
		t.Errorf("the target's lazy_tools.json holds %v, want mcp:fixture:lookup", loaded)
	}
	if _, err := os.Stat(filepath.Join(callerState, "lazy_tools.json")); !os.IsNotExist(err) {
		t.Errorf("the caller's lazy_tools.json exists or could not be checked: %v", err)
	}
	if _, err := os.Stat(callerState); !os.IsNotExist(err) {
		t.Errorf("the caller's state folder was created or could not be checked: %v", err)
	}
}
