package sessionfs

import (
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

// The ChatGPT subscription sign-in was removed. Each of its keys must fail the load with
// a message that says it is unsupported and what to use instead, not fall back to an API
// key session or be reported as an unknown key.
func TestRemovedOpenAIOAuthModelKeysAreRefusedByName(t *testing.T) {
	cases := []struct{ name, model, want string }{
		{"auth_mode openai_oauth", `{"model":"m","auth_mode":"openai_oauth"}`, "auth_mode must be api_key (openai_oauth was removed)"},
		{"auth_mode openai_oauth with a credential file", `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":"~/x.json"}`, "oauth_auth_file was removed"},
		{"auth_mode openai_oauth with a proxy", `{"model":"m","auth_mode":"openai_oauth","base_url":"http://127.0.0.1:9/proxy"}`, "auth_mode must be api_key (openai_oauth was removed)"},
		{"oauth_auth_file alone", `{"model":"m","oauth_auth_file":"~/x.json"}`, "oauth_auth_file was removed"},
		{"oauth_auth_file null", `{"model":"m","oauth_auth_file":null}`, "oauth_auth_file was removed"},
		{"oauth_auth_file with api_key", `{"model":"m","auth_mode":"api_key","oauth_auth_file":"/x.json"}`, "oauth_auth_file was removed"},
		{"auth_mode other value", `{"model":"m","auth_mode":"chatgpt"}`, "auth_mode must be api_key"},
		{"auth_mode number", `{"model":"m","auth_mode":3}`, "auth_mode must be api_key"},
		{"auth_mode openai_oauth for a translated api", `{"model":"m","api":"chat_completions","auth_mode":"openai_oauth"}`, "auth_mode must be api_key (openai_oauth was removed)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadLayoutConfig(t, layouttest.Spec{ModelRaw: c.model})
			var configErr *ConfigError
			if !errors.As(err, &configErr) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %T %v, want ConfigError containing %q", err, err, c.want)
			}
			if !strings.Contains(err.Error(), "use an OpenAI API key") || !strings.Contains(err.Error(), "no longer supported") {
				t.Fatalf("error does not say it is unsupported and to use an API key: %v", err)
			}
		})
	}
}

// api_key is the one auth_mode value left; it is what every model already is.
func TestAuthModeAPIKeyIsStillAccepted(t *testing.T) {
	for _, mode := range []string{`"api_key"`, `null`} {
		config, err := loadLayoutConfig(t, layouttest.Spec{ModelRaw: `{"model":"m","auth_mode":` + mode + `,"api_key_env":"SOME_KEY"}`})
		if err != nil {
			t.Fatalf("auth_mode %s: %v", mode, err)
		}
		if config.Model.APIKeyEnv != "SOME_KEY" || config.Model.BaseURL != "" {
			t.Fatalf("auth_mode %s: model = %+v", mode, config.Model)
		}
	}
}

func TestRemovedOpenAIOAuthSettingIsRefusedWhereverASettingCanBeGiven(t *testing.T) {
	node := map[string]any{"expiry_margin_ms": 1}
	cases := map[string]layouttest.Spec{
		"session settings": {ModelRaw: `{"model":"m"}`, Settings: map[string]any{"openai_oauth": node}},
		"root defaults":    {ModelRaw: `{"model":"m"}`, Defaults: map[string]any{"openai_oauth": node}},
		"profile settings": {ModelRaw: `{"model":"m"}`, ProfileExtra: map[string]any{"settings": map[string]any{"openai_oauth": node}}},
		"empty node":       {ModelRaw: `{"model":"m"}`, Settings: map[string]any{"openai_oauth": map[string]any{}}},
		"null node":        {ModelRaw: `{"model":"m"}`, Settings: map[string]any{"openai_oauth": nil}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			err := loadLayoutError(t, spec)
			var configErr *ConfigError
			if !errors.As(err, &configErr) || !strings.Contains(err.Error(), "settings.openai_oauth was removed") ||
				!strings.Contains(err.Error(), "no longer supported") || !strings.Contains(err.Error(), "use an OpenAI API key") {
				t.Fatalf("error = %T %v", err, err)
			}
			if strings.Contains(err.Error(), "may not carry") {
				t.Fatalf("reported as an unknown key instead of a removed one: %v", err)
			}
		})
	}
}

// loadLayoutError returns the error of loading the spec's session, whether the root setting
// load (the defaults) or the session load (settings, profile, model) refuses it.
func loadLayoutError(t *testing.T, spec layouttest.Spec) error {
	t.Helper()
	fixture := layouttest.Write(t, spec)
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		return err
	}
	location, err := Resolve(config.Roots, fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadSessionConfig(location, "", "")
	return err
}
