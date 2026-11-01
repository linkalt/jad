package sessionfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func backdateTree(t *testing.T, roots ...string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func writeFileT(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stampedLoad loads the fixture's session and checks the stamp is current right after,
// so a later "not current" can only come from the change a test makes.
func stampedLoad(t *testing.T, fixture layouttest.Fixture) (SessionConfig, SettingsStamp) {
	t.Helper()
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, stamp, err := LoadSessionSettingsStamped(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !stamp.Current() {
		t.Fatal("a stamp is not current right after its load")
	}
	return loaded, stamp
}

func TestSettingsStampSeesAPromptDocumentEditAndDelete(t *testing.T) {
	cwd := t.TempDir()
	document := filepath.Join(cwd, "rules.md")
	writeFileT(t, document, "one")
	fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd, ProfileExtra: map[string]any{"prompt_documents": []string{document}}})
	backdateTree(t, fixture.Root, cwd)

	config, stamp := stampedLoad(t, fixture)
	if config.Model.PromptText != "one" || len(config.PromptDocuments) != 1 || config.PromptDocuments[0] != document {
		t.Fatalf("prompt = %q documents = %v, want the file read", config.Model.PromptText, config.PromptDocuments)
	}
	writeFileT(t, document, "two, longer")
	if stamp.Current() {
		t.Error("stamp is current after the prompt document was edited")
	}

	writeFileT(t, document, "one")
	backdateTree(t, cwd)
	_, stamp = stampedLoad(t, fixture)
	if err := os.Remove(document); err != nil {
		t.Fatal(err)
	}
	if stamp.Current() {
		t.Error("stamp is current after the prompt document was deleted")
	}
}

func TestSettingsStampSeesTheWorkingDirectoryGoAway(t *testing.T) {
	cwd := t.TempDir()
	fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd})
	backdateTree(t, fixture.Root)

	_, stamp := stampedLoad(t, fixture)
	writeFileT(t, filepath.Join(cwd, "new.txt"), "x")
	if !stamp.Current() {
		t.Error("a file made in the working directory made the stamp stale; only its existence is read")
	}
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatal(err)
	}
	if stamp.Current() {
		t.Error("stamp is current after the working directory was removed")
	}

	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	_, stamp = stampedLoad(t, fixture)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, cwd, "now a file")
	if stamp.Current() {
		t.Error("stamp is current after the working directory became a file")
	}
}

func TestSettingsStampSeesAnEnvFileEdit(t *testing.T) {
	cwd := t.TempDir()
	envFile := filepath.Join(cwd, "tool.env")
	writeFileT(t, envFile, "A=1\n")
	fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd, Native: map[string]any{"bash": map[string]any{"env_file": envFile}}})
	backdateTree(t, fixture.Root, cwd)

	config, stamp := stampedLoad(t, fixture)
	if config.NativeBash == nil || config.NativeBash.EnvFile != envFile {
		t.Fatalf("NativeBash = %+v, want the env file read", config.NativeBash)
	}
	writeFileT(t, envFile, "A=22\n")
	if stamp.Current() {
		t.Error("stamp is current after the env file was edited")
	}
}

func TestSettingsStampSeesAStateContextDocumentEdit(t *testing.T) {
	cwd := t.TempDir()
	document := filepath.Join(cwd, "ctx.md")
	writeFileT(t, document, "context")
	fixture := layouttest.Write(t, layouttest.Spec{
		CWD:    cwd,
		Native: map[string]any{"task": map[string]any{}},
		Settings: map[string]any{"statecontext": map[string]any{"enabled": true, "triggers": map[string]any{
			"compaction": map[string]any{"tools": []string{"task"}, "prompt_documents": []string{document}},
		}}},
	})
	backdateTree(t, fixture.Root, cwd)

	config, stamp := stampedLoad(t, fixture)
	if got := config.StateContext.Triggers["compaction"].PromptDocuments; len(got) != 1 || got[0] != document {
		t.Fatalf("trigger documents = %v, want %s", got, document)
	}
	writeFileT(t, document, "context, edited")
	if stamp.Current() {
		t.Error("stamp is current after a state-context prompt document was edited")
	}
}

func TestSettingsStampSeesAnAgentFolderChange(t *testing.T) {
	cwd := t.TempDir()
	agents := filepath.Join(cwd, "agents")
	writeFileT(t, filepath.Join(agents, "helper", "prompt.md"), "be brief")
	fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd, Native: map[string]any{"subagent": map[string]any{"agents_dir": []string{agents}}}})
	backdateTree(t, fixture.Root, cwd)

	config, stamp := stampedLoad(t, fixture)
	if config.NativeSubagent == nil || len(config.NativeSubagent.Agents) != 1 {
		t.Fatalf("subagent = %+v, want one agent from the folder", config.NativeSubagent)
	}
	writeFileT(t, filepath.Join(agents, "helper", "prompt.md"), "be much briefer")
	if stamp.Current() {
		t.Error("stamp is current after an agent's prompt.md was edited")
	}

	writeFileT(t, filepath.Join(agents, "helper", "prompt.md"), "be brief")
	backdateTree(t, agents)
	_, stamp = stampedLoad(t, fixture)
	writeFileT(t, filepath.Join(agents, "second", "prompt.md"), "another")
	if stamp.Current() {
		t.Error("stamp is current after an agent folder was added")
	}

	if err := os.RemoveAll(filepath.Join(agents, "second")); err != nil {
		t.Fatal(err)
	}
	backdateTree(t, agents)
	_, stamp = stampedLoad(t, fixture)
	writeFileT(t, filepath.Join(agents, "helper", "agent.json"), "{}")
	if stamp.Current() {
		t.Error("stamp is current after an agent.json was added to a folder")
	}
}

func TestSettingsStampStaysCurrentWhenNothingChanged(t *testing.T) {
	cwd := t.TempDir()
	document := filepath.Join(cwd, "rules.md")
	writeFileT(t, document, "one")
	fixture := layouttest.Write(t, layouttest.Spec{CWD: cwd, ProfileExtra: map[string]any{"prompt_documents": []string{document}}})
	backdateTree(t, fixture.Root, cwd)

	_, stamp := stampedLoad(t, fixture)
	for i := 0; i < 3; i++ {
		if !stamp.Current() {
			t.Fatalf("stamp stopped being current on check %d with no change", i)
		}
	}
}
