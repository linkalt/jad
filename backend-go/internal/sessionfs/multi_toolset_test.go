package sessionfs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func imageToolset(maxResults string, more string) string {
	return `{"native":{"read_image":{"max_image_bytes":` + maxResults + `}` + more + `}}`
}

func loadFixtureSession(t *testing.T, fixture layouttest.Fixture) SessionConfig {
	t.Helper()
	config, err := Load(LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	loaded, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionSettings: %v", err)
	}
	return loaded
}

func TestProfileToolsetsMergeTheirNativeTools(t *testing.T) {
	config, err := loadToolsetSession(t, profileSpec([]string{"a/one", "b/two"}), map[string]string{
		"a/one": imageToolset("11", ""),
		"b/two": `{"native":{"skill":{},"request_draft":{}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeReadImage == nil || config.NativeSkill == nil || config.NativeRequestDraft == nil {
		t.Errorf("read_image %v skill %v request_draft %v, want all three", config.NativeReadImage != nil, config.NativeSkill != nil, config.NativeRequestDraft != nil)
	}
	if len(config.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", config.Warnings)
	}
}

func TestProfileToolsetsFirstNativeToolWinsWithAWarning(t *testing.T) {
	files := map[string]string{"a/one": imageToolset("11", ""), "b/two": imageToolset("22", `,"skill":{}`)}
	config, err := loadToolsetSession(t, profileSpec([]string{"a/one", "b/two"}), files)
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeReadImage == nil || config.NativeReadImage.MaxImageBytes != 11 || config.NativeSkill == nil {
		t.Fatalf("read_image = %+v skill %v, want the first's read_image (11) and the second's skill", config.NativeReadImage, config.NativeSkill != nil)
	}
	if len(config.Warnings) != 1 || !strings.Contains(config.Warnings[0], "read_image") || !strings.Contains(config.Warnings[0], "a/one") || !strings.Contains(config.Warnings[0], "b/two") {
		t.Errorf("Warnings = %v, want one naming read_image, a/one and b/two", config.Warnings)
	}

	config, err = loadToolsetSession(t, profileSpec([]string{"b/two", "a/one"}), files)
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeReadImage == nil || config.NativeReadImage.MaxImageBytes != 22 {
		t.Errorf("reversed: read_image = %+v, want the first listed (22)", config.NativeReadImage)
	}
}

func TestProfileToolsetsSameMCPServerKeepsTheFirst(t *testing.T) {
	spec := profileSpec([]string{"a/one", "b/two"})
	spec.MCP = map[string]map[string]any{"srv/files": {}}
	config, err := loadToolsetSession(t, spec, map[string]string{
		"a/one": `{"mcp":{"srv/files":{"deny_tools":["rm"]}}}`,
		"b/two": `{"native":{"skill":{}},"mcp":{"srv/files":{}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want one", config.MCPServers)
	}
	server := config.MCPServers[0]
	if server.ID != "files" || !reflect.DeepEqual(server.DenyTools, []string{"rm"}) {
		t.Errorf("server = %+v, want the first toolset's use", server)
	}
	if len(config.Warnings) != 1 || !strings.Contains(config.Warnings[0], "files") || !strings.Contains(config.Warnings[0], "a/one") || !strings.Contains(config.Warnings[0], "b/two") {
		t.Errorf("Warnings = %v, want one naming files, a/one and b/two", config.Warnings)
	}
}

func TestProfileToolsetsHooksOfTheFirstWin(t *testing.T) {
	second := strings.Replace(hooksToolsetText, `"timeout_ms":5000`, `"timeout_ms":9000`, 1)
	files := map[string]string{"a/one": hooksToolsetText, "b/two": second}
	config, err := loadToolsetSession(t, profileSpec([]string{"a/one", "b/two"}), files)
	if err != nil {
		t.Fatal(err)
	}
	if config.Hooks.TimeoutMS != 5000 {
		t.Errorf("hooks timeout = %d, want the first's 5000", config.Hooks.TimeoutMS)
	}
	found := 0
	for _, warning := range config.Warnings {
		if strings.Contains(warning, "hooks") && strings.Contains(warning, "a/one") && strings.Contains(warning, "b/two") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("Warnings = %v, want exactly one naming hooks, a/one and b/two", config.Warnings)
	}
}

func TestProfileToolsetsHooksInSettingsAndALaterToolsetAreAnError(t *testing.T) {
	spec := profileSpec([]string{"a/one", "b/two"})
	spec.Settings = map[string]any{"hooks": map[string]any{"enabled": true}}
	_, err := loadToolsetSession(t, spec, map[string]string{"a/one": grepToolsetText, "b/two": hooksToolsetText})
	if err == nil || !strings.Contains(err.Error(), "hooks is set here and in the settings") {
		t.Fatalf("err = %v, want the both-places error", err)
	}
}

func TestOneToolsetGivesTheSameConfigAsAString(t *testing.T) {
	fixture := layouttest.Write(t, profileSpec("a/one"))
	writeToolsetFile(t, fixture, "a/one", imageToolset("11", `,"request_draft":{}`))
	fromString := loadFixtureSession(t, fixture)

	profile := filepath.Join(fixture.Root, "profiles", "dev", "coder.json")
	text := `{"models":["openai/default"],"model_default":"openai/default","toolset":["a/one"]}`
	if err := os.WriteFile(profile, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	fromArray := loadFixtureSession(t, fixture)

	if fromString.Warnings != nil || fromArray.Warnings != nil {
		t.Errorf("Warnings = %v and %v, want nil", fromString.Warnings, fromArray.Warnings)
	}
	if !reflect.DeepEqual(fromString, fromArray) {
		t.Errorf("a one-element array loads differently from the string:\n%+v\n%+v", fromString, fromArray)
	}
}

func TestSubagentToolsetsMergeUnderTheCompositeKey(t *testing.T) {
	agent := map[string]any{"toolset": []string{"a/one", "b/two"}}
	config, err := loadToolsetSession(t, toolsetSpec(agent), map[string]string{
		"a/one": imageToolset("11", ""),
		"b/two": imageToolset("22", `,"skill":{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	derived, ok := config.AgentToolsets["a/one+b/two"]
	if !ok || len(config.AgentToolsets) != 1 {
		t.Fatalf("AgentToolsets = %v, want only a/one+b/two", config.AgentToolsets)
	}
	if derived.NativeReadImage == nil || derived.NativeReadImage.MaxImageBytes != 11 || derived.NativeSkill == nil || derived.NativeRequestDraft != nil {
		t.Errorf("merged tools: read_image %+v skill %v request_draft %v", derived.NativeReadImage, derived.NativeSkill != nil, derived.NativeRequestDraft != nil)
	}
	if derived.Warnings != nil {
		t.Errorf("derived Warnings = %v, want nil", derived.Warnings)
	}
	if len(config.Warnings) != 1 || !strings.Contains(config.Warnings[0], "read_image") {
		t.Errorf("Warnings = %v, want one naming read_image", config.Warnings)
	}
}

func TestSubagentsWithTheSameListLoadItOnceAndWarnOnce(t *testing.T) {
	spec := toolsetSpec(nil)
	spec.Native["subagent"] = map[string]any{"agents": map[string]any{
		"one":   map[string]any{"toolset": []string{"a/one", "b/two"}},
		"two":   map[string]any{"toolset": []string{"a/one", "b/two"}},
		"three": map[string]any{"toolset": []string{"a/one", "b/two", "c/three"}},
	}}
	config, err := loadToolsetSession(t, spec, map[string]string{
		"a/one":   imageToolset("11", ""),
		"b/two":   imageToolset("22", ""),
		"c/three": globToolsetText,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.AgentToolsets) != 2 {
		t.Errorf("AgentToolsets has %d entries, want 2", len(config.AgentToolsets))
	}
	if len(config.Warnings) != 1 {
		t.Errorf("Warnings = %v, want one", config.Warnings)
	}
}

func TestSubagentToolsetListWithAnUnknownSecondIDIsAnError(t *testing.T) {
	agent := map[string]any{"toolset": []string{"a/one", "b/missing"}}
	_, err := loadToolsetSession(t, toolsetSpec(agent), map[string]string{"a/one": grepToolsetText})
	if err == nil || !strings.Contains(err.Error(), "agent finder: toolset: ") || !strings.Contains(err.Error(), "b/missing") {
		t.Fatalf("err = %v, want an agent finder toolset error naming b/missing", err)
	}
}

// resources and prompts in a toolset's mcp entry reach the server reference; a typo or a
// value that is not a boolean is refused by the strict toolset reader.
func TestToolsetMCPEntryTurnsOnResourcesAndPrompts(t *testing.T) {
	spec := profileSpec([]string{"a/one"})
	spec.MCP = map[string]map[string]any{"srv/files": {}}
	config, err := loadToolsetSession(t, spec, map[string]string{
		"a/one": `{"mcp":{"srv/files":{"resources":true,"prompts":true}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.MCPServers) != 1 || !config.MCPServers[0].Resources || !config.MCPServers[0].Prompts {
		t.Fatalf("MCPServers = %+v, want files with resources and prompts", config.MCPServers)
	}
	for _, text := range []string{
		`{"mcp":{"srv/files":{"resource":true}}}`,
		`{"mcp":{"srv/files":{"prompts":"yes"}}}`,
	} {
		if _, err := loadToolsetSession(t, spec, map[string]string{"a/one": text}); err == nil {
			t.Errorf("toolset %s was accepted", text)
		}
	}
}

// The toolset schema accepts the same mcp entry keys as the loader.
func TestToolsetSchemaMCPEntryMatchesTheLoader(t *testing.T) {
	schema := compileSchema(t, "toolset.schema.json")
	for text, want := range map[string]bool{
		`{"mcp":{"srv/files":{"resources":true,"prompts":false}}}`:   true,
		`{"mcp":{"srv/files":{"enabled":false,"deny_tools":["x"]}}}`: true,
		`{"mcp":{"srv/files":{"resource":true}}}`:                    false,
		`{"mcp":{"srv/files":{"prompts":"yes"}}}`:                    false,
		`{"mcp":{"srv/files":{"token_path":"x"}}}`:                   false,
	} {
		if got := schemaAccepts(t, schema, text); got != want {
			t.Errorf("toolset.schema.json accepts %s = %v, want %v", text, got, want)
		}
	}
}
