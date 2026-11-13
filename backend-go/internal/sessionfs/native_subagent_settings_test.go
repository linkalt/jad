package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func subagentSettingsFor(subagent string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"subagent":` + subagent + `}}`)}
}

func TestNormalizeNativeSubagentSettingsAbsentMeansDisabled(t *testing.T) {
	for name, setting := range map[string]map[string]json.RawMessage{
		"no tools":      {},
		"no subagent":   {"tools": json.RawMessage(`{"native":{}}`)},
		"other tool on": {"tools": json.RawMessage(`{"native":{"skill":{}}}`)},
	} {
		got, err := normalizeNativeSubagentSettings(setting, "/s/setting.json", "/w", "")
		if err != nil || got != nil {
			t.Errorf("%s: got %+v, %v; want nil, nil", name, got, err)
		}
	}
	if got, err := normalizeNativeSubagentSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":null}`)}, "/s/setting.json", "/w", ""); err == nil || got != nil {
		t.Fatalf("null native: got %+v, %v; want refusal", got, err)
	}
}

func TestNormalizeNativeSubagentSettingsDefaultsAndOverrides(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"explore":{}}}`), "/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ListMaxBytes != defaultNativeSubagentListMaxBytes || defaultNativeSubagentListMaxBytes != 4096 {
		t.Fatalf("list_max_bytes default = %d", got.ListMaxBytes)
	}
	if got.KeepFinished != 50 || got.MaxQueued != 32 {
		t.Fatalf("defaults = %d %d", got.KeepFinished, got.MaxQueued)
	}
	if got.ResultMaxBytes != defaultNativeSubagentResultMaxBytes || got.MaxConcurrent != defaultNativeSubagentMaxConcurrent || got.StorageDir != "/w/.subagents" {
		t.Fatalf("defaults = %+v", got)
	}
	agent := got.Agents[0]
	if agent.Name != "explore" || agent.MaxToolRounds != defaultNativeSubagentMaxToolRounds || agent.TimeoutMS != defaultNativeSubagentTimeoutMS ||
		agent.MaxRunTokens != 0 || agent.HasTools || agent.ModelPath != "" || agent.Instructions != "" {
		t.Fatalf("agent defaults = %+v", agent)
	}

	custom, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{
		"result_max_bytes": 100, "max_concurrent": 2, "list_max_bytes": 0, "storage_dir": "kids",
		"keep_finished": 7, "max_queued": 0,
		"agents": {
			"b": {"description": "second", "model": "model-b.json", "instructions": "Be brief.", "tools": ["read_image","skill"],
			      "max_tool_rounds": 5, "max_run_tokens": 9000, "timeout_ms": 2000},
			"a": {}
		}}`), "/s/setting.json", "/w", "/w/.jad/tools")
	if err != nil {
		t.Fatal(err)
	}
	if custom.ResultMaxBytes != 100 || custom.MaxConcurrent != 2 || custom.ListMaxBytes != 0 || custom.KeepFinished != 7 || custom.MaxQueued != 0 || custom.StorageDir != "/w/kids" {
		t.Fatalf("custom = %+v", custom)
	}
	if custom.Agents[0].Name != "b" || custom.Agents[1].Name != "a" {
		t.Fatalf("agents are not in file order: %+v", custom.Agents)
	}
	b := custom.Agents[0]
	if b.ModelPath != "/s/model-b.json" || b.Instructions != "Be brief." || !b.HasTools || strings.Join(b.Tools, ",") != "read_image,skill" ||
		b.MaxToolRounds != 5 || b.MaxRunTokens != 9000 || b.TimeoutMS != 2000 || b.Description != "second" {
		t.Fatalf("agent b = %+v", b)
	}
	if found, ok := custom.Agent("a"); !ok || found.Name != "a" {
		t.Fatalf("Agent(a) = %+v, %v", found, ok)
	}
	if _, ok := custom.Agent("missing"); ok {
		t.Fatal("Agent(missing) was found")
	}
}

func TestNormalizeNativeSubagentSettingsReadsPromptDocuments(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(cwd, "rules.md"), []byte("Rule one."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "format.md"), []byte("Answer in bullets."), 0o600); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(cwd, "extra.md")
	if err := os.WriteFile(absolute, []byte("Extra."), 0o600); err != nil {
		t.Fatal(err)
	}
	setting := `{"agents":{
		"both": {"instructions":"You are a searcher.","prompt_documents":["rules.md","~/format.md",` + strconvQuote(absolute) + `]},
		"docs": {"prompt_documents":["rules.md"]},
		"none": {"instructions":"Only this."},
		"bare": {},
		"empty":{"prompt_documents":[]},
		"null": {"prompt_documents":null}
	}}`
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"both":  "You are a searcher.\n\nRule one.\n\nAnswer in bullets.\n\nExtra.",
		"docs":  "Rule one.",
		"none":  "Only this.",
		"bare":  "",
		"empty": "",
		"null":  "",
	}
	for _, agent := range got.Agents {
		if agent.PromptText != want[agent.Name] {
			t.Errorf("%s: PromptText = %q, want %q", agent.Name, agent.PromptText, want[agent.Name])
		}
	}
	both, _ := got.Agent("both")
	if strings.Join(both.PromptDocuments, ",") != filepath.Join(cwd, "rules.md")+","+filepath.Join(home, "format.md")+","+absolute {
		t.Errorf("PromptDocuments = %v", both.PromptDocuments)
	}
	// Instructions stays the inline text alone, so a reader can still tell them apart.
	if both.Instructions != "You are a searcher." {
		t.Errorf("Instructions = %q", both.Instructions)
	}
}

func TestNormalizeNativeSubagentSettingsRejectsBadPromptDocuments(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.md"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ setting, want string }{
		"not array":       {`{"agents":{"a":{"prompt_documents":"a.md"}}}`, "agents.a.prompt_documents must be an array of file paths"},
		"item blank":      {`{"agents":{"a":{"prompt_documents":["a.md"," "]}}}`, "agents.a.prompt_documents[1] must be a non-empty string"},
		"item number":     {`{"agents":{"a":{"prompt_documents":[1]}}}`, "agents.a.prompt_documents must be an array of file paths"},
		"missing file":    {`{"agents":{"a":{"prompt_documents":["a.md","nope.md"]}}}`, "agents.a.prompt_documents[1] could not be read: " + filepath.Join(cwd, "nope.md")},
		"is directory":    {`{"agents":{"a":{"prompt_documents":["."]}}}`, "agents.a.prompt_documents[0] could not be read"},
		"same file twice": {`{"agents":{"a":{"prompt_documents":["a.md","./a.md"]}}}`, "agents.a.prompt_documents lists " + filepath.Join(cwd, "a.md") + " twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeSubagentSettings(subagentSettingsFor(tc.setting), "setting.json", cwd, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestNormalizeNativeSubagentSettingsNullKeysMeanDefault(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(
		`{"result_max_bytes":null,"max_concurrent":null,"storage_dir":null,"agents":{"a":{"model":null,"tools":null,"max_run_tokens":null,"timeout_ms":null,"max_tool_rounds":null,"description":null,"instructions":null}}}`),
		"/s/setting.json", "/w", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ResultMaxBytes != defaultNativeSubagentResultMaxBytes || got.Agents[0].HasTools || got.Agents[0].ModelPath != "" {
		t.Fatalf("null keys = %+v", got)
	}
}

func TestNormalizeNativeSubagentSettingsRejectsInvalidSettings(t *testing.T) {
	long := strings.Repeat("x", maxNativeSubagentDescriptionChars+1)
	for name, tc := range map[string]struct{ setting, want string }{
		"not object":         {`[]`, "tools.native.subagent must be an object"},
		"null":               {`null`, "tools.native.subagent must be an object"},
		"no agents":          {`{}`, "tools.native.subagent needs agents or agents_dir"},
		"null agents":        {`{"agents":null}`, "tools.native.subagent needs agents or agents_dir"},
		"empty agents":       {`{"agents":{}}`, ""},
		"agents array":       {`{"agents":[]}`, "tools.native.subagent.agents must be an object"},
		"typo":               {`{"agent":{}}`, "tools.native.subagent.agent is not supported; did you mean agents?"},
		"unrelated":          {`{"agents":{"a":{}},"approval":"ask"}`, "tools.native.subagent.approval is not supported"},
		"agent not object":   {`{"agents":{"a":[]}}`, "tools.native.subagent.agents.a must be an object"},
		"agent null":         {`{"agents":{"a":null}}`, "tools.native.subagent.agents.a must be an object"},
		"agent typo":         {`{"agents":{"a":{"tool":[]}}}`, "tools.native.subagent.agents.a.tool is not supported; did you mean tools?"},
		"bad name":           {`{"agents":{"has space":{}}}`, "the name must be 1 to 64 letters, digits, underscores or hyphens"},
		"name starts dash":   {`{"agents":{"-a":{}}}`, "the name must be 1 to 64 letters, digits, underscores or hyphens"},
		"result zero":        {`{"agents":{"a":{}},"result_max_bytes":0}`, "tools.native.subagent.result_max_bytes must be a safe integer between 1 and 10485760"},
		"concurrent zero":    {`{"agents":{"a":{}},"max_concurrent":0}`, "tools.native.subagent.max_concurrent must be a safe integer between 1 and 32"},
		"concurrent over":    {`{"agents":{"a":{}},"max_concurrent":33}`, "tools.native.subagent.max_concurrent must be a safe integer between 1 and 32"},
		"removed wait_ms":    {`{"agents":{"a":{}},"wait_ms":0}`, "wait_ms"},
		"removed max wait":   {`{"agents":{"a":{}},"max_wait_ms":5000}`, "max_wait_ms"},
		"removed detached":   {`{"agents":{"a":{}},"allow_detached_writers":true}`, "allow_detached_writers"},
		"keep zero":          {`{"agents":{"a":{}},"keep_finished":0}`, "tools.native.subagent.keep_finished must be a safe integer between 1 and 1000"},
		"keep too large":     {`{"agents":{"a":{}},"keep_finished":1001}`, "tools.native.subagent.keep_finished must be a safe integer between 1 and 1000"},
		"queued negative":    {`{"agents":{"a":{}},"max_queued":-1}`, "tools.native.subagent.max_queued must be a safe integer between 0 and 1024"},
		"queued string":      {`{"agents":{"a":{}},"max_queued":"4"}`, "tools.native.subagent.max_queued must be a safe integer"},
		"list negative":      {`{"agents":{"a":{}},"list_max_bytes":-1}`, "tools.native.subagent.list_max_bytes must be a safe integer between 0 and 1048576"},
		"list too large":     {`{"agents":{"a":{}},"list_max_bytes":1048577}`, "tools.native.subagent.list_max_bytes must be a safe integer between 0 and 1048576"},
		"list string":        {`{"agents":{"a":{}},"list_max_bytes":"4096"}`, "tools.native.subagent.list_max_bytes must be a safe integer"},
		"storage blank":      {`{"agents":{"a":{}},"storage_dir":" "}`, "tools.native.subagent.storage_dir must be a non-empty string"},
		"rounds zero":        {`{"agents":{"a":{"max_tool_rounds":0}}}`, "agents.a.max_tool_rounds must be a safe integer between 1 and 50000"},
		"tokens zero":        {`{"agents":{"a":{"max_run_tokens":0}}}`, "agents.a.max_run_tokens must be a safe integer between 1 and 1000000000000"},
		"timeout too small":  {`{"agents":{"a":{"timeout_ms":999}}}`, "agents.a.timeout_ms must be a safe integer between 1000 and 21600000"},
		"timeout fraction":   {`{"agents":{"a":{"timeout_ms":1500.5}}}`, "agents.a.timeout_ms must be a safe integer"},
		"timeout string":     {`{"agents":{"a":{"timeout_ms":"2000"}}}`, "agents.a.timeout_ms must be a safe integer"},
		"model blank":        {`{"agents":{"a":{"model":" "}}}`, "agents.a.model must be a non-empty string"},
		"instructions blank": {`{"agents":{"a":{"instructions":""}}}`, "agents.a.instructions must be a non-empty string"},
		"description long":   {`{"agents":{"a":{"description":"` + long + `"}}}`, "agents.a.description must be at most 500 characters"},
		"tools not array":    {`{"agents":{"a":{"tools":"read_image"}}}`, "agents.a.tools must be an array of tool names"},
		"tools blank":        {`{"agents":{"a":{"tools":["read_image"," "]}}}`, "agents.a.tools[1] must be a non-empty string"},
		"tools twice":        {`{"agents":{"a":{"tools":["read_image","read_image"]}}}`, "agents.a.tools lists read_image twice"},
		"mcp wildcard ok":    {`{"agents":{"a":{"tools":["read_image","mcp:docs:*"]}}}`, ""},
		"mcp no tool":        {`{"agents":{"a":{"tools":["mcp:docs"]}}}`, `agents.a.tools[0] "mcp:docs" must be mcp:<server>:<tool> or mcp:<server>:*`},
		"mcp bad server":     {`{"agents":{"a":{"tools":["mcp:do.cs:*"]}}}`, `agents.a.tools[0] "mcp:do.cs:*" must be mcp:<server>:<tool>`},
		"mcp colon wildcard": {`{"agents":{"a":{"tools":["mcp:docs:foo:*"]}}}`, `agents.a.tools[0] "mcp:docs:foo:*" must be mcp:<server>:<tool>`},
		"mcp whitespace":     {`{"agents":{"a":{"tools":["mcp:docs:a b"]}}}`, `must not contain whitespace`},
		"upper limit ok":     {`{"agents":{"a":{"timeout_ms":21600000}},"max_concurrent":32,"result_max_bytes":10485760,"list_max_bytes":1048576,"keep_finished":1000,"max_queued":0}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeSubagentSettings(subagentSettingsFor(tc.setting), "setting.json", "/w", "")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadSessionConfigAcceptsTheSubagentToolAndNeverListsItForTypeScript(t *testing.T) {
	rootDir := t.TempDir()
	models := map[string]map[string]any{
		"openai/default": {"model": "gpt-test", "api_key_env": "TEST_KEY"},
		"openai/fast":    {"model": "gpt-fast", "api_key_env": "TEST_KEY"},
	}
	location, fixture := layoutLocation(t, layouttest.Spec{CWD: rootDir, Models: models, Native: map[string]any{
		"subagent": map[string]any{"agents": map[string]any{"explore": map[string]any{"model": "openai/fast"}}},
	}})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	fastPath := filepath.Join(fixture.GlobalDir, "models", "openai", "fast.json")
	if config.NativeSubagent == nil || len(config.NativeSubagent.Agents) != 1 || config.NativeSubagent.Agents[0].ModelPath != fastPath {
		t.Fatalf("subagent = %+v", config.NativeSubagent)
	}
	if config.NativeSubagent.StorageDir != filepath.Join(rootDir, ".jad", "state", "session-1", "dev", "coder", "tools", ".subagents") {
		t.Fatalf("storage dir = %q, want it under the working directory", config.NativeSubagent.StorageDir)
	}
}

func TestApprovalKeyIsRefusedOnTheSubagentTool(t *testing.T) {
	_, err := loadSetting(t, nativeTools(map[string]any{"subagent": map[string]any{"agents": map[string]any{"a": map[string]any{}}, "approval": "ask"}}))
	if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.approval is not supported") {
		t.Fatalf("subagent accepted an approval key: %v", err)
	}
}
