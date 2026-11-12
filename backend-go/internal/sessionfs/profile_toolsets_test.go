package sessionfs

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The own toolset fixture: the global toolset coding/full gives exec (a timeout, a cap and an
// env_file relative to G), async and skill, and the MCP server srv/one; dev/coder's own toolset
// coding/lean is based on it, leaves out skill and srv/one and tunes exec's timeout.
const (
	ownBaseToolset = `{"schema_version":1,"native":{"exec":{"timeout_ms":1000,"max_timeout_ms":5000,"env_file":"env/exec.env"},"async":{},"skill":{}},"mcp":{"srv/one":{}}}`
	ownLean        = `{"schema_version":1,"based_on":"global:coding/full","omit":{"native":["skill"],"mcp":["srv/one"]},"set":{"native":{"exec":{"timeout_ms":2000}}}}`
)

type ownToolsetFixture struct {
	f    wsFixture
	w    string
	own  string
	base string
}

func newOwnToolsetFixture(t *testing.T, coder string) ownToolsetFixture {
	t.Helper()
	f := newWSFixture(t)
	base := filepath.Join(f.global, "tools", "coding", "full.json")
	wsPut(t, base, ownBaseToolset)
	wsPut(t, filepath.Join(f.global, "env", "exec.env"), "A=1\n")
	wsPut(t, filepath.Join(f.global, "mcp", "srv", "one.json"), `{"command":"true"}`)
	w := f.workspace(t, "proj", `{}`)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), coder)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"]}`)
	own := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "tools", "coding", "lean.json")
	wsPut(t, own, ownLean)
	return ownToolsetFixture{f: f, w: w, own: own, base: base}
}

func (o ownToolsetFixture) load(t *testing.T, profile string) (SessionConfig, error) {
	t.Helper()
	return wsSession(t, wsMustLoad(t, o.f, o.w), "s1", profile)
}

func (o ownToolsetFixture) layout(t *testing.T) Layout {
	t.Helper()
	return *wsMustLoad(t, o.f, o.w).Roots[0].Layout
}

const leanCoder = `{"models":["openai/default"],"toolset":"profile:coding/lean"}`

// The own toolset is its base with the omitted parts left out and the set keys merged; an edit
// of the base reaches it; nothing of it is visible to another profile or a sub-agent.
func TestOwnToolsetIsComposedFromItsBaseForItsProfileOnly(t *testing.T) {
	o := newOwnToolsetFixture(t, leanCoder)
	config, err := o.load(t, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeExec == nil || config.NativeExec.Child.TimeoutMs != 2000 || config.NativeExec.Child.MaxTimeoutMs != 5000 {
		t.Fatalf("exec = %+v", config.NativeExec)
	}
	if config.NativeSkill != nil || config.NativeAsync == nil || len(config.MCPServers) != 0 {
		t.Fatalf("skill %v async %v servers %v: the omitted parts must be left out", config.NativeSkill, config.NativeAsync, config.MCPServers)
	}
	// An edit of the base reaches the own toolset.
	wsPut(t, o.base, strings.Replace(ownBaseToolset, `"max_timeout_ms":5000`, `"max_timeout_ms":4000`, 1))
	if config, err = o.load(t, "dev/coder"); err != nil || config.NativeExec.Child.MaxTimeoutMs != 4000 || config.NativeExec.Child.TimeoutMs != 2000 {
		t.Fatalf("after a base edit: %+v, %v", config.NativeExec, err)
	}
	// The global base keeps its relative paths from G.
	own, err := o.layout(t).profileToolsets(nil, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	composed, err := own.toolset(o.own)
	if err != nil {
		t.Fatal(err)
	}
	var exec map[string]json.RawMessage
	_ = json.Unmarshal(composed.Native["exec"], &exec)
	if want, _ := json.Marshal(filepath.Join(o.f.global, "env", "exec.env")); string(exec["env_file"]) != string(want) {
		t.Fatalf("env_file = %s, want %s", exec["env_file"], want)
	}
	// Another profile cannot name it: profile: there is its own (empty) scope.
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"],"toolset":"profile:coding/lean"}`)
	if _, err := o.load(t, "dev/other"); err == nil || !strings.Contains(err.Error(), "coding/lean") {
		t.Fatalf("another profile naming the own toolset: %v", err)
	}
	// A sub-agent never uses it.
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"]}`)
	wsPut(t, filepath.Join(o.f.global, "tools", "coding", "agents.json"), `{"schema_version":1,"native":{"subagent":{"agents":{"helper":{"description":"h","toolset":"profile:coding/lean"}}}}}`)
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":["profile:coding/lean","global:coding/agents"]}`)
	if _, err := o.load(t, "dev/coder"); err == nil || !strings.Contains(err.Error(), "never by a sub-agent") {
		t.Fatalf("a sub-agent naming the own toolset: %v", err)
	}
}

// An own toolset file is held to its rules at load: one that breaks them, or whose base is
// missing, or that omits or sets what its base does not have, leaves the profile out with a
// warning, and selecting it fails with an error marked as an own toolset's.
func TestOwnToolsetRulesHoldForFilesWrittenByHand(t *testing.T) {
	for _, tc := range []struct{ name, file, want string }{
		{"unknown key", `{"based_on":"global:coding/full","hooks":{}}`, "hooks is not a key"},
		{"no scope", `{"based_on":"coding/full"}`, "must start with global: or workspace:"},
		{"own base", `{"based_on":"profile:coding/x"}`, "never on another own toolset"},
		{"missing base", `{"based_on":"global:coding/gone"}`, "coding/gone"},
		{"omit hooks", `{"based_on":"global:coding/full","omit":{"hooks":["x"]}}`, "omit.hooks"},
		{"set mcp", `{"based_on":"global:coding/full","set":{"mcp":{}}}`, "set.mcp"},
		{"env_file", `{"based_on":"global:coding/full","set":{"native":{"exec":{"env_file":"/x"}}}}`, "set.native.exec.env_file cannot be changed"},
		{"env", `{"based_on":"global:coding/full","set":{"native":{"exec":{"env":{"A":"1"}}}}}`, "set.native.exec.env cannot be changed"},
		{"paths", `{"based_on":"global:coding/full","set":{"native":{"exec":{"paths":[]}}}}`, "set.native.exec.paths cannot be changed"},
		{"shell", `{"based_on":"global:coding/full","set":{"native":{"exec":{"shell":"/bin/zsh"}}}}`, "set.native.exec.shell cannot be changed"},
		{"limit", `{"based_on":"global:coding/full","set":{"native":{"exec":{"max_timeout_ms":9000}}}}`, "set.native.exec.max_timeout_ms cannot be changed"},
		{"unknown tool key", `{"based_on":"global:coding/full","set":{"native":{"exec":{"later":1}}}}`, "set.native.exec.later is not one"},
		{"object value", `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":{}}}}}`, "set.native.exec.timeout_ms must be a JSON scalar"},
		{"null value", `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":null}}}}`, "must be a JSON scalar"},
		{"unknown tool", `{"based_on":"global:coding/full","set":{"native":{"bash":{}}}}`, "set.native.bash is not a native tool"},
		{"web search endpoint", `{"based_on":"global:coding/full","set":{"extension":{"web_search":{"base_url":"https://x"}}}}`, "the endpoint, the credential"},
		{"web search key", `{"based_on":"global:coding/full","set":{"extension":{"web_search":{"api_key_env":"GITHUB_TOKEN"}}}}`, "the endpoint, the credential"},
		{"domains", `{"based_on":"global:coding/full","set":{"extension":{"web_search":{"allowed_domains":["x.org"]}}}}`, "what the tool can reach"},
		{"sub-agent depth", `{"based_on":"global:coding/full","set":{"native":{"subagent":{"max_depth":9}}}}`, "a limit on what the tool may start"},
		{"other sessions", `{"based_on":"global:coding/full","set":{"native":{"transcript":{"allow_other_sessions":true}}}}`, "what the tool can reach"},
		{"tool rules", `{"based_on":"global:coding/full","set":{"native":{"exec":{"timeout_ms":9000}}}}`, "timeout_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOwnToolsetFixture(t, leanCoder)
			wsPut(t, o.own, tc.file)
			_, err := o.load(t, "dev/coder")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want %q", err, tc.want)
			}
			if tc.name != "tool rules" && !IsOwnToolsetError(err) {
				t.Fatalf("the error is not marked as an own toolset's: %v", err)
			}
			// Another profile of the session still loads.
			if _, err := o.load(t, "dev/other"); err != nil {
				t.Fatalf("dev/other: %v", err)
			}
		})
	}
}

// A load passes over a name the own toolset omits or sets that its base does not have (an edit of
// the base dropped it): nothing is added and the profile still loads; jad check and an edit's
// trial refuse such a name.
func TestOwnToolsetPassesOverNamesItsBaseDoesNotHave(t *testing.T) {
	o := newOwnToolsetFixture(t, leanCoder)
	wsPut(t, o.own, `{"based_on":"global:coding/full","omit":{"native":["skill","lsp"],"mcp":["srv/one","srv/two"]},"set":{"native":{"read_image":{"max_image_bytes":10}}}}`)
	config, err := o.load(t, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeSkill != nil || config.NativeLSP != nil || config.NativeReadImage != nil || len(config.MCPServers) != 0 || config.NativeExec == nil {
		t.Fatalf("skill %v lsp %v read_image %v servers %v exec %v", config.NativeSkill, config.NativeLSP, config.NativeReadImage, config.MCPServers, config.NativeExec != nil)
	}
	own, err := o.layout(t).profileToolsets(nil, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.strictToolset(o.own); err == nil || !strings.Contains(err.Error(), "lsp is not a native tool of its base") {
		t.Fatalf("strict = %v", err)
	}
	for file, want := range map[string]string{
		`{"based_on":"global:coding/full","set":{"native":{"lsp":{"max_results":3}}}}`:                            "has no native tool lsp",
		`{"based_on":"global:coding/full","omit":{"native":["exec"]},"set":{"native":{"exec":{"timeout_ms":3}}}}`: "has no native tool exec",
		`{"based_on":"global:coding/full","omit":{"mcp":["srv/two"]}}`:                                            "srv/two is not an MCP server",
	} {
		wsPut(t, o.own, file)
		if own, err = o.layout(t).profileToolsets(nil, "dev/coder"); err != nil {
			t.Fatal(err)
		}
		if _, err := own.strictToolset(o.own); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("strict %s = %v, want %q", file, err, want)
		}
	}
}

// Every key of every native and extension tool is classified for an own toolset's set: allowed
// as tuning or refused with a reason. A key added to a tool fails this test until it is.
func TestOwnToolsetTableClassifiesEveryToolKey(t *testing.T) {
	keyLists := map[string][]string{
		"async": nativeAsyncKeys, "exec": nativeExecKeys, "tool_smith": nativeToolSmithKeys, "code_context": nativeCodeContextKeys,
		"lsp": nativeLSPKeys, "read_image": nativeReadImageKeys, "skill": nativeSkillKeys, "load_tool": nil, "profile": nativeProfileKeys,
		"profile_message": nativeProfileMessageKeys, "subagent": nativeSubagentKeys, "task": nativeTaskKeys, "records": nativeRecordsKeys,
		"request_draft": nativeRequestDraftKeys, "transcript": nativeTranscriptKeys, "transcripts": nativeTranscriptsKeys,
		"decide_systemone": extensionDecideSystemoneKeys, "web_search": extensionWebSearchKeys,
	}
	for part, names := range map[string][]string{"native": nativeToolNames, "extension": extensionToolNames} {
		if got := slices.Sorted(maps.Keys(ownToolsetTools[part])); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
			t.Errorf("%s tools in the table %v, tools %v", part, got, slices.Sorted(slices.Values(names)))
		}
		for _, tool := range names {
			keys, listed := keyLists[tool]
			if !listed {
				t.Errorf("%s.%s has no key list in this test", part, tool)
				continue
			}
			rule := ownToolsetTools[part][tool]
			for _, key := range keys {
				_, tune := rule.keys[key]
				_, refused := rule.refused[key]
				if tune == refused {
					t.Errorf("%s.%s.%s: tuning %v, refused %v; classify it once", part, tool, key, tune, refused)
				}
			}
			for _, key := range slices.Concat(slices.Collect(maps.Keys(rule.keys)), slices.Collect(maps.Keys(rule.refused))) {
				if !slices.Contains(keys, key) {
					t.Errorf("%s.%s.%s is in the table but not a key of the tool", part, tool, key)
				}
			}
		}
	}
	// Endpoints, credentials, paths, commands and environments are never tuning.
	for _, key := range []string{"base_url", "api_key_env", "provider", "model", "env_file", "env", "env_deny", "paths", "storage_dir", "functions_dir", "shell", "runners", "servers", "interpreter", "agents", "remotes"} {
		for part, tools := range ownToolsetTools {
			for tool, rule := range tools {
				if _, tune := rule.keys[key]; tune {
					t.Errorf("%s.%s.%s is tuning", part, tool, key)
				}
			}
		}
	}
}

// The own toolset schema allows the keys the table allows for each tool and refuses others.
func TestOwnToolsetSchemaAgreesWithTheTable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "profile-toolset.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Properties           map[string]json.RawMessage `json:"properties"`
					AdditionalProperties json.RawMessage            `json:"additionalProperties"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(got, slices.Sorted(slices.Values(ownToolsetKeys))) {
		t.Errorf("schema keys %v, loader %v", got, ownToolsetKeys)
	}
	set := schema.Properties["set"].Properties
	for part, tools := range ownToolsetTools {
		for tool, rule := range tools {
			node := set[part].Properties[tool]
			if string(node.AdditionalProperties) != "false" || !slices.Equal(slices.Sorted(maps.Keys(node.Properties)), slices.Sorted(maps.Keys(rule.keys))) {
				t.Errorf("set.%s.%s: schema %v (closed %s), loader %v", part, tool, slices.Sorted(maps.Keys(node.Properties)), node.AdditionalProperties, slices.Sorted(maps.Keys(rule.keys)))
			}
		}
	}
	compiled := compileSchema(t, "profile-toolset.schema.json")
	if !schemaAccepts(t, compiled, ownLean) {
		t.Error("a valid own toolset is refused")
	}
	for _, refused := range []string{`{"based_on":"coding/full"}`, `{"based_on":"profile:coding/x"}`, `{"based_on":"global:coding/full","set":{"native":{"exec":{"env_file":"x"}}}}`, `{"based_on":"global:coding/full","hooks":{}}`, `{"based_on":"global:coding/full","set":{"mcp":{}}}`} {
		if schemaAccepts(t, compiled, refused) {
			t.Errorf("%s is accepted", refused)
		}
	}
}

// The toolset deletion guard counts a profile one of whose own toolsets is based on the file.
func TestToolsetUsersCountsOwnToolsetsBasedOnTheFile(t *testing.T) {
	o := newOwnToolsetFixture(t, `{"models":["openai/default"]}`)
	layout := o.layout(t)
	global := layout
	global.Profiles = filepath.Join(o.w, ".jad", "profiles")
	users, err := toolsetUsers(global, "coding/full", o.base)
	if err != nil || len(users) != 1 || !strings.Contains(users[0], "dev/coder (its own toolset profile:coding/lean") {
		t.Fatalf("users = %v, %v", users, err)
	}
	// A profile: reference never names a shared file.
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"],"toolset":"profile:coding/full"}`)
	wsPut(t, o.own, `{"based_on":"workspace:coding/elsewhere"}`)
	if users, err := toolsetUsers(global, "coding/full", o.base); err != nil || len(users) != 0 {
		t.Fatalf("users = %v, %v", users, err)
	}
}

// The own toolset files are in the checkpoint exclusion (decider 3c C3): SelfEditPaths covers
// them, so a restore never rewrites them.
func TestSelfEditPathsCoverTheOwnToolsets(t *testing.T) {
	o := newOwnToolsetFixture(t, leanCoder)
	config, err := o.load(t, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(config.SelfEditPaths, func(root string) bool { return PathWithin(root, o.own) }) {
		t.Fatalf("SelfEditPaths %v leave out %s", config.SelfEditPaths, o.own)
	}
}

// otherProfileError's mark: an own toolset error wraps and keeps the configuration error.
func TestOwnToolsetErrorKeepsItsCause(t *testing.T) {
	cause := configError("x")
	marked := ownToolsetError(cause)
	var config *ConfigError
	if !IsOwnToolsetError(marked) || !errors.As(marked, &config) || IsOwnModelError(marked) || ownToolsetError(nil) != nil {
		t.Fatalf("marked = %#v", marked)
	}
	if !IsOwnToolsetError(keepOwnToolsetMark(marked, configError("wrapped: %v", marked))) || IsOwnToolsetError(keepOwnToolsetMark(cause, cause)) {
		t.Fatal("keepOwnToolsetMark")
	}
}

// A hand-written own toolset with a shared toolset's ID makes the bare ID ambiguous for its
// profile only (the profile is left out); a sub-agent's bare ID still names the shared toolset,
// since an agent's list never reads the profile's own. An error of composing that names an own
// toolset file (hooks in the settings and in the base) is marked as an own toolset's.
func TestOwnToolsetClashAndCompositionErrors(t *testing.T) {
	o := newOwnToolsetFixture(t, `{"models":["openai/default"],"toolset":["global:coding/agents"]}`)
	wsPut(t, filepath.Join(o.f.global, "tools", "coding", "agents.json"), `{"schema_version":1,"native":{"async":{},"subagent":{"agents":{"helper":{"description":"h","toolset":"coding/full"}}}}}`)
	wsPut(t, filepath.Join(o.w, ".jad", "profile-own", "dev", "coder", "tools", "coding", "full.json"), `{"based_on":"global:coding/full"}`)
	config, err := o.load(t, "dev/coder")
	if err != nil {
		t.Fatalf("an agent naming the shared coding/full: %v", err)
	}
	if _, found := config.AgentToolsets["coding/full"]; !found {
		t.Fatalf("agent toolsets %v", slices.Collect(maps.Keys(config.AgentToolsets)))
	}
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":"coding/full"}`)
	if _, err := o.load(t, "dev/coder"); err == nil || !strings.Contains(err.Error(), "profile:coding/full") {
		t.Fatalf("the profile naming the clashing bare ID: %v", err)
	}

	wsPut(t, o.base, `{"schema_version":1,"native":{"async":{}},"hooks":{"enabled":true}}`)
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"toolset":"profile:coding/lean","settings":{"hooks":{"enabled":true}}}`)
	wsPut(t, o.own, `{"based_on":"global:coding/full"}`)
	if _, err := o.load(t, "dev/coder"); err == nil || !strings.Contains(err.Error(), "hooks is set here and in the settings") || !IsOwnToolsetError(err) {
		t.Fatalf("hooks twice through an own toolset: %v (marked %v)", err, IsOwnToolsetError(err))
	}
}
