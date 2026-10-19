package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

var (
	smithCallIDPattern = regexp.MustCompile(`"call_id":"([^"]*)"`)
	smithNamePattern   = regexp.MustCompile(`"name":"([^"]*)"`)
)

func smithShortTempDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tsr")
	if err != nil {
		t.Skipf("no short temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
}

func smithInterpreter(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err == nil {
		return []string{"bun", "run", "{file}"}
	}
	if _, err := exec.LookPath("node"); err == nil {
		probe := filepath.Join(t.TempDir(), "probe.ts")
		if os.WriteFile(probe, []byte("const x: number = 1;\nconsole.log(x);\n"), 0o600) == nil {
			if out, err := exec.Command("node", probe).Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
				return []string{"node", "{file}"}
			}
		}
	}
	t.Skip("neither bun nor node (with TypeScript support) is installed")
	return nil
}

func smithChildSettings(t *testing.T, root string) sessionfs.NativeBashSettings {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	return sessionfs.NativeBashSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:            shell,
		TimeoutMs:        5_000,
		MaxTimeoutMs:     10_000,
		KillGraceMs:      100,
		KillProcessGroup: "always",
		MaxBytes:         2048,
		Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
		Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
		Spill:            sessionfs.NativeBashSpillSettings{Keep: "truncated_only", CapBytes: 1024, StorageDir: filepath.Join(root, ".bash-output")},
		MaxLineChars:     2000,
		StdinMaxBytes:    64 * 1024,
		StripANSI:        true,
		Env:              map[string]string{"NO_COLOR": "1", "GIT_TERMINAL_PROMPT": "0"},
		EnvDeny:          []string{},
	}
}

func runToolSmithThroughRunner(t *testing.T, tool tools.Tool, workspace, arguments string) (string, []string) {
	t.Helper()
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call := json.RawMessage(`{"type":"function_call","call_id":"call-smith","name":"tool_smith","arguments":` + string(quoted) + `}`)
	finalMessage := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		goldenResponseEvents("smith-response-1", call),
		goldenResponseEvents("smith-response-2", finalMessage),
	}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataFull)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"use tool_smith"`), RunOptions{
		TurnID:        "smith-turn",
		ToolCatalog:   catalog,
		ToolExecution: tools.ExecutionContext{WorkspaceRoot: workspace, SessionDir: workspace},
		MaxToolRounds: 1,
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(streamer.requests))
	}
	var records []string
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("transcript records = %d, want user, function_call, function_call_output, final assistant:\n%s", len(records), strings.Join(records, "\n"))
	}
	if !strings.HasPrefix(records[0], `{"type":"message","role":"user"`) ||
		!strings.HasPrefix(records[1], `{"type":"response.completed"`) || !strings.Contains(records[1], `"type":"function_call"`) ||
		!strings.HasPrefix(records[2], `{"type":"function_call_output","call_id":"call-smith"`) ||
		!strings.HasPrefix(records[3], `{"type":"response.completed"`) || !strings.Contains(records[3], `"type":"output_text"`) {
		t.Fatalf("unexpected record shapes:\n%s", strings.Join(records, "\n"))
	}
	outputs := 0
	for _, record := range records {
		if strings.Contains(record, `"type":"function_call_output"`) {
			outputs++
		}
		for _, match := range smithCallIDPattern.FindAllStringSubmatch(record, -1) {
			if match[1] != "call-smith" {
				t.Fatalf("record carries call id %q besides the outer call: %s", match[1], record)
			}
		}
		for _, match := range smithNamePattern.FindAllStringSubmatch(record, -1) {
			if match[1] != "tool_smith" {
				t.Fatalf("record carries tool name %q besides the outer call: %s", match[1], record)
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("function_call_output records = %d, want 1", outputs)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(records[2]), &output); err != nil {
		t.Fatal(err)
	}
	return output.Output, records
}

func TestRunnerToolSmithListPersistsOneFunctionCallOutput(t *testing.T) {
	workspace := t.TempDir()
	functions := filepath.Join(workspace, ".jad", "functions")
	if err := os.MkdirAll(functions, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha_fn", "beta_fn"} {
		source := fmt.Sprintf("// Describe %s\nexport async function %s(input: {a: string}) {\n  return 1;\n}\n", name, name)
		if err := os.WriteFile(filepath.Join(functions, name+".ts"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool, err := native.NewToolSmithTool(sessionfs.NativeToolSmithSettings{
		FunctionsDir: ".jad/functions", ListMaxResults: 50, ListMaxFiles: 200, ListHeaderBytes: 2048,
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	output, _ := runToolSmithThroughRunner(t, tool, workspace, `{"list":{}}`)
	if !strings.Contains(output, "alpha_fn") || !strings.Contains(output, "beta_fn") {
		t.Fatalf("list output lacks the two functions:\n%s", output)
	}
}

func TestRunnerToolSmithRunWithNestedReadPersistsOneFunctionCallOutput(t *testing.T) {
	smithShortTempDir(t)
	interpreter := smithInterpreter(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data.txt"), []byte("needle-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := native.NewReadTool(sessionfs.NativeReadSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, MaxBytes: 1024, MaxFileBytes: 4096, DefaultLimit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	nested := &native.ToolSmithNested{}
	nested.Bind([]tools.Tool{read}, nil)
	tool, err := native.NewToolSmithTool(sessionfs.NativeToolSmithSettings{
		Child:                smithChildSettings(t, workspace),
		CodeMaxBytes:         4096,
		ResultMaxBytes:       4096,
		NestedResultMaxBytes: 4096, NestedRequestMaxBytes: 65536,
		MaxNestedCalls: 100, MaxParallelCalls: 4,
		Tools:       []string{"read"},
		Interpreter: interpreter,
	}, "", nested)
	if err != nil {
		t.Fatal(err)
	}
	arguments := `{"run":{"function":"const r = await tools.read({ path: 'data.txt' });\nreturn { ok: r.ok, has: r.content.includes('needle-123') };"}}`
	output, _ := runToolSmithThroughRunner(t, tool, workspace, arguments)
	if !strings.Contains(output, `"has":true`) || !strings.Contains(output, "nested_calls: 1") {
		t.Fatalf("run output:\n%s", output)
	}
}
