package app

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func (f *layoutFixture) checkFiles(t *testing.T) (bool, string) {
	t.Helper()
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.root, RootSettingPath: filepath.Join(f.root, "setting.json"), Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	valid, err := CheckFiles(loaded.Roots, &out)
	if err != nil {
		t.Fatalf("CheckFiles: %v", err)
	}
	return valid, out.String()
}

func (f *layoutFixture) overwrite(t *testing.T, rel, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, rel), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func treeSnapshot(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				snapshot[path] = "dir"
				return nil
			}
			data, err := os.ReadFile(path)
			snapshot[path] = string(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func TestCheckFilesAcceptsAValidLayoutAndChangesNothing(t *testing.T) {
	f := newLayoutFixture(t)
	before := treeSnapshot(t, f.root, f.cwd)
	valid, out := f.checkFiles(t)
	if !valid || out != "checked 6 files, 0 problems\n" {
		t.Fatalf("valid = %v, output %q", valid, out)
	}
	after := treeSnapshot(t, f.root, f.cwd)
	if len(after) != len(before) {
		t.Fatalf("the check changed the file tree: %d entries before, %d after", len(before), len(after))
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("%s changed", path)
		}
	}
}

func TestCheckFilesReportsEachBrokenFileOnce(t *testing.T) {
	cases := []struct {
		name    string
		rel     string
		text    string
		want    []string
		problem int
	}{
		{"model with an empty base_url", "models/openai/large.json", `{"provider":"openai","model":"m","api_key_env":"FIXTURE_KEY","base_url":""}`, []string{"model openai/large:", "base_url"}, 1},
		{"model that is not JSON", "models/openai/large.json", `{`, []string{"model openai/large:"}, 1},
		{"toolset with an unknown key", "tools/coding/none.json", `{"bogus":1}`, []string{"toolset coding/none:", "bogus"}, 1},
		{"toolset that names a missing server", "tools/coding/none.json", `{"mcp":{"ext/none":{}}}`, []string{"toolset coding/none:", "ext/none"}, 1},
		{"profile that names a missing toolset", "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/missing"}`, []string{"profile review/reviewer:", "coding/missing"}, 1},
		{"profile with an unknown key", "profiles/dev/coder.json", `{"models":["openai/small"],"toolset":"coding/none","bogus":1}`, []string{"profile dev/coder:", "bogus"}, 1},
		{"session with an unknown key", "sessions/demo.json", `{"schema_version":1,"cwd":"/","scope":{"profiles":["dev","review"]},"bogus":1}`, []string{"session demo:", "bogus"}, 1},
		{"session that still names an active profile", "sessions/demo.json", `{"schema_version":1,"cwd":"/","active_profile":"dev/coder"}`, []string{"session demo:", "active_profile"}, 1},
		{"session whose scope matches no profile", "sessions/demo.json", `{"schema_version":1,"cwd":"/","scope":{"profiles":["nothing"]}}`, []string{"session demo:", "nothing"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLayoutFixture(t)
			f.overwrite(t, tc.rel, tc.text)
			valid, out := f.checkFiles(t)
			if valid {
				t.Fatalf("a broken file was accepted: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if got := len(lines) - 1; got != tc.problem {
				t.Errorf("%d problem lines, want %d:\n%s", got, tc.problem, out)
			}
			if want := "problems"; !strings.HasSuffix(lines[len(lines)-1], want) {
				t.Errorf("last line = %q", lines[len(lines)-1])
			}
		})
	}
}

func TestCheckFilesLoadsEveryProfileOfASession(t *testing.T) {
	f := newLayoutFixture(t)
	// Each file is valid alone and the session's active profile loads. Only loading the
	// session with the other profile finds that its toolset sets hooks that the session
	// settings set as well.
	f.overwrite(t, "tools/coding/hooked.json", `{"hooks":{"enabled":true,"timeout_ms":5000,"post_tool":[{"name":"audit","argv":["true"]}]}}`)
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/hooked"}`)
	f.overwrite(t, "sessions/demo.json", `{"schema_version":1,"cwd":"`+f.cwd+`","scope":{"profiles":["dev","review"]},"settings":{"hooks":{"enabled":false}}}`)
	valid, out := f.checkFiles(t)
	if valid || !strings.Contains(out, "session demo (profile review/reviewer):") || !strings.Contains(out, "hooks is set here and in the settings") {
		t.Fatalf("valid = %v, output %q", valid, out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("want one problem line and the summary:\n%s", out)
	}
}

func TestCheckFilesRefusesRootsWithoutTheCatalogLayout(t *testing.T) {
	var out bytes.Buffer
	if _, err := CheckFiles([]sessionfs.Root{{ID: "plain", Directory: t.TempDir()}}, &out); err == nil {
		t.Fatal("roots without the catalog layout were accepted")
	}
}

func TestCheckFilesFindsBrokenSubagentReferences(t *testing.T) {
	cases := []struct {
		name string
		text func(agentsDir string) string
		want string
	}{
		{"agents_dir that does not exist", func(dir string) string {
			return `{"native":{"async":{},"subagent":{"agents_dir":["` + dir + `/missing"]}}}`
		}, "agents_dir"},
		{"agent that names a missing toolset", func(string) string {
			return `{"native":{"async":{},"subagent":{"agents":{"helper":{"description":"d","instructions":"p","model":"openai/small","toolset":"coding/missing"}}}}}`
		}, "coding/missing"},
		{"agent that names a missing model", func(string) string {
			return `{"native":{"async":{},"subagent":{"agents":{"helper":{"description":"d","instructions":"p","model":"openai/missing"}}}}}`
		}, "openai/missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLayoutFixture(t)
			f.overwrite(t, "tools/coding/none.json", tc.text(f.cwd))
			valid, out := f.checkFiles(t)
			if valid || !strings.Contains(out, tc.want) {
				t.Fatalf("valid = %v, output %q, want a problem naming %q", valid, out, tc.want)
			}
		})
	}
}

func TestCheckFilesReportsTwoSessionsThatFailWithTheSameMessage(t *testing.T) {
	f := newLayoutFixture(t)
	// Both sessions fail in the sub-agent's toolset, whose message names the agent and the
	// toolset but not the session, so the two lines are told apart only by the session.
	f.overwrite(t, "tools/coding/hooked.json", `{"hooks":{"enabled":true,"timeout_ms":5000,"post_tool":[{"name":"audit","argv":["true"]}]}}`)
	f.overwrite(t, "tools/coding/none.json", `{"native":{"async":{},"subagent":{"agents":{"helper":{"description":"d","instructions":"p","model":"openai/small","toolset":"coding/hooked"}}}}}`)
	body := `{"schema_version":1,"cwd":"` + f.cwd + `","scope":{"profiles":["dev"]},"settings":{"hooks":{"enabled":false}}}`
	f.overwrite(t, "sessions/demo.json", body)
	f.overwrite(t, "sessions/second.json", body)
	valid, out := f.checkFiles(t)
	if valid {
		t.Fatalf("broken sessions were accepted: %s", out)
	}
	for _, id := range []string{"session demo:", "session second:"} {
		if !strings.Contains(out, id) {
			t.Errorf("output lacks %q:\n%s", id, out)
		}
	}
}

func TestCheckFilesChecksTheOtherSessionsWhenOneCannotBeRead(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "sessions/second.json", `{"schema_version":1,"cwd":"`+f.cwd+`","scope":{"profiles":["dev"]},"bogus":1}`)
	unreadable := filepath.Join(f.root, "sessions", "demo.json")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("the file is readable despite mode 0, as for root")
	}
	valid, out := f.checkFiles(t)
	if valid {
		t.Fatalf("broken sessions were accepted: %s", out)
	}
	for _, want := range []string{"session demo:", "permission denied", "session second:", "bogus"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
