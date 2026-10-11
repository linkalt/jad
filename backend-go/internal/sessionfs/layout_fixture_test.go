package sessionfs

import (
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
)

// layoutLocation writes the spec as a catalog-layout session and resolves it through the
// root setting like the server does.
func layoutLocation(t *testing.T, spec layouttest.Spec) (Location, layouttest.Fixture) {
	t.Helper()
	fixture := layouttest.Write(t, spec)
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return location, fixture
}

// loadLayoutSpec loads the settings of a catalog-layout session built from the spec.
func loadLayoutSpec(t *testing.T, spec layouttest.Spec) (SessionConfig, error) {
	t.Helper()
	location, _ := layoutLocation(t, spec)
	return LoadSessionSettings(location, "", "")
}

// loadLayoutConfig is loadLayoutSpec plus the model file.
func loadLayoutConfig(t *testing.T, spec layouttest.Spec) (SessionConfig, error) {
	t.Helper()
	location, _ := layoutLocation(t, spec)
	return LoadSessionConfig(location, "", "")
}
