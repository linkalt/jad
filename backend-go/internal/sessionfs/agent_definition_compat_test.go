package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// agentDefinitionsGolden is the normalised form of every way an agent can be defined. The JSON of
// one NativeSubagentAgent is what subagentFingerprint hashes (with the agent's tool definitions),
// so a change here is a change of every resumed run's config note, and of what a child is given.
const agentDefinitionsGolden = "testdata/agent_definitions.txt"

// TestAgentDefinitionsLoadUnchanged pins the loader's output for inline agents (array and object
// tools, limits, compaction, isolation, a toolset and a model ID), agent folders (agent.json with
// a model file and prompt documents, a prompt.md alone) and the sample agents, all in one layout
// session. It is the compatibility contract of the unified agent runs phases R3 to R5 (design
// section 3.8): agent definitions keep loading to these values byte for byte. A rewrite
// (JAD_UPDATE_RUN_BASELINE=1) is a contract change and belongs to R6.
func TestAgentDefinitionsLoadUnchanged(t *testing.T) {
	sample, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "subagents"))
	if err != nil {
		t.Fatal(err)
	}
	// The session sandbox and the reader template give the sandbox cases: an agent sandbox is
	// narrowed by the session sandbox, and sandbox.isolation sets the agent's isolation fields.
	sessionBox := envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{workspace}", "{tmp}"}}, "hide_paths": []string{"{home}/.ssh"}})
	reader := envSandbox(map[string]any{"write": map[string]any{"paths": []string{"{tmp}"}}, "tools": []string{"exec", "lsp"}, "isolation": map[string]any{"mode": "none"}})
	fixture := layouttest.Write(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"reader": reader}, Settings: map[string]any{"sandbox": sessionBox}, Native: map[string]any{
		"exec":  map[string]any{},
		"async": map[string]any{},
		"subagent": map[string]any{
			"agents_dir": []string{".jad/agents", sample},
			"agents": map[string]any{
				"plain": map[string]any{},
				"listed": map[string]any{
					"description": "listed tools", "tools": []string{"exec", "async"}, "max_tool_rounds": 5,
					"timeout_ms": 2000, "auto_compaction": true, "compaction_trigger_tokens": 1000, "state_context": true,
				},
				"objected": map[string]any{
					"tools": map[string]any{"allow": []string{"exec"}, "deny": []string{"async"}}, "instructions": "be brief",
					"max_run_tokens": 5000, "isolation": "worktree",
				},
				"finder": map[string]any{"toolset": "extra/search", "model": "openai/default"},
				"boxed":  map[string]any{"tools": []string{"exec", "async"}, "sandbox": "reader"},
				"boxed_worktree": map[string]any{"tools": []string{"exec"}, "sandbox": map[string]any{"template": "reader",
					"isolation": map[string]any{"mode": "worktree", "base": "working", "materialize": "overlay", "share_paths": []string{"node_modules"}}}},
				"unboxed_worktree": map[string]any{"tools": []string{"exec"}, "sandbox": map[string]any{"enabled": false, "isolation": map[string]any{"mode": "worktree"}}},
			},
		},
	}})
	writeToolsetFile(t, fixture, "extra/search", `{"native":{"read_image":{},"skill":{}}}`)
	folders := filepath.Join(fixture.CWD, ".jad", "agents")
	writeAgentFile(t, filepath.Join(folders, "folded", "agent.json"),
		`{"schema_version":1,"description":"folder agent","model":"own.json","prompt_documents":["notes.md"],"tools":{"allow":["exec"]},"auto_compaction":false}`)
	writeAgentFile(t, filepath.Join(folders, "folded", "prompt.md"), "folder prompt\n")
	writeAgentFile(t, filepath.Join(folders, "folded", "notes.md"), "folder notes\n")
	writeAgentFile(t, filepath.Join(folders, "folded", "own.json"), `{"provider":"openai","model":"own"}`)
	writeAgentFile(t, filepath.Join(folders, "prompt_only", "prompt.md"), "prompt only\n")
	writeAgentFile(t, filepath.Join(folders, "folded_box", "agent.json"),
		`{"schema_version":1,"description":"folder sandbox","tools":["exec"],"isolation":"worktree","sandbox":{"enabled":true,"backend":"command","command":["env"],"write":{"paths":["{tmp}"]},"isolation":{"base":"working"}}}`)
	writeAgentFile(t, filepath.Join(folders, "folded_box", "prompt.md"), "boxed folder\n")

	config, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	session, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	if session.NativeSubagent == nil {
		t.Fatal("no sub-agent settings")
	}

	var out strings.Builder
	for _, agent := range session.NativeSubagent.Agents {
		encoded, err := json.MarshalIndent(agent, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("== " + agent.Name + "\n" + string(encoded) + "\n")
	}
	keys := make([]string, 0, len(session.AgentToolsets))
	for key := range session.AgentToolsets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out.WriteString("== agent toolsets\n" + strings.Join(keys, "\n") + "\n")

	got := strings.NewReplacer(sample, "<sample>", fixture.CWD, "<cwd>", fixture.Root, "<root>", fixture.GlobalDir, "<global>").Replace(out.String())
	if os.Getenv("JAD_UPDATE_RUN_BASELINE") == "1" {
		if err := os.WriteFile(agentDefinitionsGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(agentDefinitionsGolden)
	if err != nil {
		t.Fatalf("read %s (JAD_UPDATE_RUN_BASELINE=1 writes it): %v", agentDefinitionsGolden, err)
	}
	if got != string(want) {
		t.Fatalf("agent definitions changed (%s); a change here changes resumed runs' fingerprints.\n--- got ---\n%s", agentDefinitionsGolden, got)
	}
}

func writeAgentFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
