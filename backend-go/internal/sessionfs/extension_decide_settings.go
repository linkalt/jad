package sessionfs

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const (
	defaultExtensionDecideBackend                = "openrouter"
	defaultExtensionDecideTimeoutMS        int64 = 30_000
	maxExtensionDecideTimeoutMS            int64 = 300_000
	defaultExtensionDecideMaxQuestions     int64 = 16
	maxExtensionDecideMaxQuestions         int64 = 128
	defaultExtensionDecideMaxInputBytes    int64 = 200_000
	defaultExtensionDecideMaxResponseBytes int64 = 1_048_576
	defaultExtensionDecideMaxImages        int64 = 8
	maxExtensionDecideMaxImages            int64 = 64
	defaultExtensionDecideMaxImageBytes    int64 = 5 * 1024 * 1024
)

var (
	extensionDecideKeys     = []string{"backend", "base_url", "api_key_env", "model", "timeout_ms", "max_questions", "max_input_bytes", "max_response_bytes", "max_images", "max_image_bytes", "paths"}
	extensionDecideBackends = []string{"openrouter", "openai"}
	extensionDecideEnvName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// extensionDecideBackendDefaults are the endpoint, key variable and model used when a
// setting leaves them out. They are defaults, not limits: every one can be set.
var extensionDecideBackendDefaults = map[string]struct{ BaseURL, APIKeyEnv, Model string }{
	"openrouter": {"https://openrouter.ai/api/alpha", "OPENROUTER_API_KEY", "openai/gpt-6-luna-decisions"},
	"openai":     {"https://api.openai.com/v1", "OPENAI_API_KEY", "gpt-6-luna"},
}

// ExtensionDecideSettings contains the normalized endpoint and limits for extension.decide.
// Every field is resolved: a key left out of setting.json holds its backend's default.
type ExtensionDecideSettings struct {
	// Backend is "openrouter" (its alpha Decisions endpoint) or "openai" (/v1/decisions).
	Backend string
	// BaseURL is the URL the tool appends "/decisions" to, without a trailing slash.
	BaseURL string
	// APIKeyEnv names the environment variable holding the key. It is read on each call.
	APIKeyEnv string
	Model     string
	TimeoutMS int64
	// MaxQuestions is the most questions one call may carry.
	MaxQuestions int64
	// MaxInputBytes is the most bytes of input text one call may carry.
	MaxInputBytes int64
	// MaxResponseBytes is the most response bytes read from the provider.
	MaxResponseBytes int64
	// MaxImages is the most image parts one call may carry.
	MaxImages int64
	// MaxImageBytes is the largest image, as a file or as decoded base64, read for a call.
	MaxImageBytes int64
	// Policy is decide's own path rules for an image given by path. Without paths it is
	// the empty policy, which reaches the workspace only.
	Policy NativePathPolicy
}

func normalizeExtensionDecideSettings(setting map[string]json.RawMessage, globalJadRoot, settingPath string) (*ExtensionDecideSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["extension"]
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.extension")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["decide"]
	if !exists {
		return nil, nil
	}
	const section = "tools.extension.decide"
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(extensionDecideKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, extensionDecideKeys, "")
		}
	}

	settings := &ExtensionDecideSettings{Policy: NativePathPolicy{Paths: []NativePathRule{}}}
	if rawPaths, exists := fields["paths"]; exists {
		if settings.Policy, err = normalizeNativePathPolicy(rawPaths, globalJadRoot, settingPath+": "+section+".paths"); err != nil {
			return nil, err
		}
	}
	if settings.Backend, err = extensionDecideString(fields, "backend", defaultExtensionDecideBackend, settingPath); err != nil {
		return nil, err
	}
	if !containsString(extensionDecideBackends, settings.Backend) {
		return nil, configError("%s: %s.backend must be one of %s", settingPath, section, strings.Join(extensionDecideBackends, ", "))
	}
	defaults := extensionDecideBackendDefaults[settings.Backend]
	if settings.BaseURL, err = extensionDecideString(fields, "base_url", defaults.BaseURL, settingPath); err != nil {
		return nil, err
	}
	settings.BaseURL = strings.TrimRight(settings.BaseURL, "/")
	if parsed, err := url.Parse(settings.BaseURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, configError("%s: %s.base_url must be an http or https URL", settingPath, section)
	}
	if settings.APIKeyEnv, err = extensionDecideString(fields, "api_key_env", defaults.APIKeyEnv, settingPath); err != nil {
		return nil, err
	}
	if !extensionDecideEnvName.MatchString(settings.APIKeyEnv) {
		return nil, configError("%s: %s.api_key_env must be an environment variable name", settingPath, section)
	}
	if settings.Model, err = extensionDecideString(fields, "model", defaults.Model, settingPath); err != nil {
		return nil, err
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultExtensionDecideTimeoutMS, 1, maxExtensionDecideTimeoutMS, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxQuestions, err = configuredInteger(fields, "max_questions", defaultExtensionDecideMaxQuestions, 1, maxExtensionDecideMaxQuestions, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxInputBytes, err = configuredInteger(fields, "max_input_bytes", defaultExtensionDecideMaxInputBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxResponseBytes, err = configuredInteger(fields, "max_response_bytes", defaultExtensionDecideMaxResponseBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxImages, err = configuredInteger(fields, "max_images", defaultExtensionDecideMaxImages, 1, maxExtensionDecideMaxImages, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxImageBytes, err = configuredInteger(fields, "max_image_bytes", defaultExtensionDecideMaxImageBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	return settings, nil
}

// extensionDecideString reads a non-empty string key, or the fallback when it is absent or null.
func extensionDecideString(fields map[string]json.RawMessage, name, fallback, settingPath string) (string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: tools.extension.decide.%s must be a non-empty string", settingPath, name)
	}
	return strings.TrimSpace(value), nil
}
