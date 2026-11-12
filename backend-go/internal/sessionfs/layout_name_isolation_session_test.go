package sessionfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// A sandbox template file or a session file whose name cannot be used ("base (1).json", the copy a
// file manager makes) is warned about and checked, like the catalog files of layout_name_isolation_test.go.
// The sandbox folder holds templates only, so every .json in it is one. A session root may hold other
// JSON, so a file there is reported only when its content is a session. Neither is ever loaded under
// that name, and nothing that loaded before stops loading.

func templateDir(fixture layouttest.Fixture) string {
	return filepath.Join(fixture.Root, ".jad", "sandbox")
}

func plantTemplateExtras(t *testing.T, dir string) {
	t.Helper()
	writeFileT(t, filepath.Join(dir, "README.md"), "notes")
	writeFileT(t, filepath.Join(dir, ".draft.json"), "{")
	if err := os.MkdirAll(filepath.Join(dir, "folder.json"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMisnamedSandboxTemplatesAreWarnedAboutAndNeverApplied(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"template in use":     {"sandbox": "box"},
		"no template in use":  {},
		"inline sandbox only": {"sandbox": envSandbox(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			location, fixture := layoutLocation(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: settings})
			dir := templateDir(fixture)
			// A misnamed copy that would turn the sandbox off, if it were ever read.
			copyPath := filepath.Join(dir, "box (1).json")
			writeFileT(t, copyPath, `{"enabled":false}`)
			writeFileT(t, filepath.Join(dir, "other copy.json"), `{"enabled":false}`)
			plantTemplateExtras(t, dir)

			config, err := LoadSessionConfig(location, "", "")
			if err != nil {
				t.Fatalf("a misnamed template failed the load: %v", err)
			}
			for _, path := range []string{copyPath, filepath.Join(dir, "other copy.json")} {
				if !hasWarning(config, "sandbox template file "+path+" left out") {
					t.Errorf("no warning names %s; warnings = %q", path, config.Warnings)
				}
			}
			if settings["sandbox"] != nil && config.Sandbox == nil {
				t.Errorf("the sandbox of the real template was lost: %+v", config.Sandbox)
			}
			for _, warning := range config.Warnings {
				for _, quiet := range []string{"README.md", ".draft.json", "folder.json"} {
					if strings.Contains(warning, quiet) {
						t.Errorf("a file that is not a template was reported: %s", warning)
					}
				}
			}
		})
	}
}

func TestAMisnamedSandboxTemplateCannotBeNamedAndTheMissingOneIsExplained(t *testing.T) {
	// Naming the bad name is refused on the name, as before.
	_, fixture := layoutLocation(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: map[string]any{"sandbox": "box (1)"}})
	writeFileT(t, filepath.Join(templateDir(fixture), "box (1).json"), `{"enabled":false}`)
	if _, err := loadSandboxFixture(t, fixture); err == nil || !strings.Contains(err.Error(), "sandbox template name") {
		t.Fatalf("a template named by its bad name: %v", err)
	}

	// The only copy of a template has the bad name: the template is missing, and the error says
	// which file to rename.
	fixture = layouttest.Write(t, layouttest.Spec{Settings: map[string]any{"sandbox": "base"}})
	writeFileT(t, filepath.Join(templateDir(fixture), "base (1).json"), `{"enabled":false}`)
	_, err := loadSandboxFixture(t, fixture)
	if err == nil || !strings.Contains(err.Error(), "sandbox template \"base\" is not in") || !strings.Contains(err.Error(), "base (1).json") || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("missing template error = %v", err)
	}
	// It never fell back to the file: nothing was applied.
	if err := os.Rename(filepath.Join(templateDir(fixture), "base (1).json"), filepath.Join(templateDir(fixture), "base.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSandboxFixture(t, fixture); err != nil {
		t.Fatalf("after the rename: %v", err)
	}
}

// A rename must change the stamp of a load that listed the folder, so the session list is not
// answered from its cache.
func TestRenamingAMisnamedTemplateChangesTheStamp(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: map[string]any{"sandbox": "box"}})
	dir := templateDir(fixture)
	writeFileT(t, filepath.Join(dir, "second (1).json"), `{"enabled":false}`)
	backdateTree(t, fixture.Root, fixture.CWD)
	config, stamp, err := LoadSessionSettingsStamped(location, "", "")
	if err != nil || !hasWarning(config, "second (1).json") {
		t.Fatalf("load: %v warnings %q", err, config.Warnings)
	}
	if !stamp.Current() {
		t.Fatal("a fresh stamp is not current")
	}
	if err := os.Rename(filepath.Join(dir, "second (1).json"), filepath.Join(dir, "second.json")); err != nil {
		t.Fatal(err)
	}
	if stamp.Current() {
		t.Fatal("the stamp did not notice the renamed template")
	}
	config, err = LoadSessionConfig(location, "", "")
	if err != nil || hasWarning(config, "left out") {
		t.Fatalf("after the rename: %v warnings %q", err, config.Warnings)
	}
}

// plantSessionFiles writes files into dir: copies of a session under bad names, files that are not
// sessions, and sessions under valid names. It returns the paths a finder must name.
func plantSessionFiles(t *testing.T, dir, cwd string) (misnamed []string) {
	t.Helper()
	sessionBody := `{"schema_version":1,"cwd":` + strconv.Quote(cwd) + `,"scope":{"profiles":["dev"]}}`
	long := strings.Repeat("a", 129)
	for name, body := range map[string]string{
		"my session (1).json":    sessionBody,
		"copy of demo.json":      `{"cwd":` + strconv.Quote(cwd) + `,"name":"Demo"}`,
		long + ".json":           `{"schema_version":1,"cwd":` + strconv.Quote(cwd) + `}`,
		"scoped only (2).json":   `{"scope":{"profiles":["dev"]}}`,
		"settings only (3).json": `{"settings":{"cwd":` + strconv.Quote(cwd) + `}}`,
	} {
		writeFileT(t, filepath.Join(dir, name), body)
		misnamed = append(misnamed, filepath.Join(dir, name))
	}
	for name, body := range map[string]string{
		"package (1).json":                 `{"name":"x","version":"1.0.0","scripts":{}}`,
		"notes (1).json":                   `{"name":"x","description":"y"}`,
		"typo (1).json":                    `{"cwd":` + strconv.Quote(cwd) + `,"bogus":1}`,
		"list (1).json":                    `[1,2]`,
		"empty (1).json":                   ``,
		"broken (1).json":                  `{`,
		"setting (1).json":                 `{"schema_version":1,"session_roots":[{"id":"a","path":"s"}]}`,
		".hidden (1).json":                 sessionBody,
		"readme (1).md":                    sessionBody,
		"upper (1).JSON":                   sessionBody,
		"valid-name.json":                  sessionBody,
		"a.b_c-d.json":                     sessionBody,
		strings.Repeat("b", 128) + ".json": sessionBody,
	} {
		writeFileT(t, filepath.Join(dir, name), body)
	}
	if err := os.MkdirAll(filepath.Join(dir, "folder (1).json"), 0o755); err != nil {
		t.Fatal(err)
	}
	return misnamed
}

func findMisnamed(t *testing.T, finder *MisnamedSessions, root Root) []string {
	t.Helper()
	entries, err := os.ReadDir(root.Directory)
	if err != nil {
		t.Fatal(err)
	}
	return finder.Find(root, entries)
}

func TestOnlyMisnamedFilesThatHoldASessionAreReported(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{})
	misnamed := plantSessionFiles(t, fixture.SessionsDir, fixture.CWD)

	for _, finder := range []*MisnamedSessions{{}, nil} {
		warnings := findMisnamed(t, finder, location.Root)
		if len(warnings) != len(misnamed) {
			t.Fatalf("%d warnings, want %d:\n%s", len(warnings), len(misnamed), strings.Join(warnings, "\n"))
		}
		for _, path := range misnamed {
			found := false
			for _, warning := range warnings {
				found = found || strings.Contains(warning, path+" left out")
			}
			if !found {
				t.Errorf("no warning names %s:\n%s", path, strings.Join(warnings, "\n"))
			}
		}
	}

	// No valid session is lost: the names that route are still sessions, and the misnamed are not.
	files, err := ListSessionFiles([]Root{location.Root})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, file := range files {
		listed[file.ID] = true
	}
	for _, id := range []string{fixture.SessionID, "valid-name", "a.b_c-d", strings.Repeat("b", 128)} {
		if !listed[id] {
			t.Errorf("session %q is not listed; listed = %v", id, listed)
		}
	}
	for id := range listed {
		if strings.Contains(id, " ") || len(id) > 128 || strings.HasPrefix(id, ".") {
			t.Errorf("a misnamed file was listed as a session: %q", id)
		}
	}
	// Each valid session still loads.
	for _, id := range []string{"valid-name", "a.b_c-d"} {
		resolved, err := Resolve([]Root{location.Root}, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSessionFile([]Root{location.Root}, resolved.Reference); err != nil {
			t.Errorf("session %s: %v", id, err)
		}
	}
}

// The finder remembers a file's verdict by size and time and notices a rewrite, a rename and a
// delete at the next listing; the verdicts of a root that lost a file do not pile up.
func TestMisnamedSessionVerdictsFollowTheFile(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{})
	path := filepath.Join(fixture.SessionsDir, "draft (1).json")
	writeFileT(t, path, `{"name":"x"}`)
	old := time.Now().Add(-time.Hour)
	touch := func() {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	touch()
	var finder MisnamedSessions
	if got := findMisnamed(t, &finder, location.Root); len(got) != 0 {
		t.Fatalf("not a session: %v", got)
	}
	// It becomes a session: the size changes, the verdict is read again.
	writeFileT(t, path, `{"schema_version":1,"cwd":`+strconv.Quote(fixture.CWD)+`}`)
	touch()
	if got := findMisnamed(t, &finder, location.Root); len(got) != 1 {
		t.Fatalf("became a session: %v", got)
	}
	// The same size and time: the remembered verdict is used.
	if got := findMisnamed(t, &finder, location.Root); len(got) != 1 {
		t.Fatalf("remembered: %v", got)
	}
	if err := os.Rename(path, filepath.Join(fixture.SessionsDir, "draft.json")); err != nil {
		t.Fatal(err)
	}
	if got := findMisnamed(t, &finder, location.Root); len(got) != 0 {
		t.Fatalf("renamed: %v", got)
	}
	if len(finder.seen) != 0 {
		t.Fatalf("verdicts left for files that are gone or valid: %v", finder.seen)
	}
}

func TestCheckNamesMisnamedSessionsAndTemplatesOnly(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{SandboxTemplates: map[string]map[string]any{"box": envSandbox(nil)}, Settings: map[string]any{"sandbox": "box"}})
	accept := func(string, []byte) error { return nil }
	roots := []Root{location.Root}
	clean, err := CheckLayout(roots, accept)
	if err != nil || len(clean.Problems) != 0 {
		t.Fatalf("clean layout: %+v, %v", clean.Problems, err)
	}
	misnamed := plantSessionFiles(t, fixture.SessionsDir, fixture.CWD)
	dir := templateDir(fixture)
	templates := []string{filepath.Join(dir, "box (1).json"), filepath.Join(dir, "other copy.json")}
	for _, path := range templates {
		writeFileT(t, path, `{"enabled":false}`)
	}
	plantTemplateExtras(t, dir)

	report, err := CheckLayout(roots, accept)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, problem := range report.Problems {
		text.WriteString(problem.Kind + " " + problem.ID + ": " + problem.Message + "\n")
	}
	for _, path := range append(append([]string(nil), misnamed...), templates...) {
		if strings.Count(text.String(), path+" left out") != 1 {
			t.Errorf("%s is not reported exactly once:\n%s", path, text.String())
		}
	}
	if want := len(misnamed) + len(templates); len(report.Problems) != want {
		t.Errorf("%d problems, want %d:\n%s", len(report.Problems), want, text.String())
	}
	for _, quiet := range []string{"package (1)", "notes (1)", "typo (1)", "list (1)", "empty (1)", "broken (1)", "setting (1)", ".hidden", "readme", "upper", "folder", "README.md", ".draft.json"} {
		if strings.Contains(text.String(), quiet) {
			t.Errorf("%q was reported although it is not a session or template:\n%s", quiet, text.String())
		}
	}
	// The valid sessions are still loaded and checked: the real session is among the Checked.
	if report.Checked < clean.Checked+len(misnamed)+len(templates) {
		t.Errorf("Checked = %d, was %d clean", report.Checked, clean.Checked)
	}
}

// With catalogs.sandbox = {cwd}/<folder> the folder is known per session, so check reads it with
// each session's cwd.
func TestCheckReadsACWDSandboxFolderPerSession(t *testing.T) {
	cwd := t.TempDir()
	location, _ := layoutLocation(t, layouttest.Spec{CWD: cwd, SandboxCatalog: "{cwd}/.templates"})
	writeFileT(t, filepath.Join(cwd, ".templates", "base (1).json"), `{"enabled":false}`)
	report, err := CheckLayout([]Root{location.Root}, func(string, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Problems) != 1 || !strings.Contains(report.Problems[0].Message, filepath.Join(cwd, ".templates", "base (1).json")+" left out") {
		t.Fatalf("problems = %+v", report.Problems)
	}
}
