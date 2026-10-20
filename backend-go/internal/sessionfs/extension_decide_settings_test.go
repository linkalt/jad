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

func decideSettingsFor(native string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":` + native + `}`)}
}

func TestNormalizeExtensionDecideSettingsDefaultsPerBackend(t *testing.T) {
	absent, err := normalizeExtensionDecideSettings(extensionSettingsFor(`{}`), "/jad", "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("absent decide = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeExtensionDecideSettings(map[string]json.RawMessage{}, "/jad", "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   ExtensionDecideSettings
	}{
		"empty object is openrouter": {`{"decide":{}}`, ExtensionDecideSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"openai":                     {`{"decide":{"backend":"openai"}}`, ExtensionDecideSettings{Backend: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-6-luna", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"nulls use the defaults":     {`{"decide":{"backend":null,"model":null,"timeout_ms":null}}`, ExtensionDecideSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"every key set":              {`{"decide":{"backend":"openai","base_url":"http://127.0.0.1:9/v1/","api_key_env":"MY_KEY","model":"m","timeout_ms":5,"max_questions":2,"max_input_bytes":7,"max_response_bytes":9,"max_images":3,"max_image_bytes":11}}`, ExtensionDecideSettings{Backend: "openai", BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "MY_KEY", Model: "m", TimeoutMS: 5, MaxQuestions: 2, MaxInputBytes: 7, MaxResponseBytes: 9, MaxImages: 3, MaxImageBytes: 11}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeExtensionDecideSettings(extensionSettingsFor(tc.native), "/jad", "setting.json")
			tc.want.Policy = NativePathPolicy{Paths: []NativePathRule{}}
			if err != nil || got == nil || !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

// decide's path rules are its own: read's rules are not borrowed, and without paths the
// policy is the empty default (the workspace only).
func TestNormalizeExtensionDecideSettingsTakesOnlyItsOwnPaths(t *testing.T) {
	defaults, err := normalizeExtensionDecideSettings(extensionSettingsFor(`{"read":{"paths":{"allow":["/tmp/pics"]}},"decide":{}}`), "/jad", "setting.json")
	if err != nil || defaults == nil {
		t.Fatalf("settings = %+v, %v", defaults, err)
	}
	if len(defaults.Policy.Paths) != 0 || defaults.Policy.UnrestrictedPaths {
		t.Fatalf("decide took read's rules: %+v", defaults.Policy)
	}
	set, err := normalizeExtensionDecideSettings(extensionSettingsFor(`{"decide":{"paths":{"allow":["/tmp/pics"],"deny":["private"]}}}`), "/jad", "setting.json")
	if err != nil || set == nil {
		t.Fatalf("settings = %+v, %v", set, err)
	}
	actions := map[string]string{}
	for _, rule := range set.Policy.Paths {
		actions[rule.Action] = rule.Path
	}
	if actions["allow"] != "/tmp/pics" || actions["deny"] == "" {
		t.Fatalf("decide.paths not parsed: %+v", set.Policy.Paths)
	}
	_, err = normalizeExtensionDecideSettings(extensionSettingsFor(`{"decide":{"paths":3}}`), "/jad", "setting.json")
	if err == nil || !strings.Contains(err.Error(), "tools.extension.decide.paths") {
		t.Fatalf("bad paths error = %v, want one naming tools.extension.decide.paths", err)
	}
}

func TestNormalizeExtensionDecideSettingsRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"typo":                {`{"decide":{"max_question":10}}`, "tools.extension.decide.max_question is not supported; did you mean max_questions?"},
		"unrelated":           {`{"decide":{"approval":"ask"}}`, "tools.extension.decide.approval is not supported; supported keys are api_key_env, backend, base_url, max_image_bytes, max_images, max_input_bytes, max_questions, max_response_bytes, model, paths, timeout_ms"},
		"not object":          {`{"decide":[]}`, "tools.extension.decide must be an object"},
		"null":                {`{"decide":null}`, "tools.extension.decide must be an object"},
		"unknown backend":     {`{"decide":{"backend":"anthropic"}}`, "tools.extension.decide.backend must be one of openrouter, openai"},
		"backend not string":  {`{"decide":{"backend":3}}`, "tools.extension.decide.backend must be a non-empty string"},
		"empty model":         {`{"decide":{"model":"  "}}`, "tools.extension.decide.model must be a non-empty string"},
		"base url scheme":     {`{"decide":{"base_url":"ftp://x"}}`, "tools.extension.decide.base_url must be an http or https URL"},
		"base url no host":    {`{"decide":{"base_url":"https://"}}`, "tools.extension.decide.base_url must be an http or https URL"},
		"key name":            {`{"decide":{"api_key_env":"A B"}}`, "tools.extension.decide.api_key_env must be an environment variable name"},
		"key name digit":      {`{"decide":{"api_key_env":"1KEY"}}`, "tools.extension.decide.api_key_env must be an environment variable name"},
		"timeout zero":        {`{"decide":{"timeout_ms":0}}`, "tools.extension.decide.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout high":        {`{"decide":{"timeout_ms":300001}}`, "tools.extension.decide.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout upper":       {`{"decide":{"timeout_ms":300000}}`, ""},
		"questions zero":      {`{"decide":{"max_questions":0}}`, "tools.extension.decide.max_questions must be a safe integer between 1 and 128"},
		"questions high":      {`{"decide":{"max_questions":129}}`, "tools.extension.decide.max_questions must be a safe integer between 1 and 128"},
		"input zero":          {`{"decide":{"max_input_bytes":0}}`, "tools.extension.decide.max_input_bytes must be a safe integer"},
		"response fractional": {`{"decide":{"max_response_bytes":1.5}}`, "tools.extension.decide.max_response_bytes must be a safe integer"},
		"response string":     {`{"decide":{"max_response_bytes":"9"}}`, "tools.extension.decide.max_response_bytes must be a safe integer"},
		"images zero":         {`{"decide":{"max_images":0}}`, "tools.extension.decide.max_images must be a safe integer between 1 and 64"},
		"images high":         {`{"decide":{"max_images":65}}`, "tools.extension.decide.max_images must be a safe integer between 1 and 64"},
		"images upper":        {`{"decide":{"max_images":64}}`, ""},
		"image bytes zero":    {`{"decide":{"max_image_bytes":0}}`, "tools.extension.decide.max_image_bytes must be a safe integer"},
		"image bytes string":  {`{"decide":{"max_image_bytes":"9"}}`, "tools.extension.decide.max_image_bytes must be a safe integer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeExtensionDecideSettings(extensionSettingsFor(tc.native), "/jad", "setting.json")
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
func TestDecideSchemaListsTheKeysTheLoaderAccepts(t *testing.T) {
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
							Decide struct {
								Properties map[string]struct {
									Default any      `json:"default"`
									Enum    []string `json:"enum"`
								} `json:"properties"`
							} `json:"decide"`
						} `json:"properties"`
					} `json:"extension"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.Tools.Properties.Extension.Properties.Decide.Properties
	var got []string
	for key := range properties {
		got = append(got, key)
	}
	// paths comes from the shared read_style definition, which the flatten test covers.
	var want []string
	for _, key := range extensionDecideKeys {
		if key != "paths" {
			want = append(want, key)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema keys %v, loader accepts %v", got, want)
	}
	if !reflect.DeepEqual(properties["backend"].Enum, extensionDecideBackends) {
		t.Fatalf("schema backends %v, loader accepts %v", properties["backend"].Enum, extensionDecideBackends)
	}
	defaults := map[string]float64{"timeout_ms": float64(defaultExtensionDecideTimeoutMS), "max_questions": float64(defaultExtensionDecideMaxQuestions), "max_input_bytes": float64(defaultExtensionDecideMaxInputBytes), "max_response_bytes": float64(defaultExtensionDecideMaxResponseBytes), "max_images": float64(defaultExtensionDecideMaxImages), "max_image_bytes": float64(defaultExtensionDecideMaxImageBytes)}
	for key, value := range defaults {
		if properties[key].Default != value {
			t.Errorf("schema default of %s = %v, loader default %v", key, properties[key].Default, value)
		}
	}
	if properties["backend"].Default != defaultExtensionDecideBackend {
		t.Errorf("schema default backend = %v, loader default %v", properties["backend"].Default, defaultExtensionDecideBackend)
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
	if !triager.HasTools || !reflect.DeepEqual(triager.Tools, []string{"decide"}) {
		t.Fatalf("triager tools = %v (has %v)", triager.Tools, triager.HasTools)
	}
	if !strings.Contains(triager.PromptText, "decide") || triager.Description == "" {
		t.Fatalf("triager = %+v", triager)
	}
}

func TestDecideLivesUnderToolsExtensionOnly(t *testing.T) {
	config, err := loadApprovalSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{"decide": map[string]any{"backend": "openai"}}}})
	if err != nil {
		t.Fatalf("tools.extension.decide was rejected: %v", err)
	}
	if config.ExtensionDecide == nil || config.ExtensionDecide.Backend != "openai" {
		t.Fatalf("ExtensionDecide = %+v", config.ExtensionDecide)
	}
	_, err = loadApprovalSetting(t, nativeTools(map[string]any{"decide": map[string]any{}}))
	if err == nil || !strings.Contains(err.Error(), "tools.native.decide is not supported") {
		t.Fatalf("tools.native.decide error = %v, want it rejected as an unsupported key", err)
	}
	_, err = loadApprovalSetting(t, map[string]any{"tools": map[string]any{"native": map[string]any{}, "extension": map[string]any{"nope": map[string]any{}}}})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.nope is not supported; did you mean") && !strings.Contains(err.Error(), "tools.extension.nope is not supported") {
		t.Fatalf("unknown extension tool error = %v", err)
	}
}

func TestProfileToolEntryAcceptsDecideAsAnExtensionTool(t *testing.T) {
	if err := checkProfileToolEntry("decide"); err != nil {
		t.Fatalf("decide entry: %v", err)
	}
	if err := checkProfileToolEntry("raed"); err == nil || !strings.Contains(err.Error(), "decide") {
		t.Fatalf("misspelled entry error = %v, want one that lists decide", err)
	}
}
