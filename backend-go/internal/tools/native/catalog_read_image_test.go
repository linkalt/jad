package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// sampleReadImageManifest is the manifest users are told to list in tool_definition_paths.
var sampleReadImageManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json")

func TestBuildCatalogLoadsReadImageFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleReadImageManifest},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != readImageToolID || definitions[0].Name != "read_image" {
		t.Fatalf("catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogNeedsTheReadImageManifestWhenTheToolIsConfigured(t *testing.T) {
	_, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:     filepath.Join(t.TempDir(), "setting.json"),
		NativeReadImage: &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
	})
	if err == nil || !strings.Contains(err.Error(), "tools.native.read_image is configured but native.read_image is missing from tool_definition_paths") {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
}

func TestReadImageIsASubagentReadOnlyTool(t *testing.T) {
	tool, err := NewReadImageTool(sessionfs.NativeReadImageSettings{MaxImageBytes: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.ReadOnly || tool.MayChangeFiles != nil {
		t.Fatalf("read_image must be read-only so an agent can list it: %+v", tool)
	}
}
