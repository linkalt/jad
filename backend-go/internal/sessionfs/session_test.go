package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigSelectsProfileModelAndStorage(t *testing.T) {
	cwd := t.TempDir()
	for name, text := range map[string]string{"prompt.md": "system instructions", "profile-green.md": "green instructions", "profile-blue.md": "blue instructions"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := map[string]any{"schema_version": 1, "provider": "openai", "model": "gpt-test", "api_key_env": "TEST_OPENAI_KEY", "base_url": "https://api.example.test/v1/", "request": map[string]any{"temperature": 0.2}}
	location, fixture := layoutLocation(t, layouttest.Spec{
		CWD:    cwd,
		Models: map[string]map[string]any{"openai/model": model, "openai/alternate": model},
		Profiles: map[string]map[string]any{
			"dev/green": {"models": []any{"openai/model", "openai/alternate"}, "prompt_role": "developer", "prompt_documents": []any{"prompt.md", "profile-green.md"}},
			"dev/blue":  {"models": []any{"openai/model", "openai/alternate"}, "model_default": "openai/alternate", "prompt_role": "developer", "prompt_documents": []any{"prompt.md", "profile-blue.md"}},
		},
		ActiveProfile: "dev/green",
		Settings:      map[string]any{"persistence": map[string]any{"durability": "process", "max_item_bytes": 4096, "response_metadata": "full"}},
	})
	modelsDir := filepath.Join(fixture.Root, "models", "openai")
	stateDir := filepath.Join(fixture.StateDir, "session-1")

	active, err := LoadSessionConfig(location, "", "openai/alternate")
	if err != nil {
		t.Fatalf("LoadSessionConfig(active) error = %v", err)
	}
	if active.Profile != "dev/green" || active.DefaultProfile != "dev/green" || active.MessagesPath != filepath.Join(stateDir, "dev", "green", "messages.jsonl") || active.StagingDir != filepath.Join(stateDir, "dev", "green", "staging") {
		t.Errorf("active profile config = %+v", active)
	}
	if active.WorkingDir != cwd || !active.SessionFiles.Enabled || active.StoragePaths.HTTPDebug != filepath.Join(stateDir, "dev", "green", "debug", "http") {
		t.Errorf("active file/storage settings = %+v", active)
	}
	if active.SessionName != "session-1" || active.SettingPath != fixture.SessionFile {
		t.Errorf("session metadata = name %q, setting %q", active.SessionName, active.SettingPath)
	}
	if len(active.Profiles) != 2 || active.Profiles[0] != "dev/blue" || active.Profiles[1] != "dev/green" {
		t.Errorf("path profiles = %#v", active.Profiles)
	}
	if active.ModelDefault != "openai/alternate" || len(active.Models) != 2 {
		t.Errorf("model selection metadata = default %q, models %#v", active.ModelDefault, active.Models)
	}
	if active.ModelPath != filepath.Join(modelsDir, "alternate.json") || active.Model.Name != "gpt-test" {
		t.Errorf("active model config = %+v", active)
	}
	if active.Model.BaseURL != "https://api.example.test/v1" || active.Model.APIKeyEnv != "TEST_OPENAI_KEY" {
		t.Errorf("provider config = %+v", active.Model)
	}
	if active.Model.PromptRole != "developer" || active.Model.PromptText != "system instructions\n\ngreen instructions" {
		t.Errorf("prompt config = role %q, text %q", active.Model.PromptRole, active.Model.PromptText)
	}
	if active.Persistence.Durability != "process" || active.Persistence.MaxItemBytes != 4096 || active.Persistence.ResponseMetadata != "full" {
		t.Errorf("persistence config = %+v", active.Persistence)
	}

	other, err := LoadSessionConfig(location, "dev/blue", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig(blue) error = %v", err)
	}
	if other.Profile != "dev/blue" || other.ModelPath != filepath.Join(modelsDir, "alternate.json") {
		t.Errorf("non-default profile = %+v", other)
	}
	if other.Model.PromptText != "system instructions\n\nblue instructions" {
		t.Errorf("profile-specific prompt text = %q", other.Model.PromptText)
	}
	if other.MessagesPath != filepath.Join(stateDir, "dev", "blue", "messages.jsonl") || other.StagingDir != filepath.Join(stateDir, "dev", "blue", "staging") {
		t.Errorf("non-default storage = messages %q, staging %q", other.MessagesPath, other.StagingDir)
	}
}

func TestLoadSessionConfigRejectsUnknownProfileModelAndOAuth(t *testing.T) {
	location, _ := layoutLocation(t, layouttest.Spec{
		Models:   map[string]map[string]any{"openai/model": {"model": "gpt-test", "api_key_env": "TEST_OPENAI_KEY"}, "openai/other": {"model": "gpt-other"}},
		Profiles: map[string]map[string]any{"dev/one": {"models": []any{"openai/model"}, "model_default": "openai/model"}},
	})
	if _, err := LoadSessionConfig(location, "dev/missing", ""); err == nil {
		t.Fatal("LoadSessionConfig(unknown profile) succeeded")
	} else {
		var configErr *ConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("unknown profile error = %T %v, want ConfigError", err, err)
		}
	}
	if _, err := LoadSessionConfig(location, "dev/one", "openai/other"); err == nil {
		t.Fatal("LoadSessionConfig(unavailable model path) succeeded")
	} else {
		var clientErr *ClientError
		if !errors.As(err, &clientErr) || clientErr.Message == "" {
			t.Fatalf("unavailable model error = %T %v, want ClientError", err, err)
		}
	}

	if _, err := loadLayoutConfig(t, layouttest.Spec{ModelRaw: `{"model":"gpt-test","oauth_auth_file":null}`}); err == nil {
		t.Fatal("LoadSessionConfig(API key with oauth_auth_file) succeeded")
	} else {
		var configErr *ConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("API key oauth_auth_file error = %T %v, want ConfigError", err, err)
		}
	}
}

func TestLoadSessionConfigRejectsNullPathsPersistenceAndCompactionObjects(t *testing.T) {
	for _, field := range []string{"paths", "persistence", "compaction"} {
		t.Run(field, func(t *testing.T) {
			_, err := loadLayoutConfig(t, layouttest.Spec{RawSettings: map[string]json.RawMessage{field: json.RawMessage("null")}})
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("LoadSessionConfig(%s:null) error = %T %v, want ConfigError", field, err, err)
			}
		})
	}
}

func TestLoadSessionConfigDefaultsWithoutProviderCredentials(t *testing.T) {
	location, fixture := layoutLocation(t, layouttest.Spec{
		ModelRaw: `{"model":"gpt-test","provider":null,"auth_mode":null,"api_key_env":null}`,
		RawSettings: map[string]json.RawMessage{
			"persistence": json.RawMessage(`{"durability":null,"max_item_bytes":null,"response_metadata":null}`),
		},
	})
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig(defaults) error = %v", err)
	}
	profileState := filepath.Join(fixture.StateDir, "session-1", "dev", "coder")
	if config.MessagesPath != filepath.Join(profileState, "messages.jsonl") || config.StagingDir != filepath.Join(profileState, "staging") {
		t.Errorf("default paths = messages %q, staging %q", config.MessagesPath, config.StagingDir)
	}
	if config.Persistence.Durability != "turn" || config.Persistence.MaxItemBytes != defaultMaxItemBytes || config.Persistence.ResponseMetadata != "minimal" || config.Persistence.CursorCacheCheckpointBytes != defaultCursorCacheCheckpointBytes {
		t.Errorf("default persistence = %+v", config.Persistence)
	}
}

func TestLoadSessionConfigRunErrorCacheMaxEntriesBounds(t *testing.T) {
	cases := []struct {
		setting string
		want    int
		wantErr bool
	}{
		{setting: `{}`, want: defaultRunErrorCacheMaxEntries},
		{setting: `{"persistence":{"run_error_cache_max_entries":null}}`, want: defaultRunErrorCacheMaxEntries},
		{setting: `{"persistence":{"run_error_cache_max_entries":1}}`, want: 1},
		{setting: `{"persistence":{"run_error_cache_max_entries":10000}}`, want: 10000},
		{setting: `{"persistence":{"run_error_cache_max_entries":0}}`, wantErr: true},
		{setting: `{"persistence":{"run_error_cache_max_entries":10001}}`, wantErr: true},
		{setting: `{"persistence":{"run_error_cache_max_entries":1.5}}`, wantErr: true},
		{setting: `{"persistence":{"run_error_cache_max_entries":"50"}}`, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.setting, func(t *testing.T) {
			var settings map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.setting), &settings); err != nil {
				t.Fatal(err)
			}
			location, fixture := layoutLocation(t, layouttest.Spec{RawSettings: settings})
			config, err := LoadSessionConfig(location, "", "")
			if test.wantErr {
				if err == nil {
					t.Fatal("LoadSessionConfig unexpectedly accepted run_error_cache_max_entries")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadSessionConfig: %v", err)
			}
			if config.Persistence.RunErrorCacheMaxEntries != test.want {
				t.Fatalf("run error cache max entries = %d, want %d", config.Persistence.RunErrorCacheMaxEntries, test.want)
			}
			if want := filepath.Join(fixture.StateDir, "session-1", "dev", "coder", "staging", "run-error-cache.json"); config.StoragePaths.RunErrorCache != want {
				t.Fatalf("run error cache path = %q, want %q", config.StoragePaths.RunErrorCache, want)
			}
		})
	}
}

func TestLoadSessionConfigCursorCacheCheckpointBounds(t *testing.T) {
	cases := []struct {
		value   string
		want    int64
		wantErr bool
	}{
		{value: "1", want: 1},
		{value: "1e3", want: 1000},
		{value: "null", want: defaultCursorCacheCheckpointBytes},
		{value: "0", wantErr: true},
		{value: "67108865", wantErr: true},
		{value: "1.5", wantErr: true},
		{value: `"1024"`, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.value, func(t *testing.T) {
			config, err := loadLayoutConfig(t, layouttest.Spec{RawSettings: map[string]json.RawMessage{
				"persistence": json.RawMessage(`{"cursor_cache_checkpoint_bytes":` + test.value + `}`),
			}})
			if test.wantErr {
				if err == nil {
					t.Fatal("LoadSessionConfig unexpectedly accepted cursor_cache_checkpoint_bytes")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadSessionConfig: %v", err)
			}
			if config.Persistence.CursorCacheCheckpointBytes != test.want {
				t.Fatalf("cursor cache checkpoint bytes = %d, want %d", config.Persistence.CursorCacheCheckpointBytes, test.want)
			}
		})
	}
}

func hostedToolsModel(t *testing.T, extra string) (Model, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.json")
	body := `{"schema_version":1,"provider":"openai","model":"gpt-test"` + extra + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadModel(path, "", "")
}

func TestLoadModelReadsHostedTools(t *testing.T) {
	model, err := hostedToolsModel(t, `,"hosted_tools":[ {"type":"web_search","search_context_size":"low"}, {"type":"file_search"} ]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.HostedTools) != 2 {
		t.Fatalf("HostedTools = %d entries, want 2", len(model.HostedTools))
	}
	if got := string(model.HostedTools[0]); got != `{"type":"web_search","search_context_size":"low"}` {
		t.Errorf("first hosted tool = %s, want the object unchanged and trimmed", got)
	}
	if string(model.Request) != `{}` {
		t.Errorf("hosted_tools must not leak into request: %s", model.Request)
	}
}

func TestLoadModelHostedToolsDefaultsToNone(t *testing.T) {
	for name, extra := range map[string]string{
		"absent": ``,
		"null":   `,"hosted_tools":null`,
		"empty":  `,"hosted_tools":[]`,
	} {
		model, err := hostedToolsModel(t, extra)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if model.HostedTools != nil {
			t.Errorf("%s: HostedTools = %v, want nil", name, model.HostedTools)
		}
	}
}

func TestLoadModelRejectsBadHostedTools(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"not an array":     {`,"hosted_tools":{"type":"web_search"}`, "hosted_tools must be an array"},
		"string entry":     {`,"hosted_tools":["web_search"]`, "hosted_tools[0] must be an object"},
		"null entry":       {`,"hosted_tools":[{"type":"web_search"},null]`, "hosted_tools[1] must be an object"},
		"missing type":     {`,"hosted_tools":[{"name":"x"}]`, "hosted_tools[0].type must be a non-empty string"},
		"empty type":       {`,"hosted_tools":[{"type":""}]`, "hosted_tools[0].type must be a non-empty string"},
		"blank type":       {`,"hosted_tools":[{"type":"   "}]`, "hosted_tools[0].type must be a non-empty string"},
		"type not string":  {`,"hosted_tools":[{"type":7}]`, "hosted_tools[0].type must be a non-empty string"},
		"function refused": {`,"hosted_tools":[{"type":"function","name":"x"}]`, "must not be function"},
		"function padded":  {`,"hosted_tools":[{"type":" function "}]`, "must not be function"},
		"function cased":   {`,"hosted_tools":[{"type":"FUNCTION"}]`, "must not be function"},
	}
	for name, c := range cases {
		_, err := hostedToolsModel(t, c.extra)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", name, err, c.want)
		}
	}
}

func TestLoadModelStillRefusesToolsInRequest(t *testing.T) {
	// hosted_tools is the way to add provider tools; request.tools stays managed by the runner.
	_, err := hostedToolsModel(t, `,"request":{"tools":[{"type":"web_search"}]}`)
	if err == nil || !strings.Contains(err.Error(), "request.tools is managed by the session runner") {
		t.Fatalf("err = %v, want the request.tools refusal", err)
	}
}
