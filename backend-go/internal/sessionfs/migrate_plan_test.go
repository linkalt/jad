package sessionfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// migrateFixture is a v1 setting in W/.jad, shaped like this repository's: every catalog in
// W/.jad, G at $HOME/.jad.
type migrateFixture struct {
	base, home, workspace, jad, global string
	setting                            map[string]any
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	base := t.TempDir()
	f := &migrateFixture{base: base, home: filepath.Join(base, "home"), workspace: filepath.Join(base, "project")}
	f.jad = filepath.Join(f.workspace, ".jad")
	f.global = filepath.Join(f.home, ".jad")
	for _, dir := range []string{f.home, f.jad} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.setting = map[string]any{
		"schema_version": 1,
		"server":         map[string]any{"port": 4100, "log_file": "../../logs/jad.log"},
		"session_roots":  []any{map[string]any{"id": "local", "label": "Local sessions", "path": "sessions"}},
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": "state"},
		"libraries":      map[string]any{"prompt_dirs": []string{"prompts/prompts"}, "tool_definition_paths": []string{"../../toolsdef/native/exec.json"}},
		"defaults":       map[string]any{"cwd": f.workspace, "env_file": "env"},
	}
	f.put(t, "models/openai/default.json", map[string]any{"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY"})
	f.put(t, "models/openai/other.json", map[string]any{"provider": "openai", "model": "gpt-other", "api_key": "fixture-inline-value"})
	f.put(t, "tools/coding/default.json", map[string]any{"native": map[string]any{"async": map[string]any{}, "exec": map[string]any{"env": map[string]any{"TOKEN": "fixture-secret-value"}}}, "mcp": map[string]any{"web/search": map[string]any{}}})
	f.put(t, "mcp/web/search.json", map[string]any{"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": "http://127.0.0.1:1/mcp", "headers": map[string]any{"Authorization": "Bearer fixture-header-value"}}})
	f.put(t, "profiles/dev/coder.json", map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": "coding/default"})
	f.put(t, "sessions/work.json", map[string]any{"schema_version": 1, "cwd": f.workspace})
	f.write(t, "state/work/dev/coder/messages.jsonl", "{}\n")
	f.write(t, "env", "")
	f.writeSetting(t)
	return f
}

func (f *migrateFixture) put(t *testing.T, rel string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, rel, string(data))
}

// write puts a file below W/.jad, or at rel itself when it is absolute.
func (f *migrateFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.jad, rel)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *migrateFixture) writeSetting(t *testing.T) {
	t.Helper()
	f.put(t, "setting.json", f.setting)
}

func (f *migrateFixture) plan(t *testing.T, root string) MigrationPlan {
	t.Helper()
	plan, err := PlanWorkspaceMigration(MigrateOptions{WorkspaceDir: f.workspace, Root: root, Environment: map[string]string{"HOME": f.home}, LibraryDefaults: map[string][]string{"skill_dirs": {".agent/skills"}}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// snapshotTree records every entry below dir: its mode, size, modification time and content.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		record := info.Mode().String() + " " + info.ModTime().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			record += " " + hex.EncodeToString(sum[:])
		}
		tree[path] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func blockerKinds(plan MigrationPlan) []string {
	kinds := make([]string, 0, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		kinds = append(kinds, blocker.Kind)
	}
	return kinds
}

func hasBlocker(plan MigrationPlan, kind string) bool {
	for _, blocker := range plan.Blockers {
		if blocker.Kind == kind {
			return true
		}
	}
	return false
}

func fileOf(t *testing.T, plan MigrationPlan, source string) MigrationFile {
	t.Helper()
	for _, file := range plan.Files {
		if file.Source == source {
			return file
		}
	}
	t.Fatalf("no planned file for %s in %+v", source, plan.Files)
	return MigrationFile{}
}

func settingOf(t *testing.T, plan MigrationPlan, path string) (MigrationSettingFile, map[string]any) {
	t.Helper()
	for _, setting := range plan.Settings {
		if setting.Path == path {
			var content map[string]any
			if len(setting.Content) > 0 {
				if err := json.Unmarshal(setting.Content, &content); err != nil {
					t.Fatal(err)
				}
			}
			return setting, content
		}
	}
	t.Fatalf("no planned setting %s in %+v", path, plan.Settings)
	return MigrationSettingFile{}, nil
}

func TestMigrationPlanOfARepositoryShapedSettingWritesNothing(t *testing.T) {
	f := newMigrateFixture(t)
	before := snapshotTree(t, f.base)
	plan := f.plan(t, "")
	if after := snapshotTree(t, f.base); len(after) != len(before) {
		t.Fatalf("the plan changed the tree: %d entries before, %d after", len(before), len(after))
	} else {
		for path, record := range before {
			if after[path] != record {
				t.Fatalf("the plan changed %s", path)
			}
		}
	}
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	if plan.Root != "local" || plan.Setting != filepath.Join(f.jad, "setting.json") {
		t.Fatalf("root %q setting %q", plan.Root, plan.Setting)
	}

	// Profiles, sessions, state, models and tools are already where v2 reads them.
	for _, rel := range []string{"sessions/work.json", "profiles/dev/coder.json", "models/openai/default.json", "tools/coding/default.json"} {
		file := fileOf(t, plan, filepath.Join(f.jad, rel))
		if file.Action != MigrationInPlace || file.Global {
			t.Fatalf("%s: %+v", rel, file)
		}
	}
	for _, file := range plan.Files {
		if file.Group == "state" {
			t.Fatalf("state in place is not listed file by file: %+v", file)
		}
	}
	// v2 reads MCP servers from G only.
	mcp := fileOf(t, plan, filepath.Join(f.jad, "mcp/web/search.json"))
	if mcp.Action != MigrationCopy || !mcp.Global || mcp.Destination != filepath.Join(f.global, "mcp/web/search.json") || mcp.SHA256 == "" {
		t.Fatalf("mcp: %+v", mcp)
	}

	_, workspace := settingOf(t, plan, filepath.Join(f.jad, "workspace.json"))
	if workspace["schema_version"] != float64(2) || workspace["id"] != "local" || workspace["label"] != "Local sessions" {
		t.Fatalf("workspace.json: %v", workspace)
	}
	if defaults := workspace["defaults"].(map[string]any); defaults["cwd"] != f.workspace || defaults["env_file"] != "env" {
		t.Fatalf("defaults keep their values (same base): %v", defaults)
	}
	libraries := workspace["libraries"].(map[string]any)
	if got := libraries["prompt_dirs"].([]any); len(got) != 1 || got[0] != "prompts/prompts" {
		t.Fatalf("prompt_dirs: %v", libraries)
	}
	if got := libraries["skill_dirs"].([]any); len(got) != 1 || got[0] != ".agent/skills" {
		t.Fatalf("a v1 fallback folder is written out: %v", libraries)
	}
	if _, global := libraries["tool_definition_paths"]; global {
		t.Fatalf("tool_definition_paths belongs to global.json: %v", libraries)
	}

	_, global := settingOf(t, plan, filepath.Join(f.global, "global.json"))
	server := global["server"].(map[string]any)
	if server["log_file"] != filepath.Join(f.base, "logs/jad.log") || server["port"] != float64(4100) {
		t.Fatalf("server: %v", server)
	}
	paths := global["libraries"].(map[string]any)["tool_definition_paths"].([]any)
	if len(paths) != 1 || paths[0] != filepath.Join(f.base, "toolsdef/native/exec.json") {
		t.Fatalf("tool_definition_paths: %v", paths)
	}
	rewrites := map[string]MigrationRewrite{}
	for _, item := range plan.Rewrites {
		rewrites[item.Key] = item
	}
	if rewrites["server.log_file"].From != "../../logs/jad.log" || rewrites["libraries.tool_definition_paths[0]"].To != filepath.Join(f.base, "toolsdef/native/exec.json") {
		t.Fatalf("rewrites: %+v", plan.Rewrites)
	}
	warned := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(warned, "stay workspace-private") || strings.Contains(warned, "tied to this workspace") {
		t.Fatalf("warnings: %s", warned)
	}

	// Secrets are named by key, never shown.
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"fixture-secret-value", "fixture-header-value", "fixture-inline-value"} {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("the plan shows a secret value %q", value)
		}
	}
	named := map[string]bool{}
	for _, secret := range plan.Secrets {
		named[secret.Key] = true
	}
	if !named["native.exec.env.TOKEN"] || !named["transport.headers.Authorization"] || !named["api_key"] || named["api_key_env"] {
		t.Fatalf("secrets: %+v", plan.Secrets)
	}
}

// otherFolders moves the v1 catalogs and the session root out of W/.jad (absolute catalogs);
// the setting stays in W/.jad, so the relative paths of the workspace files keep their base.
func otherFolders(t *testing.T, f *migrateFixture) string {
	t.Helper()
	v1 := filepath.Join(f.base, "v1")
	for _, name := range []string{"profiles", "models", "tools", "mcp", "state", "sessions"} {
		if err := os.Rename(filepath.Join(f.jad, name), filepath.Join(v1, name)); err != nil {
			if err := os.MkdirAll(v1, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(f.jad, name), filepath.Join(v1, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.setting["catalogs"] = map[string]any{"profiles": filepath.Join(v1, "profiles"), "models": filepath.Join(v1, "models"), "tools": filepath.Join(v1, "tools"), "mcp": filepath.Join(v1, "mcp"), "state": filepath.Join(v1, "state")}
	f.setting["session_roots"] = []any{map[string]any{"id": "local", "path": filepath.Join(v1, "sessions")}}
	f.setting["defaults"] = map[string]any{"cwd": f.workspace, "tool_storage_dir": filepath.Join(v1, "state")}
	f.writeSetting(t)
	return v1
}

func TestMigrationPlanCopiesFilesFromOtherFoldersAndRewritesMovedPaths(t *testing.T) {
	f := newMigrateFixture(t)
	v1 := otherFolders(t, f)
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), map[string]any{"native": map[string]any{"async": map[string]any{}, "exec": map[string]any{"env_file": "exec.env"}}, "hooks": map[string]any{"env_file": "hooks.env"}, "mcp": map[string]any{"web/search": map[string]any{}}})
	f.write(t, filepath.Join(v1, "state/work/dev/coder/staging/usage-cache.json"), "{}")
	f.write(t, filepath.Join(v1, "state/work/.messages.jsonl.jad-lock"), "")
	f.write(t, filepath.Join(v1, "state/.mcp-oauth/web/search.json"), "{}")
	f.write(t, filepath.Join(v1, "state/other/x.jsonl"), "")
	before := snapshotTree(t, f.base)
	plan := f.plan(t, "")
	if after := snapshotTree(t, f.base); len(after) != len(before) {
		t.Fatalf("the plan changed the tree")
	}
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	for rel, want := range map[string]string{
		"sessions/work.json":                            filepath.Join(f.jad, "sessions/work.json"),
		"profiles/dev/coder.json":                       filepath.Join(f.jad, "profiles/dev/coder.json"),
		"state/work/dev/coder/messages.jsonl":           filepath.Join(f.jad, "state/work/dev/coder/messages.jsonl"),
		"state/work/dev/coder/staging/usage-cache.json": filepath.Join(f.jad, "state/work/dev/coder/staging/usage-cache.json"),
		"models/openai/default.json":                    filepath.Join(f.global, "models/openai/default.json"),
		"tools/coding/default.json":                     filepath.Join(f.global, "tools/coding/default.json"),
	} {
		file := fileOf(t, plan, filepath.Join(v1, rel))
		if file.Action != MigrationCopy || file.Destination != want || file.SHA256 == "" {
			t.Fatalf("%s: %+v", rel, file)
		}
	}
	if !fileOf(t, plan, filepath.Join(v1, "tools/coding/default.json")).Rewritten {
		t.Fatal("the toolset's paths are rewritten")
	}
	rewrites := map[string]MigrationRewrite{}
	for _, item := range plan.Rewrites {
		rewrites[item.Key] = item
	}
	if rewrites["native.exec.env_file"].To != filepath.Join(f.jad, "exec.env") || rewrites["hooks.env_file"].To != filepath.Join(f.jad, "hooks.env") {
		t.Fatalf("a moved toolset keeps the env files it named in v1: %+v", plan.Rewrites)
	}
	notes := map[string]string{}
	for _, note := range plan.NotMigrated {
		notes[note.Path] = note.Reason
	}
	if !strings.Contains(notes[filepath.Join(v1, "state/work/.messages.jsonl.jad-lock")], "lock") ||
		!strings.Contains(notes[filepath.Join(v1, "state/.mcp-oauth")], "credentials") ||
		!strings.Contains(notes[filepath.Join(v1, "state/other")], "not the state") {
		t.Fatalf("not migrated: %+v", plan.NotMigrated)
	}
}

// Decider 4c: a write target outside the copied state is shared by v1 and v2, never copied; the
// plan says so once per setting and does not stop. v2 refuses a target in G or in this
// workspace's profile-own folder, and the plan stops on those.
func TestMigrationPlanSharesStorageOutsideTheCopiedState(t *testing.T) {
	f := newMigrateFixture(t)
	otherFolders(t, f)
	// The default tool storage is <cwd>/.jad/state, outside the copied v1 state.
	f.setting["defaults"] = map[string]any{"cwd": f.workspace}
	f.writeSetting(t)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("shared storage is not a blocker: %+v", plan.Blockers)
	}
	warned := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(warned, "shared storage: tool_storage_dir writes "+filepath.Join(f.jad, "state", "work")) || !strings.Contains(warned, "nothing is copied") {
		t.Fatalf("a note names the setting and where it writes: %s", warned)
	}
	if strings.Count(warned, "shared storage: tool_storage_dir") != 1 {
		t.Fatalf("one note per setting: %s", warned)
	}
}

func TestMigrationPlanStopsOnStorageInPlacesV2KeepsAway(t *testing.T) {
	for name, dir := range map[string]func(f *migrateFixture) string{
		"G":                func(f *migrateFixture) string { return filepath.Join(f.global, "work") },
		"G through a link": nil,
		"profile-own":      func(f *migrateFixture) string { return filepath.Join(f.jad, "profile-own", "x") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMigrateFixture(t)
			otherFolders(t, f)
			target := ""
			if dir != nil {
				target = dir(f)
			} else {
				link := filepath.Join(f.base, "glink")
				if err := os.MkdirAll(f.global, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.global, link); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(link, "work")
			}
			f.setting["defaults"] = map[string]any{"cwd": f.workspace, "tool_storage_dir": target}
			f.writeSetting(t)
			plan := f.plan(t, "")
			found := false
			for _, blocker := range plan.Blockers {
				found = found || blocker.Kind == migrationBlockForbidden && strings.Contains(blocker.Message, "tool_storage_dir writes")
			}
			if !found {
				t.Fatalf("want %s: %+v", migrationBlockForbidden, plan.Blockers)
			}
		})
	}
}

func TestMigrationPlanStopsOnStorageInAnotherListedWorkspace(t *testing.T) {
	f := newMigrateFixture(t)
	otherFolders(t, f)
	other := filepath.Join(f.base, "other")
	f.write(t, filepath.Join(f.global, "global.json"), `{"schema_version":2,"workspaces":[{"id":"other","path":"`+other+`"}]}`)
	f.write(t, filepath.Join(other, ".jad", "state", "x"), "")
	f.setting["defaults"] = map[string]any{"cwd": f.workspace, "tool_storage_dir": filepath.Join(other, ".jad", "state")}
	f.writeSetting(t)
	if plan := f.plan(t, ""); !hasBlocker(plan, migrationBlockForbidden) {
		t.Fatalf("a target in another workspace's .jad: %+v", plan.Blockers)
	}
}

func TestMigrationPlanRootSelection(t *testing.T) {
	f := newMigrateFixture(t)
	f.setting["session_roots"] = []any{map[string]any{"id": "local", "path": "sessions"}, map[string]any{"id": "extra", "path": "sessions-extra"}}
	f.put(t, "sessions-extra/side.json", map[string]any{"schema_version": 1, "cwd": f.workspace})
	f.writeSetting(t)

	plan := f.plan(t, "")
	if kinds := blockerKinds(plan); len(kinds) != 1 || kinds[0] != migrationBlockRoots || !strings.Contains(plan.Blockers[0].Message, "local, extra") {
		t.Fatalf("several roots need --root: %+v", plan.Blockers)
	}
	if len(plan.Roots) != 2 || len(plan.Roots[1].Sessions) != 1 || plan.Roots[1].Sessions[0] != "side" {
		t.Fatalf("roots are listed with their sessions: %+v", plan.Roots)
	}
	if plan = f.plan(t, "nope"); !hasBlocker(plan, migrationBlockUnknownRoot) {
		t.Fatalf("unknown root: %+v", plan.Blockers)
	}
	plan = f.plan(t, "local")
	if plan.Blocked() || plan.Root != "local" {
		t.Fatalf("named root: %+v", plan.Blockers)
	}
	left := false
	for _, note := range plan.NotMigrated {
		left = left || note.Path == filepath.Join(f.jad, "sessions-extra/side.json")
	}
	if !left {
		t.Fatalf("the other root's sessions are listed as not migrated: %+v", plan.NotMigrated)
	}
}

func TestMigrationPlanBlockers(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *migrateFixture)
		root  string
		kind  string
	}{
		{"workspace exists", func(t *testing.T, f *migrateFixture) { f.write(t, "workspace.json", "{}") }, "", migrationBlockWorkspaceExists},
		{"no v1 setting", func(t *testing.T, f *migrateFixture) {
			if err := os.Remove(filepath.Join(f.jad, "setting.json")); err != nil {
				t.Fatal(err)
			}
		}, "", migrationBlockNoSetting},
		{"setting outside W/.jad", func(t *testing.T, f *migrateFixture) {
			if err := os.Rename(filepath.Join(f.jad, "setting.json"), filepath.Join(f.workspace, "setting.json")); err != nil {
				t.Fatal(err)
			}
		}, "", migrationBlockPathBase},
		{"unknown top-level key", func(t *testing.T, f *migrateFixture) { f.setting["extra"] = true; f.writeSetting(t) }, "", migrationBlockUnknownKey},
		{"unknown libraries key", func(t *testing.T, f *migrateFixture) {
			f.setting["libraries"] = map[string]any{"odd_dirs": []string{}}
			f.writeSetting(t)
		}, "", migrationBlockUnknownKey},
		{"workspace clash with other content", func(t *testing.T, f *migrateFixture) {
			otherFolders(t, f)
			f.write(t, "profiles/dev/coder.json", `{"models":[]}`)
		}, "", migrationBlockClash},
		{"G clash with other content", func(t *testing.T, f *migrateFixture) {
			f.write(t, filepath.Join(f.global, "mcp/web/search.json"), `{"schema_version":1}`)
		}, "", migrationBlockClash},
		{"same ID in G and in the workspace", func(t *testing.T, f *migrateFixture) {
			f.write(t, filepath.Join(f.global, "models/openai/default.json"), "{}")
		}, "", migrationBlockAmbiguous},
		{"conflicting global.json", func(t *testing.T, f *migrateFixture) {
			f.write(t, filepath.Join(f.global, "global.json"), `{"schema_version":2,"server":{"port":5000}}`)
		}, "", migrationBlockGlobalSetting},
		{"symbolic link in a source folder", func(t *testing.T, f *migrateFixture) {
			if err := os.Symlink(filepath.Join(f.jad, "profiles/dev/coder.json"), filepath.Join(f.jad, "profiles/dev/linked.json")); err != nil {
				t.Fatal(err)
			}
		}, "", migrationBlockLink},
		{"G inside the workspace", func(t *testing.T, f *migrateFixture) {
			f.home = f.workspace
		}, "", migrationBlockGlobalPlace},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newMigrateFixture(t)
			test.setup(t, f)
			before := snapshotTree(t, f.base)
			plan := f.plan(t, test.root)
			if !hasBlocker(plan, test.kind) {
				t.Fatalf("want %s, got %+v", test.kind, plan.Blockers)
			}
			after := snapshotTree(t, f.base)
			if len(after) != len(before) {
				t.Fatalf("a blocked plan changed the tree")
			}
		})
	}
}

func TestMigrationPlanSkipsWhatGAlreadyHolds(t *testing.T) {
	f := newMigrateFixture(t)
	data, err := os.ReadFile(filepath.Join(f.jad, "mcp/web/search.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, filepath.Join(f.global, "mcp/web/search.json"), string(data))
	// A global.json that agrees with the v1 server, cli and libraries is kept.
	f.write(t, filepath.Join(f.global, "global.json"), `{"schema_version":2,"server":{"port":4100,"log_file":"`+filepath.Join(f.base, "logs/jad.log")+`"},"libraries":{"tool_definition_paths":["`+filepath.Join(f.base, "toolsdef/native/exec.json")+`"]}}`)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	if file := fileOf(t, plan, filepath.Join(f.jad, "mcp/web/search.json")); file.Action != MigrationSame {
		t.Fatalf("same content in G is skipped: %+v", file)
	}
	if setting, _ := settingOf(t, plan, filepath.Join(f.global, "global.json")); setting.Action != MigrationKeep {
		t.Fatalf("global.json: %+v", setting)
	}
}

func TestMigrationPlanStopsOnGlobalPathsTiedToTheWorkspace(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *migrateFixture)
		key   string
		file  func(f *migrateFixture) string
	}{
		{"relative server log file", func(t *testing.T, f *migrateFixture) {
			f.setting["server"] = map[string]any{"port": 4100, "log_file": "jad.log"}
		}, "server.log_file", func(f *migrateFixture) string { return filepath.Join(f.global, "global.json") }},
		{"relative remote api tokens file", func(t *testing.T, f *migrateFixture) {
			f.setting["server"] = map[string]any{"port": 4100, "remote_api": map[string]any{"tokens_file": "remote-tokens.json"}}
		}, "server.remote_api.tokens_file", func(f *migrateFixture) string { return filepath.Join(f.global, "global.json") }},
		{"relative token file of a remote", func(t *testing.T, f *migrateFixture) {
			f.setting["catalogs"] = map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": "state", "remotes": "remotes"}
			f.put(t, "remotes/home.json", map[string]any{"schema_version": 1, "remote": map[string]any{"url": "http://127.0.0.1:1", "token_file": "tokens/home"}})
		}, "remote.token_file", func(f *migrateFixture) string { return filepath.Join(f.global, "remotes/home.json") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newMigrateFixture(t)
			test.setup(t, f)
			f.writeSetting(t)
			before := snapshotTree(t, f.base)
			plan := f.plan(t, "")
			found := false
			for _, blocker := range plan.Blockers {
				found = found || blocker.Kind == migrationBlockTied && strings.Contains(blocker.Message, test.key+" is ") && blocker.Path == test.file(f)
			}
			if !found {
				t.Fatalf("want %s on %s in %s: %+v", migrationBlockTied, test.key, test.file(f), plan.Blockers)
			}
			if len(snapshotTree(t, f.base)) != len(before) {
				t.Fatal("the plan changed the tree")
			}
		})
	}
}

func TestMigrationPlanKeepsGlobalPathsOutsideTheWorkspaceAndWarnsForSharedToolsets(t *testing.T) {
	f := newMigrateFixture(t)
	v1 := otherFolders(t, f)
	f.put(t, filepath.Join(v1, "tools/coding/default.json"), map[string]any{"native": map[string]any{"async": map[string]any{}, "exec": map[string]any{"env_file": "exec.env"}}, "mcp": map[string]any{"web/search": map[string]any{}}})
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("a path outside the workspace and a toolset's env file are not blockers: %+v", plan.Blockers)
	}
	warned := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(warned, "native.exec.env_file is "+filepath.Join(f.jad, "exec.env")) || !strings.Contains(warned, "tied to this workspace") {
		t.Fatalf("a shared toolset that names the workspace's file is warned about: %s", warned)
	}
	_, global := settingOf(t, plan, filepath.Join(f.global, "global.json"))
	paths := global["libraries"].(map[string]any)["tool_definition_paths"].([]any)
	if len(paths) != 1 || paths[0] != filepath.Join(f.base, "toolsdef/native/exec.json") {
		t.Fatalf("a path outside the workspace stays absolute in G: %v", paths)
	}
}

// Decider 4c: a v1 tool definition path that names a place in W goes to workspace.json
// (libraries.tool_definition_paths, from W/.jad), the others stay in G's list.
func TestMigrationPlanMovesToolDefinitionsInTheWorkspaceToWorkspaceJSON(t *testing.T) {
	f := newMigrateFixture(t)
	f.setting["libraries"] = map[string]any{
		"prompt_dirs": []string{"prompts/prompts"},
		"tool_definition_paths": []string{
			"tools_defintion/native/exec.json",                    // relative, inside .jad: as written
			filepath.Join(f.jad, "tools_defintion/native/a.json"), // absolute, inside .jad: relative
			filepath.Join(f.workspace, "defs/b.json"),             // absolute, in W outside .jad: absolute
			"../../toolsdef/native/exec.json",                     // outside W: G's list
			"../defs/c.json",                                      // relative, in W outside .jad: as written
		},
	}
	f.writeSetting(t)
	before := snapshotTree(t, f.base)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	if len(snapshotTree(t, f.base)) != len(before) {
		t.Fatal("the plan changed the tree")
	}
	_, workspace := settingOf(t, plan, filepath.Join(f.jad, "workspace.json"))
	own := workspace["libraries"].(map[string]any)["tool_definition_paths"].([]any)
	want := []any{"tools_defintion/native/exec.json", "tools_defintion/native/a.json", filepath.Join(f.workspace, "defs/b.json"), "../defs/c.json"}
	if !reflect.DeepEqual(own, want) {
		t.Fatalf("workspace.json: %v, want %v", own, want)
	}
	_, shared := settingOf(t, plan, filepath.Join(f.global, "global.json"))
	paths := shared["libraries"].(map[string]any)["tool_definition_paths"].([]any)
	if len(paths) != 1 || paths[0] != filepath.Join(f.base, "toolsdef/native/exec.json") {
		t.Fatalf("global.json: %v", paths)
	}
	rewritten := false
	for _, rewrite := range plan.Rewrites {
		rewritten = rewritten || rewrite.File == filepath.Join(f.jad, "workspace.json") && rewrite.From == filepath.Join(f.jad, "tools_defintion/native/a.json") && rewrite.To == "tools_defintion/native/a.json"
	}
	if !rewritten {
		t.Fatalf("the absolute path made relative is reported: %+v", plan.Rewrites)
	}
}

func TestMigrationPlanWithOnlyWorkspaceToolDefinitionsWritesNoGlobalLibraries(t *testing.T) {
	f := newMigrateFixture(t)
	f.setting["libraries"] = map[string]any{"tool_definition_paths": []string{"tools_defintion/native/exec.json"}}
	f.writeSetting(t)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	_, global := settingOf(t, plan, filepath.Join(f.global, "global.json"))
	if _, has := global["libraries"]; has {
		t.Fatalf("G carries a libraries object for a workspace's tool definitions: %v", global)
	}
}

func TestMigrationPlanStopsOnToolDefinitionsInPlacesV2WillNotReadThemFrom(t *testing.T) {
	for _, rel := range []string{"profile-own/dev/coder/documents/t.json", "data/profiles/dev/coder/t.json", "profiles/dev/t.json"} {
		t.Run(rel, func(t *testing.T) {
			f := newMigrateFixture(t)
			f.setting["libraries"] = map[string]any{"tool_definition_paths": []string{rel}}
			f.writeSetting(t)
			if plan := f.plan(t, ""); !hasBlocker(plan, migrationBlockForbidden) {
				t.Fatalf("blockers: %+v", plan.Blockers)
			}
		})
	}
	f := newMigrateFixture(t)
	f.setting["libraries"] = map[string]any{"tool_definition_paths": []string{filepath.Join(f.global, "auth", "t.json")}}
	f.writeSetting(t)
	if plan := f.plan(t, ""); !hasBlocker(plan, migrationBlockForbidden) {
		t.Fatalf("G/auth: %+v", plan.Blockers)
	}
}

// Decider 4c: a global.json default that v2 would apply to sessions with no tool_storage_dir of
// their own is pinned in workspace.json to v1's default.
func TestMigrationPlanPinsTheDefaultToolStorageAGlobalDefaultWouldMove(t *testing.T) {
	f := newMigrateFixture(t)
	// A global.json is kept only when it agrees with the v1 server and global libraries: v1 has none here.
	delete(f.setting, "server")
	f.setting["libraries"] = map[string]any{"prompt_dirs": []string{"prompts/prompts"}}
	f.writeSetting(t)
	f.write(t, filepath.Join(f.global, "global.json"), `{"schema_version":2,"defaults":{"tool_storage_dir":"/elsewhere"}}`)
	plan := f.plan(t, "")
	if plan.Blocked() {
		t.Fatalf("blocked: %+v", plan.Blockers)
	}
	_, workspace := settingOf(t, plan, filepath.Join(f.jad, "workspace.json"))
	defaults := workspace["defaults"].(map[string]any)
	if defaults["tool_storage_dir"] != ".jad/state" || defaults["cwd"] != f.workspace {
		t.Fatalf("defaults = %v", defaults)
	}
	// Set in v1 already: kept as v1 has it. Not in G: nothing is added.
	f.setting["defaults"] = map[string]any{"cwd": f.workspace, "tool_storage_dir": "mine"}
	f.writeSetting(t)
	_, workspace = settingOf(t, f.plan(t, ""), filepath.Join(f.jad, "workspace.json"))
	if workspace["defaults"].(map[string]any)["tool_storage_dir"] != "mine" {
		t.Fatalf("v1's own value changed: %v", workspace["defaults"])
	}
	// No v1 defaults at all (the cwd is the session's own): the pin makes the object.
	delete(f.setting, "defaults")
	f.writeSetting(t)
	_, workspace = settingOf(t, f.plan(t, ""), filepath.Join(f.jad, "workspace.json"))
	if only, _ := workspace["defaults"].(map[string]any); len(only) != 1 || only["tool_storage_dir"] != ".jad/state" {
		t.Fatalf("defaults made by the pin: %v", workspace["defaults"])
	}
	if err := os.Remove(filepath.Join(f.global, "global.json")); err != nil {
		t.Fatal(err)
	}
	f.setting["defaults"] = map[string]any{"cwd": f.workspace}
	f.writeSetting(t)
	_, workspace = settingOf(t, f.plan(t, ""), filepath.Join(f.jad, "workspace.json"))
	if _, has := workspace["defaults"].(map[string]any)["tool_storage_dir"]; has {
		t.Fatalf("pinned without a global default: %v", workspace["defaults"])
	}
}
