package sessionfs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// T-035: a root setting in a ".jad" folder used to read its default catalogs from
// <.jad>/.jad/<catalog>. The default is now <.jad>/<catalog>, unless the doubled folder
// exists: then it stays the legacy root and nothing is moved.

var allCatalogs = []string{"profiles", "models", "tools", "mcp", "remotes", "state", "sandbox"}

// legacyProject writes <project>/.jad/setting.json with the given extra keys and returns the
// project folder and a Load that discovers it.
func legacyProject(t *testing.T, extra string) (project string, load func() (Config, error)) {
	t.Helper()
	base := t.TempDir()
	project, home := filepath.Join(base, "project"), filepath.Join(base, "home")
	for _, dir := range []string{home, filepath.Join(project, ".jad")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	text := `{"session_roots":[{"id":"local","path":"sessions"}]` + extra + `}`
	if err := os.WriteFile(filepath.Join(project, ".jad", "setting.json"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return project, func() (Config, error) {
		return Load(LoadOptions{WorkingDir: project, Environment: map[string]string{"HOME": home}})
	}
}

func catalogFolders(l *Layout) map[string]string {
	return map[string]string{"profiles": l.Profiles, "models": l.Models, "tools": l.Tools, "mcp": l.MCP, "remotes": l.Remotes, "state": l.State, "sandbox": l.Sandbox}
}

func mustMkdir(t *testing.T, path ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path...), 0o755); err != nil {
		t.Fatal(err)
	}
}

func warningsAbout(config Config, text string) []string {
	var found []string
	for _, warning := range config.Warnings {
		if strings.Contains(warning, text) {
			found = append(found, warning)
		}
	}
	return found
}

func TestJadSettingDefaultsDoNotDoubleTheJadFolder(t *testing.T) {
	project, load := legacyProject(t, "")
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	folders := catalogFolders(config.Roots[0].Layout)
	for _, kind := range allCatalogs {
		if want := filepath.Join(project, ".jad", kind); folders[kind] != want {
			t.Errorf("%s = %s, want %s", kind, folders[kind], want)
		}
	}
	if len(config.Warnings) != 0 {
		t.Errorf("warnings = %q", config.Warnings)
	}
	if _, err := os.Lstat(filepath.Join(project, ".jad", ".jad")); !os.IsNotExist(err) {
		t.Fatalf("loading made the doubled folder: %v", err)
	}
}

func TestFirstWriteWithoutTheDoubledFolderCreatesNoDoubledFolder(t *testing.T) {
	project, load := legacyProject(t, "")
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	body := []byte(`{"provider":"openai","model":"m","api_key_env":"K"}`)
	if _, _, err := WriteCatalogFile(*layout, config.Roots, CatalogModels, "openai/first", body, WriteCondition{IfNoneMatch: "*"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".jad", "models", "openai", "first.json")); err != nil {
		t.Fatalf("the model is not in .jad/models: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, ".jad", ".jad")); !os.IsNotExist(err) {
		t.Fatalf("the first write made the doubled folder: %v", err)
	}
	// the next load reads the same place and still warns of nothing
	again, err := load()
	if err != nil || again.Roots[0].Layout.Models != layout.Models || len(again.Warnings) != 0 {
		t.Fatalf("second load: %v, %s, %q", err, again.Roots[0].Layout.Models, again.Warnings)
	}
}

func TestADoubledFolderStaysTheLegacyRootWithOneWarning(t *testing.T) {
	project, load := legacyProject(t, "")
	legacy := filepath.Join(project, ".jad", ".jad")
	mustMkdir(t, legacy, "models", "openai")
	body := `{"provider":"openai","model":"old","api_key_env":"K"}`
	if err := os.WriteFile(filepath.Join(legacy, "models", "openai", "kept.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	for kind, folder := range catalogFolders(layout) {
		if want := filepath.Join(legacy, kind); folder != want {
			t.Errorf("%s = %s, want the legacy %s", kind, folder, want)
		}
	}
	warnings := warningsAbout(config, legacy)
	if len(warnings) != 1 || strings.Contains(warnings[0], "Both places") {
		t.Fatalf("warnings = %q", config.Warnings)
	}
	files, err := ListCatalogFiles(*layout, CatalogModels)
	if err != nil || len(files) != 1 || files[0].ID != "openai/kept" {
		t.Fatalf("the legacy model is not listed: %v, %+v", err, files)
	}
	// reading never creates the new folders
	for _, kind := range allCatalogs {
		if _, err := os.Lstat(filepath.Join(project, ".jad", kind)); !os.IsNotExist(err) {
			t.Errorf(".jad/%s exists after the load: %v", kind, err)
		}
	}
}

func TestBothPlacesKeepTheLegacyRootAndTheWarningNamesThem(t *testing.T) {
	project, load := legacyProject(t, `,"catalogs":{"tools":"tools"}`)
	legacy := filepath.Join(project, ".jad", ".jad")
	mustMkdir(t, legacy, "models")
	mustMkdir(t, project, ".jad", "models") // in both: defaulted
	mustMkdir(t, project, ".jad", "state")  // in both: defaulted
	mustMkdir(t, project, ".jad", "tools")  // named in catalogs, not a default
	mustMkdir(t, project, ".jad", "mcp")    // in both: defaulted
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	if layout.Models != filepath.Join(legacy, "models") || layout.State != filepath.Join(legacy, "state") {
		t.Fatalf("defaults left the legacy root: %s, %s", layout.Models, layout.State)
	}
	if layout.Tools != filepath.Join(project, ".jad", "tools") {
		t.Fatalf("an explicit folder moved: %s", layout.Tools)
	}
	warnings := warningsAbout(config, "Both places hold")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q", config.Warnings)
	}
	warning := warnings[0]
	for _, want := range []string{legacy, filepath.Join(project, ".jad"), "models, mcp, state", "only the doubled folder is read"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning does not say %q: %s", want, warning)
		}
	}
	if strings.Contains(warning, "tools,") || strings.Contains(warning, ", tools") {
		t.Errorf("an explicit catalog is listed as a default: %s", warning)
	}
}

func TestExplicitCatalogFoldersAreNotChangedByTheFix(t *testing.T) {
	for _, withLegacy := range []bool{false, true} {
		project, load := legacyProject(t, `,"catalogs":{"profiles":"p","models":"/abs/m","tools":"tools","mcp":"mcp","remotes":"r","state":"s","sandbox":"sb"}`)
		if withLegacy {
			mustMkdir(t, project, ".jad", ".jad", "profiles")
		}
		config, err := load()
		if err != nil {
			t.Fatal(err)
		}
		layout := config.Roots[0].Layout
		jad := filepath.Join(project, ".jad")
		want := map[string]string{"profiles": filepath.Join(jad, "p"), "models": "/abs/m", "tools": filepath.Join(jad, "tools"), "mcp": filepath.Join(jad, "mcp"), "remotes": filepath.Join(jad, "r"), "state": filepath.Join(jad, "s"), "sandbox": filepath.Join(jad, "sb")}
		if got := catalogFolders(layout); !reflect.DeepEqual(got, want) {
			t.Errorf("legacy=%v: folders = %v", withLegacy, got)
		}
		if len(config.Warnings) != 0 {
			t.Errorf("legacy=%v: a setting that names every folder warned: %q", withLegacy, config.Warnings)
		}
	}
	// some named, the rest by default: the named ones stay, the others follow the rule
	project, load := legacyProject(t, `,"catalogs":{"profiles":"profiles","state":"/abs/state"}`)
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	if layout.Profiles != filepath.Join(project, ".jad", "profiles") || layout.State != "/abs/state" || layout.Models != filepath.Join(project, ".jad", "models") {
		t.Fatalf("partly named: %v", catalogFolders(layout))
	}
}

func TestSettingOutsideAJadFolderKeepsItsDefaults(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	mustMkdir(t, project, ".jad", ".jad")
	if err := os.WriteFile(filepath.Join(project, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(LoadOptions{WorkingDir: project, Environment: map[string]string{"HOME": filepath.Join(base, "home")}})
	if err != nil {
		t.Fatal(err)
	}
	for kind, folder := range catalogFolders(config.Roots[0].Layout) {
		if want := filepath.Join(project, ".jad", kind); folder != want {
			t.Errorf("%s = %s, want %s", kind, folder, want)
		}
	}
	if len(config.Warnings) != 0 {
		t.Errorf("warnings = %q", config.Warnings)
	}
}

func TestADoubledNameThatIsNotAFolderIsNotALegacyRoot(t *testing.T) {
	project, load := legacyProject(t, "")
	if err := os.WriteFile(filepath.Join(project, ".jad", ".jad"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(project, ".jad", "profiles"); config.Roots[0].Layout.Profiles != want || len(config.Warnings) != 0 {
		t.Fatalf("profiles = %s, warnings = %q", config.Roots[0].Layout.Profiles, config.Warnings)
	}
}

func TestALinkedDoubledFolderIsALegacyRoot(t *testing.T) {
	project, load := legacyProject(t, "")
	target := filepath.Join(filepath.Dir(project), "elsewhere")
	mustMkdir(t, target)
	if err := os.Symlink(target, filepath.Join(project, ".jad", ".jad")); err != nil {
		t.Skip("no symlinks here")
	}
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(project, ".jad", ".jad", "profiles"); config.Roots[0].Layout.Profiles != want || len(warningsAbout(config, "doubled")) != 1 {
		t.Fatalf("profiles = %s, warnings = %q", config.Roots[0].Layout.Profiles, config.Warnings)
	}
}

func TestHomeJadSettingGetsTheSameRule(t *testing.T) {
	base := t.TempDir()
	home, cwd := filepath.Join(base, "home"), filepath.Join(base, "elsewhere")
	mustMkdir(t, home, ".jad")
	mustMkdir(t, cwd)
	if err := os.WriteFile(filepath.Join(home, ".jad", "setting.json"), []byte(`{"session_roots":[{"id":"home","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(LoadOptions{WorkingDir: cwd, Environment: map[string]string{"HOME": home}})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".jad", "models"); config.Roots[0].Layout.Models != want {
		t.Fatalf("models = %s, want %s", config.Roots[0].Layout.Models, want)
	}
}

// A workspace layout never reads a setting.json, so the rule and its warning are v1 only.
func TestAV2StartIgnoresTheLegacyDefaultRule(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	wsPut(t, filepath.Join(w, ".jad", "setting.json"), `{"session_roots":[{"id":"x","path":"s"}]}`)
	mustMkdir(t, w, ".jad", ".jad", "models")
	config := wsMustLoad(t, f, w)
	if want := filepath.Join(w, ".jad", "profiles"); config.Roots[0].Layout.Profiles != want {
		t.Fatalf("v2 profiles = %s, want %s", config.Roots[0].Layout.Profiles, want)
	}
	if want := filepath.Join(f.global, "models"); config.Roots[0].Layout.Models != want {
		t.Fatalf("v2 models = %s, want %s", config.Roots[0].Layout.Models, want)
	}
	if len(warningsAbout(config, "doubled")) != 0 {
		t.Fatalf("v2 warned of the doubled folder: %q", config.Warnings)
	}
}

func TestMigrationPlanOfAJadSettingWithoutCatalogsPlansFromTheFixedFolders(t *testing.T) {
	explicit := newMigrateFixture(t)
	want := planKey(explicit.plan(t, ""))

	f := newMigrateFixture(t)
	delete(f.setting, "catalogs")
	f.writeSetting(t)
	plan := f.plan(t, "")
	if got := planKey(plan); !reflect.DeepEqual(got, mapOfSuffixes(want, explicit.base, f.base)) {
		t.Fatalf("plan differs from the one with explicit folders:\n got %v\nwant %v", got, want)
	}
	if hasBlocker(plan, migrationBlockPathBase) {
		t.Fatalf("blockers: %v", blockerKinds(plan))
	}
}

// legacyMigrateFixture is the migration fixture with no `catalogs` and its profiles, models,
// tools, mcp and state in the doubled <.jad>/.jad folder, where v1 reads them.
func legacyMigrateFixture(t *testing.T) (*migrateFixture, string) {
	t.Helper()
	f := newMigrateFixture(t)
	delete(f.setting, "catalogs")
	f.writeSetting(t)
	legacy := filepath.Join(f.jad, ".jad")
	mustMkdir(t, legacy)
	for _, kind := range []string{"profiles", "models", "tools", "mcp", "state"} {
		if err := os.Rename(filepath.Join(f.jad, kind), filepath.Join(legacy, kind)); err != nil {
			t.Fatal(err)
		}
	}
	return f, legacy
}

func TestMigrationPlanOfAJadSettingWithADoubledFolderPlansFromIt(t *testing.T) {
	f, legacy := legacyMigrateFixture(t)
	before := snapshotTree(t, f.workspace)
	plan := f.plan(t, "")
	if !reflect.DeepEqual(before, snapshotTree(t, f.workspace)) {
		t.Fatal("the plan wrote to the workspace")
	}
	profile := fileOf(t, plan, filepath.Join(legacy, "profiles", "dev", "coder.json"))
	if profile.Destination != filepath.Join(f.jad, "profiles", "dev", "coder.json") || profile.Action != MigrationCopy {
		t.Fatalf("profile = %+v", profile)
	}
	state := fileOf(t, plan, filepath.Join(legacy, "state", "work", "dev", "coder", "messages.jsonl"))
	if state.Destination != filepath.Join(f.jad, "state", "work", "dev", "coder", "messages.jsonl") {
		t.Fatalf("state = %+v", state)
	}
	if len(warningsAbout(Config{Warnings: plan.Warnings}, legacy)) != 1 {
		t.Fatalf("the plan does not carry the doubled-folder warning: %q", plan.Warnings)
	}
}

// planKey is the planned files as "group source -> destination action", the paths relative
// to the workspace so two fixtures compare.
func planKey(plan MigrationPlan) map[string]string {
	keys := map[string]string{}
	for _, file := range plan.Files {
		keys[file.Group+" "+file.Source] = file.Destination + " " + string(file.Action)
	}
	return keys
}

func mapOfSuffixes(keys map[string]string, from, to string) map[string]string {
	out := make(map[string]string, len(keys))
	for key, value := range keys {
		out[strings.ReplaceAll(key, from, to)] = strings.ReplaceAll(value, from, to)
	}
	return out
}

// When the doubled folder cannot be inspected the defaults stay where they were, with a warning,
// rather than pick a place that may hide data. A NUL in the path makes the inspection fail.
func TestAnUninspectableDoubledFolderKeepsTheOldDefaultsWithAWarning(t *testing.T) {
	settingPath := filepath.Join(t.TempDir(), "bad\x00name", ".jad", "setting.json")
	folders, warnings := catalogFolderDefaults(settingPath, map[string]string{})
	if folders["profiles"] != ".jad/profiles" || len(warnings) != 1 || !strings.Contains(warnings[0], "cannot tell whether") {
		t.Fatalf("folders = %v, warnings = %q", folders, warnings)
	}
}

// An apply (in a temporary base) from a legacy root copies the files from it, leaves it as
// it was, and the result loads as v2 with the data in W/.jad.
func TestApplyFromTheDoubledFolderCopiesAndLeavesItUntouched(t *testing.T) {
	f, legacy := legacyMigrateFixture(t)
	before := hashes(t, f.base)
	result, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, result.Rollback)
	}
	unchanged(t, before, hashes(t, f.base))
	for _, rel := range []string{"profiles/dev/coder.json", "state/work/dev/coder/messages.jsonl"} {
		want, err := os.ReadFile(filepath.Join(legacy, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(f.jad, rel))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s was not copied byte for byte: %v", rel, err)
		}
	}
	config, err := Load(LoadOptions{WorkingDir: f.workspace, Environment: map[string]string{"HOME": f.home}})
	if err != nil || len(config.Workspaces) == 0 {
		t.Fatalf("v2 load: %v %+v", err, config.Workspaces)
	}
}
