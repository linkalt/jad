package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"jad/backend-go/internal/checkpoint"
	"jad/backend-go/internal/sessionfs"
)

// A checkpoint restore leaves what profile self-edit owns (3b.2): restoring a checkpoint taken
// before a self-edit puts the workspace's own files back, while the edited profile file, the
// edited document, the edited own model (3c.2) and the history stay as the edit left them, and
// the profile tool's revert still undoes the edit.
func TestCheckpointRestoreLeavesWhatProfileSelfEditOwns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	w := filepath.Join(base, "w")
	writeWorkspaceTestFile(t, filepath.Join(home, ".jad", "models", "openai", "default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
	writeWorkspaceTestFile(t, filepath.Join(w, ".jad", "workspace.json"), `{}`)
	writeWorkspaceTestFile(t, filepath.Join(w, ".jad", "sessions", "s1.json"), `{"schema_version":1}`)
	file := filepath.Join(w, ".jad", "profiles", "dev", "coder.json")
	writeWorkspaceTestFile(t, file, `{"models":["openai/default"],"settings":{"max_tool_rounds":3}}`)
	writeWorkspaceTestFile(t, filepath.Join(w, "a.txt"), "a0\n")
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
	config := running.Runner.(boundRunner).config
	if config.WorkingDir != w {
		t.Fatalf("cwd %s, want the workspace %s", config.WorkingDir, w)
	}
	config.Checkpoint = sessionfs.CheckpointSettings{Enabled: true, GitBinary: "git", MaxCheckpoints: 20, MaxFiles: 20_000,
		MaxTotalBytes: 512 << 20, TimeoutMS: 120_000, SnapshotOn: "first_mutation", SafeRestore: true}
	service := checkpoint.ForSession(config, nil)

	actions := profileMessenger{resolver: resolver}
	owner := "s1\x00dev/coder"
	edit := func(request sessionfs.ProfileEditRequest) sessionfs.ProfileEditResult {
		t.Helper()
		view, err := actions.ProfileConfig(ctx, owner, sessionfs.ProfileRun{})
		if err != nil {
			t.Fatal(err)
		}
		request.IfMatch = view.Revision
		result, err := actions.EditProfile(ctx, owner, request)
		if err != nil || !result.Changed {
			t.Fatalf("%s: %+v, %v", request.Action, result, err)
		}
		return result
	}
	edit(sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit, Documents: []sessionfs.ProfileDocumentChange{{Path: "notes.md", Content: []byte("n0\n")}},
		Models: []sessionfs.ProfileDocumentChange{{Path: "openai/fast", Content: []byte(`{"based_on":"global:openai/default","set":{"request":{"temperature":0.1}}}`)}}})
	document := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "documents", "notes.md")
	model := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "models", "openai", "fast.json")
	modelBefore := readFile(t, model)
	history := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "history")

	id := service.Begin(ctx, "turn-1", 1)
	if id == "" {
		t.Fatalf("no checkpoint was taken: %+v", service.List())
	}
	writeWorkspaceTestFile(t, filepath.Join(w, "a.txt"), "a1\n")
	sum := sessionfs.CatalogETag([]byte("n0\n"))
	changed := edit(sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionEdit,
		Contents:  json.RawMessage(`{"models":["openai/default"],"settings":{"max_tool_rounds":7}}`),
		Documents: []sessionfs.ProfileDocumentChange{{Path: "notes.md", Content: []byte("n1\n"), IfMatch: sum}},
		Models:    []sessionfs.ProfileDocumentChange{{Path: "openai/fast", Content: []byte(`{"based_on":"global:openai/default","set":{"request":{"temperature":0.9}}}`), IfMatch: sessionfs.CatalogETag([]byte(modelBefore))}}})
	service.Finish(id)
	profileAfter, documentAfter, modelAfter, entriesAfter := readFile(t, file), readFile(t, document), readFile(t, model), listNames(t, history)

	for _, mode := range []string{checkpoint.ModeSafe, checkpoint.ModeFull} {
		writeWorkspaceTestFile(t, filepath.Join(w, "a.txt"), "a1\n")
		result, err := service.Restore(ctx, id, checkpoint.RestoreOptions{Mode: mode})
		if err != nil {
			t.Fatalf("%s restore: %v", mode, err)
		}
		if got := readFile(t, filepath.Join(w, "a.txt")); got != "a0\n" {
			t.Fatalf("%s restore left a.txt %q: %+v", mode, got, result)
		}
		if readFile(t, file) != profileAfter || readFile(t, document) != documentAfter || readFile(t, model) != modelAfter || !slices.Equal(listNames(t, history), entriesAfter) {
			t.Fatalf("%s restore changed what self-edit owns: %+v", mode, result)
		}
	}
	reverted := edit(sessionfs.ProfileEditRequest{Action: sessionfs.ProfileEditActionRevert, HistoryID: changed.HistoryID})
	if reverted.RevertedTo != changed.HistoryID || readFile(t, document) != "n0\n" || readFile(t, model) != modelBefore {
		t.Fatalf("revert after the restores: %+v, document %q", reverted, readFile(t, document))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
