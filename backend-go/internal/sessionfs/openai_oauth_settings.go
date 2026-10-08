package sessionfs

import (
	"encoding/json"
	"net/url"
	"strings"
)

// Defaults for the `openai_oauth` setting node. The endpoint is not a documented public
// API and may change, which is why it is configuration and not a constant in the transport.
const (
	defaultOpenAIOAuthBaseURL        = "https://chatgpt.com/backend-api/codex"
	defaultOpenAIOAuthExpiryMarginMS = 300_000
	maxOpenAIOAuthExpiryMarginMS     = 86_400_000
)

var openAIOAuthKeys = []string{"base_url", "expiry_margin_ms"}

// OpenAIOAuthSettings configures requests sent in auth_mode openai_oauth. The backend only
// reads the credential file; it never refreshes or writes it (a separate CLI does that).
type OpenAIOAuthSettings struct {
	// BaseURL is the Codex endpoint the Responses path is appended to.
	BaseURL string
	// ExpiryMarginMS is how long before its expiry an access token counts as expired, so a
	// request is not sent with a token that dies in flight.
	ExpiryMarginMS int64
}

// normalizeOpenAIOAuthSettings reads the `openai_oauth` node. Only an absent key takes a
// default: a null or misspelt value must not quietly pick one.
func normalizeOpenAIOAuthSettings(raw json.RawMessage, settingPath string) (OpenAIOAuthSettings, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		decoded, err := decodeConfigObject(raw, settingPath+": openai_oauth")
		if err != nil {
			return OpenAIOAuthSettings{}, configError("%s: openai_oauth must be an object", settingPath)
		}
		fields = decoded
	}
	for key := range fields {
		if !containsString(openAIOAuthKeys, key) {
			return OpenAIOAuthSettings{}, unsupportedToolKey(settingPath+": openai_oauth", key, openAIOAuthKeys, "")
		}
	}
	settings := OpenAIOAuthSettings{
		BaseURL:        defaultOpenAIOAuthBaseURL,
		ExpiryMarginMS: defaultOpenAIOAuthExpiryMarginMS,
	}
	if value, ok := fields["base_url"]; ok {
		var configured string
		if err := json.Unmarshal(value, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return OpenAIOAuthSettings{}, configError("%s: openai_oauth.base_url must be a non-empty string", settingPath)
		}
		configured = strings.TrimRight(strings.TrimSpace(configured), "/")
		parsed, err := url.Parse(configured)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return OpenAIOAuthSettings{}, configError("%s: openai_oauth.base_url must use http or https", settingPath)
		}
		settings.BaseURL = configured
	}
	if value, ok := fields["expiry_margin_ms"]; ok {
		configured, valid := configSafeInteger(value)
		if !valid || configured < 0 || configured > maxOpenAIOAuthExpiryMarginMS {
			return OpenAIOAuthSettings{}, configError("%s: openai_oauth.expiry_margin_ms must be a safe integer between 0 and %d", settingPath, maxOpenAIOAuthExpiryMarginMS)
		}
		settings.ExpiryMarginMS = configured
	}
	return settings, nil
}
