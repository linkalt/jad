package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

type layoutSettingsCase struct {
	Name     string                     `json:"name"`
	Native   map[string]any             `json:"native"`
	Settings map[string]json.RawMessage `json:"settings"`
	OK       bool                       `json:"ok"`
	Error    string                     `json:"error"`
}

// goOnlyNativeTools are native tools older error texts in the case file do not list.
var goOnlyNativeTools = []string{"notebook", "lsp", "apply_patch", "subagent", "read_image", "profile_message", "tool_smith", "transcript", "transcripts", "load_tool", "code_context"}

// layoutErrorPrefix strips the file path (and the "settings" label) a layout error starts with.
var layoutErrorPrefix = regexp.MustCompile(`^\S+\.json(: settings)?: `)

// TestLayoutSettingsCases replays 138 settings through the catalog layout (the `native`
// block goes in the toolset, the rest in the session's settings) and compares each
// rejection's message with the text in testdata/layout-settings.cases.json. The texts were
// written by the removed TypeScript loader and are fixed test data.
func TestLayoutFileRefusesDataAfterTheObject(t *testing.T) {
	for _, contents := range []string{`{"name":"a"} garbage`, `{"name":"a"} }`, `{"name":"a"} {}`, `{"name":"a"} 1`} {
		var into struct {
			Name string `json:"name"`
		}
		if err := parseLayoutFile("f.json", []byte(contents), &into); err == nil || !strings.Contains(err.Error(), "unexpected data after the JSON object") {
			t.Fatalf("parseLayoutFile(%s) error = %v, want the trailing-data refusal", contents, err)
		}
	}
	var into struct {
		Name string `json:"name"`
	}
	if err := parseLayoutFile("f.json", []byte("{\"name\":\"a\"}\n"), &into); err != nil {
		t.Fatalf("a trailing newline must be allowed: %v", err)
	}
}

func TestLayoutSettingsCases(t *testing.T) {
	var cases []layoutSettingsCase
	readJSONFile(t, "testdata/layout-settings.cases.json", &cases)
	if len(cases) != 138 {
		t.Fatalf("%d cases, want 138", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			config, err := loadLayoutConfig(t, layouttest.Spec{Native: tc.Native, RawSettings: tc.Settings})
			if tc.OK {
				if err != nil {
					t.Fatalf("rejected a setting that was accepted: %v", err)
				}
				_ = config
				return
			}
			var configErr *ConfigError
			var clientErr *ClientError
			message := ""
			switch {
			case errors.As(err, &configErr):
				message = configErr.Message
			case errors.As(err, &clientErr):
				message = clientErr.Message
			case err != nil:
				message = err.Error()
			default:
				t.Fatalf("accepted a setting that was rejected with %q", tc.Error)
			}
			message = layoutErrorPrefix.ReplaceAllString(message, "")
			for _, name := range goOnlyNativeTools {
				if strings.Contains(message, ", "+name) {
					message = strings.Replace(message, ", "+name, "", 1)
				} else {
					message = strings.Replace(message, " "+name+", ", " ", 1)
				}
			}
			if message != tc.Error {
				t.Fatalf("error mismatch\n got:  %s\n want: %s", message, tc.Error)
			}
		})
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, target); err != nil {
		t.Fatal(err)
	}
}
