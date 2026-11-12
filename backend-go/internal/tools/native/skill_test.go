package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// skillFixture lays out a global root with two skill directories, like the
// TypeScript skill-tool tests: the earlier directory wins a name clash.
type skillFixture struct {
	root, first, second string
	tool                tools.Tool
}

func newSkillFixture(t *testing.T, settings sessionfs.NativeSkillSettings) skillFixture {
	t.Helper()
	root := t.TempDir()
	fixture := skillFixture{root: root, first: filepath.Join(root, "skills-a"), second: filepath.Join(root, "skills-b")}
	for _, directory := range []string{fixture.first, fixture.second} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeSkillRootSetting(t, root, `{"libraries":{"skill_dirs":["skills-a","skills-b"]}}`)
	tool, err := NewSkillTool(settings, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.tool = tool
	return fixture
}

func writeSkillRootSetting(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "setting.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func addSkill(t *testing.T, directory, name, description, body string, folder ...string) {
	t.Helper()
	folderName := name
	if len(folder) > 0 {
		folderName = folder[0]
	}
	if err := os.MkdirAll(filepath.Join(directory, folderName), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(directory, folderName, "SKILL.md"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run calls the tool the way the catalog does: normalize, validate, execute.
func (f skillFixture) run(t *testing.T, arguments string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded = f.tool.NormalizeArguments(decoded)
	output, err := f.tool.Execute(context.Background(), decoded, tools.ExecutionContext{CallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func skillStatus(t *testing.T, output string) string {
	t.Helper()
	var header struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(output, "\n", 2)[0]), &header); err != nil {
		t.Fatalf("result header: %v\n%s", err, output)
	}
	return header.Status
}

func skillFields(output string) map[string]string {
	fields := map[string]string{}
	inInformation := false
	for _, line := range strings.Split(output, "\n") {
		switch {
		case line == "--- INFORMATION ---":
			inInformation = true
		case line == "" && inInformation:
			return fields
		case inInformation:
			key, value, _ := strings.Cut(line, ": ")
			fields[key] = value
		}
	}
	return fields
}

func TestSkillToolListsSkillsWithDescriptions(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "summarize", "Summarize a document.", "Body one.")
	addSkill(t, f.first, "agent-tool-design", "Design tools for agents.", "Body two.")
	output := f.run(t, `{}`)
	if skillStatus(t, output) != "ok" || skillFields(output)["skills"] != "2" {
		t.Fatalf("list output = %s", output)
	}
	if !strings.Contains(output, "agent-tool-design: Design tools for agents.\nsummarize: Summarize a document.") {
		t.Fatalf("skills are not listed by name with their descriptions:\n%s", output)
	}
}

func TestSkillToolEmptyListIsNotAnError(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	output := f.run(t, `{}`)
	if skillStatus(t, output) != "ok" || skillFields(output)["skills"] != "0" || strings.Contains(output, "--- CONTENT") {
		t.Fatalf("empty list output = %s", output)
	}
}

func TestSkillToolReturnsInstructionsWithoutFrontmatter(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "summarize", "Summarize a document.", "# Summarize\n\nKeep it short.")
	output := f.run(t, `{"name":"summarize"}`)
	fields := skillFields(output)
	if skillStatus(t, output) != "ok" || fields["name"] != "summarize" || fields["directory"] != filepath.Join(f.first, "summarize") {
		t.Fatalf("load output = %s", output)
	}
	if !strings.Contains(output, "# Summarize\n\nKeep it short.") || strings.Contains(output, "description: Summarize a document.") {
		t.Fatalf("instructions were not returned without frontmatter:\n%s", output)
	}
}

func TestSkillToolAcceptsHarmlessNameVariants(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "agent-tool-design", "Design tools.", "Body.")
	for _, variant := range []string{"Agent_Tool_Design", "  agent tool design  ", "AGENT-TOOL-DESIGN"} {
		encoded, _ := json.Marshal(map[string]string{"name": variant})
		output := f.run(t, string(encoded))
		if skillStatus(t, output) != "ok" || skillFields(output)["name"] != "agent-tool-design" {
			t.Fatalf("variant %q did not load the skill:\n%s", variant, output)
		}
	}
	for _, arguments := range []map[string]any{{"name": "   "}, {}} {
		if got := normalizeSkillArguments(arguments); len(got) != 0 {
			t.Fatalf("normalizeSkillArguments(%v) = %v, want no name", arguments, got)
		}
	}
	// JavaScript's \s includes U+FEFF and excludes U+0085; so does the port.
	if got := normalizeSkillArguments(map[string]any{"name": "\ufeffa\u00a0\u2003b__c\u0085"})["name"]; got != "a-b-c\u0085" {
		t.Fatalf("whitespace handling differs from JavaScript: %q", got)
	}
}

func TestSkillToolUnknownNameListsValidNames(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "summarize", "Summarize.", "Body.")
	addSkill(t, f.first, "review", "Review.", "Body.")
	output := f.run(t, `{"name":"summarise"}`)
	fields := skillFields(output)
	if skillStatus(t, output) != "not_found" || fields["error_code"] != "skill_not_found" || fields["available"] != "review, summarize" || fields["skill"] != "summarise" {
		t.Fatalf("not-found output = %s", output)
	}
	if !strings.Contains(output, `"summary":"Skill summarise was not found"`) {
		t.Fatalf("summary = %s", output)
	}
	empty := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	if got := skillFields(empty.run(t, `{"name":"x"}`))["available"]; got != "none" {
		t.Fatalf("available with no skills = %q, want none", got)
	}
}

func TestSkillToolEarlierDirectoryWins(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "summarize", "From project.", "Project body.")
	addSkill(t, f.second, "summarize", "From shared.", "Shared body.")
	addSkill(t, f.second, "only-shared", "Only shared.", "Other body.")
	loaded := f.run(t, `{"name":"summarize"}`)
	if !strings.Contains(loaded, "Project body.") || strings.Contains(loaded, "Shared body.") {
		t.Fatalf("the later directory won:\n%s", loaded)
	}
	listed := f.run(t, `{}`)
	if skillFields(listed)["skills"] != "2" || !strings.Contains(listed, "summarize: From project.") || strings.Contains(listed, "From shared.") {
		t.Fatalf("list output = %s", listed)
	}
}

func TestSkillToolHidesSkillWhoseFolderDoesNotMatchItsName(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "real-name", "Mismatched.", "Hidden body.", "other-folder")
	if got := skillFields(f.run(t, `{}`))["skills"]; got != "0" {
		t.Fatalf("mismatched skill was listed: %s", got)
	}
	for _, name := range []string{"real-name", "other-folder"} {
		if status := skillStatus(t, f.run(t, `{"name":"`+name+`"}`)); status != "not_found" {
			t.Fatalf("name %q status = %s, want not_found", name, status)
		}
	}
}

func TestSkillToolNameCannotLeaveTheSkillDirectories(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.root, "outside", "Not in a skill directory.", "Secret body.")
	addSkill(t, f.first, "inside", "Inside.", "Body.")
	for _, name := range []string{"../outside", "..", "inside/../../outside", "/etc/passwd"} {
		encoded, _ := json.Marshal(map[string]string{"name": name})
		output := f.run(t, string(encoded))
		if skillStatus(t, output) != "not_found" || strings.Contains(output, "Secret body.") {
			t.Fatalf("name %q reached outside the skill directories:\n%s", name, output)
		}
	}
	// A name over the 64-unit limit is refused without touching the disk.
	long := strings.Repeat("a", 65)
	addSkill(t, f.first, long, "Too long.", "Body.")
	if status := skillStatus(t, f.run(t, `{"name":"`+long+`"}`)); status != "not_found" {
		t.Fatalf("a 65-character name status = %s, want not_found", status)
	}
}

func TestSkillToolCutsAtMaxBytesOnACharacterBoundary(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 11})
	// Each "é" is two bytes, so a cut at 11 bytes would land inside a character.
	addSkill(t, f.first, "long", "Long.", strings.Repeat("é", 13))
	output := f.run(t, `{"name":"long"}`)
	fields := skillFields(output)
	if skillStatus(t, output) != "ok" || fields["truncated"] != "true" || fields["content_bytes"] != "26" {
		t.Fatalf("cut output = %s", output)
	}
	if !strings.Contains(output, strings.Repeat("é", 5)+"\n") || strings.Contains(output, strings.Repeat("é", 6)) || strings.Contains(output, "\ufffd") {
		t.Fatalf("the cut did not land on a character boundary:\n%s", output)
	}
}

func TestSkillToolWholeSkillWithinMaxBytesHasNoTruncationMarker(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 1000})
	addSkill(t, f.first, "short", "Short.", "Short body.")
	if _, exists := skillFields(f.run(t, `{"name":"short"}`))["truncated"]; exists {
		t.Fatal("a skill within max_bytes was marked truncated")
	}
}

func TestSkillToolCutsTheListToo(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 20})
	addSkill(t, f.first, "alpha", "A long enough description.", "Body.")
	addSkill(t, f.first, "beta", "Another description.", "Body.")
	output := f.run(t, `{}`)
	fields := skillFields(output)
	if fields["skills"] != "2" || fields["truncated"] != "true" {
		t.Fatalf("list cut output = %s", output)
	}
}

func TestSkillToolListCollapsesDescriptionWhitespace(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	if err := os.MkdirAll(filepath.Join(f.first, "folded"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "---\nname: folded\ndescription: |\n  line one\n  line two\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(f.first, "folded", "SKILL.md"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := f.run(t, `{}`); !strings.Contains(output, "folded: line one line two\n") {
		t.Fatalf("description whitespace was not collapsed:\n%s", output)
	}
}

func TestSkillToolRereadsDirectoriesOnEveryCall(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "one", "First.", "Body.")
	addSkill(t, f.second, "two", "Second.", "Body.")
	if got := skillFields(f.run(t, `{}`))["skills"]; got != "2" {
		t.Fatalf("skills before the edit = %s", got)
	}
	writeSkillRootSetting(t, f.root, `{"libraries":{"skill_dirs":["skills-b"]}}`)
	if got := skillFields(f.run(t, `{}`))["skills"]; got != "1" {
		t.Fatalf("skills after the edit = %s", got)
	}
	if status := skillStatus(t, f.run(t, `{"name":"one"}`)); status != "not_found" {
		t.Fatalf("a removed directory still served its skill: %s", status)
	}
}

func TestSkillToolUnreadableRootSettingIsARecoverableError(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	writeSkillRootSetting(t, f.root, "{ not json")
	output := f.run(t, `{}`)
	if skillStatus(t, output) != "error" || skillFields(output)["error_code"] != "skill_directories_unavailable" {
		t.Fatalf("output = %s", output)
	}
	if !strings.Contains(skillFields(output)["reason"], "invalid JSON") {
		t.Fatalf("the reason does not say why: %s", output)
	}
}

func TestSkillToolDefaultsToTheEmbeddedDirectoryWithoutARootSetting(t *testing.T) {
	root := t.TempDir()
	tool, err := NewSkillTool(sessionfs.NativeSkillSettings{MaxBytes: 131072}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	addSkill(t, filepath.Join(root, ".agent", "skills"), "default-dir", "In the default directory.", "Body.")
	output, err := tool.Execute(context.Background(), map[string]any{}, tools.ExecutionContext{CallID: "call_1"})
	if err != nil || skillFields(output)["skills"] != "1" {
		t.Fatalf("output = %s (err %v)", output, err)
	}
}

func TestSkillToolSkipsSkillsOverTheAssetSizeLimit(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	addSkill(t, f.first, "small", "Fits.", "Body.")
	addSkill(t, f.first, "big", "Too large.", strings.Repeat("x", 1<<20))
	output := f.run(t, `{}`)
	if skillFields(output)["skills"] != "1" || strings.Contains(output, "big") {
		t.Fatalf("list does not hide the oversized skill: %s", output)
	}
	output = f.run(t, `{"name":"big"}`)
	if skillStatus(t, output) != "not_found" {
		t.Fatalf("Read of the oversized skill = %s, want not_found", output)
	}
}

func TestSkillToolSchemaTakesOneOptionalNameOnly(t *testing.T) {
	f := newSkillFixture(t, sessionfs.NativeSkillSettings{MaxBytes: 131072})
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(f.tool.Definition.InputSchema, &schema); err != nil || schema.Required != nil {
		t.Fatalf("the schema requires %v (err %v)", schema.Required, err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{f.tool})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{"name":"x","extra":true}`, `{"name":7}`} {
		output := executeCatalogCall(t, catalog, "skill", arguments)
		if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, `example_operation: {"name":"agent-tool-design-guidelines"}`) {
			t.Fatalf("arguments %s were accepted or lack the example:\n%s", arguments, output)
		}
	}
}

func executeCatalogCall(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func TestSkillToolDefinitionMatchesTypeScriptFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "native-skill-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected tools.Definition
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	tool, err := NewSkillTool(sessionfs.NativeSkillSettings{MaxBytes: 131072}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Definition.ID != expected.ID || tool.Definition.Name != expected.Name || tool.Definition.Source != expected.Source || tool.Definition.Description != expected.Description {
		t.Fatalf("definition = %+v, want %+v", tool.Definition, expected)
	}
	var got, want any
	if err := json.Unmarshal(tool.Definition.InputSchema, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected.InputSchema, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inputSchema = %s, want %s", tool.Definition.InputSchema, expected.InputSchema)
	}
}

// In the workspace layout the skill tool reads the session's libraries: a skill in both the
// global and the workspace library is a skill_ambiguous error naming both folders, and is not
// listed; a skill in one scope loads (Q3, decider fix B).
func TestWorkspaceSkillToolRefusesANameInBothLibraries(t *testing.T) {
	root := t.TempDir()
	global, own := filepath.Join(root, "g", "skills"), filepath.Join(root, "w", ".jad", "skills")
	addSkill(t, global, "both", "Global copy.", "G.")
	addSkill(t, own, "both", "Workspace copy.", "W.")
	addSkill(t, own, "mine", "Mine.", "Only here.")
	// A setting.json in the root is never read in the workspace layout.
	writeSkillRootSetting(t, root, `{"libraries":{"skill_dirs":["nothing"]}}`)
	libraries := &sessionfs.Libraries{GlobalDir: filepath.Join(root, "g"), Global: sessionfs.LibraryFolders{Skills: []string{global}}, Workspace: sessionfs.LibraryFolders{Skills: []string{own}}}
	tool, err := NewSkillTool(sessionfs.NativeSkillSettings{MaxBytes: 131072}, root, libraries)
	if err != nil {
		t.Fatal(err)
	}
	fixture := skillFixture{root: root, tool: tool}
	if output := fixture.run(t, `{"name":"both"}`); !strings.Contains(output, "skill_ambiguous") || !strings.Contains(output, global) || !strings.Contains(output, own) {
		t.Fatalf("both:\n%s", output)
	}
	if output := fixture.run(t, `{"name":"mine"}`); !strings.Contains(output, "Only here.") {
		t.Fatalf("mine:\n%s", output)
	}
	if output := fixture.run(t, `{}`); !strings.Contains(output, "mine") || strings.Contains(output, "both") {
		t.Fatalf("list:\n%s", output)
	}
}
