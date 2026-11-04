package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

func testTarget(t *testing.T) (Target, layouttest.Fixture) {
	t.Helper()
	fixture := layouttest.Write(t, layouttest.Spec{})
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{RootSettingPath: fixture.RootSetting})
	if err != nil {
		t.Fatal(err)
	}
	return Target{
		Layout:         *loaded.Roots[0].Layout,
		Roots:          loaded.Roots,
		SkillDir:       filepath.Join(fixture.Root, "prompts", "skills"),
		SkillMaxBytes:  1 << 20,
		ValidateServer: mcpclient.ValidateServerFile,
		ReadSkill: func(ctx context.Context, directory, name string, maxBytes int64) error {
			if _, ok, err := contentlibrary.ReadSkill(ctx, []string{directory}, name, maxBytes); err != nil || !ok {
				return errors.Join(err, errors.New("not a valid skill"))
			}
			return nil
		},
	}, fixture
}

// writePlugin writes a plugin folder from relative paths to contents.
func writePlugin(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func acmePlugin(version string) map[string]string {
	return map[string]string{
		"plugin.json":                 `{"schema_version":1,"name":"acme","version":"` + version + `","description":"Review tools"}`,
		"README.md":                   "# acme\n",
		"mcp/docs.json":               `{"transport":{"type":"stdio","command":"${JAD_PLUGIN_FILES}/server.sh"}}`,
		"tools/review.json":           `{"schema_version":1,"mcp":{"acme/docs":{"resources":true}}}`,
		"profiles/reviewer.json":      `{"models":["openai/default"],"model_default":"openai/default","toolset":"acme/review"}`,
		"skills/acme-review/SKILL.md": "---\nname: acme-review\ndescription: Review with acme.\n---\nRun ./check.sh.\n",
		"skills/acme-review/check.sh": "#!/bin/sh\necho ok\n",
		"files/server.sh":             "#!/bin/sh\nexec acme-mcp\n",
	}
}

func TestInstallPlacesEveryPartAndRemoveTakesItBack(t *testing.T) {
	target, _ := testTarget(t)
	source := writePlugin(t, acmePlugin("1.0.0"))
	result, err := target.Install(context.Background(), source, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upgraded != nil || len(result.Record.Entries) != 5 || result.Record.Version != "1.0.0" {
		t.Fatalf("result = %+v", result)
	}

	// The MCP server names the installed files folder; the script keeps its mode.
	server, err := os.ReadFile(filepath.Join(target.Layout.MCP, "acme", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Transport struct{ Command string } `json:"transport"`
	}
	if json.Unmarshal(server, &file) != nil || file.Transport.Command != filepath.Join(target.Layout.State, ".plugins", "acme", "files", "server.sh") {
		t.Fatalf("installed MCP server = %s", server)
	}
	if info, err := os.Stat(file.Transport.Command); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed script: %v %v", info, err)
	}
	for _, path := range []string{
		filepath.Join(target.Layout.Tools, "acme", "review.json"),
		filepath.Join(target.Layout.Profiles, "acme", "reviewer.json"),
		filepath.Join(target.SkillDir, "acme-review", "SKILL.md"),
		filepath.Join(target.SkillDir, "acme-review", "check.sh"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("not installed: %v", err)
		}
	}
	// The installed profile loads in a session like any other.
	if _, err := sessionfs.CheckLayout(target.Roots, mcpclient.ValidateServerFile); err != nil {
		t.Fatal(err)
	}
	report, _ := sessionfs.CheckLayout(target.Roots, mcpclient.ValidateServerFile)
	if len(report.Problems) != 0 {
		t.Fatalf("jad check after install: %+v", report.Problems)
	}

	statuses, err := target.List()
	if err != nil || len(statuses) != 1 || statuses[0].Record.Name != "acme" || len(statuses[0].Modified) != 0 {
		t.Fatalf("List() = %+v, %v", statuses, err)
	}

	// Installing the same version again is an upgrade in place.
	again, err := target.Install(context.Background(), source, false)
	if err != nil || again.Upgraded == nil || len(again.Removed) != 0 {
		t.Fatalf("reinstall = %+v, %v", again, err)
	}

	removed, err := target.Remove(context.Background(), "acme", false)
	if err != nil || removed.Name != "acme" {
		t.Fatalf("Remove() = %+v, %v", removed, err)
	}
	for _, path := range []string{
		filepath.Join(target.Layout.MCP, "acme"),
		filepath.Join(target.Layout.Tools, "acme"),
		filepath.Join(target.Layout.Profiles, "acme"),
		filepath.Join(target.SkillDir, "acme-review"),
		filepath.Join(target.Layout.State, ".plugins", "acme"),
		filepath.Join(target.Layout.State, ".plugins", "acme.json"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is left after remove: %v", path, err)
		}
	}
	assertNoHiddenLeftovers(t, target)
	if _, err := target.Remove(context.Background(), "acme", false); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("second Remove() = %v", err)
	}
}

func TestUpgradeRemovesWhatTheNewVersionDropsAndKeepsEditsUnlessForced(t *testing.T) {
	target, _ := testTarget(t)
	if _, err := target.Install(context.Background(), writePlugin(t, acmePlugin("1.0.0")), false); err != nil {
		t.Fatal(err)
	}
	// Version 2 drops the skill and the profile and changes the toolset.
	next := acmePlugin("2.0.0")
	delete(next, "skills/acme-review/SKILL.md")
	delete(next, "skills/acme-review/check.sh")
	delete(next, "profiles/reviewer.json")
	next["tools/review.json"] = `{"schema_version":1,"mcp":{"acme/docs":{"prompts":true}}}`

	// A hand edit of an installed file blocks the upgrade, and nothing changes.
	toolset := filepath.Join(target.Layout.Tools, "acme", "review.json")
	if err := os.WriteFile(toolset, []byte(`{"schema_version":1,"mcp":{"acme/docs":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var changed *ChangedError
	if _, err := target.Install(context.Background(), writePlugin(t, next), false); !errors.As(err, &changed) || len(changed.Entries) != 1 || changed.Entries[0].ID != "acme/review" {
		t.Fatalf("upgrade over an edit = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target.SkillDir, "acme-review")); err != nil {
		t.Fatal("a refused upgrade removed the skill")
	}
	statuses, _ := target.List()
	if len(statuses) != 1 || len(statuses[0].Modified) != 1 {
		t.Fatalf("List() after an edit = %+v", statuses)
	}

	result, err := target.Install(context.Background(), writePlugin(t, next), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upgraded == nil || result.Upgraded.Version != "1.0.0" || len(result.Removed) != 2 {
		t.Fatalf("upgrade = %+v", result)
	}
	if data, _ := os.ReadFile(toolset); !strings.Contains(string(data), `"prompts":true`) {
		t.Fatalf("toolset after a forced upgrade = %s", data)
	}
	for _, path := range []string{filepath.Join(target.SkillDir, "acme-review"), filepath.Join(target.Layout.Profiles, "acme")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be gone after the upgrade: %v", path, err)
		}
	}
	assertNoHiddenLeftovers(t, target)
}

// A file that is not the plugin's is never overwritten, and an install that fails its
// check is undone: no file of it is left and the files it would replace are as they were.
func TestInstallRefusesConflictsAndUndoesAFailedCheck(t *testing.T) {
	target, _ := testTarget(t)
	mine := filepath.Join(target.SkillDir, "acme-review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("---\nname: acme-review\ndescription: Mine.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Install(context.Background(), writePlugin(t, acmePlugin("1.0.0")), true); err == nil || !strings.Contains(err.Error(), "was not installed by this plugin") {
		t.Fatalf("install over a skill of the user's = %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(mine)); err != nil {
		t.Fatal(err)
	}

	broken := acmePlugin("1.0.0")
	broken["profiles/reviewer.json"] = `{"models":["openai/missing"],"model_default":"openai/missing","toolset":"acme/review"}`
	if _, err := target.Install(context.Background(), writePlugin(t, broken), false); err == nil || !strings.Contains(err.Error(), "profile acme/reviewer") {
		t.Fatalf("install of a profile naming a missing model = %v", err)
	}
	for _, path := range []string{
		filepath.Join(target.Layout.MCP, "acme"),
		filepath.Join(target.Layout.Tools, "acme"),
		filepath.Join(target.Layout.Profiles, "acme"),
		filepath.Join(target.SkillDir, "acme-review"),
		filepath.Join(target.Layout.State, ".plugins", "acme"),
		filepath.Join(target.Layout.State, ".plugins", "acme.json"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is left after a failed install: %v", path, err)
		}
	}
	assertNoHiddenLeftovers(t, target)

	badSkill := acmePlugin("1.0.0")
	badSkill["skills/acme-review/SKILL.md"] = "# no front matter\n"
	if _, err := target.Install(context.Background(), writePlugin(t, badSkill), false); err == nil || !strings.Contains(err.Error(), "skill acme-review") {
		t.Fatalf("install of an invalid skill = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target.SkillDir, "acme-review")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an invalid skill was left in place")
	}
}

// Removing a plugin whose profile a session uses is refused as a whole.
func TestRemoveIsRefusedWhileASessionNeedsTheProfile(t *testing.T) {
	target, fixture := testTarget(t)
	if _, err := target.Install(context.Background(), writePlugin(t, acmePlugin("1.0.0")), false); err != nil {
		t.Fatal(err)
	}
	session, err := os.ReadFile(fixture.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(session, &fields); err != nil {
		t.Fatal(err)
	}
	fields["scope"] = map[string]any{"profiles": []string{"acme/reviewer"}}
	updated, _ := json.Marshal(fields)
	if err := os.WriteFile(fixture.SessionFile, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Remove(context.Background(), "acme", true); err == nil || !strings.Contains(err.Error(), "needed by session") {
		t.Fatalf("Remove() while a session needs the profile = %v", err)
	}
	statuses, err := target.List()
	if err != nil || len(statuses) != 1 || len(statuses[0].Modified) != 0 {
		t.Fatalf("after a refused remove: %+v, %v", statuses, err)
	}
	assertNoHiddenLeftovers(t, target)
}

func TestReadBundleRefusals(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no manifest", map[string]string{"mcp/a.json": `{}`}, "plugin.json"},
		{"unknown manifest key", map[string]string{"plugin.json": `{"schema_version":1,"name":"a","author":"x"}`}, "unknown field"},
		{"bad version", map[string]string{"plugin.json": `{"schema_version":2,"name":"a"}`}, "schema_version must be 1"},
		{"bad name", map[string]string{"plugin.json": `{"schema_version":1,"name":"a/b"}`}, "becomes the category"},
		{"empty", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`}, "holds no catalog file and no skill"},
		{"stray part", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "agents/x.json": `{}`}, "is not a plugin part"},
		{"nested catalog file", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "mcp/sub/x.json": `{}`}, "must be a file <name>.json"},
		{"not json", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "tools/x.yaml": ``}, "must be a file <name>.json"},
		{"skill without SKILL.md", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "skills/x/notes.md": ``}, "has no SKILL.md"},
		{"placeholder without files", map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "mcp/x.json": `{"transport":{"type":"stdio","command":"${JAD_PLUGIN_FILES}/x"}}`}, "has no files folder"},
	} {
		if _, err := readBundle(writePlugin(t, test.files)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v, want %q", test.name, err, test.want)
		}
	}
	link := writePlugin(t, map[string]string{"plugin.json": `{"schema_version":1,"name":"a"}`, "skills/x/SKILL.md": "x"})
	if err := os.Symlink("/etc/passwd", filepath.Join(link, "skills", "x", "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundle(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a link in a skill: %v", err)
	}
}

// assertNoHiddenLeftovers fails when a staging or backup folder was left behind.
func assertNoHiddenLeftovers(t *testing.T, target Target) {
	t.Helper()
	for _, folder := range []string{target.SkillDir, filepath.Join(target.Layout.State, ".plugins"), filepath.Join(target.Layout.State, ".plugins", "acme")} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".old-") || strings.Contains(entry.Name(), ".new-") || strings.Contains(entry.Name(), ".tmp-") {
				t.Fatalf("left behind: %s", filepath.Join(folder, entry.Name()))
			}
		}
	}
}

// plugin.schema.json accepts the manifests ReadManifest accepts.
func TestManifestSchemaMatchesTheReader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/plugin.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	validator, err := tools.CompileSchema(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []string{
		`{"schema_version":1,"name":"acme"}`,
		`{"schema_version":1,"name":"a.b_c-1","version":"2","description":"d"}`,
		`{"schema_version":2,"name":"acme"}`,
		`{"name":"acme"}`,
		`{"schema_version":1}`,
		`{"schema_version":1,"name":""}`,
		`{"schema_version":1,"name":".acme"}`,
		`{"schema_version":1,"name":"a/b"}`,
		`{"schema_version":1,"name":"acme","author":"x"}`,
		`{"schema_version":1,"name":"acme","version":2}`,
	} {
		folder := writePlugin(t, map[string]string{ManifestName: manifest})
		issues, err := validator.Validate([]byte(manifest))
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := ReadManifest(folder)
		if schemaOK, readerOK := len(issues) == 0, readErr == nil; schemaOK != readerOK {
			t.Errorf("%s: schema accepts %v, reader accepts %v (%v)", manifest, schemaOK, readerOK, readErr)
		}
	}
}
