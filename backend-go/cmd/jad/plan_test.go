package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// TestRunPlanMode runs jad run with --plan and /plan against the real service and a
// stand-in provider, and checks what the provider was sent.
func TestRunPlanMode(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]json.RawMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		mu.Lock()
		requests = append(requests, fields)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"plan\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	var definitions []string
	for _, name := range []string{"read_image", "exec", "task"} {
		path, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, path)
	}
	fixture := useFixture(t, layouttest.Spec{
		ToolDefinitionPaths: definitions,
		Models:              map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}},
		Server:              map[string]any{"log_level": "off"},
		Native:              map[string]any{"read_image": map[string]any{}, "exec": map[string]any{}, "task": map[string]any{}},
		Settings:            map[string]any{"plan_mode": map[string]any{"instructions": "PLAN-ONLY", "allow_tools": []string{"task"}, "execute_prompt": "GO-AHEAD"}},
	})
	t.Setenv("FIXTURE_KEY", "k")
	session := "local:" + fixture.SessionID
	last := func() (tools []string, lastInput string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		request := requests[len(requests)-1]
		var listed []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(request["tools"], &listed)
		for _, tool := range listed {
			tools = append(tools, tool.Name)
		}
		sort.Strings(tools)
		var input []json.RawMessage
		_ = json.Unmarshal(request["input"], &input)
		return tools, string(input[len(input)-1])
	}

	stdout, stderr, err := runJad(t, "", "run", "--session", session, "--plan", "--format", "json", "add a cache")
	var result headless.Result
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || !result.Plan || result.Text != "plan" {
		t.Fatalf("--plan: %q %q %v", stdout, stderr, err)
	}
	tools, lastInput := last()
	if strings.Join(tools, ",") != "read_image,task" {
		t.Fatalf("plan-mode tools = %v", tools)
	}
	if !strings.Contains(lastInput, `"role":"developer"`) || !strings.Contains(lastInput, "PLAN-ONLY") {
		t.Fatalf("the last input item is not the plan prompt: %s", lastInput)
	}

	stdout, _, err = runJad(t, "", "run", "--session", session, "/plan", "and", "a", "test")
	if err != nil || stdout != "plan\n" {
		t.Fatalf("/plan: %q %v", stdout, err)
	}
	if tools, lastInput = last(); strings.Join(tools, ",") != "read_image,task" || !strings.Contains(lastInput, "PLAN-ONLY") {
		t.Fatalf("/plan request: %v %s", tools, lastInput)
	}

	if _, _, err = runJad(t, "", "run", "--session", session, "do it"); err != nil {
		t.Fatal(err)
	}
	tools, lastInput = last()
	if strings.Join(tools, ",") != "exec,read_image,task" || strings.Contains(lastInput, "PLAN-ONLY") {
		t.Fatalf("a run without plan mode: %v %s", tools, lastInput)
	}
	data, err := os.ReadFile(filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PLAN-ONLY") || !strings.Contains(string(data), `"and a test"`) {
		t.Fatalf("transcript:\n%s", data)
	}

	// --tools narrows the run's tools; an entry the profile does not have narrows nothing more.
	if _, _, err = runJad(t, "", "run", "--session", session, "--tools", "read_image, exec,mcp:docs:*", "look"); err != nil {
		t.Fatal(err)
	}
	if tools, _ = last(); strings.Join(tools, ",") != "exec,read_image" {
		t.Fatalf("--tools: %v", tools)
	}

	// --execute-plan sends the configured prompt, then the prompt given, without plan mode,
	// and stores it as the user message.
	stdout, stderr, err = runJad(t, "", "run", "--session", session, "--execute-plan", "--format", "json")
	result = headless.Result{}
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || !result.ExecutePlan || result.Plan {
		t.Fatalf("--execute-plan: %q %q %v", stdout, stderr, err)
	}
	if tools, lastInput = last(); strings.Join(tools, ",") != "exec,read_image,task" || !strings.Contains(lastInput, "GO-AHEAD") {
		t.Fatalf("--execute-plan request: %v %s", tools, lastInput)
	}
	if _, _, err = runJad(t, "", "run", "--session", session, "--execute-plan", "skip", "the", "docs"); err != nil {
		t.Fatal(err)
	}
	if _, lastInput = last(); !strings.Contains(lastInput, `GO-AHEAD\n\nskip the docs`) {
		t.Fatalf("--execute-plan with text: %s", lastInput)
	}
	data, err = os.ReadFile(filepath.Join(fixture.SessionStateDir, "dev", "coder", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"GO-AHEAD"`) || !strings.Contains(string(data), `"GO-AHEAD\n\nskip the docs"`) {
		t.Fatalf("transcript after --execute-plan:\n%s", data)
	}

	for name, args := range map[string][]string{
		"plan and execute":    {"run", "--session", session, "--plan", "--execute-plan"},
		"unknown tool":        {"run", "--session", session, "--tools", "rm", "x"},
		"plan with an action": {"run", "--session", session, "--plan", "/reset"},
		"plan with nothing":   {"run", "--session", session, "/plan"},
		"plan then compact":   {"run", "--session", session, "/plan", "/compact"},
	} {
		if _, _, err := runJad(t, "", args...); exitCode(err) != exitUsage {
			t.Errorf("%s: err = %v, want exit %d", name, err, exitUsage)
		}
	}
}
