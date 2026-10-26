package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func extensionSettingsFor(extension string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"extension":` + extension + `}`)}
}

func decideSystemoneSettingsFor(native string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":` + native + `}`)}
}

func TestNormalizeExtensionDecideSystemoneSettingsDefaultsPerBackend(t *testing.T) {
	absent, err := normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(`{}`), "/jad", "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("absent decide = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeExtensionDecideSystemoneSettings(map[string]json.RawMessage{}, "/jad", "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   ExtensionDecideSystemoneSettings
	}{
		"empty object is openrouter": {`{"decide_systemone":{}}`, ExtensionDecideSystemoneSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"openai":                     {`{"decide_systemone":{"backend":"openai"}}`, ExtensionDecideSystemoneSettings{Backend: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-6-luna", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"nulls use the defaults":     {`{"decide_systemone":{"backend":null,"model":null,"timeout_ms":null}}`, ExtensionDecideSystemoneSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"every key set":              {`{"decide_systemone":{"backend":"openai","base_url":"http://127.0.0.1:9/v1/","api_key_env":"MY_KEY","model":"m","timeout_ms":5,"max_questions":2,"max_input_bytes":7,"max_response_bytes":9,"max_images":3,"max_image_bytes":11}}`, ExtensionDecideSystemoneSettings{Backend: "openai", BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "MY_KEY", Model: "m", TimeoutMS: 5, MaxQuestions: 2, MaxInputBytes: 7, MaxResponseBytes: 9, MaxImages: 3, MaxImageBytes: 11}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(tc.native), "/jad", "setting.json")
			tc.want.Policy = NativePathPolicy{Paths: []NativePathRule{}}
			if err != nil || got == nil || !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

// decide's path rules are its own: read's rules are not borrowed, and without paths the
// policy is the empty default (the workspace only).
func TestNormalizeExtensionDecideSystemoneSettingsTakesOnlyItsOwnPaths(t *testing.T) {
	defaults, err := normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(`{"read":{"paths":{"allow":["/tmp/pics"]}},"decide_systemone":{}}`), "/jad", "setting.json")
	if err != nil || defaults == nil {
		t.Fatalf("settings = %+v, %v", defaults, err)
	}
	if len(defaults.Policy.Paths) != 0 || defaults.Policy.UnrestrictedPaths {
		t.Fatalf("decide_systemone took read's rules: %+v", defaults.Policy)
	}
	set, err := normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(`{"decide_systemone":{"paths":{"allow":["/tmp/pics"],"deny":["private"]}}}`), "/jad", "setting.json")
	if err != nil || set == nil {
		t.Fatalf("settings = %+v, %v", set, err)
	}
	actions := map[string]string{}
	for _, rule := range set.Policy.Paths {
		actions[rule.Action] = rule.Path
	}
	if actions["allow"] != "/tmp/pics" || actions["deny"] == "" {
		t.Fatalf("decide_systemone.paths not parsed: %+v", set.Policy.Paths)
	}
	_, err = normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(`{"decide_systemone":{"paths":3}}`), "/jad", "setting.json")
	if err == nil || !strings.Contains(err.Error(), "tools.extension.decide_systemone.paths") {
		t.Fatalf("bad paths error = %v, want one naming tools.extension.decide_systemone.paths", err)
	}
}

func TestNormalizeExtensionDecideSystemoneSettingsRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"typo":                {`{"decide_systemone":{"max_question":10}}`, "tools.extension.decide_systemone.max_question is not supported; did you mean max_questions?"},
		"unrelated":           {`{"decide_systemone":{"approval":"ask"}}`, "tools.extension.decide_systemone.approval is not supported; supported keys are api_key_env, backend, base_url, max_image_bytes, max_images, max_input_bytes, max_questions, max_response_bytes, model, paths, timeout_ms"},
		"not object":          {`{"decide_systemone":[]}`, "tools.extension.decide_systemone must be an object"},
		"null":                {`{"decide_systemone":null}`, "tools.extension.decide_systemone must be an object"},
		"unknown backend":     {`{"decide_systemone":{"backend":"anthropic"}}`, "tools.extension.decide_systemone.backend must be one of openrouter, openai"},
		"backend not string":  {`{"decide_systemone":{"backend":3}}`, "tools.extension.decide_systemone.backend must be a non-empty string"},
		"empty model":         {`{"decide_systemone":{"model":"  "}}`, "tools.extension.decide_systemone.model must be a non-empty string"},
		"base url scheme":     {`{"decide_systemone":{"base_url":"ftp://x"}}`, "tools.extension.decide_systemone.base_url must be an http or https URL"},
		"base url no host":    {`{"decide_systemone":{"base_url":"https://"}}`, "tools.extension.decide_systemone.base_url must be an http or https URL"},
		"key name":            {`{"decide_systemone":{"api_key_env":"A B"}}`, "tools.extension.decide_systemone.api_key_env must be an environment variable name"},
		"key name digit":      {`{"decide_systemone":{"api_key_env":"1KEY"}}`, "tools.extension.decide_systemone.api_key_env must be an environment variable name"},
		"timeout zero":        {`{"decide_systemone":{"timeout_ms":0}}`, "tools.extension.decide_systemone.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout high":        {`{"decide_systemone":{"timeout_ms":300001}}`, "tools.extension.decide_systemone.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout upper":       {`{"decide_systemone":{"timeout_ms":300000}}`, ""},
		"questions zero":      {`{"decide_systemone":{"max_questions":0}}`, "tools.extension.decide_systemone.max_questions must be a safe integer between 1 and 128"},
		"questions high":      {`{"decide_systemone":{"max_questions":129}}`, "tools.extension.decide_systemone.max_questions must be a safe integer between 1 and 128"},
		"input zero":          {`{"decide_systemone":{"max_input_bytes":0}}`, "tools.extension.decide_systemone.max_input_bytes must be a safe integer"},
		"response fractional": {`{"decide_systemone":{"max_response_bytes":1.5}}`, "tools.extension.decide_systemone.max_response_bytes must be a safe integer"},
		"response string":     {`{"decide_systemone":{"max_response_bytes":"9"}}`, "tools.extension.decide_systemone.max_response_bytes must be a safe integer"},
		"images zero":         {`{"decide_systemone":{"max_images":0}}`, "tools.extension.decide_systemone.max_images must be a safe integer between 1 and 64"},
		"images high":         {`{"decide_systemone":{"max_images":65}}`, "tools.extension.decide_systemone.max_images must be a safe integer between 1 and 64"},
		"images upper":        {`{"decide_systemone":{"max_images":64}}`, ""},
		"image bytes zero":    {`{"decide_systemone":{"max_image_bytes":0}}`, "tools.extension.decide_systemone.max_image_bytes must be a safe integer"},
		"image bytes string":  {`{"decide_systemone":{"max_image_bytes":"9"}}`, "tools.extension.decide_systemone.max_image_bytes must be a safe integer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeExtensionDecideSystemoneSettings(extensionSettingsFor(tc.native), "/jad", "setting.json")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a valid setting: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The schema in sample/schemes is the user-facing contract; it must list the keys the
// loader accepts and the backend defaults it describes.
func TestDecideSystemoneSchemaListsTheKeysTheLoaderAccepts(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Extension struct {
						Properties struct {
							DecideSystemone struct {
								Properties map[string]struct {
									Default any      `json:"default"`
									Enum    []string `json:"enum"`
								} `json:"properties"`
							} `json:"decide_systemone"`
						} `json:"properties"`
					} `json:"extension"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.Tools.Properties.Extension.Properties.DecideSystemone.Properties
	var got []string
	for key := range properties {
		got = append(got, key)
	}
	// paths comes from the shared read_style definition, which the flatten test covers.
	var want []string
	for _, key := range extensionDecideSystemoneKeys {
		if key != "paths" {
			want = append(want, key)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema keys %v, loader accepts %v", got, want)
	}
	if !reflect.DeepEqual(properties["backend"].Enum, extensionDecideSystemoneBackends) {
		t.Fatalf("schema backends %v, loader accepts %v", properties["backend"].Enum, extensionDecideSystemoneBackends)
	}
	defaults := map[string]float64{"timeout_ms": float64(defaultExtensionDecideSystemoneTimeoutMS), "max_questions": float64(defaultExtensionDecideSystemoneMaxQuestions), "max_input_bytes": float64(defaultExtensionDecideSystemoneMaxInputBytes), "max_response_bytes": float64(defaultExtensionDecideSystemoneMaxResponseBytes), "max_images": float64(defaultExtensionDecideSystemoneMaxImages), "max_image_bytes": float64(defaultExtensionDecideSystemoneMaxImageBytes)}
	for key, value := range defaults {
		if properties[key].Default != value {
			t.Errorf("schema default of %s = %v, loader default %v", key, properties[key].Default, value)
		}
	}
	if properties["backend"].Default != defaultExtensionDecideSystemoneBackend {
		t.Errorf("schema default backend = %v, loader default %v", properties["backend"].Default, defaultExtensionDecideSystemoneBackend)
	}
}

// The shipped triager sample must load as an agent folder and list decide, the only
// tool it needs; a stale sample would send users to a configuration error.
func TestShippedTriagerSampleLoadsAsAnAgent(t *testing.T) {
	root := t.TempDir()
	samples, err := filepath.Abs("../../../sample/subagents")
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents_dir":[`+strconv.Quote(samples)+`]}`), filepath.Join(root, "setting.json"), root, "")
	if err != nil {
		t.Fatalf("the sample agents do not load: %v", err)
	}
	var triager *NativeSubagentAgent
	for index := range got.Agents {
		if got.Agents[index].Name == "triager" {
			triager = &got.Agents[index]
		}
	}
	if triager == nil {
		t.Fatalf("no triager among %d agents", len(got.Agents))
	}
	if !triager.HasTools || !reflect.DeepEqual(triager.Tools, []string{"decide_systemone"}) {
		t.Fatalf("triager tools = %v (has %v)", triager.Tools, triager.HasTools)
	}
	if !strings.Contains(triager.PromptText, "decide_systemone") || triager.Description == "" {
		t.Fatalf("triager = %+v", triager)
	}
}

func TestDecideSystemoneLivesUnderToolsExtensionOnly(t *testing.T) {
	config, err := loadSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{"decide_systemone": map[string]any{"backend": "openai"}}}})
	if err != nil {
		t.Fatalf("tools.extension.decide_systemone was rejected: %v", err)
	}
	if config.ExtensionDecideSystemone == nil || config.ExtensionDecideSystemone.Backend != "openai" {
		t.Fatalf("ExtensionDecideSystemone = %+v", config.ExtensionDecideSystemone)
	}
	_, err = loadSetting(t, nativeTools(map[string]any{"decide_systemone": map[string]any{}}))
	if err == nil || !strings.Contains(err.Error(), "tools.native.decide_systemone is not supported") {
		t.Fatalf("tools.native.decide_systemone error = %v, want it rejected as an unsupported key", err)
	}
	_, err = loadSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{"nope": map[string]any{}}}})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.nope is not supported; did you mean") && !strings.Contains(err.Error(), "tools.extension.nope is not supported") {
		t.Fatalf("unknown extension tool error = %v", err)
	}
}

func TestProfileToolEntryAcceptsDecideSystemoneAsAnExtensionTool(t *testing.T) {
	if err := checkProfileToolEntry("decide_systemone"); err != nil {
		t.Fatalf("decide_systemone entry: %v", err)
	}
	if err := checkProfileToolEntry("raed"); err == nil || !strings.Contains(err.Error(), "decide_systemone") {
		t.Fatalf("misspelled entry error = %v, want one that lists decide_systemone", err)
	}
}

func TestOldDecideNameIsAnUnknownToolWithNoAlias(t *testing.T) {
	_, err := loadSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{"decide": map[string]any{}}}})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.decide is not supported") || !strings.Contains(err.Error(), "decide_systemone") {
		t.Fatalf("tools.extension.decide error = %v, want unsupported key naming decide_systemone", err)
	}
	if err := checkProfileToolEntry("decide"); err == nil || !strings.Contains(err.Error(), `"decide" is not a native tool`) {
		t.Fatalf("profile entry decide error = %v", err)
	}
}
