package app

import (
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// layoutConfig writes the spec as a catalog-layout session (id "demo" unless the spec names
// one, in root "local") and loads the server config from its root setting, like the server
// does at start.
func layoutConfig(t *testing.T, spec layouttest.Spec, environment map[string]string) (Config, layouttest.Fixture) {
	t.Helper()
	if spec.SessionID == "" {
		spec.SessionID = "demo"
	}
	fixture := layouttest.Write(t, spec)
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: fixture.Workspace, GlobalDir: fixture.GlobalDir, Environment: environment})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	return config, fixture
}

// profileState is the folder that holds the transcript (messages.jsonl) and staging of
// the session's default profile, dev/coder.
func profileState(fixture layouttest.Fixture) string {
	return filepath.Join(fixture.StateDir, fixture.SessionID, "dev", "coder")
}
