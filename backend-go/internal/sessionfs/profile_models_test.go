package sessionfs

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ownModelFixture is a workspace whose profile dev/coder has the own model openai/fast based on
// the global openai/default, and a second profile dev/other.
type ownModelFixture struct {
	f    wsFixture
	w    string
	own  string
	base string
}

const ownFast = `{"schema_version":1,"based_on":"global:openai/default","set":{"name":"Fast","request":{"temperature":0.2},"efforts":["low","high"]}}`

func newOwnModelFixture(t *testing.T, coder string) ownModelFixture {
	t.Helper()
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), coder)
	wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"]}`)
	own := filepath.Join(w, ".jad", "profile-own", "dev", "coder", "models", "openai", "fast.json")
	wsPut(t, own, ownFast)
	return ownModelFixture{f: f, w: w, own: own, base: filepath.Join(f.global, "models", "openai", "default.json")}
}

func (o ownModelFixture) load(t *testing.T, profile string) (SessionConfig, error) {
	t.Helper()
	return wsSession(t, wsMustLoad(t, o.f, o.w), "s1", profile)
}

func (o ownModelFixture) mustLoad(t *testing.T, profile string) SessionConfig {
	t.Helper()
	config, err := o.load(t, profile)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// The own model is offered by its profile, composed from the base and its set, and follows an
// edit of the base's endpoint; nothing of it is visible to another profile.
func TestOwnModelIsComposedFromItsBaseForItsProfileOnly(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai/fast"],"model_default":"profile:openai/fast@high"}`)
	config := o.mustLoad(t, "dev/coder")
	if !slices.Contains(config.Models, "openai/fast") || config.ModelPath != o.own || !config.OwnModel(o.own) || config.OwnModel(o.base) {
		t.Fatalf("models %v, path %s, bases %v", config.Models, config.ModelPath, config.ModelBases)
	}
	if config.ModelDefault != "openai/fast@high" || config.ReasoningEffort != "high" {
		t.Fatalf("default %q effort %q", config.ModelDefault, config.ReasoningEffort)
	}
	loaded, err := config.WithModel()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model.Name != "gpt-test" || loaded.Model.APIKeyEnv != "FIXTURE_KEY" || !strings.Contains(string(loaded.Model.Request), `"temperature":0.2`) || !strings.Contains(string(loaded.Model.Request), `"effort":"high"`) {
		t.Fatalf("model = %+v", loaded.Model)
	}
	if files := config.ModelSourceFiles(o.own); !slices.Equal(files, []string{o.own, o.base}) {
		t.Fatalf("source files = %v", files)
	}

	// An edit of the base's endpoint reaches the own model; nothing is copied.
	wsPut(t, o.base, `{"provider":"openai","model":"gpt-next","base_url":"https://proxy.example/v1","api_key_env":"FIXTURE_KEY"}`)
	if loaded, err = config.WithModel(); err != nil || loaded.Model.Name != "gpt-next" || loaded.Model.BaseURL != "https://proxy.example/v1" {
		t.Fatalf("after the base edit: %+v, %v", loaded.Model, err)
	}
	if fields, err := config.ModelFields(o.own); err != nil || string(fields["model"]) != `"gpt-next"` || string(fields["name"]) != `"Fast"` {
		t.Fatalf("fields = %s, %v", fields, err)
	}

	// Read as a whole model by any other way, the own file fails.
	if _, err := loadModel(o.own, "", ""); err == nil || !strings.Contains(err.Error(), "based_on is a key of a profile's own model") {
		t.Fatalf("plain load of an own file = %v", err)
	}
	// A changed based_on is not followed by a session loaded before.
	wsPut(t, o.own, `{"based_on":"workspace:openai/default"}`)
	if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), "load the session again") {
		t.Fatalf("changed based_on = %v", err)
	}
	wsPut(t, o.own, ownFast)

	other := o.mustLoad(t, "dev/other")
	if other.ModelBases != nil || slices.Contains(other.Models, "openai/fast") {
		t.Fatalf("dev/other sees the own model: %v %v", other.Models, other.ModelBases)
	}
	for _, w := range other.Warnings {
		if strings.Contains(w, "fast") {
			t.Fatalf("dev/other is told about dev/coder's own model: %s", w)
		}
	}
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "other.json"), `{"models":["profile:openai/fast"]}`)
	if _, err := o.load(t, "dev/other"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("dev/other naming dev/coder's own model = %v", err)
	}
}

// Every refused key, an unknown one, a base without its scope or another own model, and a
// missing or broken base leave the own model out of what the profile offers, with a warning;
// the profile still loads with its other models.
func TestOwnModelThatFailsIsLeftOutWithAWarning(t *testing.T) {
	cases := map[string]string{
		"same endpoint, other key": `{"based_on":"global:openai/default","set":{"api_key_env":"GITHUB_TOKEN"}}`,
		"pricing":                  `{"based_on":"global:openai/default","set":{"pricing":{}}}`,
		"hosted tools":             `{"based_on":"global:openai/default","set":{"hosted_tools":[{"type":"web_search"}]}}`,
		"unknown set key":          `{"based_on":"global:openai/default","set":{"later_key":1}}`,
		"top-level endpoint":       `{"based_on":"global:openai/default","base_url":"https://evil.example"}`,
		"no scope":                 `{"based_on":"openai/default"}`,
		"own base":                 `{"based_on":"profile:openai/other"}`,
		"missing base":             `{"based_on":"workspace:openai/default"}`,
		"bad tuning value":         `{"based_on":"global:openai/default","set":{"efforts":"high"}}`,
		"bad schema":               `{"schema_version":2,"based_on":"global:openai/default"}`,
	}
	for refused := range ownModelSet.refused {
		cases["refused "+refused] = `{"based_on":"global:openai/default","set":{"` + refused + `":"x"}}`
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai"]}`)
			wsPut(t, o.own, text)
			config := o.mustLoad(t, "dev/coder")
			if slices.Contains(config.Models, "openai/fast") || !slices.Contains(config.Models, "openai/default") {
				t.Fatalf("models = %v", config.Models)
			}
			want := "openai/fast left out"
			if strings.HasPrefix(name, "refused ") || name == "same endpoint, other key" {
				// Refused by name, before the allow-list: the reason says the base gives it.
				want = "cannot be changed by a profile's own model"
			}
			if !slices.ContainsFunc(config.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
				t.Fatalf("no warning %q: %q", want, config.Warnings)
			}
		})
	}

	o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai"]}`)
	wsPut(t, o.base, `{"provider":"openai","api_key_env":"FIXTURE_KEY"}`)
	config := o.mustLoad(t, "dev/coder")
	if slices.Contains(config.Models, "openai/fast") {
		t.Fatalf("an own model on a broken base is offered: %v", config.Models)
	}
	if err := os.Remove(o.base); err != nil {
		t.Fatal(err)
	}
	wsPut(t, filepath.Join(o.f.global, "models", "openai", "mini.json"), `{"provider":"openai","model":"m","api_key_env":"FIXTURE_KEY"}`)
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/mini","profile:openai"]}`)
	config = o.mustLoad(t, "dev/coder")
	if slices.Contains(config.Models, "openai/fast") || !slices.ContainsFunc(config.Warnings, func(w string) bool { return strings.Contains(w, "based_on") }) {
		t.Fatalf("missing base: models %v, warnings %q", config.Models, config.Warnings)
	}
}

// An own model file, its category folder or its models folder that is a link is not used.
func TestOwnModelLinksAreNotFollowed(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai"]}`)
	elsewhere := filepath.Join(o.f.base, "elsewhere.json")
	wsPut(t, elsewhere, ownFast)
	if err := os.Remove(o.own); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, o.own); err != nil {
		t.Fatal(err)
	}
	if config := o.mustLoad(t, "dev/coder"); slices.Contains(config.Models, "openai/fast") {
		t.Fatalf("a linked own model file is offered: %v", config.Models)
	}

	// Without a models selection the profile offers every model it sees; a linked folder adds none.
	o = newOwnModelFixture(t, `{}`)
	models := filepath.Dir(filepath.Dir(o.own))
	moved := filepath.Join(o.f.base, "moved-models")
	if err := os.Rename(models, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, models); err != nil {
		t.Fatal(err)
	}
	config := o.mustLoad(t, "dev/coder")
	if slices.Contains(config.Models, "openai/fast") || !slices.ContainsFunc(config.Warnings, func(w string) bool { return strings.Contains(w, "own models are left out") }) {
		t.Fatalf("a linked models folder: models %v, warnings %q", config.Models, config.Warnings)
	}
}

// An own model with a shared model's ID needs a prefix in its profile, and never hides the
// shared model from another profile.
func TestOwnModelWithASharedIDNeedsItsScope(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["profile:openai/default"]}`)
	clash := filepath.Join(filepath.Dir(o.own), "default.json")
	wsPut(t, clash, `{"based_on":"global:openai/default","set":{"name":"Mine"}}`)
	config := o.mustLoad(t, "dev/coder")
	if config.ModelPath != clash || !slices.Equal(config.Models, []string{"openai/default"}) {
		t.Fatalf("prefixed own model: %s %v", config.ModelPath, config.Models)
	}
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"]}`)
	if _, err := o.load(t, "dev/coder"); err == nil || !strings.Contains(err.Error(), "more than one scope") {
		t.Fatalf("bare clash = %v", err)
	}
	if other := o.mustLoad(t, "dev/other"); other.ModelPath != o.base {
		t.Fatalf("dev/other model = %s, want the shared %s", other.ModelPath, o.base)
	}
}

// model_fallback may name the selected profile's own model; a sub-agent never can.
func TestOwnModelAsFallbackButNotForASubAgent(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["openai/default"],"settings":{"model_fallback":{"models":["profile:openai/fast@low"]}}}`)
	config := o.mustLoad(t, "dev/coder")
	if config.ModelFallback == nil || len(config.ModelFallback.Models) != 1 || config.ModelFallback.Models[0].ModelPath != o.own || config.ModelFallback.Models[0].ReasoningEffort != "low" {
		t.Fatalf("fallback = %+v", config.ModelFallback)
	}
	wsPut(t, filepath.Join(o.w, ".jad", "profiles", "dev", "other.json"), `{"models":["openai/default"],"settings":{"model_fallback":{"models":["profile:openai/fast"]}}}`)
	if _, err := o.load(t, "dev/other"); err == nil || !strings.Contains(err.Error(), "model_fallback") {
		t.Fatalf("another profile's fallback = %v", err)
	}

	layout := *wsMustLoad(t, o.f, o.w).Roots[0].Layout
	settings := &NativeSubagentSettings{Agents: []NativeSubagentAgent{{Name: "a", Model: "profile:openai/fast"}}}
	if err := resolveSubagentModels(settings, layout, "s1.json", nil); err == nil || !strings.Contains(err.Error(), "never by a sub-agent") {
		t.Fatalf("sub-agent naming an own model = %v", err)
	}
	// Named by its path, the own file is read as a whole model and fails.
	settings = &NativeSubagentSettings{Agents: []NativeSubagentAgent{{Name: "a", Model: o.own}}}
	if err := resolveSubagentModels(settings, layout, "s1.json", nil); err != nil {
		t.Fatal(err)
	}
	// The run of such a sub-agent (childConfig) reads the file without its base and fails; the
	// session's own selection of it still composes.
	coder := o.mustLoad(t, "dev/coder")
	if _, err := coder.WithAgentModel(o.own, "").WithModel(); err == nil || !strings.Contains(err.Error(), "based_on") {
		t.Fatalf("a sub-agent's run on the own file = %v", err)
	}
	own := coder
	own.ModelPath = o.own
	if _, err := own.WithModel(); err != nil || !coder.OwnModel(o.own) {
		t.Fatalf("WithAgentModel changed the session's bases: %v", err)
	}
}

// A sub-agent's model choice offers the profile's shared models only.
func TestSubAgentModelChoiceLeavesOutOwnModels(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai/fast"],"toolset":"dev/t"}`)
	wsPut(t, filepath.Join(o.f.global, "tools", "dev", "t.json"), `{"native":{"async":{},"subagent":{"model_choice":true,"agents":{"a":{"description":"d","instructions":"p"}}}}}`)
	config := o.mustLoad(t, "dev/coder")
	if config.NativeSubagent == nil {
		t.Fatal("no subagent tool")
	}
	if _, found := config.NativeSubagent.ProfileModels["openai/fast"]; found || config.NativeSubagent.ProfileModels["openai/default"] != o.base {
		t.Fatalf("sub-agent models = %v", config.NativeSubagent.ProfileModels)
	}
}

// In the workspace layout a shared model file that sets based_on is refused: only own model
// files take it. v1 ignores the key, as it did before phase 3c (decider 3c.1b).
func TestSharedModelFileCannotSetBasedOn(t *testing.T) {
	stray := []byte(`{"provider":"openai","model":"m","based_on":"global:openai/default"}`)
	if err := validateModelFile("/g/m.json", stray, true); err == nil || !strings.Contains(err.Error(), "based_on") {
		t.Fatalf("workspace layout: shared file with based_on = %v", err)
	}
	if err := validateModelFile("/g/m.json", stray, false); err != nil {
		t.Fatalf("v1: shared file with based_on = %v", err)
	}

	o := newOwnModelFixture(t, `{"models":["openai/default","openai/mini"]}`)
	wsPut(t, filepath.Join(o.f.global, "models", "openai", "mini.json"), `{"provider":"openai","model":"m","api_key_env":"FIXTURE_KEY","based_on":"global:openai/default"}`)
	config := o.mustLoad(t, "dev/coder")
	if slices.Contains(config.Models, "openai/mini") || !slices.ContainsFunc(config.Warnings, func(w string) bool { return strings.Contains(w, "based_on") }) {
		t.Fatalf("workspace layout: models %v, warnings %q", config.Models, config.Warnings)
	}
}

// A base is in use while an own model is based on it; the catalog routes check a profile with
// its own models.
func TestOwnModelKeepsItsBaseFromBeingDeleted(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["profile:openai/fast"]}`)
	roots := wsMustLoad(t, o.f, o.w).Roots
	global, globalRoots, err := ScopedCatalogLayout(roots, "global", CatalogModels)
	if err != nil {
		t.Fatal(err)
	}
	file, err := ReadCatalogFile(global, CatalogModels, "openai/default")
	if err != nil {
		t.Fatal(err)
	}
	err = DeleteCatalogFile(global, globalRoots, CatalogModels, "openai/default", file.ETag)
	if err == nil || wsCatalogKind(t, err) != CatalogInUse || !strings.Contains(err.Error(), "dev/coder (its own model profile:openai/fast is based on it)") {
		t.Fatalf("delete of the base = %v", err)
	}

	profiles, profileRoots, err := ScopedCatalogLayout(roots, "workspace:proj", CatalogProfiles)
	if err != nil {
		t.Fatal(err)
	}
	current, err := ReadCatalogFile(profiles, CatalogProfiles, "dev/coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteCatalogFile(profiles, profileRoots, CatalogProfiles, "dev/coder", []byte(`{"models":["profile:openai/fast"],"model_default":"profile:openai/fast@low"}`), WriteCondition{IfMatch: current.ETag}, false, nil); err != nil {
		t.Fatalf("a profile naming its own model is refused: %v", err)
	}
	if _, _, err := WriteCatalogFile(profiles, profileRoots, CatalogProfiles, "dev/new", []byte(`{"models":["profile:openai/fast"]}`), WriteCondition{IfNoneMatch: "*"}, false, nil); err == nil {
		t.Fatal("a profile naming another profile's own model is written")
	}

	location, err := Resolve(roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	update, err := SetLayoutModelDefaultIfMatch(location, "dev/coder", "openai/fast@high", "", "")
	if err != nil || update.ModelDefault != "openai/fast@high" {
		t.Fatalf("model default = %+v, %v", update, err)
	}
	if _, err := SetLayoutModelDefaultIfMatch(location, "dev/coder", "openai/fast@max", "", ""); err == nil {
		t.Fatal("an effort the own model does not offer is set")
	}
}

// The shared catalogs have two scopes; a third folder is an error, never named workspace.
func TestSharedCatalogTakesNoThirdFolder(t *testing.T) {
	if _, err := NewCatalogScans().scanScopes("model", t.TempDir(), t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "only 2 shared scopes") {
		t.Fatalf("three folders = %v", err)
	}
}

// The schema of an own model file has the loader's keys and refuses what it refuses.
func TestOwnModelSchemaAgreesWithTheLoader(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "profile-model.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema ownSchemaNode
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if top := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(top, slices.Sorted(slices.Values(ownModelKeys))) {
		t.Fatalf("schema keys %v, loader %v", top, ownModelKeys)
	}
	compareOwnSchema(t, "set", schema.Properties["set"], ownModelSet)
	compiled := compileSchema(t, "profile-model.schema.json")
	if !schemaAccepts(t, compiled, ownFast) {
		t.Error("a valid own model is refused")
	}
	for _, refused := range []string{`{"based_on":"openai/default"}`, `{"based_on":"profile:openai/x"}`, `{"based_on":"global:openai/default","set":{"base_url":"https://x"}}`, `{"based_on":"global:openai/default","api_key_env":"K"}`} {
		if schemaAccepts(t, compiled, refused) {
			t.Errorf("%s is accepted", refused)
		}
	}
}

// ownSchemaNode is the part of profile-model.schema.json the loader is compared with.
type ownSchemaNode struct {
	Type                 string                   `json:"type"`
	Properties           map[string]ownSchemaNode `json:"properties"`
	AdditionalProperties json.RawMessage          `json:"additionalProperties"`
	Enum                 []json.RawMessage        `json:"enum"`
}

// compareOwnSchema checks that the schema at name allows the keys the rule allows, closes the
// object the same way and gives each leaf the rule's kind.
func compareOwnSchema(t *testing.T, name string, node ownSchemaNode, rule ownKeyRule) {
	t.Helper()
	switch {
	case rule.kind == "string" && node.Enum != nil:
	case node.Type != rule.kind:
		t.Errorf("%s: schema type %q, loader %q", name, node.Type, rule.kind)
	}
	if rule.kind != "object" {
		return
	}
	if rule.values != nil {
		var values ownSchemaNode
		if err := json.Unmarshal(node.AdditionalProperties, &values); err != nil || values.Type != rule.values.kind {
			t.Errorf("%s: schema values %s, loader %q", name, node.AdditionalProperties, rule.values.kind)
		}
		return
	}
	if string(node.AdditionalProperties) != "false" {
		t.Errorf("%s: the schema does not refuse other keys", name)
	}
	if keys := slices.Sorted(maps.Keys(node.Properties)); !slices.Equal(keys, slices.Sorted(maps.Keys(rule.keys))) {
		t.Errorf("%s: schema keys %v, loader %v", name, keys, slices.Sorted(maps.Keys(rule.keys)))
	}
	for key, inner := range rule.keys {
		if child, found := node.Properties[key]; found {
			compareOwnSchema(t, name+"."+key, child, inner)
		}
	}
}

// ownComposed loads dev/coder with its own model openai/fast set to text over the base text and
// returns the composed model; err is the warning when the own model is left out.
func ownComposed(t *testing.T, base, own string) (Model, string) {
	t.Helper()
	o := newOwnModelFixture(t, `{"models":["openai/default","profile:openai/fast"],"model_default":"profile:openai/fast"}`)
	wsPut(t, o.base, base)
	wsPut(t, o.own, own)
	config := o.mustLoad(t, "dev/coder")
	if !slices.Contains(config.Models, "openai/fast") {
		for _, w := range config.Warnings {
			if strings.Contains(w, "fast") {
				return Model{}, w
			}
		}
		t.Fatalf("own model left out without a warning: %q", config.Warnings)
	}
	loaded, err := config.WithModel()
	if err != nil {
		t.Fatal(err)
	}
	return loaded.Model, ""
}

func jsonObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return value
}

// set.request is merged into the base's request key by key, nested objects too: a base's
// store: false survives an own model that sets reasoning. Keys that change the price, keep
// data at the provider, pull in account content or change attribution are refused, as is
// every key not on the list (decider 3c.1b).
func TestOwnModelRequestIsMergedIntoTheBase(t *testing.T) {
	base := `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","request":{"store":false,"reasoning":{"summary":"auto"},"text":{"format":{"type":"text"}},"temperature":1}}`
	model, warning := ownComposed(t, base, `{"based_on":"global:openai/default","set":{"request":{"reasoning":{"effort":"low"},"text":{"verbosity":"low"},"temperature":0.2,"max_output_tokens":100,"top_p":0.9,"parallel_tool_calls":false,"truncation":"auto"}}}`)
	if warning != "" {
		t.Fatal(warning)
	}
	request := jsonObject(t, model.Request)
	want := map[string]any{
		"store":       false,
		"reasoning":   map[string]any{"summary": "auto", "effort": "low"},
		"text":        map[string]any{"format": map[string]any{"type": "text"}, "verbosity": "low"},
		"temperature": 0.2, "max_output_tokens": float64(100), "top_p": 0.9, "parallel_tool_calls": false, "truncation": "auto",
	}
	if encoded, _ := json.Marshal(request); string(encoded) != func() string { e, _ := json.Marshal(want); return string(e) }() {
		t.Fatalf("request = %s", encoded)
	}

	refused := map[string]string{
		"service_tier":         `"priority"`,
		"store":                `true`,
		"background":           `true`,
		"metadata":             `{"a":"b"}`,
		"user":                 `"u"`,
		"safety_identifier":    `"s"`,
		"previous_response_id": `"resp_1"`,
		"conversation":         `"conv_1"`,
		"prompt":               `{"id":"pmpt_1"}`,
		"prompt_cache_key":     `"k"`,
		"model":                `"other"`,
		"include":              `["reasoning.encrypted_content"]`,
		"later_key":            `1`,
		"text":                 `{"format":{"type":"json_object"}}`,
		"reasoning":            `{"encrypted":true}`,
		"temperature":          `null`,
		"max_output_tokens":    `1.5`,
		"top_p":                `"0.9"`,
	}
	for key, value := range refused {
		t.Run(key+" "+value, func(t *testing.T) {
			_, warning := ownComposed(t, base, `{"based_on":"global:openai/default","set":{"request":{"`+key+`":`+value+`}}}`)
			if warning == "" || !strings.Contains(warning, "set.request."+key) {
				t.Fatalf("set.request.%s = %s is not refused: %q", key, value, warning)
			}
			if byName := !slices.Contains([]string{"include", "later_key", "text", "reasoning", "temperature", "max_output_tokens", "top_p"}, key); byName != strings.Contains(warning, "cannot be changed by a profile's own model") {
				t.Fatalf("set.request.%s: refused by name %v, warning %q", key, byName, warning)
			}
		})
	}
}

// The adapter blocks take a few tuning keys, merged into the base's block; a key the own
// model does not set stays as the base had it. What could replace the credential, pass any
// field to the provider or write the key into a log is refused (decider 3c.1b).
func TestOwnModelAdapterBlocksAreMergedIntoTheBase(t *testing.T) {
	anthropicBase := `{"provider":"anthropic","api":"anthropic_messages","api_key_env":"FIXTURE_KEY","model":"claude-test","anthropic":{"upstream":{"auth_header":"x-api-key"},"default_max_tokens":1000,"cache":{"system":true,"history":true},"default_profile":{"thinking":"adaptive","effort_budget":{"low":2000,"high":8000}}}}`
	model, warning := ownComposed(t, anthropicBase, `{"based_on":"global:openai/default","set":{"anthropic":{"default_max_tokens":2000,"cache":{"history":false,"ttl":"1h"},"default_profile":{"effort_budget":{"high":16000}}}}}`)
	if warning != "" {
		t.Fatal(warning)
	}
	block := jsonObject(t, model.Anthropic)
	cache, _ := block["cache"].(map[string]any)
	profile, _ := block["default_profile"].(map[string]any)
	budget, _ := profile["effort_budget"].(map[string]any)
	upstream, _ := block["upstream"].(map[string]any)
	if block["default_max_tokens"] != float64(2000) || cache["system"] != true || cache["history"] != false || cache["ttl"] != "1h" ||
		profile["thinking"] != "adaptive" || budget["low"] != float64(2000) || budget["high"] != float64(16000) || upstream["auth_header"] != "x-api-key" {
		t.Fatalf("anthropic = %s", model.Anthropic)
	}

	chatBase := `{"provider":"ollama","api":"chat_completions","base_url":"http://127.0.0.1:11434/v1","api_key_env":"FIXTURE_KEY","model":"q","chat_completions":{"max_tokens_field":"max_tokens","system_role":"system","extra_headers":{"X-Team":"a"}}}`
	model, warning = ownComposed(t, chatBase, `{"based_on":"global:openai/default","set":{"chat_completions":{"request_timeout_ms":60000,"system_role":"developer"}}}`)
	if warning != "" {
		t.Fatal(warning)
	}
	chat := jsonObject(t, model.ChatCompletions)
	headers, _ := chat["extra_headers"].(map[string]any)
	if chat["request_timeout_ms"] != float64(60000) || chat["system_role"] != "developer" || chat["max_tokens_field"] != "max_tokens" || headers["X-Team"] != "a" {
		t.Fatalf("chat_completions = %s", model.ChatCompletions)
	}

	refused := map[string][2]string{
		"anthropic.upstream.auth_header>anthropic.upstream": {anthropicBase, `{"anthropic":{"upstream":{"auth_header":"bearer"}}}`},
		"anthropic.upstream.base_url>anthropic.upstream":    {anthropicBase, `{"anthropic":{"upstream":{"base_url":"https://evil.example"}}}`},
		"anthropic.log.headers>anthropic.log":               {anthropicBase, `{"anthropic":{"log":{"headers":["x-api-key"]}}}`},
		"anthropic.models":                                  {anthropicBase, `{"anthropic":{"models":[]}}`},
		"anthropic.extra_headers":                           {anthropicBase, `{"anthropic":{"extra_headers":{"a":"b"}}}`},
		"anthropic.default_profile.extra_body":              {anthropicBase, `{"anthropic":{"default_profile":{"extra_body":{"a":1}}}}`},
		"anthropic.default_profile.thinking":                {anthropicBase, `{"anthropic":{"default_profile":{"thinking":"off"}}}`},
		"anthropic.max_request_bytes":                       {anthropicBase, `{"anthropic":{"max_request_bytes":1}}`},
		"anthropic.cache.later":                             {anthropicBase, `{"anthropic":{"cache":{"later":true}}}`},
		"chat_completions.extra_headers":                    {chatBase, `{"chat_completions":{"extra_headers":{"Authorization":"Bearer x"}}}`},
		"chat_completions.extra_body":                       {chatBase, `{"chat_completions":{"extra_body":{"models":["a","b"]}}}`},
		"chat_completions.max_tokens_field":                 {chatBase, `{"chat_completions":{"max_tokens_field":"x"}}`},
		"chat_completions.request_timeout_ms":               {chatBase, `{"chat_completions":{"request_timeout_ms":"slow"}}`},
		"anthropic.default_profile.effort_budget.high":      {anthropicBase, `{"anthropic":{"default_profile":{"effort_budget":{"high":"many"}}}}`},
	}
	for name, test := range refused {
		t.Run(name, func(t *testing.T) {
			_, warning := ownComposed(t, test[0], `{"based_on":"global:openai/default","set":`+test[1]+`}`)
			// name>at: the refusal names the key it is at; upstream and log are refused whole.
			at := name
			if _, parent, found := strings.Cut(name, ">"); found {
				at = parent
			}
			if warning == "" || !strings.Contains(warning, "set."+at+" ") {
				t.Fatalf("%s is not refused: %q", name, warning)
			}
			byName := strings.Contains(at, "upstream") || strings.Contains(at, "log") || strings.HasSuffix(at, "models") || strings.HasSuffix(at, "extra_headers") || strings.HasSuffix(at, "extra_body")
			if byName != strings.Contains(warning, "cannot be changed by a profile's own model") {
				t.Fatalf("%s: refused by name %v, warning %q", name, byName, warning)
			}
		})
	}
	// A block of another api is refused by the composed model's rules.
	if _, warning := ownComposed(t, chatBase, `{"based_on":"global:openai/default","set":{"anthropic":{"default_max_tokens":1}}}`); !strings.Contains(warning, "anthropic needs api") {
		t.Fatalf("anthropic block on a chat base = %q", warning)
	}
}

// A profile that offers its own model and a shared one under the same name, each with its
// scope, is refused: within a profile a bare ID names one model (decider 3c.1b).
func TestOwnAndSharedModelOfOneNameAreNotBothOffered(t *testing.T) {
	o := newOwnModelFixture(t, `{"models":["profile:openai/default","global:openai/default"]}`)
	wsPut(t, filepath.Join(filepath.Dir(o.own), "default.json"), `{"based_on":"global:openai/default","set":{"name":"Mine"}}`)
	if _, err := o.load(t, "dev/coder"); err == nil || !strings.Contains(err.Error(), "openai/default") {
		t.Fatalf("own and shared openai/default together = %v", err)
	}
}

// A base that fails the model rules is not repaired by an own model's set.
func TestOwnModelDoesNotRepairAFailingBase(t *testing.T) {
	_, warning := ownComposed(t, `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY","request":"not an object"}`, `{"based_on":"global:openai/default","set":{"request":{"temperature":0.2}}}`)
	if !strings.Contains(warning, "its base global:openai/default") || !strings.Contains(warning, "request must be a JSON object") {
		t.Fatalf("own model on a failing base = %q", warning)
	}
}
