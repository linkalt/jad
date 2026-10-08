package sessionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oauthSession writes a session whose model.json and setting.json are given and loads its
// model, the way a run does.
func loadOAuthSession(t *testing.T, modelJSON, settingExtra string) (SessionConfig, error) {
	t.Helper()
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := `{"cwd":"` + rootDir + `"` + settingExtra + `}`
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(modelJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		return SessionConfig{}, err
	}
	return config.WithModel()
}

func TestOpenAIOAuthModelEndpointDefaultsToCodexAndTakesBaseURLFromModelJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config, err := loadOAuthSession(t, `{"model":"m","auth_mode":"openai_oauth"}`, "")
	if err != nil || config.Model.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("default endpoint = %q, error %v", config.Model.BaseURL, err)
	}
	config, err = loadOAuthSession(t, `{"model":"m","auth_mode":"openai_oauth","base_url":"http://127.0.0.1:9/proxy/"}`, "")
	if err != nil || config.Model.BaseURL != "http://127.0.0.1:9/proxy" {
		t.Fatalf("configured endpoint = %q, error %v; want the trailing slash trimmed", config.Model.BaseURL, err)
	}
	config, err = loadOAuthSession(t, `{"model":"m"}`, "")
	if err != nil || config.Model.BaseURL != "" {
		t.Fatalf("api_key mode endpoint = %q, error %v; the Codex default must not leak into it", config.Model.BaseURL, err)
	}
}

func TestOpenAIOAuthModelUsesTheDefaultCredentialFileInTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config, err := loadOAuthSession(t, `{"model":"gpt-5.4","auth_mode":"openai_oauth"}`, "")
	if err != nil {
		t.Fatalf("load error = %v", err)
	}
	if config.Model.AuthMode != "openai_oauth" {
		t.Errorf("AuthMode = %q", config.Model.AuthMode)
	}
	if want := filepath.Join(home, ".jad", "auth", "openai-codex.json"); config.Model.OAuthAuthFile != want {
		t.Errorf("OAuthAuthFile = %q, want %q", config.Model.OAuthAuthFile, want)
	}
}

func TestOpenAIOAuthModelExpandsAndCleansTheConfiguredCredentialFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config, err := loadOAuthSession(t, `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":"~/auth/../auth/x.json"}`, "")
	if err != nil {
		t.Fatalf("load error = %v", err)
	}
	if want := filepath.Join(home, "auth", "x.json"); config.Model.OAuthAuthFile != want {
		t.Errorf("OAuthAuthFile = %q, want %q", config.Model.OAuthAuthFile, want)
	}
	absolute := filepath.Join(t.TempDir(), "codex.json")
	config, err = loadOAuthSession(t, `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":"`+absolute+`"}`, "")
	if err != nil || config.Model.OAuthAuthFile != absolute {
		t.Errorf("absolute file = %q, error %v; want %q", config.Model.OAuthAuthFile, err, absolute)
	}
}

func TestOpenAIOAuthModelRejectsBadCombinationsAndValues(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"base_url not http in oauth mode", `{"model":"m","auth_mode":"openai_oauth","base_url":"ftp://x"}`, "base_url must use http or https"},
		{"empty base_url in oauth mode", `{"model":"m","auth_mode":"openai_oauth","base_url":""}`, "base_url must be a non-empty string"},
		{"relative credential file", `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":"auth/x.json"}`, "oauth_auth_file must be absolute or start with ~"},
		{"empty credential file", `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":"  "}`, "oauth_auth_file must be a non-empty string"},
		{"numeric credential file", `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":3}`, "oauth_auth_file must be a non-empty string"},
		{"null credential file", `{"model":"m","auth_mode":"openai_oauth","oauth_auth_file":null}`, "oauth_auth_file must be a non-empty string"},
		{"credential file in api_key mode", `{"model":"m","oauth_auth_file":"~/x.json"}`, "oauth_auth_file requires auth_mode openai_oauth"},
		{"unknown auth mode", `{"model":"m","auth_mode":"chatgpt"}`, "auth_mode must be api_key or openai_oauth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadOAuthSession(t, c.model, "")
			var configErr *ConfigError
			if !errors.As(err, &configErr) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %T %v, want ConfigError containing %q", err, err, c.want)
			}
		})
	}
}

func TestOpenAIOAuthModeIgnoresTheAPIKeyEnvironmentName(t *testing.T) {
	config, err := loadOAuthSession(t, `{"model":"m","auth_mode":"openai_oauth","api_key_env":"SOME_KEY"}`, "")
	if err != nil {
		t.Fatalf("load error = %v", err)
	}
	if config.Model.APIKeyEnv != "SOME_KEY" {
		t.Errorf("APIKeyEnv = %q; the name is kept so bash/exec/process still hide that variable", config.Model.APIKeyEnv)
	}
}

func TestOpenAIOAuthSettingsDefaultsAndEveryKey(t *testing.T) {
	config, err := loadOAuthSession(t, `{"model":"m"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	want := OpenAIOAuthSettings{ExpiryMarginMS: 300000}
	if config.OpenAIOAuth != want {
		t.Errorf("defaults = %+v, want %+v", config.OpenAIOAuth, want)
	}
	config, err = loadOAuthSession(t, `{"model":"m"}`, `,"openai_oauth":{"expiry_margin_ms":0}`)
	if err != nil {
		t.Fatal(err)
	}
	want = OpenAIOAuthSettings{ExpiryMarginMS: 0}
	if config.OpenAIOAuth != want {
		t.Errorf("configured = %+v, want %+v", config.OpenAIOAuth, want)
	}
}

func TestOpenAIOAuthSettingsRejectBadValuesNullAndUnknownKeys(t *testing.T) {
	cases := []struct{ name, node, want string }{
		{"not an object", `[]`, "openai_oauth must be an object"},
		{"null node", `null`, "openai_oauth must be an object"},
		{"unknown key", `{"expiry_margin_m":1}`, "openai_oauth.expiry_margin_m is not supported; did you mean expiry_margin_ms?"},
		{"removed key", `{"client_version":"1.0.0"}`, "openai_oauth.client_version is not supported"},
		{"endpoint moved to model.json", `{"base_url":"https://x.example"}`, "openai_oauth.base_url is not supported"},
		{"margin negative", `{"expiry_margin_ms":-1}`, "openai_oauth.expiry_margin_ms must be a safe integer between 0 and 86400000"},
		{"margin too large", `{"expiry_margin_ms":86400001}`, "openai_oauth.expiry_margin_ms must be a safe integer between 0 and 86400000"},
		{"margin fraction", `{"expiry_margin_ms":1.5}`, "openai_oauth.expiry_margin_ms must be a safe integer"},
		{"margin null", `{"expiry_margin_ms":null}`, "openai_oauth.expiry_margin_ms must be a safe integer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadOAuthSession(t, `{"model":"m"}`, `,"openai_oauth":`+c.node)
			var configErr *ConfigError
			if !errors.As(err, &configErr) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %T %v, want ConfigError containing %q", err, err, c.want)
			}
		})
	}
}
