package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/evalrun"
	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// useProviderFixture writes a layout whose one model points at a fake Responses endpoint
// that answers every request with the assistant message "ok".
func useProviderFixture(t *testing.T) layouttest.Fixture {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	fixture := useFixture(t, layouttest.Spec{
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}},
		Server: map[string]any{"log_level": "off"},
	})
	t.Setenv("FIXTURE_KEY", "k")
	return fixture
}

func runJad(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := dispatchCommand(args, commandStreams{stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

func exitCode(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestRunCommandFormats(t *testing.T) {
	fixture := useProviderFixture(t)
	session := "local:" + fixture.SessionID

	stdout, stderr, err := runJad(t, "", "run", "--session", session, "say", "ok")
	if err != nil || stdout != "ok\n" {
		t.Fatalf("text: stdout %q stderr %q err %v", stdout, stderr, err)
	}

	stdout, _, err = runJad(t, "from stdin", "run", "--session", session, "--format", "json", "-")
	var result headless.Result
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Status != headless.StatusCompleted || result.Text != "ok" || len(result.Usage) == 0 {
		t.Fatalf("json: %q %v", stdout, err)
	}
	transcript := filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(transcript); err != nil || !strings.Contains(string(data), "from stdin") {
		t.Fatalf("the stdin prompt is not in the transcript: %v %s", err, data)
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "--profile", "dev/coder", "--format", "jsonl", "hi")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var line struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("jsonl line %q: %v", scanner.Text(), err)
		}
		names = append(names, line.Event)
	}
	if len(names) < 3 || names[len(names)-3] != "completed" || names[len(names)-2] != "done" || names[len(names)-1] != "result" {
		t.Fatalf("jsonl events = %v", names)
	}
}

func TestRunCommandUsageAndRefusalExitCodes(t *testing.T) {
	fixture := useProviderFixture(t)
	for name, args := range map[string][]string{
		"no session":     {"run", "hi"},
		"bad format":     {"run", "--session", "x", "--format", "xml", "hi"},
		"unknown flag":   {"run", "--bogus"},
		"empty stdin":    {"run", "--session", "x", "-"},
		"negative limit": {"run", "--session", "x", "--timeout", "-1s", "hi"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitUsage {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitUsage)
		}
	}
	_, _, err := runJad(t, "", "run", "--session", "local:missing", "hi")
	if exitCode(err) != exitFailed || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown session: err = %v, want exit 1 with the refusal", err)
	}
	stdout, _, err := runJad(t, "", "run", "--help")
	if exitCode(err) != 0 || !strings.Contains(stdout, "usage: jad run") {
		t.Fatalf("help: %q %v", stdout, err)
	}
	_ = fixture
}

func TestEvalCommandWritesTheReportAndFailsOnAFailedCase(t *testing.T) {
	fixture := useProviderFixture(t)
	dir := t.TempDir()
	suite := map[string]any{
		"schema_version": 1, "name": "cmd", "profile": "dev/coder", "work_dir": dir,
		"cases": []any{
			map[string]any{"name": "answers", "prompt": "say ok", "expect_text": []string{"^ok$"}, "checks": []string{`test "$(cat "$JAD_EVAL_ANSWER_FILE")" = ok`}},
			map[string]any{"name": "fails", "prompt": "say ok", "checks": []string{"test -f missing.txt"}},
		},
	}
	raw, _ := json.Marshal(suite)
	suitePath := filepath.Join(dir, "suite.json")
	if err := os.WriteFile(suitePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.json")
	stdout, stderr, err := runJad(t, "", "eval", "--out", reportPath, suitePath)
	if exitCode(err) != exitFailed {
		t.Fatalf("err = %v, want exit 1 for the failed case\nstderr: %s", err, stderr)
	}
	if stdout != "" || !strings.Contains(stderr, "PASS  answers#1") || !strings.Contains(stderr, "FAIL  fails#1") || !strings.Contains(stderr, "cmd: 1/2 passed") {
		t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report evalrun.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.Passed != 1 || report.Summary.Failed != 1 || report.Summary.InputTokens != 10 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	sessionFile := filepath.Join(fixture.SessionsDir, strings.TrimPrefix(report.Results[0].Session, "local:")+".json")
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("the case session is not kept for inspection: %v", err)
	}

	if _, _, err := runJad(t, "", "eval", "--cases", "^answers$", suitePath); exitCode(err) != 0 {
		t.Fatalf("filtered to the passing case: err = %v, want exit 0", err)
	}
	if _, _, err := runJad(t, "", "eval"); exitCode(err) != exitUsage {
		t.Fatalf("no suite: err = %v, want exit %d", err, exitUsage)
	}
}

func TestEvalCommandReadsSlashCommands(t *testing.T) {
	useProviderFixture(t)
	dir := t.TempDir()
	write := func(name string, cases ...map[string]any) string {
		raw, _ := json.Marshal(map[string]any{"schema_version": 1, "name": "cmds", "profile": "dev/coder", "work_dir": dir, "cases": cases})
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	planned := write("plan.json", map[string]any{"name": "planned", "prompt": "/plan say ok", "expect_text": []string{"^ok$"}})
	stdout, stderr, err := runJad(t, "", "eval", planned)
	var report evalrun.Report
	if exitCode(err) != 0 || json.Unmarshal([]byte(stdout), &report) != nil {
		t.Fatalf("/plan case: %v\n%s", err, stderr)
	}
	if run := report.Results[0].Run; run == nil || !run.Plan || run.Command != "plan" {
		t.Fatalf("the /plan case did not run in plan mode: %+v", report.Results[0].Run)
	}

	reset := write("reset.json", map[string]any{"name": "reset", "prompt": "/reset", "expect_text": []string{"^ok$"}})
	if _, _, err := runJad(t, "", "eval", reset); exitCode(err) != exitFailed || !strings.Contains(err.Error(), "runs no turn") {
		t.Fatalf("/reset case: err = %v, want exit 1 naming the action", err)
	}
	stdout, stderr, err = runJad(t, "", "eval", "--no-commands", reset)
	report = evalrun.Report{}
	if exitCode(err) != 0 || json.Unmarshal([]byte(stdout), &report) != nil || report.Results[0].Run.Command != "" {
		t.Fatalf("--no-commands: %v\n%s", err, stderr)
	}
}
