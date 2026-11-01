package native

import (
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

var sampleProfileManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile.json")

func TestBuildCatalogLoadsProfileFromTheSampleManifest(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleProfileManifest},
		NativeProfile:       &sessionfs.NativeProfileSettings{MaxEntries: 5, ScanMaxEntries: 5, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != profileToolID || definitions[0].Name != "profile" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestBuildCatalogNeedsTheProfileManifest(t *testing.T) {
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(t.TempDir(), "setting.json"), NativeProfile: &sessionfs.NativeProfileSettings{}})
	if err == nil {
		t.Fatal("a profile tool without its manifest was accepted")
	}
}
