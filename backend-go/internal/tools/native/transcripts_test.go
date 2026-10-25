package native

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleTranscriptsManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "transcripts.json")

func transcriptsTestSettings() sessionfs.NativeTranscriptsSettings {
	return sessionfs.NativeTranscriptsSettings{MaxEntries: 50, ScanMaxEntries: 2000, TimeoutMs: 30000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
}

func callTranscripts(t *testing.T, tool tools.Tool, arguments string, execution tools.ExecutionContext) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		t.Fatal(err)
	}
	if execution.CallID == "" {
		execution.CallID = "call_1"
	}
	out, err := tool.Execute(context.Background(), parsed, execution)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildCatalogLoadsTranscriptsFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleTranscriptsManifest},
		NativeTranscripts:   &sessionfs.NativeTranscriptsSettings{MaxEntries: 5, ScanMaxEntries: 5, TimeoutMs: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != transcriptsToolID || definitions[0].Name != "transcripts" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestBuildCatalogNeedsTheTranscriptsManifest(t *testing.T) {
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(t.TempDir(), "setting.json"), NativeTranscripts: &sessionfs.NativeTranscriptsSettings{}})
	if err == nil || !strings.Contains(err.Error(), "tools.native.transcripts is configured but native.transcripts is missing from tool_definition_paths") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseTranscriptsPath(t *testing.T) {
	good := map[string]transcriptsLevel{
		"":                       {kind: "profiles"},
		"sessions":               {kind: "sessions"},
		"session:demo":           {kind: "profiles", session: "demo"},
		"profile:dev/coder":      {kind: "runs", profile: "dev/coder"},
		"profile:demo/dev/coder": {kind: "runs", session: "demo", profile: "dev/coder"},
	}
	for path, want := range good {
		got, ok := parseTranscriptsPath(path)
		if !ok || got != want || got.text() != path {
			t.Errorf("%q: got %+v ok=%v text=%q", path, got, ok, got.text())
		}
	}
	for _, path := range []string{"session:", "session:a/b", "session:..", "profile:dev", "profile:a/b/c/d", "profile:a/../b", "profile:a//b", "subagent:sub_00000000", "path:/etc", "nope", "sessions:x", "profile:a\\b/c", "session:a\x00"} {
		if _, ok := parseTranscriptsPath(path); ok {
			t.Errorf("%q was accepted", path)
		}
	}
}

func TestTranscriptsRejectsBadInputNamingTheCause(t *testing.T) {
	tool, err := NewTranscriptsTool(transcriptsTestSettings(), TranscriptFiles{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if issues, err := tool.Validator.Validate([]byte(`{"nope":1}`)); err != nil || len(issues) == 0 || !strings.Contains(fmt.Sprint(issues), "nope") {
		t.Fatalf("unknown key issues = %v, err = %v", issues, err)
	}
	execution := tools.ExecutionContext{SessionKey: "demo\x00dev/coder"}
	cursor := encodeTranscriptsCursor(transcriptsCursor{Level: "sessions"})
	for arguments, want := range map[string]string{
		`{"path":"subagent:sub_00000000"}`:              "invalid_path",
		`{"path":"session:a/b"}`:                        "invalid_path",
		`{"status":"done"}`:                             "status only applies",
		`{"path":"sessions","status":"done"}`:           "status only applies",
		`{"cursor":"!!"}`:                               "invalid_cursor",
		`{"cursor":"` + cursor + `","path":"sessions"}`: "conflicting_selectors",
		`{"cursor":"` + cursor + `","filters":["a"]}`:   "conflicting_selectors",
	} {
		if out := callTranscripts(t, tool, arguments, execution); !strings.Contains(out, want) {
			t.Errorf("%s: %s", arguments, out)
		}
	}
}

func TestTranscriptsAccessSwitchesGateTheLevel(t *testing.T) {
	settings := transcriptsTestSettings()
	settings.AllowOtherSessions, settings.AllowOtherProfiles = false, false
	tool, _ := NewTranscriptsTool(settings, TranscriptFiles{}, nil, nil, nil)
	execution := tools.ExecutionContext{SessionKey: "demo\x00dev/coder"}
	for arguments, want := range map[string]string{
		`{"path":"session:elsewhere"}`:       "transcripts_session_denied",
		`{"path":"profile:elsewhere/dev/x"}`: "transcripts_session_denied",
		`{"path":"profile:dev/other"}`:       "transcripts_profile_denied",
	} {
		if out := callTranscripts(t, tool, arguments, execution); !strings.Contains(out, want) {
			t.Errorf("%s: %s", arguments, out)
		}
	}
	child := execution
	child.AgentDepth = 1
	settings.AllowChildren = false
	tool, _ = NewTranscriptsTool(settings, TranscriptFiles{}, nil, nil, nil)
	if out := callTranscripts(t, tool, `{}`, child); !strings.Contains(out, "transcripts_children_denied") {
		t.Errorf("child: %s", out)
	}
}

func TestTranscriptsCursorRoundTrip(t *testing.T) {
	want := transcriptsCursor{Level: "profile:dev/coder", Filters: []string{"a*"}, Status: "done", Activity: 42, Key: "sub_00000001"}
	got, ok := decodeTranscriptsCursor(encodeTranscriptsCursor(want))
	if !ok || got.Level != want.Level || got.Status != want.Status || got.Activity != 42 || got.Key != want.Key || len(got.Filters) != 1 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}
