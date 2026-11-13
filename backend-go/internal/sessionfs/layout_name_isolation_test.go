package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// A catalog file or folder whose name is not an identifier ("file (1).json", the copy a file
// manager makes) is left out of its kind with a warning that names the path. It is in no
// catalog, so it is not listed, offered, selectable or run; every other file of the kind, and
// every session that reads the kind, keeps working. These tests pin that for each kind, that an
// explicit reference to such a name is still refused, that a rename brings the file back, and
// that none of it loosens a security check.

// nameFixture is a session on a profile with a toolset that uses an MCP server, plus a second
// profile, so every catalog kind has a valid file to keep and a place for a bad name.
func nameFixture(t *testing.T, spec layouttest.Spec) (Location, layouttest.Fixture) {
	t.Helper()
	if spec.Native == nil {
		spec.Native = map[string]any{"read_image": map[string]any{}}
		spec.MCP = map[string]map[string]any{"srv/one": {}}
	}
	if spec.Profiles == nil {
		spec.Profiles = map[string]map[string]any{
			"dev/coder":    {"models": []string{"openai/default"}, "toolset": []string{"coding/default"}},
			"review/other": {"models": []string{"openai/default"}},
		}
	}
	return layoutLocation(t, spec)
}

// badNameFiles are files each kind's scan must leave out, keyed by the folder under the root.
var badNameFiles = map[string][]string{
	"profiles": {"dev/file (1).json", "loose.json", "bad cat/x.json"},
	"models":   {"openai/default (copy).json", "loose.json", "bad cat/x.json"},
	"tools":    {"coding/default (1).json", "loose.json", "bad cat/x.json"},
	"mcp":      {"srv/one (1).json", "loose.json", "bad cat/x.json"},
}

func plantBadNames(t *testing.T, fixture layouttest.Fixture, kinds ...string) []string {
	t.Helper()
	var paths []string
	for _, kind := range kinds {
		root := catalogBase(fixture, kind)
		for _, name := range badNameFiles[kind] {
			path := filepath.Join(root, kind, filepath.FromSlash(name))
			// A valid-looking body: only the name is wrong, so a skip cannot be a content failure.
			writeFileT(t, path, `{"schema_version":1}`)
			if folder, _, nested := strings.Cut(name, "/"); nested && strings.Contains(folder, " ") {
				path = filepath.Join(root, kind, folder) // a bad folder is left out whole, and named
			}
			paths = append(paths, path)
		}
	}
	return paths
}

// catalogBase is the folder that holds the catalog kind's folder: the workspace's .jad for the
// profiles, the global folder for the shared catalogs.
func catalogBase(fixture layouttest.Fixture, kind string) string {
	if kind == "profiles" {
		return fixture.Root
	}
	return fixture.GlobalDir
}

func plantIgnoredFiles(t *testing.T, fixture layouttest.Fixture) {
	t.Helper()
	for _, kind := range []string{"profiles", "models", "tools", "mcp"} {
		root := catalogBase(fixture, kind)
		writeFileT(t, filepath.Join(root, kind, "README.md"), "notes")
		writeFileT(t, filepath.Join(root, kind, "dev", "notes.txt"), "notes")
		writeFileT(t, filepath.Join(root, kind, ".hidden", "x.json"), "{")
		writeFileT(t, filepath.Join(root, kind, "dev", ".draft.json"), "{")
		if err := os.MkdirAll(filepath.Join(root, kind, "dev", "folder.json"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertWarnsFor(t *testing.T, config SessionConfig, kind string, paths []string) {
	t.Helper()
	for _, path := range paths {
		if !hasWarning(config, kind+" file "+path+" left out") {
			t.Errorf("no warning names %s; warnings = %q", path, config.Warnings)
		}
	}
}

func TestBadFileNamesInEveryKindDoNotFailTheSession(t *testing.T) {
	for _, kind := range []string{"profiles", "models", "tools", "mcp"} {
		t.Run(kind, func(t *testing.T) {
			location, fixture := nameFixture(t, layouttest.Spec{})
			paths := plantBadNames(t, fixture, kind)
			plantIgnoredFiles(t, fixture)

			config, err := LoadSessionConfig(location, "", "")
			if err != nil {
				t.Fatalf("a bad file name failed the load: %v", err)
			}
			singular := map[string]string{"profiles": "profile", "models": "model", "tools": "toolset", "mcp": "mcp"}[kind]
			assertWarnsFor(t, config, singular, paths)
			if want := []string{"dev/coder", "review/other"}; !reflect.DeepEqual(config.Profiles, want) {
				t.Errorf("Profiles = %v, want %v", config.Profiles, want)
			}
			if want := []string{"openai/default"}; !reflect.DeepEqual(config.Models, want) {
				t.Errorf("Models = %v, want %v", config.Models, want)
			}
			if config.Profile != "dev/coder" || config.ModelDefault != "openai/default" {
				t.Errorf("profile %q, model %q", config.Profile, config.ModelDefault)
			}
			// The files that are not configuration are ignored without a word.
			for _, warning := range config.Warnings {
				for _, quiet := range []string{"README.md", "notes.txt", ".hidden", ".draft.json", "folder.json"} {
					if strings.Contains(warning, quiet) {
						t.Errorf("a file that is not configuration was reported: %s", warning)
					}
				}
			}
			info, err := ReadLayoutProfileInfo(location)
			if err != nil || !reflect.DeepEqual(info.Profiles, []string{"dev/coder", "review/other"}) || info.ActiveProfile != "dev/coder" {
				t.Errorf("profile info = %+v, %v", info, err)
			}
			// The other profile still loads, and a request by name works.
			if other, err := LoadSessionConfig(location, "review/other", ""); err != nil || other.Profile != "review/other" {
				t.Errorf("review/other: %+v %v", other.Profile, err)
			}
		})
	}
}

// A name that is bad is not a way to a different profile, model or toolset: the file is never
// the default even when it sorts first, never run, never merged, and asking for it is refused.
func TestASkippedProfileIsNeverSubstitutedOrRun(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{
		SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)},
		Settings:         map[string]any{"sandbox": "box"},
	})
	// "a b" sorts before "coder"; its body asks for no sandbox and another model.
	loose := filepath.Join(fixture.Root, "profiles", "dev", "a b.json")
	writeFileT(t, loose, `{"models":["openai/default"],"settings":{"sandbox":{"enabled":false}}}`)

	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Profile != "dev/coder" || config.Sandbox == nil {
		t.Fatalf("profile %q, sandbox %+v: the skipped file's settings were applied", config.Profile, config.Sandbox)
	}
	if !hasWarning(config, "profile file "+loose+" left out", "name") {
		t.Errorf("warnings = %q", config.Warnings)
	}
	if _, err := LoadSessionConfig(location, "dev/a b", ""); err == nil {
		t.Error("the skipped profile was accepted by name")
	}
}

// Every explicit reference to a name that fails the rule is still refused, with the same error
// as for any malformed reference, and never answered with another file.
func TestExplicitReferencesToABadNameAreStillRefused(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{})
	plantBadNames(t, fixture, "profiles", "models", "tools", "mcp")

	if _, err := LoadSessionConfig(location, "dev/file (1)", ""); err == nil || !strings.Contains(err.Error(), "must be written <category>/<name>") {
		t.Errorf("requested profile: err = %v", err)
	}
	if _, err := LoadSessionConfig(location, "", "openai/default (copy)"); err == nil {
		t.Error("a requested model with a skipped name was accepted")
	}
	// A profile that lists a skipped model, toolset or server by name is itself left out (the
	// profile is the broken file) and named profile loads fail with the reference's error.
	for _, c := range []struct {
		name  string
		file  map[string]any
		wants string
	}{
		{"model", map[string]any{"models": []string{"openai/default (copy)"}}, "openai/default (copy)"},
		{"toolset", map[string]any{"models": []string{"openai/default"}, "toolset": []string{"coding/default (1)"}}, "coding/default (1)"},
	} {
		editJSON(t, filepath.Join(fixture.Root, "profiles", "review", "other.json"), func(m map[string]any) {
			delete(m, "toolset")
			for k, v := range c.file {
				m[k] = v
			}
		})
		if _, err := LoadSessionConfig(location, "review/other", ""); err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: err = %v, want it to name %q", c.name, err, c.wants)
		}
		config, err := LoadSessionConfig(location, "", "")
		if err != nil || config.Profile != "dev/coder" || !hasWarning(config, "profile review/other left out") {
			t.Errorf("%s: default load: %v %+v", c.name, err, config.Warnings)
		}
	}
	// The toolset file of the selected profile that names an MCP server by a bad name.
	editJSON(t, filepath.Join(fixture.GlobalDir, "tools", "coding", "default.json"), func(m map[string]any) {
		m["mcp"] = map[string]any{"srv/one (1)": map[string]any{}}
	})
	if _, err := LoadSessionConfig(location, "", ""); err == nil || !strings.Contains(err.Error(), "srv/one (1)") {
		t.Errorf("an MCP server named by a bad name: err = %v", err)
	}
}

// When the only profile has a bad name there is nothing to run, and the error says why.
func TestTheOnlyProfileWithABadNameIsReportedByTheLoad(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{Profiles: map[string]map[string]any{"dev/coder": {"models": []string{"openai/default"}}}})
	renamed := filepath.Join(fixture.Root, "profiles", "dev", "coder (1).json")
	if err := os.Rename(filepath.Join(fixture.Root, "profiles", "dev", "coder.json"), renamed); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSessionConfig(location, "", "")
	if err == nil || !strings.Contains(err.Error(), "matches no profile") || !strings.Contains(err.Error(), renamed) {
		t.Fatalf("err = %v, want the missing profile and the left-out file", err)
	}
}

// Renaming the file to a valid name brings it back at the next read, with no restart.
func TestARenamedFileComesBack(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{})
	copyOf := filepath.Join(fixture.Root, "profiles", "review", "other (1).json")
	writeFileT(t, copyOf, `{"models":["openai/default"]}`)
	modelCopy := filepath.Join(fixture.GlobalDir, "models", "openai", "second (1).json")
	writeFileT(t, modelCopy, `{"provider":"openai","model":"gpt-second","api_key_env":"FIXTURE_KEY"}`)

	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(config, copyOf) || !hasWarning(config, modelCopy) {
		t.Fatalf("warnings = %q", config.Warnings)
	}
	layout := *location.Root.Layout
	if listed, _ := ListCatalogFiles(layout, CatalogModels); len(listed) != 1 {
		t.Fatalf("models listed before the rename: %+v", listed)
	}

	if err := os.Rename(modelCopy, filepath.Join(fixture.GlobalDir, "models", "openai", "second.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(copyOf, filepath.Join(fixture.Root, "profiles", "review", "third.json")); err != nil {
		t.Fatal(err)
	}
	config, err = LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContains(config.Profiles, "review/third") || hasWarning(config, "left out: name") {
		t.Fatalf("after the rename: profiles %v, warnings %q", config.Profiles, config.Warnings)
	}
	listed, _ := ListCatalogFiles(layout, CatalogModels)
	if len(listed) != 2 || listed[1].ID != "openai/second" {
		t.Fatalf("models listed after the rename: %+v", listed)
	}
	// And a profile can now offer the renamed model.
	editJSON(t, filepath.Join(fixture.Root, "profiles", "review", "third.json"), func(m map[string]any) {
		m["models"] = []string{"openai/second"}
	})
	if third, err := LoadSessionConfig(location, "review/third", ""); err != nil || third.ModelDefault != "openai/second" {
		t.Fatalf("review/third: model %q, %v", third.ModelDefault, err)
	}
}

func slicesContains(list []string, item string) bool {
	for _, have := range list {
		if have == item {
			return true
		}
	}
	return false
}

// The catalog listing shows only files that can be opened by their id: a bad name is not a row,
// and the other rows of the kind are still listed.
func TestCatalogListingLeavesOutBadNamesAndKeepsTheRest(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{})
	plantBadNames(t, fixture, "profiles", "models", "tools", "mcp")
	layout := *location.Root.Layout
	want := map[CatalogKind][]string{
		CatalogProfiles: {"dev/coder", "review/other"},
		CatalogModels:   {"openai/default"},
		CatalogToolsets: {"coding/default"},
		CatalogMCP:      {"srv/one"},
	}
	for kind, ids := range want {
		files, err := ListCatalogFiles(layout, kind)
		if err != nil {
			t.Fatalf("%s: a bad name failed the listing: %v", kind, err)
		}
		var got []string
		for _, file := range files {
			got = append(got, file.ID)
			if file.Error != "" || file.ETag == "" {
				t.Errorf("%s %s: row = %+v", kind, file.ID, file)
			}
		}
		if !reflect.DeepEqual(got, ids) {
			t.Errorf("%s: listed %v, want %v", kind, got, ids)
		}
	}
	// The skipped file cannot be opened, replaced or deleted through the catalog by its name.
	for _, id := range []string{"dev/file (1)", "loose"} {
		if _, err := ReadCatalogFile(layout, CatalogProfiles, id); !isBadID(err) {
			t.Errorf("read %q: err = %v, want a bad id", id, err)
		}
		if err := DeleteCatalogFile(layout, []Root{location.Root}, CatalogProfiles, id, "*"); !isBadID(err) {
			t.Errorf("delete %q: err = %v, want a bad id", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.Root, "profiles", "dev", "file (1).json")); err != nil {
		t.Errorf("a refused delete removed the file: %v", err)
	}
}

func isBadID(err error) bool {
	var catalogErr *CatalogError
	return errors.As(err, &catalogErr) && catalogErr.Kind == CatalogBadID
}

// jad check names each bad file on its own, as the loader's warning words it, and still checks
// every other file; a clean layout reports nothing.
func TestCheckNamesEachBadFileAndStillChecksTheRest(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{})
	accept := func(string, []byte) error { return nil }
	roots := []Root{location.Root}

	clean, err := CheckLayout(roots, accept)
	if err != nil || len(clean.Problems) != 0 {
		t.Fatalf("clean layout: %+v, %v", clean.Problems, err)
	}
	paths := plantBadNames(t, fixture, "profiles", "models", "tools", "mcp")
	plantIgnoredFiles(t, fixture)
	// A real fault in a valid file is reported next to the names, so the names do not hide it.
	corrupt(t, profileFileOf(fixture, "review/other"))

	report, err := CheckLayout(roots, accept)
	if err != nil {
		t.Fatal(err)
	}
	reported := map[string]CheckProblem{}
	for _, problem := range report.Problems {
		reported[problem.ID] = problem
		if problem.ID == profileFileOf(fixture, "review/other") {
			t.Errorf("a content fault was filed under a path: %+v", problem)
		}
	}
	for _, path := range paths {
		problem, found := reported[path]
		if !found || !strings.Contains(problem.Message, "left out") || !strings.Contains(problem.Message, path) {
			t.Errorf("no problem names %s; problems = %+v", path, report.Problems)
		}
	}
	if _, found := reported["review/other"]; !found {
		t.Errorf("the broken review/other file was not checked; problems = %+v", report.Problems)
	}
	if want := len(paths) + 1; len(report.Problems) < want {
		t.Errorf("%d problems, want at least %d", len(report.Problems), want)
	}
	for _, problem := range report.Problems {
		for _, quiet := range []string{"README.md", "notes.txt", ".hidden", ".draft.json", "folder.json"} {
			if strings.Contains(problem.Message, quiet) {
				t.Errorf("a file that is not configuration was reported: %+v", problem)
			}
		}
	}
	if report.Checked <= clean.Checked {
		t.Errorf("Checked = %d, was %d without the bad names", report.Checked, clean.Checked)
	}
}

// The in-use guards read the other kinds' folders. A bad name there must neither refuse the
// delete (the file it refuses is not the one being deleted) nor, worse, hide a real user.
func TestDeleteGuardsSeeRealUsersWhenAFolderHasBadNames(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{
		Models: map[string]map[string]any{
			"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY"},
			"openai/spare":   {"provider": "openai", "model": "gpt-spare", "api_key_env": "FIXTURE_KEY"},
		},
		Profiles: map[string]map[string]any{
			"dev/coder": {"models": []string{"openai/default"}, "toolset": []string{"coding/default"}, "settings": map[string]any{"sandbox": "box"}},
		},
		SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil), "spare": envSandbox(nil)},
	})
	plantBadNames(t, fixture, "profiles", "models", "tools", "mcp")
	layout := *location.Root.Layout
	roots := []Root{location.Root}
	inUse := func(kind CatalogKind, id string) {
		t.Helper()
		file, err := ReadCatalogFile(layout, kind, id)
		if err != nil {
			t.Fatalf("read %s %s: %v", kind, id, err)
		}
		var refused *CatalogError
		if err := DeleteCatalogFile(layout, roots, kind, id, file.ETag); !errors.As(err, &refused) || refused.Kind != CatalogInUse {
			t.Errorf("delete %s %s: err = %v, want in use", kind, id, err)
		}
	}
	inUse(CatalogModels, "openai/default")
	inUse(CatalogToolsets, "coding/default")
	inUse(CatalogMCP, "srv/one")
	inUse(CatalogProfiles, "dev/coder")
	// The sandbox guard used to drop its profile and toolset groups when their folder had a bad
	// name, which let a template in use be deleted.
	inUse(CatalogSandboxes, "box")

	for _, c := range []struct {
		kind CatalogKind
		id   string
	}{{CatalogModels, "openai/spare"}, {CatalogSandboxes, "spare"}} {
		file, err := ReadCatalogFile(layout, c.kind, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := DeleteCatalogFile(layout, roots, c.kind, c.id, file.ETag); err != nil {
			t.Errorf("delete the unused %s %s: %v", c.kind, c.id, err)
		}
	}
}

// A profile saved while a model, toolset or server folder holds a bad name is judged on the
// files that can be named, so one stray copy does not block every edit.
func TestSavingAProfileIgnoresBadNamesInTheFoldersItReads(t *testing.T) {
	location, fixture := nameFixture(t, layouttest.Spec{})
	plantBadNames(t, fixture, "models", "tools")
	layout := *location.Root.Layout
	body := []byte(`{"models":["openai/default"],"toolset":["coding/default"]}`)
	if _, _, err := WriteCatalogFile(layout, []Root{location.Root}, CatalogProfiles, "dev/new", body, WriteCondition{IfNoneMatch: "*"}, false, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Naming the bad one is still an error, as for any file that is not there.
	bad := []byte(`{"models":["openai/default (copy)"]}`)
	if _, _, err := WriteCatalogFile(layout, []Root{location.Root}, CatalogProfiles, "dev/bad", bad, WriteCondition{IfNoneMatch: "*"}, false, nil); err == nil {
		t.Fatal("a profile naming a bad file name was saved")
	}
}
