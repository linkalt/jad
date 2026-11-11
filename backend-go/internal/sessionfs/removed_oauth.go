package sessionfs

import (
	"bytes"
	"encoding/json"
)

// The ChatGPT subscription sign-in (auth_mode openai_oauth) was removed, with the
// openai-auth command and every key that only served it. Its keys are refused by name,
// never ignored: a model file that named a credential file and is quietly read as an API
// key session would fail later with a missing OPENAI_API_KEY, or worse, spend a different
// credential than the one the file promised.
const removedOpenAIOAuthHint = "signing in with a ChatGPT subscription is no longer supported; use an OpenAI API key (api_key_env in the model file, default OPENAI_API_KEY) and delete the key"

// rejectRemovedOAuthFields refuses the removed model.json keys. oauth_auth_file is refused
// whatever auth_mode says. auth_mode itself stays accepted with its one remaining value,
// api_key, which every model already behaves as (null means absent, as for every field);
// any other value, including the removed openai_oauth, is refused.
func rejectRemovedOAuthFields(fields map[string]json.RawMessage, path string) error {
	if _, exists := fields["oauth_auth_file"]; exists {
		return configError("%s: oauth_auth_file was removed: %s", path, removedOpenAIOAuthHint)
	}
	if raw, exists := fields["auth_mode"]; exists {
		var mode string
		if string(bytes.TrimSpace(raw)) != "null" && (json.Unmarshal(raw, &mode) != nil || mode != "api_key") {
			return configError("%s: auth_mode must be api_key (openai_oauth was removed): %s", path, removedOpenAIOAuthHint)
		}
	}
	return nil
}

// rejectRemovedOpenAIOAuthSetting refuses the removed openai_oauth setting. Its only key,
// expiry_margin_ms, tuned the credential-file expiry check of the removed sign-in.
func rejectRemovedOpenAIOAuthSetting(settings map[string]json.RawMessage, where string) error {
	if _, exists := settings["openai_oauth"]; exists {
		return configError("%s: settings.openai_oauth was removed: %s", where, removedOpenAIOAuthHint)
	}
	return nil
}
