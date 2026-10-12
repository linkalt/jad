package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// subagentTree writes files (relative path -> text) under a fresh directory and
// returns it.
func subagentTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadWithDirs(t *testing.T, root, subagent string) (*NativeSubagentSettings, error) {
	t.Helper()
	return normalizeNativeSubagentSettings(subagentSettingsFor(subagent), filepath.Join(root, "setting.json"), root, "")
}

func TestSubagentAgentsDirLoadsOneAgentPerFolderInNameOrder(t *testing.T) {
	root := subagentTree(t, map[string]string{
		"agents/zeta/agent.json":    `{"schema_version":1,"description":"last","model":"zeta-model.json","tools":["read"],"max_tool_rounds":7,"max_run_tokens":5000,"timeout_ms":4000,"instructions":"Inline."}`,
		"agents/zeta/prompt.md":     "  Prompt file text.\n\n",
		"agents/zeta/extra.md":      "Extra document.",
		"agents/alpha/agent.json":   `{"description":"first","prompt_documents":["notes.md"]}`,
		"agents/alpha/notes.md":     "Notes next to the agent.",
		"agents/beta/prompt.md":     "Only a prompt.",
		"agents/.hidden/agent.json": `{"description":"never loaded"}`,
		"agents/README.txt":         "not a folder",
	})
	// zeta also lists its extra document.
	if err := os.WriteFile(filepath.Join(root, "agents/zeta/agent.json"),
		[]byte(`{"schema_version":1,"description":"last","model":"zeta-model.json","tools":["read"],"max_tool_rounds":7,"max_run_tokens":5000,"timeout_ms":4000,"instructions":"Inline.","prompt_documents":["extra.md"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadWithDirs(t, root, `{"agents_dir":["agents"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, agent := range got.Agents {
		names = append(names, agent.Name)
	}
	if strings.Join(names, ",") != "alpha,beta,zeta" {
		t.Fatalf("agents = %v, want alpha,beta,zeta (name order, no hidden folder, no file)", names)
	}
	if len(got.AgentDirs) != 1 || got.AgentDirs[0] != filepath.Join(root, "agents") {
		t.Fatalf("AgentDirs = %v", got.AgentDirs)
	}

	alpha, _ := got.Agent("alpha")
	if alpha.Description != "first" || alpha.PromptText != "Notes next to the agent." || alpha.Dir != filepath.Join(root, "agents", "alpha") {
		t.Errorf("alpha = %+v", alpha)
	}
	// A folder with only a prompt.md is an agent with defaults.
	beta, _ := got.Agent("beta")
	if beta.PromptText != "Only a prompt." || beta.MaxToolRounds != defaultNativeSubagentMaxToolRounds || beta.TimeoutMS != defaultNativeSubagentTimeoutMS || beta.HasTools || beta.ModelPath != "" {
		t.Errorf("beta = %+v", beta)
	}
	if beta.PromptDocuments[0] != filepath.Join(root, "agents", "beta", "prompt.md") {
		t.Errorf("beta documents = %v", beta.PromptDocuments)
	}
	// Order inside one prompt: inline instructions, then prompt.md, then documents.
	zeta, _ := got.Agent("zeta")
	if zeta.PromptText != "Inline.\n\nPrompt file text.\n\nExtra document." {
		t.Errorf("zeta prompt = %q", zeta.PromptText)
	}
	if zeta.Instructions != "Inline." {
		t.Errorf("zeta instructions = %q", zeta.Instructions)
	}
	// model resolves from the agent's folder, not from setting.json's directory.
	if zeta.ModelPath != filepath.Join(root, "agents", "zeta", "zeta-model.json") {
		t.Errorf("zeta model = %q", zeta.ModelPath)
	}
	if !zeta.HasTools || strings.Join(zeta.Tools, ",") != "read" || zeta.MaxToolRounds != 7 || zeta.MaxRunTokens != 5000 || zeta.TimeoutMS != 4000 {
		t.Errorf("zeta = %+v", zeta)
	}
}

func TestSubagentAgentsDirRelativePathsFollowTheFolderAndHomeIsExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := subagentTree(t, map[string]string{
		"agents/a/agent.json": `{"model":"../../shared/model.json","prompt_documents":["../../shared/rules.md","~/home-rules.md"]}`,
		"shared/rules.md":     "Shared rules.",
	})
	if err := os.WriteFile(filepath.Join(home, "home-rules.md"), []byte("Home rules."), 0o600); err != nil {
		t.Fatal(err)
	}
	// agents_dir itself is relative to the session cwd and may use ~.
	got, err := loadWithDirs(t, root, `{"agents_dir":["./agents"]}`)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := got.Agent("a")
	if a.ModelPath != filepath.Join(root, "shared", "model.json") {
		t.Errorf("model = %q", a.ModelPath)
	}
	if a.PromptText != "Shared rules.\n\nHome rules." {
		t.Errorf("prompt = %q", a.PromptText)
	}

	if err := os.MkdirAll(filepath.Join(home, "my-agents", "h"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "my-agents", "h", "prompt.md"), []byte("From home."), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = loadWithDirs(t, root, `{"agents_dir":["~/my-agents"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := got.Agent("h"); !ok || h.PromptText != "From home." {
		t.Errorf("home agents = %+v", got.Agents)
	}
}

func TestSubagentAgentsDirRelativePathResolvesFromCwdNotTheSettingDirectory(t *testing.T) {
	cwd := subagentTree(t, map[string]string{"agents/a/prompt.md": "From cwd."})
	sessionDir := t.TempDir() // setting.json lives somewhere else entirely
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents_dir":["agents"]}`), filepath.Join(sessionDir, "setting.json"), cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent, ok := got.Agent("a"); !ok || agent.PromptText != "From cwd." {
		t.Fatalf("agents = %+v", got.Agents)
	}
	// The same relative path next to setting.json is not consulted.
	if _, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents_dir":["agents"]}`), filepath.Join(cwd, "agents", "setting.json"), t.TempDir(), ""); err == nil {
		t.Fatal("a relative agents_dir was resolved from the setting directory")
	}
}

func TestSubagentAgentsDirFollowsASymlinkedFolder(t *testing.T) {
	root := subagentTree(t, map[string]string{"shared/linked/prompt.md": "Shared agent."})
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "shared", "linked"), filepath.Join(root, "agents", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := loadWithDirs(t, root, `{"agents_dir":["agents"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if agent, ok := got.Agent("linked"); !ok || agent.PromptText != "Shared agent." {
		t.Fatalf("agents = %+v", got.Agents)
	}
}

func TestSubagentAgentsDirMergesWithInlineAgentsInlineFirst(t *testing.T) {
	root := subagentTree(t, map[string]string{
		"one/b/prompt.md": "B.",
		"one/a/prompt.md": "A.",
		"two/c/prompt.md": "C.",
	})
	got, err := loadWithDirs(t, root, `{"agents":{"inline":{"description":"x"}},"agents_dir":["one","two"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, agent := range got.Agents {
		names = append(names, agent.Name)
	}
	if strings.Join(names, ",") != "inline,a,b,c" {
		t.Fatalf("agents = %v, want inline,a,b,c", names)
	}
	if inline, _ := got.Agent("inline"); inline.Dir != "" {
		t.Errorf("an inline agent has Dir %q", inline.Dir)
	}
	// An inline agent with an empty agents object is fine when a directory supplies agents.
	if _, err := loadWithDirs(t, root, `{"agents":{},"agents_dir":["one"]}`); err != nil {
		t.Fatalf("empty inline agents with agents_dir: %v", err)
	}
	if _, err := loadWithDirs(t, root, `{"agents":null,"agents_dir":["one"]}`); err != nil {
		t.Fatalf("null inline agents with agents_dir: %v", err)
	}
}

func TestSubagentAgentsDirRejectsInvalidSettings(t *testing.T) {
	root := subagentTree(t, map[string]string{
		"ok/a/prompt.md":              "A.",
		"other/a/prompt.md":           "A again.",
		"empty/.keep":                 "",
		"nothing/x/notes.txt":         "no agent.json or prompt.md",
		"badname/has space/prompt.md": "x",
		"badjson/a/agent.json":        `{"description":`,
		"typo/a/agent.json":           `{"tool":["read"]}`,
		"version/a/agent.json":        `{"schema_version":2}`,
		"blankinstr/a/agent.json":     `{"instructions":" "}`,
		"timeout/a/agent.json":        `{"timeout_ms":5}`,
		"missingdoc/a/agent.json":     `{"prompt_documents":["nope.md"]}`,
		"emptyfile/a/agent.json":      ``,
		"notobject/a/agent.json":      `[]`,
		"file.txt":                    "I am a file, not a directory.",
	})
	missingDoc := filepath.Join(root, "missingdoc", "a", "nope.md")
	for name, tc := range map[string]struct{ setting, want string }{
		"not array":        {`{"agents_dir":"ok"}`, "agents_dir must be an array of directory paths"},
		"item blank":       {`{"agents_dir":["ok"," "]}`, "agents_dir[1] must be a non-empty string"},
		"item number":      {`{"agents_dir":[1]}`, "agents_dir must be an array of directory paths"},
		"missing dir":      {`{"agents_dir":["nowhere"]}`, "agents_dir[0] is not a directory: " + filepath.Join(root, "nowhere")},
		"a file":           {`{"agents_dir":["file.txt"]}`, "agents_dir[0] is not a directory"},
		"listed twice":     {`{"agents_dir":["ok","./ok"]}`, "agents_dir lists " + filepath.Join(root, "ok") + " twice"},
		"neither file":     {`{"agents_dir":["nothing"]}`, "agent folder x has neither agent.json nor prompt.md"},
		"bad folder name":  {`{"agents_dir":["badname"]}`, "the name must be 1 to 64 letters, digits, underscores or hyphens"},
		"bad json":         {`{"agents_dir":["badjson"]}`, "invalid JSON"},
		"unknown key":      {`{"agents_dir":["typo"]}`, "agent.tool is not supported; did you mean tools?"},
		"schema version":   {`{"agents_dir":["version"]}`, "unsupported schema_version 2"},
		"blank instr":      {`{"agents_dir":["blankinstr"]}`, "agent.instructions must be a non-empty string"},
		"timeout":          {`{"agents_dir":["timeout"]}`, "agent.timeout_ms must be a safe integer between 1000 and 3600000"},
		"missing doc":      {`{"agents_dir":["missingdoc"]}`, "prompt_documents[0] could not be read: " + missingDoc},
		"empty agent.json": {`{"agents_dir":["emptyfile"]}`, "file is empty"},
		"not an object":    {`{"agents_dir":["notobject"]}`, "expected a JSON object"},
		"same as inline":   {`{"agents":{"a":{}},"agents_dir":["ok"]}`, "agent a is defined twice"},
		"same in two dirs": {`{"agents_dir":["ok","other"]}`, "agent a is defined twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWithDirs(t, root, tc.setting)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSubagentAgentsDirNullMeansAbsent(t *testing.T) {
	root := subagentTree(t, nil)
	_, err := loadWithDirs(t, root, `{"agents_dir":null}`)
	if err == nil || !strings.Contains(err.Error(), "needs agents or agents_dir") {
		t.Fatalf("error = %v", err)
	}
}

// A new project starts with no agent and the model creates them, so none is a valid
// state: an empty folder, an empty list, an empty agents object, or missing folders.
func TestSubagentLoadsWithZeroAgents(t *testing.T) {
	root := subagentTree(t, map[string]string{"empty/keep.txt": "x"})
	for name, tc := range map[string]struct {
		setting    string
		wantDirs   int
		wantListed int
	}{
		"folder without agents": {`{"agents_dir":["empty"]}`, 1, 1},
		"empty list":            {`{"agents_dir":[]}`, 0, 0},
		"empty inline object":   {`{"agents":{}}`, 0, 0},
		"both empty":            {`{"agents":{},"agents_dir":["empty"]}`, 1, 1},
		"missing folder":        {`{"agents_dir_missing_ok":true,"agents_dir":["nowhere"]}`, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := loadWithDirs(t, root, tc.setting)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Agents) != 0 || len(got.AgentDirs) != tc.wantDirs || len(got.ConfiguredAgentDirs) != tc.wantListed {
				t.Fatalf("agents = %d, dirs = %v, configured = %v", len(got.Agents), got.AgentDirs, got.ConfiguredAgentDirs)
			}
		})
	}
	// A missing folder stays an error without agents_dir_missing_ok: a typo is still caught.
	if _, err := loadWithDirs(t, root, `{"agents_dir":["nowhere"]}`); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("missing folder without the flag: %v", err)
	}
}

// The model-facing folders include the ones that do not exist yet, because that is
// where the model creates its first agent.
func TestSubagentConfiguredAgentDirsKeepMissingFolders(t *testing.T) {
	root := subagentTree(t, map[string]string{"general/a/prompt.md": "A."})
	got, err := loadWithDirs(t, root, `{"agents_dir_missing_ok":true,"agents_dir":["general","project"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "general"), filepath.Join(root, "project")}
	if strings.Join(got.ConfiguredAgentDirs, "|") != strings.Join(want, "|") {
		t.Fatalf("ConfiguredAgentDirs = %v, want %v", got.ConfiguredAgentDirs, want)
	}
}

func TestSubagentAgentFolderErrorsNameTheFile(t *testing.T) {
	root := subagentTree(t, map[string]string{"agents/a/agent.json": `{"tool":[]}`})
	_, err := loadWithDirs(t, root, `{"agents_dir":["agents"]}`)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(root, "agents", "a", "agent.json")) {
		t.Fatalf("error = %v, want it to name the agent.json path", err)
	}
}

func TestSubagentSchemasAgreeWithTheLoader(t *testing.T) {
	read := func(name string) map[string]any {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", name))
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return value
	}
	agentSchema := read("agent.schema.json")
	settingSchema := read("setting.schema.json")
	walk := func(value map[string]any, path ...string) map[string]any {
		for _, key := range path {
			next, ok := value[key].(map[string]any)
			if !ok {
				t.Fatalf("schema has no %v", path)
			}
			value = next
		}
		return value
	}
	subagent := walk(settingSchema, "properties", "tools", "properties", "native", "properties", "subagent")
	inline := walk(subagent, "properties", "agents", "additionalProperties", "properties")
	file := walk(agentSchema, "properties")

	keys := func(m map[string]any, skip string) []string {
		var out []string
		for key := range m {
			if key != skip {
				out = append(out, key)
			}
		}
		sort.Strings(out)
		return out
	}
	want := append([]string(nil), nativeSubagentAgentKeys...)
	sort.Strings(want)
	if got := keys(inline, ""); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("setting.schema.json agent keys = %v, loader accepts %v", got, want)
	}
	if got := keys(file, "schema_version"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agent.schema.json keys = %v, loader accepts %v", got, want)
	}
	// Each key is described the same in both files, so one cannot drift from the other.
	for _, key := range want {
		a, _ := json.Marshal(inline[key])
		b, _ := json.Marshal(file[key])
		if string(a) != string(b) {
			t.Errorf("key %s differs between setting.schema.json and agent.schema.json:\n%s\n%s", key, a, b)
		}
	}
	// The subagent block lists exactly the keys the loader accepts.
	var blockKeys []string
	for key := range walk(subagent, "properties") {
		blockKeys = append(blockKeys, key)
	}
	sort.Strings(blockKeys)
	wantBlock := append([]string(nil), nativeSubagentKeys...)
	sort.Strings(wantBlock)
	if strings.Join(blockKeys, ",") != strings.Join(wantBlock, ",") {
		t.Errorf("tools.native.subagent schema keys = %v, loader accepts %v", blockKeys, wantBlock)
	}
}

func TestAgentSchemaValidatesAgentFilesTheLoaderAccepts(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "..", "sample", "schemes", "agent.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	valid := []string{
		`{}`,
		`{"schema_version":1,"description":"d","model":"m.json","instructions":"i","prompt_documents":["a.md"],"tools":["read"],"max_tool_rounds":3,"max_run_tokens":100,"timeout_ms":1000}`,
		`{"toolset":"extra/search"}`,
		`{"toolset":"extra.v2/search-tools_1"}`,
	}
	invalid := map[string]string{
		"unknown key":    `{"tool":["read"]}`,
		"rounds zero":    `{"max_tool_rounds":0}`,
		"timeout small":  `{"timeout_ms":999}`,
		"tools twice":    `{"tools":["read","read"]}`,
		"documents once": `{"prompt_documents":["a.md","a.md"]}`,
		"version":        `{"schema_version":2}`,
		"description":    `{"description":"` + strings.Repeat("x", 501) + `"}`,
		"instructions":   `{"instructions":""}`,
		"toolset bare":   `{"toolset":"search"}`,
		"toolset deep":   `{"toolset":"a/b/c"}`,
		"toolset blank":  `{"toolset":""}`,
		"toolset dash":   `{"toolset":"-a/b"}`,
	}
	check := func(text string) error {
		var value any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			t.Fatal(err)
		}
		return schema.Validate(value)
	}
	for _, text := range valid {
		if err := check(text); err != nil {
			t.Errorf("schema rejects %s: %v", text, err)
		}
	}
	for name, text := range invalid {
		if err := check(text); err == nil {
			t.Errorf("%s: schema accepts %s", name, text)
		}
	}
}

func TestSampleSubagentFolderLoadsAndMatchesTheSchema(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "subagents"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents_dir":[`+strconvQuote(root)+`]}`), filepath.Join(t.TempDir(), "setting.json"), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	explore, ok := got.Agent("explore")
	if !ok || explore.Description == "" || !strings.Contains(explore.PromptText, "read-only research helper") ||
		strings.Join(explore.Tools, ",") != "read,grep,glob" {
		t.Fatalf("explore = %+v", explore)
	}
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "..", "sample", "schemes", "agent.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "explore", "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("sample agent.json does not match agent.schema.json: %v", err)
	}
}

// A toolset shared by many projects lists a general folder and the project's own folder.
// A project that has no folder of its own must still load when the setting allows it.
func TestSubagentAgentsDirMayBeMissingWhenConfigured(t *testing.T) {
	root := subagentTree(t, map[string]string{"general/a/prompt.md": "A."})
	_, err := loadWithDirs(t, root, `{"agents_dir":["general","project"]}`)
	if err == nil || !strings.Contains(err.Error(), "agents_dir[1] is not a directory") {
		t.Fatalf("default must stay strict, error = %v", err)
	}
	got, err := loadWithDirs(t, root, `{"agents_dir_missing_ok":true,"agents_dir":["general","project"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "a" {
		t.Fatalf("agents = %+v", got.Agents)
	}
	if len(got.AgentDirs) != 1 || got.AgentDirs[0] != filepath.Join(root, "general") {
		t.Fatalf("AgentDirs = %v, want only the folder that exists", got.AgentDirs)
	}
	// A path that exists but is a file is still an error.
	root = subagentTree(t, map[string]string{"general/a/prompt.md": "A.", "file.txt": "x"})
	if _, err := loadWithDirs(t, root, `{"agents_dir_missing_ok":true,"agents_dir":["general","file.txt"]}`); err == nil {
		t.Fatal("a file in agents_dir was accepted")
	}
}

// With agents_dir_override a later folder replaces an earlier one's agent of the same
// name, so a project can adapt a general agent. Inline agents are never replaced.
func TestSubagentAgentsDirLaterFolderOverridesWhenConfigured(t *testing.T) {
	root := subagentTree(t, map[string]string{
		"general/a/prompt.md": "general A.",
		"general/b/prompt.md": "general B.",
		"project/a/prompt.md": "project A.",
		"project/c/prompt.md": "project C.",
	})
	if _, err := loadWithDirs(t, root, `{"agents_dir":["general","project"]}`); err == nil || !strings.Contains(err.Error(), "agent a is defined twice") {
		t.Fatalf("default must stay strict, error = %v", err)
	}
	got, err := loadWithDirs(t, root, `{"agents_dir_override":true,"agents_dir":["general","project"]}`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, agent := range got.Agents {
		names = append(names, agent.Name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("agents = %v, want a,b,c", names)
	}
	a, _ := got.Agent("a")
	if a.PromptText != "project A." || a.Dir != filepath.Join(root, "project", "a") {
		t.Fatalf("agent a = prompt %q dir %q, want the project's", a.PromptText, a.Dir)
	}
	// An inline agent of the same name is still refused.
	_, err = loadWithDirs(t, root, `{"agents_dir_override":true,"agents":{"a":{"description":"x"}},"agents_dir":["general"]}`)
	if err == nil || !strings.Contains(err.Error(), "agent a is defined twice") {
		t.Fatalf("inline duplicate, error = %v", err)
	}
}

// The model creates an agent folder during a run; the next load must see it, because
// nothing caches the folders between loads.
func TestSubagentNewFolderAppearsOnTheNextLoad(t *testing.T) {
	root := subagentTree(t, map[string]string{"agents/keep.txt": "x"})
	setting := `{"agents_dir":["agents"]}`
	first, err := loadWithDirs(t, root, setting)
	if err != nil || len(first.Agents) != 0 {
		t.Fatalf("first load: agents = %v, error = %v", first, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents", "scout"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "scout", "prompt.md"), []byte("Scout."), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := loadWithDirs(t, root, setting)
	if err != nil || len(second.Agents) != 1 || second.Agents[0].Name != "scout" {
		t.Fatalf("second load: agents = %+v, error = %v", second.Agents, err)
	}
}
