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
var goOnlyNativeTools = []string{"subagent", "decide", "read_image", "profile_message", "tool_smith"}

// layoutErrorPrefix strips the file path (and the "settings" label) a layout error starts with.
var layoutErrorPrefix = regexp.MustCompile(`^\S+\.json(: settings)?: `)

// TestLayoutSettingsCases replays 149 settings through the catalog layout (the `native`
// block goes in the toolset, the rest in the session's settings) and compares each
// rejection's message with the text in testdata/layout-settings.cases.json. The texts were
// written by the removed TypeScript loader and are fixed test data.
func TestLayoutSettingsCases(t *testing.T) {
	var cases []layoutSettingsCase
	readJSONFile(t, "testdata/layout-settings.cases.json", &cases)
	if len(cases) != 149 {
		t.Fatalf("%d cases, want 149", len(cases))
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
				message = strings.Replace(message, ", "+name, "", 1)
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
