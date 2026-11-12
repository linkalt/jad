package sessionfs

import (
	"encoding/json"
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
	for _, refused := range ownModelRefusedKeys {
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
	if err := CheckModelEffort(o.own, "low"); err == nil || !strings.Contains(err.Error(), "based_on") {
		t.Fatalf("a sub-agent's check of the own file = %v", err)
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

// A shared model file that sets based_on is refused: only own model files take it.
func TestSharedModelFileCannotSetBasedOn(t *testing.T) {
	if err := validateModelFile("/g/m.json", []byte(`{"provider":"openai","model":"m","based_on":"global:openai/default"}`)); err == nil || !strings.Contains(err.Error(), "based_on") {
		t.Fatalf("shared file with based_on = %v", err)
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
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var top, set []string
	for key := range schema.Properties {
		top = append(top, key)
	}
	for key := range schema.Properties["set"].Properties {
		set = append(set, key)
	}
	slices.Sort(top)
	slices.Sort(set)
	if !slices.Equal(top, slices.Sorted(slices.Values(ownModelKeys))) || !slices.Equal(set, slices.Sorted(slices.Values(ownModelSetKeys))) {
		t.Fatalf("schema keys %v / %v, loader %v / %v", top, set, ownModelKeys, ownModelSetKeys)
	}
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
