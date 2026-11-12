package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A profile's edit goes through its owner key to its own file in its own workspace, and a run
// resolved before the edit keeps the settings it loaded while the next run reads the new file
// (decider S7: nothing reloads a profile file in the middle of a run).
func TestProfileEditFromARunAppliesFromTheNextRun(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	w := filepath.Join(base, "w")
	writeWorkspaceTestFile(t, filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceTestFile(t, filepath.Join(w, ".jad", "workspace.json"), `{}`)
	writeWorkspaceTestFile(t, filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	file := filepath.Join(w, ".jad", "profiles", "dev", "coder.json")
	writeWorkspaceTestFile(t, file, `{"models":["openai/default"],"settings":{"max_tool_rounds":3}}`)
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: w, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := resolver.Resolve(ctx, "s1", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	actions := profileMessenger{resolver: resolver}
	owner := "s1\x00dev/coder"
	view, err := actions.ProfileConfig(ctx, owner, sessionfs.ProfileRun{})
	if err != nil {
		t.Fatal(err)
	}
	if view.SelfEdit != sessionfs.ProfileSelfEditOn || len(view.Editable) == 0 {
		t.Fatalf("config view: level %q editable %v", view.SelfEdit, view.Editable)
	}
	result, err := actions.EditProfile(ctx, owner, sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit, IfMatch: view.Revision,
		Contents: json.RawMessage(`{"models":["openai/default"],"settings":{"max_tool_rounds":7}}`)})
	if err != nil || !result.Changed {
		t.Fatalf("edit: %+v, %v", result, err)
	}
	if rounds := running.Runner.(boundRunner).config.MaxToolRounds; rounds != 3 {
		t.Fatalf("the running run's settings changed to %d rounds", rounds)
	}
	next, err := resolver.Resolve(ctx, "s1", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if rounds := next.Runner.(boundRunner).config.MaxToolRounds; rounds != 7 {
		t.Fatalf("the next run has %d rounds", rounds)
	}
	history, err := actions.ProfileHistory(ctx, owner, 10)
	if err != nil || len(history.Entries) != 1 || history.Entries[0].Audit.Session == "" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if _, err := actions.EditProfile(ctx, "s1", sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit}); err == nil {
		t.Fatal("an owner key without a profile edited")
	}
	if _, err := os.Stat(filepath.Join(w, ".jad", "profile-own", "dev", "coder", "history", result.HistoryID+".json")); err != nil {
		t.Fatal(err)
	}
}
