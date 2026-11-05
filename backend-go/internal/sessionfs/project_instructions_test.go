package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectInstructionsSettingsDefaultIsOff(t *testing.T) {
	settings, err := normalizeProjectInstructions(nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectInstructionsSettings{FileNames: []string{"AGENTS.md", "CLAUDE.md"}, StopAt: "git_root", MaxBytes: 65536, SubfolderMaxBytes: 16384}
	if !reflect.DeepEqual(settings, want) || settings.SubfoldersActive() {
		t.Fatalf("default = %+v", settings)
	}
	configured, err := normalizeProjectInstructions(json.RawMessage(`{"enabled":true,"file_names":["RULES.md","RULES.md","AGENTS.md"],"stop_at":"cwd","max_bytes":10,"subfolders":true,"subfolder_max_bytes":20}`), "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	want = ProjectInstructionsSettings{Enabled: true, FileNames: []string{"RULES.md", "AGENTS.md"}, StopAt: "cwd", MaxBytes: 10, Subfolders: true, SubfolderMaxBytes: 20}
	if !reflect.DeepEqual(configured, want) || !configured.SubfoldersActive() {
		t.Fatalf("configured = %+v", configured)
	}
}

func TestProjectInstructionsSettingsRefuseBadValues(t *testing.T) {
	for raw, want := range map[string]string{
		`[]`:                             "project_instructions",
		`{"extra":1}`:                    "project_instructions.extra is not supported",
		`{"enabled":"yes"}`:              "enabled must be a boolean",
		`{"stop_at":"home"}`:             "stop_at must be git_root, cwd or filesystem_root",
		`{"file_names":[]}`:              "file_names must be a list of 1 to 16 file names",
		`{"file_names":["a/AGENTS.md"]}`: "must be one file name",
		`{"file_names":[".."]}`:          "must be one file name",
		`{"max_bytes":0}`:                "max_bytes",
		`{"subfolder_max_bytes":-1}`:     "subfolder_max_bytes",
	} {
		if _, err := normalizeProjectInstructions(json.RawMessage(raw), "setting.json"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", raw, err, want)
		}
	}
}

func TestProjectInstructionsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties["project_instructions"].Properties
	if len(properties) != len(projectInstructionsKeys) {
		t.Fatalf("schema keys = %d, loader keys = %v", len(properties), projectInstructionsKeys)
	}
	defaults := DefaultProjectInstructionsSettings()
	want := map[string]any{"enabled": false, "subfolders": false, "stop_at": defaults.StopAt, "max_bytes": float64(defaults.MaxBytes), "subfolder_max_bytes": float64(defaults.SubfolderMaxBytes), "file_names": []any{"AGENTS.md", "CLAUDE.md"}}
	for _, key := range projectInstructionsKeys {
		field, ok := properties[key]
		if !ok {
			t.Errorf("schema has no project_instructions.%s", key)
			continue
		}
		if !reflect.DeepEqual(field["default"], want[key]) {
			t.Errorf("project_instructions.%s schema default = %v, loader = %v", key, field["default"], want[key])
		}
	}
	for _, key := range []string{"max_bytes", "subfolder_max_bytes"} {
		if properties[key]["maximum"] != float64(maxPromptDocumentBytes) {
			t.Errorf("project_instructions.%s maximum = %v", key, properties[key]["maximum"])
		}
	}
}

func writeInstructionFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverProjectInstructionsWalksUpToTheGitRoot(t *testing.T) {
	outside := t.TempDir()
	repository := filepath.Join(outside, "repo")
	cwd := filepath.Join(repository, "service", "api")
	writeInstructionFile(t, filepath.Join(outside, "AGENTS.md"), "ABOVE THE REPOSITORY")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeInstructionFile(t, filepath.Join(repository, "AGENTS.md"), "ROOT RULES\n")
	writeInstructionFile(t, filepath.Join(repository, "CLAUDE.md"), "ROOT CLAUDE")
	writeInstructionFile(t, filepath.Join(repository, "service", "CLAUDE.md"), "SERVICE RULES")
	writeInstructionFile(t, filepath.Join(cwd, "AGENTS.md"), "API RULES")
	settings := DefaultProjectInstructionsSettings()
	settings.Enabled = true

	text, paths, err := discoverProjectInstructions(settings, cwd, nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{filepath.Join(repository, "AGENTS.md"), filepath.Join(repository, "service", "CLAUDE.md"), filepath.Join(cwd, "AGENTS.md")}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %v", paths)
	}
	if strings.Contains(text, "ABOVE THE REPOSITORY") || strings.Contains(text, "ROOT CLAUDE") {
		t.Fatalf("text holds a file outside the repository or a second file of one folder:\n%s", text)
	}
	if !(strings.Index(text, "ROOT RULES") < strings.Index(text, "SERVICE RULES") && strings.Index(text, "SERVICE RULES") < strings.Index(text, "API RULES")) {
		t.Fatalf("files are not outermost first:\n%s", text)
	}
	if !strings.Contains(text, "# Project instructions: "+filepath.Join(cwd, "AGENTS.md")+"\n\nAPI RULES") {
		t.Fatalf("heading missing:\n%s", text)
	}

	// A file already read as a prompt document is not read again.
	_, paths, _ = discoverProjectInstructions(settings, cwd, []string{filepath.Join(repository, "AGENTS.md")}, "setting.json")
	if len(paths) != 2 {
		t.Fatalf("paths with a prompt document = %v", paths)
	}

	settings.StopAt = ProjectInstructionsStopCWD
	_, paths, _ = discoverProjectInstructions(settings, cwd, nil, "setting.json")
	if !reflect.DeepEqual(paths, []string{filepath.Join(cwd, "AGENTS.md")}) {
		t.Fatalf("cwd only = %v", paths)
	}
	settings.StopAt = ProjectInstructionsStopRoot
	text, _, _ = discoverProjectInstructions(settings, cwd, nil, "setting.json")
	if !strings.Contains(text, "ABOVE THE REPOSITORY") {
		t.Fatalf("filesystem_root did not reach above the repository:\n%s", text)
	}

	settings.Enabled = false
	if text, paths, _ := discoverProjectInstructions(settings, cwd, nil, "setting.json"); text != "" || paths != nil {
		t.Fatalf("disabled found %v", paths)
	}
}

func TestDiscoverProjectInstructionsWithoutAGitRootReadsOnlyCWDAndCutsAtTheLimit(t *testing.T) {
	parent := t.TempDir()
	cwd := filepath.Join(parent, "work")
	writeInstructionFile(t, filepath.Join(parent, "AGENTS.md"), "PARENT")
	writeInstructionFile(t, filepath.Join(cwd, "AGENTS.md"), "héllo world")
	settings := DefaultProjectInstructionsSettings()
	settings.Enabled = true
	settings.MaxBytes = 2
	text, paths, err := discoverProjectInstructions(settings, cwd, nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || strings.Contains(text, "PARENT") {
		t.Fatalf("paths = %v text = %s", paths, text)
	}
	// "hé" is three bytes; the cut keeps whole characters.
	if !strings.Contains(text, "\n\nh\n\n[") || !strings.Contains(text, "was cut at the project_instructions byte limit") {
		t.Fatalf("cut text = %q", text)
	}
}
