package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A prompt document edited between LockSession and Resolve is now seen by the stamp, so
// Resolve loads again and the run gets the new text. Before, the parked config (and its
// old prompt) served the Resolve.
func TestResolveLoadsAgainWhenAPromptDocumentChangedAfterLockSession(t *testing.T) {
	cwd := t.TempDir()
	document := filepath.Join(cwd, "rules.md")
	if err := os.WriteFile(document, []byte("old rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := lockSpec()
	spec.CWD = cwd
	spec.Profiles["dev/green"]["prompt_documents"] = []string{document}
	config, fixture := layoutConfig(t, spec, map[string]string{})
	resolver, err := NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	backdate(t, fixture.Root)
	backdate(t, fixture.GlobalDir)
	backdate(t, cwd)

	release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if resolver.lockedConfigs.len() != 1 {
		t.Fatalf("LockSession parked %d configs, want 1", resolver.lockedConfigs.len())
	}
	if err := os.WriteFile(document, []byte("new rules, edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Runner.(boundRunner).config.Model.PromptText; got != "new rules, edited" {
		t.Fatalf("Resolve used the parked prompt %q; the edited document should have forced a load", got)
	}
}
