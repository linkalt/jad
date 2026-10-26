package sessionfs

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const (
	defaultExtensionDecideSystemoneBaseURL                = "https://openrouter.ai/api/alpha"
	defaultExtensionDecideSystemonePath                   = "/decisions"
	defaultExtensionDecideSystemoneAPIKeyEnv              = "OPENROUTER_API_KEY"
	defaultExtensionDecideSystemoneModel                  = "openai/gpt-6-luna-decisions"
	defaultExtensionDecideSystemoneTimeoutMS        int64 = 30_000
	maxExtensionDecideSystemoneTimeoutMS            int64 = 300_000
	defaultExtensionDecideSystemoneMaxQuestions     int64 = 16
	maxExtensionDecideSystemoneMaxQuestions         int64 = 128
	defaultExtensionDecideSystemoneMaxInputBytes    int64 = 200_000
	defaultExtensionDecideSystemoneMaxResponseBytes int64 = 1_048_576
)

var (
	extensionDecideSystemoneKeys    = []string{"base_url", "path", "api_key_env", "model", "timeout_ms", "max_questions", "max_input_bytes", "max_response_bytes"}
	extensionDecideSystemoneEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ExtensionDecideSystemoneSettings contains the normalized endpoint and limits for extension.decide_systemone.
// Every field is resolved: a key left out of setting.json holds its default.
type ExtensionDecideSystemoneSettings struct {
	// BaseURL is the URL the tool appends Path to, without a trailing slash.
	BaseURL string
	// Path is appended to BaseURL: "/decisions" for OpenRouter, "/v1/systemone" for the TypeSafe API.
	Path string
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
}

func normalizeExtensionDecideSystemoneSettings(setting map[string]json.RawMessage, settingPath string) (*ExtensionDecideSystemoneSettings, error) {
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
	raw, exists := nativeFields["decide_systemone"]
	if !exists {
		return nil, nil
	}
	const section = "tools.extension.decide_systemone"
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(extensionDecideSystemoneKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, extensionDecideSystemoneKeys, "")
		}
	}

	settings := &ExtensionDecideSystemoneSettings{}
	if settings.BaseURL, err = extensionDecideSystemoneString(fields, "base_url", defaultExtensionDecideSystemoneBaseURL, settingPath); err != nil {
		return nil, err
	}
	settings.BaseURL = strings.TrimRight(settings.BaseURL, "/")
	if parsed, err := url.Parse(settings.BaseURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, configError("%s: %s.base_url must be an http or https URL", settingPath, section)
	}
	if settings.Path, err = extensionDecideSystemoneString(fields, "path", defaultExtensionDecideSystemonePath, settingPath); err != nil {
		return nil, err
	}
	if parsed, err := url.Parse(settings.Path); err != nil || !strings.HasPrefix(settings.Path, "/") || strings.HasPrefix(settings.Path, "//") || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, configError("%s: %s.path must start with a single / and be a path only", settingPath, section)
	}
	if settings.APIKeyEnv, err = extensionDecideSystemoneString(fields, "api_key_env", defaultExtensionDecideSystemoneAPIKeyEnv, settingPath); err != nil {
		return nil, err
	}
	if !extensionDecideSystemoneEnvName.MatchString(settings.APIKeyEnv) {
		return nil, configError("%s: %s.api_key_env must be an environment variable name", settingPath, section)
	}
	if settings.Model, err = extensionDecideSystemoneString(fields, "model", defaultExtensionDecideSystemoneModel, settingPath); err != nil {
		return nil, err
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultExtensionDecideSystemoneTimeoutMS, 1, maxExtensionDecideSystemoneTimeoutMS, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxQuestions, err = configuredInteger(fields, "max_questions", defaultExtensionDecideSystemoneMaxQuestions, 1, maxExtensionDecideSystemoneMaxQuestions, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxInputBytes, err = configuredInteger(fields, "max_input_bytes", defaultExtensionDecideSystemoneMaxInputBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxResponseBytes, err = configuredInteger(fields, "max_response_bytes", defaultExtensionDecideSystemoneMaxResponseBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	return settings, nil
}

// extensionDecideSystemoneString reads a non-empty string key, or the fallback when it is absent or null.
func extensionDecideSystemoneString(fields map[string]json.RawMessage, name, fallback, settingPath string) (string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: tools.extension.decide_systemone.%s must be a non-empty string", settingPath, name)
	}
	return strings.TrimSpace(value), nil
}
