package app

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// R5 pre-change characterisation of a mixed chain through a remote hop: dev/coder's sub-agent
// starts a sub-agent on another server (remote:peer/...). The local start is nested (depth 1),
// so it passes the depth gate and takes a remote reservation in the local root and session
// counts (reserveRemote); the remote child runs on the server at depth 0, as any run the remote
// API starts. Pins the local transcripts, the server's child transcript and every local request's
// tools before admission is merged (R5a).
func TestRunBaselineMixedChainThroughARemoteSubagent(t *testing.T) {
	server, serverFixture := e2eServer(t)
	f := newLayoutFixture(t)
	f.bindRemote(t, "peer", server.url)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"async":{},"subagent":{"agents":{"explore":{"description":"Finds facts.","tools":["subagent"]}},"remotes":["peer"]}},"mcp":{}}`)
	f.captureBodies = true
	f.script = func(_, body string) string {
		switch {
		case strings.Contains(body, "function_call_output"):
			return ""
		case strings.Contains(body, "ask the remote"):
			return layoutCall("call-remote", "subagent", map[string]any{"agent": "remote:peer/demo/dev/coder/explore", "task": "find the answer"})
		default:
			return layoutCall("call-sub", "subagent", map[string]any{"agent": "explore", "task": "ask the remote"})
		}
	}
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the chain"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>", server.url: "<server>",
		serverFixture.workspace: "<server-workspace>", serverFixture.toolDir: "<server-tools>"})
	transcripts := runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
	if strings.Count(transcripts, "=== ") != 2 || !strings.Contains(transcripts, "the answer is 42") {
		t.Fatalf("want the coder and its child, the child having the remote child's answer:\n%s", transcripts)
	}
	for _, run := range mixedRunManifests(t, f) {
		if run.State != "done" {
			t.Fatalf("run %+v did not end done", run)
		}
	}
	compareRunBaseline(t, "mixed_chain_remote_transcripts.txt", transcripts)
	compareRunBaseline(t, "mixed_chain_remote_tools.txt", requestTools(t, f.capturedBodies(), m))
	compareRunBaseline(t, "mixed_chain_remote_server_children.txt", runTranscripts(t, serverFixture.toolDir, m))
}
