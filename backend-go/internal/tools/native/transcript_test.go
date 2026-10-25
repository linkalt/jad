package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleTranscriptManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "transcript.json")

func TestBuildCatalogLoadsTranscriptFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleTranscriptManifest},
		NativeTranscript:    &sessionfs.NativeTranscriptSettings{AllowChildren: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != transcriptToolID || definitions[0].Name != "transcript" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestBuildCatalogNeedsTheTranscriptManifest(t *testing.T) {
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(t.TempDir(), "setting.json"), NativeTranscript: &sessionfs.NativeTranscriptSettings{}})
	if err == nil || !strings.Contains(err.Error(), "tools.native.transcript is configured but native.transcript is missing from tool_definition_paths") {
		t.Fatalf("error = %v", err)
	}
}

func TestTranscriptListShowsTheOwnFiles(t *testing.T) {
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(messages, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, err := NewTranscriptTool(sessionfs.NativeTranscriptSettings{AllowChildren: true}, TranscriptFiles{Messages: messages, Backup: filepath.Join(dir, "messages.backup.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(t.Context(), map[string]any{"list": true}, tools.ExecutionContext{CallID: "c1"})
	if err != nil || !strings.Contains(out, "messages  3 bytes") || strings.Contains(out, "backup") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestTranscriptRefusesAChildWhenAllowChildrenIsOff(t *testing.T) {
	tool, err := NewTranscriptTool(sessionfs.NativeTranscriptSettings{}, TranscriptFiles{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(t.Context(), map[string]any{}, tools.ExecutionContext{CallID: "c1", AgentDepth: 1})
	if err != nil || !strings.Contains(out, "transcript_children_denied") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestTranscriptIsAReadOnlyToolSoAnAgentCanListIt(t *testing.T) {
	tool, err := NewTranscriptTool(sessionfs.NativeTranscriptSettings{}, TranscriptFiles{})
	if err != nil || !tool.ReadOnly {
		t.Fatalf("tool = %+v, err = %v", tool, err)
	}
}
