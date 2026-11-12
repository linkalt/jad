package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMigrateSource writes a v1 setting in <workspace>/.jad with one session, profile,
// model and MCP server, the MCP server holding a header value that must never be shown.
func writeMigrateSource(t *testing.T) (workspace, home string) {
	t.Helper()
	base := t.TempDir()
	workspace, home = filepath.Join(base, "project"), filepath.Join(base, "home")
	files := map[string]any{
		"setting.json":               map[string]any{"schema_version": 1, "session_roots": []any{map[string]any{"id": "local", "path": "sessions"}}, "catalogs": map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": "state"}, "defaults": map[string]any{"cwd": workspace}},
		"models/openai/default.json": map[string]any{"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY"},
		"mcp/web/search.json":        map[string]any{"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": "http://127.0.0.1:1/mcp", "headers": map[string]any{"Authorization": "Bearer cli-secret-value"}}},
		"profiles/dev/coder.json":    map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default"},
		"sessions/work.json":         map[string]any{"schema_version": 1, "cwd": workspace},
	}
	for rel, value := range files {
		path := filepath.Join(workspace, ".jad", rel)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("JAD_GLOBAL_DIR", "")
	t.Setenv("JAD_ROOT_SETTING_PATH", "")
	t.Chdir(workspace)
	return workspace, home
}

func TestMigrateWorkspacePrintsThePlanAndWritesNothing(t *testing.T) {
	workspace, home := writeMigrateSource(t)
	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := dispatchCommand([]string{"migrate", "workspace"}, streams); err != nil {
		t.Fatalf("dry run: %v\n%s", err, stdout.String())
	}
	text := stdout.String()
	for _, want := range []string{"dry run, nothing was written", "create " + filepath.Join(workspace, ".jad", "workspace.json"), "additions to the global folder", filepath.Join(home, ".jad", "mcp", "web", "search.json"), "transport.headers.Authorization"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the plan lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "cli-secret-value") {
		t.Fatalf("the plan shows a secret value:\n%s", text)
	}
	for _, path := range []string{filepath.Join(workspace, ".jad", "workspace.json"), filepath.Join(home, ".jad")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was written", path)
		}
	}

	stdout.Reset()
	if err := dispatchCommand([]string{"migrate", "workspace", "--json"}, streams); err != nil {
		t.Fatalf("json: %v", err)
	}
	var plan struct {
		Root  string `json:"root"`
		Files []struct {
			Action string `json:"action"`
		} `json:"files"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil || plan.Root != "local" || len(plan.Files) == 0 {
		t.Fatalf("json plan %+v: %v\n%s", plan, err, stdout.String())
	}
	if strings.Contains(stdout.String(), "cli-secret-value") {
		t.Fatal("the JSON plan shows a secret value")
	}
}

func TestMigrateWorkspaceRefusesOtherArguments(t *testing.T) {
	workspace, _ := writeMigrateSource(t)
	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	for _, args := range [][]string{{"migrate"}, {"migrate", "other"}, {"migrate", "workspace", "extra"}, {"migrate", "workspace", "--apply", "--rollback", "x"}} {
		if err := dispatchCommand(args, streams); !isExit(err, exitFailed) {
			t.Fatalf("%q: %v", args, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".jad", "workspace.json")); !os.IsNotExist(err) {
		t.Fatal("a refused command wrote workspace.json")
	}
}

func TestMigrateWorkspaceExitsOneWhenBlocked(t *testing.T) {
	workspace, _ := writeMigrateSource(t)
	if err := os.WriteFile(filepath.Join(workspace, ".jad", "workspace.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	err := dispatchCommand([]string{"migrate", "workspace"}, streams)
	if !isExit(err, exitFailed) || !strings.Contains(stdout.String(), "[workspace_exists]") {
		t.Fatalf("blocked plan: %v\n%s", err, stdout.String())
	}
}

func TestMigrateWorkspaceApplyThenRollback(t *testing.T) {
	workspace, home := writeMigrateSource(t)
	var stdout, stderr bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := dispatchCommand([]string{"migrate", "workspace", "--apply"}, streams); err != nil {
		t.Fatalf("apply: %v\n%s", err, stdout.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "applied:") || strings.Contains(text, "cli-secret-value") {
		t.Fatalf("apply output:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".jad", "workspace.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".jad", "mcp", "web", "search.json")); err != nil {
		t.Fatal(err)
	}
	journals, _ := filepath.Glob(filepath.Join(workspace, ".jad", "migrations", "*.jsonl"))
	if len(journals) != 1 {
		t.Fatalf("journals: %v", journals)
	}

	// Applying again is blocked: an existing workspace is never changed.
	stdout.Reset()
	if err := dispatchCommand([]string{"migrate", "workspace", "--apply"}, streams); !isExit(err, exitFailed) || !strings.Contains(stdout.String(), "[workspace_exists]") {
		t.Fatalf("second apply: %v\n%s", err, stdout.String())
	}

	stdout.Reset()
	if err := dispatchCommand([]string{"migrate", "workspace", "--rollback", journals[0]}, streams); err != nil {
		t.Fatalf("rollback: %v\n%s", err, stdout.String())
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".jad", "workspace.json")); !os.IsNotExist(err) {
		t.Fatal("workspace.json is still there")
	}
	if _, err := os.Lstat(filepath.Join(home, ".jad", "mcp")); !os.IsNotExist(err) {
		t.Fatal("the folders the run made in G are still there")
	}
	if err := dispatchCommand([]string{"migrate", "workspace", "--rollback", filepath.Join(workspace, "nope.jsonl")}, streams); !isExit(err, exitFailed) {
		t.Fatalf("a missing journal: %v", err)
	}
}
