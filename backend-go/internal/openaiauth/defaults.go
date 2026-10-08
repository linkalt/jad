// Package openaiauth signs in to a ChatGPT account, refreshes its tokens and reports the
// state of the credential file that the backend reads (internal/openaioauth).
//
// This package is the only writer of that file. The backend never writes it, so there is no
// second writer to race with; two runs of this tool exclude each other with a lock file.
//
// The sign-in endpoints, the client id and the request shapes are not documented by OpenAI.
// They are the ones the Codex CLI uses, as found in the source of the `openai-oauth` library.
// Every one of them is a config key, so a change on OpenAI's side needs no code change.
package openaiauth

// Defaults for every configurable key. The config schema documents the same values;
// config_test.go keeps the two in step.
const (
	DefaultAuthFile       = "~/.jad/auth/openai-codex.json"
	DefaultIssuer         = "https://auth.openai.com"
	DefaultClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultScope          = "openid profile email offline_access"
	DefaultRedirectHost   = "localhost"
	DefaultLoginPort      = 1455
	DefaultLoginTimeoutMS = 300_000
	DefaultHTTPTimeoutMS  = 30_000
	DefaultExpiryMarginMS = 300_000
	DefaultKeepBackup     = true
	DefaultOpenBrowser    = true
	MaxTimeoutMS          = 60 * 60 * 1000
	MaxTokenResponseBytes = 1 << 20
	// AuthModeChatGPT is the auth_mode value the Codex CLI and the openai-oauth library write.
	AuthModeChatGPT = "chatgpt"
)

// defaultAuthorizeParams are sent on the authorize URL in addition to the standard OAuth
// parameters. They are the two the Codex CLI sends.
func defaultAuthorizeParams() map[string]string {
	return map[string]string{
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	}
}

// reservedAuthorizeParams are set by the tool and cannot be overridden by authorize_params.
var reservedAuthorizeParams = []string{
	"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method",
}
