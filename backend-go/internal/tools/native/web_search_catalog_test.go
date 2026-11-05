package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/extension"
)

func TestBuildCatalogLoadsConfiguredWebSearchFromItsExtensionManifest(t *testing.T) {
	root := t.TempDir()
	settings := &sessionfs.ExtensionWebSearchSettings{Provider: sessionfs.WebSearchProviderBrave, BaseURL: "https://api.example/search", APIKeyEnv: "SEARCH_KEY", DefaultResults: 3, MaxResults: 5, TimeoutMS: 1000, MaxResponseBytes: 4096, MaxSnippetChars: 100, MaxQueryChars: 100, UserAgent: "test", AllowedDomains: []string{}, BlockedDomains: []string{}}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(root, "setting.json"), ExtensionWebSearch: settings})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.web_search is configured but extension.web_search is missing from tool_definition_paths") {
		t.Fatalf("missing manifest error = %v", err)
	}
	manifest := filepath.Join("..", "..", "..", "..", "sample", "tools", "extension", "web_search.json")
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(root, "setting.json"), ToolDefinitionPaths: []string{manifest}, ExtensionWebSearch: settings})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != extension.WebSearchToolID || definitions[0].Source != "extension" || definitions[0].Name != "web_search" {
		t.Fatalf("definitions = %+v", definitions)
	}
	tool, err := extension.NewWebSearchTool(*settings)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || !tool.LeafBudget || definitions[0].Description != tool.Definition.Description {
		t.Fatalf("tool flags or description differ: read-only %t leaf %t %q", tool.ReadOnly, tool.LeafBudget, definitions[0].Description)
	}
}
