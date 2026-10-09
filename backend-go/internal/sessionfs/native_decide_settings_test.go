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

func decideSettingsFor(native string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":` + native + `}`)}
}

func TestNormalizeNativeDecideSettingsDefaultsPerBackend(t *testing.T) {
	absent, err := normalizeNativeDecideSettings(decideSettingsFor(`{}`), "setting.json")
	if err != nil || absent != nil {
		t.Fatalf("absent decide = %+v, %v; want nil", absent, err)
	}
	noTools, err := normalizeNativeDecideSettings(map[string]json.RawMessage{}, "setting.json")
	if err != nil || noTools != nil {
		t.Fatalf("no tools = %+v, %v; want nil", noTools, err)
	}
	for name, tc := range map[string]struct {
		native string
		want   NativeDecideSettings
	}{
		"empty object is openrouter": {`{"decide":{}}`, NativeDecideSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"openai":                     {`{"decide":{"backend":"openai"}}`, NativeDecideSettings{Backend: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-6-luna", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"nulls use the defaults":     {`{"decide":{"backend":null,"model":null,"timeout_ms":null}}`, NativeDecideSettings{Backend: "openrouter", BaseURL: "https://openrouter.ai/api/alpha", APIKeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna-decisions", TimeoutMS: 30000, MaxQuestions: 16, MaxInputBytes: 200000, MaxResponseBytes: 1048576, MaxImages: 8, MaxImageBytes: 5242880}},
		"every key set":              {`{"decide":{"backend":"openai","base_url":"http://127.0.0.1:9/v1/","api_key_env":"MY_KEY","model":"m","timeout_ms":5,"max_questions":2,"max_input_bytes":7,"max_response_bytes":9,"max_images":3,"max_image_bytes":11}}`, NativeDecideSettings{Backend: "openai", BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "MY_KEY", Model: "m", TimeoutMS: 5, MaxQuestions: 2, MaxInputBytes: 7, MaxResponseBytes: 9, MaxImages: 3, MaxImageBytes: 11}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeNativeDecideSettings(decideSettingsFor(tc.native), "setting.json")
			if err != nil || got == nil || *got != tc.want {
				t.Fatalf("settings = %+v, %v\nwant %+v", got, err, tc.want)
			}
		})
	}
}

func TestNormalizeNativeDecideSettingsRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct{ native, want string }{
		"typo":                {`{"decide":{"max_question":10}}`, "tools.native.decide.max_question is not supported; did you mean max_questions?"},
		"unrelated":           {`{"decide":{"approval":"ask"}}`, "tools.native.decide.approval is not supported; supported keys are api_key_env, backend, base_url, max_image_bytes, max_images, max_input_bytes, max_questions, max_response_bytes, model, timeout_ms"},
		"not object":          {`{"decide":[]}`, "tools.native.decide must be an object"},
		"null":                {`{"decide":null}`, "tools.native.decide must be an object"},
		"unknown backend":     {`{"decide":{"backend":"anthropic"}}`, "tools.native.decide.backend must be one of openrouter, openai"},
		"backend not string":  {`{"decide":{"backend":3}}`, "tools.native.decide.backend must be a non-empty string"},
		"empty model":         {`{"decide":{"model":"  "}}`, "tools.native.decide.model must be a non-empty string"},
		"base url scheme":     {`{"decide":{"base_url":"ftp://x"}}`, "tools.native.decide.base_url must be an http or https URL"},
		"base url no host":    {`{"decide":{"base_url":"https://"}}`, "tools.native.decide.base_url must be an http or https URL"},
		"key name":            {`{"decide":{"api_key_env":"A B"}}`, "tools.native.decide.api_key_env must be an environment variable name"},
		"key name digit":      {`{"decide":{"api_key_env":"1KEY"}}`, "tools.native.decide.api_key_env must be an environment variable name"},
		"timeout zero":        {`{"decide":{"timeout_ms":0}}`, "tools.native.decide.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout high":        {`{"decide":{"timeout_ms":300001}}`, "tools.native.decide.timeout_ms must be a safe integer between 1 and 300000"},
		"timeout upper":       {`{"decide":{"timeout_ms":300000}}`, ""},
		"questions zero":      {`{"decide":{"max_questions":0}}`, "tools.native.decide.max_questions must be a safe integer between 1 and 128"},
		"questions high":      {`{"decide":{"max_questions":129}}`, "tools.native.decide.max_questions must be a safe integer between 1 and 128"},
		"input zero":          {`{"decide":{"max_input_bytes":0}}`, "tools.native.decide.max_input_bytes must be a safe integer"},
		"response fractional": {`{"decide":{"max_response_bytes":1.5}}`, "tools.native.decide.max_response_bytes must be a safe integer"},
		"response string":     {`{"decide":{"max_response_bytes":"9"}}`, "tools.native.decide.max_response_bytes must be a safe integer"},
		"images zero":         {`{"decide":{"max_images":0}}`, "tools.native.decide.max_images must be a safe integer between 1 and 64"},
		"images high":         {`{"decide":{"max_images":65}}`, "tools.native.decide.max_images must be a safe integer between 1 and 64"},
		"images upper":        {`{"decide":{"max_images":64}}`, ""},
		"image bytes zero":    {`{"decide":{"max_image_bytes":0}}`, "tools.native.decide.max_image_bytes must be a safe integer"},
		"image bytes string":  {`{"decide":{"max_image_bytes":"9"}}`, "tools.native.decide.max_image_bytes must be a safe integer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeNativeDecideSettings(decideSettingsFor(tc.native), "setting.json")
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
					Native struct {
						Properties struct {
							Decide struct {
								Properties map[string]struct {
									Default any      `json:"default"`
									Enum    []string `json:"enum"`
								} `json:"properties"`
							} `json:"decide"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.Tools.Properties.Native.Properties.Decide.Properties
	var got []string
	for key := range properties {
		got = append(got, key)
	}
	want := append([]string(nil), nativeDecideKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema keys %v, loader accepts %v", got, want)
	}
	if !reflect.DeepEqual(properties["backend"].Enum, nativeDecideBackends) {
		t.Fatalf("schema backends %v, loader accepts %v", properties["backend"].Enum, nativeDecideBackends)
	}
	defaults := map[string]float64{"timeout_ms": float64(defaultNativeDecideTimeoutMS), "max_questions": float64(defaultNativeDecideMaxQuestions), "max_input_bytes": float64(defaultNativeDecideMaxInputBytes), "max_response_bytes": float64(defaultNativeDecideMaxResponseBytes), "max_images": float64(defaultNativeDecideMaxImages), "max_image_bytes": float64(defaultNativeDecideMaxImageBytes)}
	for key, value := range defaults {
		if properties[key].Default != value {
			t.Errorf("schema default of %s = %v, loader default %v", key, properties[key].Default, value)
		}
	}
	if properties["backend"].Default != defaultNativeDecideBackend {
		t.Errorf("schema default backend = %v, loader default %v", properties["backend"].Default, defaultNativeDecideBackend)
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
