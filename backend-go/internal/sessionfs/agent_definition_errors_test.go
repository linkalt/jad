package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentDefinitionErrorsGolden holds the exact refusal texts of agent definition keys, for an
// inline agent and for an agent.json folder. Both sources share checkAgentDefinitionKeys since
// unified agent runs R3; the golden was taken from the loader before that change, so the shared
// check keeps every text and the order in which a definition's keys are judged.
const agentDefinitionErrorsGolden = "testdata/agent_definition_errors.txt"

// TestAgentDefinitionKeyErrorsAreUnchanged pins the key refusals of both agent sources: the
// removed paths and read_only keys with their hints, an unknown key with the list of known keys,
// the first bad key winning when there are several, and schema_version (accepted in agent.json,
// an unknown key inline). A rewrite (JAD_UPDATE_RUN_BASELINE=1) changes what users read.
func TestAgentDefinitionKeyErrorsAreUnchanged(t *testing.T) {
	cases := []struct{ name, definition string }{
		{"paths", `{"description":"a","paths":["src"]}`},
		{"read_only", `{"read_only":true}`},
		{"unknown", `{"descripton":"a"}`},
		{"unknown_before_paths", `{"zzz":1,"paths":["src"]}`},
		{"read_only_before_unknown", `{"read_only":true,"zzz":1}`},
		{"schema_version_then_unknown", `{"schema_version":1,"zzz":1}`},
		{"schema_version_only", `{"schema_version":1,"description":"a"}`},
	}
	dir := t.TempDir()
	var out strings.Builder
	for _, test := range cases {
		inline := "ok"
		if _, err := normalizeNativeSubagentAgent(test.name, []byte(test.definition), filepath.Join(dir, "setting.json"), dir, dir); err != nil {
			inline = err.Error()
		}
		folder := filepath.Join(dir, "agents", test.name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "agent.json"), []byte(test.definition), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "prompt.md"), []byte("prompt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fromFolder := "ok"
		if _, err := loadSubagentFolder(test.name, folder); err != nil {
			fromFolder = err.Error()
		}
		out.WriteString("== " + test.name + "\ninline: " + inline + "\nfolder: " + fromFolder + "\n")
	}
	got := strings.ReplaceAll(out.String(), dir, "<dir>")
	if os.Getenv("JAD_UPDATE_RUN_BASELINE") == "1" {
		if err := os.WriteFile(agentDefinitionErrorsGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(agentDefinitionErrorsGolden)
	if err != nil {
		t.Fatalf("read %s (JAD_UPDATE_RUN_BASELINE=1 writes it): %v", agentDefinitionErrorsGolden, err)
	}
	if got != string(want) {
		t.Fatalf("agent definition key errors changed (%s).\n--- got ---\n%s", agentDefinitionErrorsGolden, got)
	}
}
