package sessionfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func pathsAgent(t *testing.T, agent string, allowWrite bool) (NativeSubagentAgent, error) {
	t.Helper()
	flag := ""
	if allowWrite {
		flag = `"allow_write":true,`
	}
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{`+flag+`"agents":{"fixer":`+agent+`}}`), "/s/setting.json", "/w", "")
	if err != nil {
		return NativeSubagentAgent{}, err
	}
	return got.Agents[0], nil
}

func TestSubagentPathsAreParsed(t *testing.T) {
	agent, err := pathsAgent(t, `{"tools":["read","write","edit"],"paths":{"allow":["src"," docs/api "],"deny":["src/secret"]}}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(agent.Paths.Allow, "|"); got != "src|docs/api" {
		t.Errorf("allow = %q, want entries trimmed and in order", got)
	}
	if got := strings.Join(agent.Paths.Deny, "|"); got != "src/secret" {
		t.Errorf("deny = %q", got)
	}
	for _, same := range []string{`{"tools":["write"]}`, `{"tools":["write"],"paths":null}`} {
		a, err := pathsAgent(t, same, true)
		if err != nil || a.Paths.Set() {
			t.Errorf("%s: paths = %+v, err = %v; want none", same, a.Paths, err)
		}
	}
	denyOnly, err := pathsAgent(t, `{"tools":["edit"],"paths":{"deny":["x"]}}`, true)
	if err != nil || !denyOnly.Paths.Set() || len(denyOnly.Paths.Allow) != 0 {
		t.Errorf("deny only: %+v, %v", denyOnly.Paths, err)
	}
}

func TestSubagentPathsAreRefusedWhenTheyCannotHold(t *testing.T) {
	for name, tc := range map[string]struct{ agent, want string }{
		"bash with paths":    {`{"tools":["write","bash"],"paths":{"allow":["src"]}}`, "a shell can change any path"},
		"exec with paths":    {`{"tools":["edit","exec"],"paths":{"allow":["src"]}}`, "a shell can change any path"},
		"no change tool":     {`{"tools":["read"],"paths":{"allow":["src"]}}`, "neither write nor edit"},
		"no tools list":      {`{"paths":{"allow":["src"]}}`, "neither write nor edit"},
		"empty object":       {`{"tools":["write"],"paths":{}}`, "at least one allow or deny"},
		"empty lists":        {`{"tools":["write"],"paths":{"allow":[],"deny":[]}}`, "at least one allow or deny"},
		"unknown key":        {`{"tools":["write"],"paths":{"permit":["src"]}}`, "paths.permit is not supported"},
		"not an object":      {`{"tools":["write"],"paths":["src"]}`, "must be a JSON object"},
		"allow not an array": {`{"tools":["write"],"paths":{"allow":"src"}}`, "paths.allow must be an array"},
		"blank entry":        {`{"tools":["write"],"paths":{"allow":["src","  "]}}`, "paths.allow[1] must be a non-empty string"},
		"duplicate entry":    {`{"tools":["write"],"paths":{"deny":["a","a"]}}`, "lists a twice"},
		"non-string entry":   {`{"tools":["write"],"paths":{"allow":[1]}}`, "must be an array of path strings"},
	} {
		if _, err := pathsAgent(t, tc.agent, true); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestSubagentPathsInAnAgentFolder(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "fixer")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "agent.json"), []byte(`{"tools":["write"],"paths":{"allow":["src"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"allow_write":true,"agents_dir":[`+strconv.Quote(dir)+`]}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if paths := got.Agents[0].Paths; len(paths.Allow) != 1 || paths.Allow[0] != "src" {
		t.Fatalf("folder agent paths = %+v", paths)
	}
	if err := os.WriteFile(filepath.Join(folder, "agent.json"), []byte(`{"tools":["write","bash"],"paths":{"allow":["src"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"allow_write":true,"agents_dir":[`+strconv.Quote(dir)+`]}`), "/s/setting.json", "/w", ""); err == nil || !strings.Contains(err.Error(), "a shell can change any path") {
		t.Fatalf("folder agent with bash and paths: %v", err)
	}
}

func TestClonedPathPolicyKeepsItsWithinRoots(t *testing.T) {
	original := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: "/x"}}, Within: []string{"/a", "/b"}}
	copied := cloneNativePathPolicy(original)
	if len(copied.Within) != 2 || copied.Within[0] != "/a" {
		t.Fatalf("clone lost Within: %+v", copied)
	}
	copied.Within[0] = "/changed"
	if original.Within[0] != "/a" {
		t.Fatal("clone shares Within with the original")
	}
}

func TestSubagentChangedFilesMaxSetting(t *testing.T) {
	read := func(setting string) (int64, error) {
		got, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", "")
		if err != nil {
			return 0, err
		}
		return got.ChangedFilesMax, nil
	}
	for name, tc := range map[string]struct {
		setting string
		want    int64
	}{
		"default":  {`{"agents":{"a":{}}}`, 100},
		"null":     {`{"changed_files_max":null,"agents":{"a":{}}}`, 100},
		"explicit": {`{"changed_files_max":7,"agents":{"a":{}}}`, 7},
		"off":      {`{"changed_files_max":0,"agents":{"a":{}}}`, 0},
		"maximum":  {`{"changed_files_max":10000,"agents":{"a":{}}}`, 10000},
	} {
		if got, err := read(tc.setting); err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v; want %d", name, got, err, tc.want)
		}
	}
	for name, value := range map[string]string{"negative": "-1", "too big": "10001", "text": `"many"`, "float": "1.5", "bool": "true"} {
		if _, err := read(`{"changed_files_max":` + value + `,"agents":{"a":{}}}`); err == nil || !strings.Contains(err.Error(), "changed_files_max") {
			t.Errorf("%s: error = %v, want one naming changed_files_max", name, err)
		}
	}
}
