package app

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R5 pre-change baseline of a profile_message sandbox choice (the R4 open item): dev/coder sends
// review/reviewer a message with sandbox "reader" (a command-backend template, so bwrap is not
// needed). It pins the caller's profile_message definition (the sandbox argument and the list of
// choices), the tools the target is sent while it runs in the choice, and both transcripts,
// which routing profile_message through StartRun must keep byte for byte.
func TestRunBaselineProfileMessageSandboxChoice(t *testing.T) {
	f := newLayoutFixture(t)
	reader := `{"schema_version":1,"description":"Reader: writes nothing but scratch","enabled":true,"backend":"command","command":["env"],` +
		`"tools":["exec"],"write":{"paths":["{tmp}"],"plan_paths":["{tmp}"]}}`
	if err := os.MkdirAll(filepath.Join(f.global, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.overwrite(t, "sandbox/reader.json", reader)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"exec":{"spill":{"enabled":false}},"profile_message":{"sandbox_choice":["reader"]}},"mcp":{}}`)
	f.captureBodies = true
	f.script = func(model, body string) string {
		if model == "model-small" && !strings.Contains(body, "function_call_output") {
			return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "please review", "sandbox": "reader"})
		}
		return ""
	}
	status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"start the review"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	m := newRunBaselineMasker(map[string]string{f.cwd: "<cwd>", f.root: "<root>", f.api.URL: "<api>"})
	transcripts := runTranscripts(t, filepath.Join(f.cwd, ".jad", "state"), m)
	if strings.Count(transcripts, "=== ") != 2 || strings.Contains(transcripts, "error_code") {
		t.Fatalf("want the coder's and the reviewer's transcripts, the message answered:\n%s", transcripts)
	}
	tools := requestTools(t, f.capturedBodies(), m)
	if !strings.Contains(tools, `"sandbox"`) {
		t.Fatalf("the caller's profile_message definition has no sandbox argument:\n%s", tools)
	}
	compareRunBaseline(t, "profile_sandbox_choice_transcripts.txt", transcripts)
	compareRunBaseline(t, "profile_sandbox_choice_tools.txt", tools)
}
