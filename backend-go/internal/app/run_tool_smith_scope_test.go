package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

// appToolSmithInterpreter is the TypeScript interpreter tool_smith runs scripts with here, bun or
// a node that runs TypeScript; the test is skipped without one.
func appToolSmithInterpreter(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err == nil {
		return `["bun","run","{file}"]`
	}
	if _, err := exec.LookPath("node"); err == nil {
		probe := filepath.Join(t.TempDir(), "probe.ts")
		if os.WriteFile(probe, []byte("const x: number = 1;\nconsole.log(x);\n"), 0o600) == nil {
			if out, err := exec.Command("node", probe).Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
				return `["node","{file}"]`
			}
		}
	}
	t.Skip("no TypeScript interpreter (bun, or node with type stripping)")
	return ""
}

// A tool_smith script reaches only the tools its own run may use (U3 follow-up): a run whose
// request names tools (RunOptions.Tools, the run route's tools) gives its scripts no other, and the
// script is told the tool is not allowed. Without a request list the profile's tools stay reachable,
// as before; a list that names the tool keeps it.
func TestToolSmithScriptReachesOnlyTheToolsTheRunsRequestNamed(t *testing.T) {
	interpreter := appToolSmithInterpreter(t)
	for name, tc := range map[string]struct {
		only []string
		want string
	}{
		"no request list":     {nil, "skill=ok"},
		"list without skill":  {[]string{"tool_smith"}, "skill=invalid_input:tool skill not allowed; allowed: "},
		"list that names it":  {[]string{"tool_smith", "skill"}, "skill=ok"},
		"list by entry match": {[]string{"tool_smith", "mcp:docs:*"}, "skill=invalid_input"},
	} {
		t.Run(name, func(t *testing.T) {
			f := toolSmithFixture(t)
			f.overwrite(t, "tools/coding/none.json", `{"native":{"skill":{},"async":{},"tool_smith":{"interpreter":`+interpreter+`,"tools":["skill"]}},"mcp":{}}`)
			f.captureBodies = true
			f.script = scriptFunctionCall("tool_smith", map[string]any{"code": "const r = await tools.skill({}); return 'skill=' + r.status + (r.status === 'ok' ? '' : ':' + r.summary);"})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := boundRunnerOf(t, f, "dev/coder").Run(ctx, json.RawMessage(`"go"`), session.RunOptions{Tools: tc.only}); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			last := f.bodies[len(f.bodies)-1]
			f.mu.Unlock()
			var request struct {
				Input []struct {
					Type   string `json:"type"`
					Output string `json:"output"`
				} `json:"input"`
			}
			if err := json.Unmarshal([]byte(last), &request); err != nil {
				t.Fatal(err)
			}
			output := ""
			for _, item := range request.Input {
				if item.Type == "function_call_output" {
					output = item.Output
				}
			}
			if !strings.Contains(output, tc.want) {
				t.Fatalf("the script's skill call: want %q in\n%s", tc.want, output)
			}
		})
	}
}

// toolSmithFixture is the layout fixture with the sample tool_smith definition in its library,
// which lists only the tools the other app tests use.
func toolSmithFixture(t *testing.T) *layoutFixture {
	t.Helper()
	definition, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", "tool_smith.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newLayoutFixtureWithBudget(t, nil, nil, func(config *Config) {
		for _, root := range config.Roots {
			if root.Layout != nil {
				root.Layout.ToolDefinitionPaths = append(root.Layout.ToolDefinitionPaths, definition)
			}
		}
	})
}
