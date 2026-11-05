package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func loadWebSearch(t *testing.T, raw string) (*NativeWebSearchSettings, error) {
	t.Helper()
	return normalizeNativeWebSearchSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"web_search":` + raw + `}}`)}, "setting.json")
}

func TestWebSearchSettingsDefaultsPerProvider(t *testing.T) {
	brave, err := loadWebSearch(t, `{"provider":"brave"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := NativeWebSearchSettings{
		Provider: "brave", BaseURL: defaultNativeWebSearchBraveBaseURL, APIKeyEnv: "BRAVE_SEARCH_API_KEY",
		DefaultResults: 5, MaxResults: 10, TimeoutMS: 15000, MaxResponseBytes: 2_000_000, MaxSnippetChars: 500, MaxQueryChars: 400,
		UserAgent: defaultNativeWebSearchUserAgent, AllowedDomains: []string{}, BlockedDomains: []string{},
	}
	if !reflect.DeepEqual(*brave, want) {
		t.Fatalf("brave = %+v\nwant %+v", *brave, want)
	}
	tavily, err := loadWebSearch(t, `{"provider":"tavily","max_results":3}`)
	if err != nil || tavily.BaseURL != defaultNativeWebSearchTavilyBaseURL || tavily.APIKeyEnv != "TAVILY_API_KEY" || tavily.DefaultResults != 3 {
		t.Fatalf("tavily = %+v, %v (default_results follows a smaller max_results)", tavily, err)
	}
	searx, err := loadWebSearch(t, `{"provider":"searxng","base_url":"http://127.0.0.1:8888/search","allowed_domains":["WWW.Go.Dev","go.dev"],"blocked_domains":null}`)
	if err != nil || searx.APIKeyEnv != "" || !reflect.DeepEqual(searx.AllowedDomains, []string{"go.dev"}) || len(searx.BlockedDomains) != 0 {
		t.Fatalf("searxng = %+v, %v", searx, err)
	}
	none, err := normalizeNativeWebSearchSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{}}`)}, "setting.json")
	if err != nil || none != nil {
		t.Fatalf("absent web_search = %+v, %v, want nil", none, err)
	}
}

func TestWebSearchSettingsRefuseBadValues(t *testing.T) {
	for raw, want := range map[string]string{
		`{}`:                     "provider is required",
		`{"provider":"google"}`:  "provider must be",
		`{"provider":"searxng"}`: "base_url is required",
		`{"provider":"brave","base_url":"ftp://x"}`:                "base_url must be",
		`{"provider":"brave","api_key_env":"1BAD"}`:                "api_key_env",
		`{"provider":"brave","api_key_env":""}`:                    "api_key_env must name",
		`{"provider":"brave","max_results":51}`:                    "max_results",
		`{"provider":"brave","default_results":0}`:                 "default_results",
		`{"provider":"brave","default_results":9,"max_results":3}`: "must not be larger than max_results",
		`{"provider":"brave","timeout_ms":0}`:                      "timeout_ms",
		`{"provider":"brave","max_response_bytes":10}`:             "max_response_bytes",
		`{"provider":"brave","max_snippet_chars":-1}`:              "max_snippet_chars",
		`{"provider":"brave","user_agent":"a\nb"}`:                 "user_agent",
		`{"provider":"brave","allowed_domains":["a b"]}`:           "allowed_domains",
		`{"provider":"brave","blocked_domains":"x.com"}`:           "blocked_domains",
		`{"provider":"brave","region":"de"}`:                       "region is not supported",
		`[]`:                                                       "web_search",
	} {
		if _, err := loadWebSearch(t, raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to contain %q", raw, err, want)
		}
	}
}

func TestWebSearchSettingsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if candidate, ok := value["web_search"].(map[string]any); ok && found == nil {
				if _, hasProvider := candidate["properties"].(map[string]any)["provider"]; hasProvider {
					found = candidate
					return
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(schema)
	if found == nil {
		t.Fatal("setting.schema.json has no tools.native.web_search")
	}
	properties := found["properties"].(map[string]any)
	if len(properties) != len(nativeWebSearchKeys) {
		t.Fatalf("schema keys = %d, loader keys = %v", len(properties), nativeWebSearchKeys)
	}
	brave, err := loadWebSearch(t, `{"provider":"brave"}`)
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[string]any{
		"default_results": brave.DefaultResults, "max_results": brave.MaxResults, "timeout_ms": brave.TimeoutMS,
		"max_response_bytes": brave.MaxResponseBytes, "max_snippet_chars": brave.MaxSnippetChars, "max_query_chars": brave.MaxQueryChars,
		"user_agent": brave.UserAgent,
	}
	for _, key := range nativeWebSearchKeys {
		field, ok := properties[key].(map[string]any)
		if !ok {
			t.Errorf("schema has no web_search.%s", key)
			continue
		}
		if want, ok := defaults[key]; ok {
			encoded, _ := json.Marshal(want)
			var expected any
			_ = json.Unmarshal(encoded, &expected)
			if !reflect.DeepEqual(field["default"], expected) {
				t.Errorf("web_search.%s schema default = %v, loader default = %v", key, field["default"], expected)
			}
		}
	}
	bounds := map[string][2]float64{
		"max_results": {1, float64(maxNativeWebSearchMaxResults)}, "default_results": {1, float64(maxNativeWebSearchMaxResults)},
		"timeout_ms": {1, float64(maxNativeWebSearchTimeoutMS)}, "max_response_bytes": {1024, float64(maxNativeWebSearchResponseBytes)},
		"max_snippet_chars": {0, float64(maxNativeWebSearchSnippetChars)}, "max_query_chars": {1, float64(maxNativeWebSearchMaxQueryChars)},
	}
	for key, bound := range bounds {
		field := properties[key].(map[string]any)
		if field["minimum"] != bound[0] || field["maximum"] != bound[1] {
			t.Errorf("web_search.%s schema bounds = %v..%v, loader %v", key, field["minimum"], field["maximum"], bound)
		}
	}
}
