package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type parityCase struct {
	Name    string          `json:"name"`
	Profile string          `json:"profile"`
	Model   string          `json:"model"`
	Setting json.RawMessage `json:"setting"`
}

type parityExpectation struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Error   string `json:"error"`
	OK      *struct {
		Paths     map[string]json.RawMessage `json:"paths"`
		ModelPath string                     `json:"model_path"`
	} `json:"ok"`
}

// goOnlyNativeTools are native tools the golden's TypeScript loader never had. The
// TypeScript backend is removed, so the golden will never list them.
var goOnlyNativeTools = []string{"subagent", "decide"}

// goOnlyToolKeys are keys of a native tool that exist only in Go, by the text they add to a
// "supported keys are ..." list. The golden's exec has no env_file.
var goOnlyToolKeys = []string{"env_file, "}

// TestConfigAgainstFixedGolden replays settings through the legacy loader (LoadSessionConfig
// on a session folder with a setting.json) and compares resolved storage paths and error
// messages with a fixed file. The file was written by the TypeScript SessionConfigLoader;
// that backend is removed (tag legacy-final has it), so the file is never regenerated and
// is test data, not a second implementation. It is replaced by layout cases when the legacy
// loader goes (backend-go/DECISIONS.md).
func TestConfigAgainstFixedGolden(t *testing.T) {
	var cases []parityCase
	readJSONFile(t, "testdata/config-parity.cases.json", &cases)
	var expected []parityExpectation
	readJSONFile(t, "testdata/config-parity.golden.json", &expected)
	if len(cases) != len(expected) {
		t.Fatalf("cases=%d golden=%d; edit both files together", len(cases), len(expected))
	}
	home, _ := os.UserHomeDir()
	for index, tc := range cases {
		want := expected[index]
		t.Run(tc.Name, func(t *testing.T) {
			if want.Name != tc.Name {
				t.Fatalf("golden order mismatch: %q vs %q", want.Name, tc.Name)
			}
			root := t.TempDir()
			dir := filepath.Join(root, "session-1")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			setting := strings.ReplaceAll(string(tc.Setting), "$ROOT", root)
			if err := os.WriteFile(filepath.Join(dir, "setting.json"), []byte(setting), 0o600); err != nil {
				t.Fatal(err)
			}
			model := `{"schema_version":1,"provider":"openai","model":"gpt-test","api_key_env":"TEST_OPENAI_KEY","base_url":"https://api.example.test/v1/","request":{"temperature":0.2}}`
			for _, name := range []string{"model.json", "alternate.json", "other.json"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(model), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			config, err := LoadSessionConfig(Location{SessionID: "session-1", Directory: dir, Root: Root{GlobalJadRoot: root}}, tc.Profile, tc.Model)
			if want.OK == nil {
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
					t.Fatalf("Go accepted a setting TypeScript rejected with %q", want.Error)
				}
				message = strings.ReplaceAll(message, root, "$ROOT")
				message = strings.TrimPrefix(message, "$ROOT/session-1/setting.json: ")
				// The golden comes from the frozen TypeScript loader, which does not have
				// these tools, so the list of supported tools in an error differs by them.
				for _, name := range goOnlyNativeTools {
					message = strings.Replace(message, ", "+name, "", 1)
				}
				if strings.HasPrefix(message, "tools.native.exec.") {
					for _, key := range goOnlyToolKeys {
						message = strings.Replace(message, key, "", 1)
					}
				}
				if message != want.Error {
					t.Fatalf("error mismatch\n go: %s\n ts: %s", message, want.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("Go rejected a setting TypeScript accepted: %v", err)
			}
			got := map[string]any{}
			sub := func(value string) string {
				value = strings.ReplaceAll(value, root, "$ROOT")
				if home != "" {
					value = strings.ReplaceAll(value, home, "$HOME")
				}
				return value
			}
			storage := config.StoragePaths
			for field, value := range storage.pathFields() {
				got[field] = sub(value)
			}
			if storage.ToolStorage != "" {
				got["tool_storage"] = sub(storage.ToolStorage)
			}
			if config.Profile != "" {
				got["active_profile"] = config.Profile
			}
			if config.DefaultProfile != "" {
				got["default_profile"] = config.DefaultProfile
			}
			if config.ModelDefault != "" {
				got["model_default"] = config.ModelDefault
			}
			if config.Models != nil {
				got["models"] = config.Models
			}
			wantPaths := map[string]any{}
			for field, raw := range want.OK.Paths {
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if text, ok := value.(string); ok && home != "" {
					value = strings.ReplaceAll(text, home, "$HOME")
				}
				wantPaths[field] = value
			}
			gotNormalized := map[string]any{}
			encoded, _ := json.Marshal(got)
			_ = json.Unmarshal(encoded, &gotNormalized)
			if !reflect.DeepEqual(gotNormalized, wantPaths) {
				t.Fatalf("paths mismatch\n go: %v\n ts: %v", gotNormalized, wantPaths)
			}
			if sub(config.ModelPath) != strings.ReplaceAll(want.OK.ModelPath, home, "$HOME") {
				t.Fatalf("model path go=%s ts=%s", sub(config.ModelPath), want.OK.ModelPath)
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
