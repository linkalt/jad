package sessionfs

import "encoding/json"

// defaultOpenAIOAuthBaseURL is the endpoint of a model file in auth_mode openai_oauth that
// sets no base_url. It is not a documented public API and may change, which is why model.json
// can override it with base_url (for example to reach it through a proxy).
const defaultOpenAIOAuthBaseURL = "https://chatgpt.com/backend-api/codex"

// Defaults for the `openai_oauth` setting node.
const (
	defaultOpenAIOAuthExpiryMarginMS = 300_000
	maxOpenAIOAuthExpiryMarginMS     = 86_400_000
)

var openAIOAuthKeys = []string{"expiry_margin_ms"}

// OpenAIOAuthSettings configures requests sent in auth_mode openai_oauth. The backend only
// reads the credential file; it never refreshes or writes it (a separate CLI does that).
type OpenAIOAuthSettings struct {
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
		ExpiryMarginMS: defaultOpenAIOAuthExpiryMarginMS,
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
