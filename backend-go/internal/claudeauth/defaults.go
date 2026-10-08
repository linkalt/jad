// Package claudeauth signs in to a Claude account (Pro, Max, Team or Enterprise), refreshes its
// tokens and reports the state of the credential file that the Anthropic adapter reads
// (anthropicadapter upstream.credential_file).
//
// This package is the only writer of that file. The adapter never writes it, so there is no
// second writer to race with; two runs of this tool exclude each other with a lock file.
//
// The sign-in endpoints, the client id and the request shapes are not documented by Anthropic.
// They are the ones Claude Code uses, as found in the source of the pi coding agent
// (pi-ai auth/oauth/anthropic.js) and in the installed Claude Code binary. Anthropic's terms
// restrict subscription credentials to its own applications, so using them from this tool is
// the account owner's decision and risk. Every endpoint, id and scope is a config key, so a
// change on Anthropic's side needs no code change.
package claudeauth

// Defaults for every configurable key. The config schema documents the same values;
// config_test.go keeps the two in step.
const (
	DefaultAuthFile       = "~/.jad/auth/claude-oauth.json"
	DefaultAuthorizeURL   = "https://claude.ai/oauth/authorize"
	DefaultTokenURL       = "https://platform.claude.com/v1/oauth/token"
	DefaultClientID       = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	DefaultScope          = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	DefaultRedirectHost   = "localhost"
	DefaultLoginPort      = 53692
	DefaultCallbackPath   = "/callback"
	DefaultCopyCodeURI    = "https://platform.claude.com/oauth/code/callback"
	DefaultLoginTimeoutMS = 300_000
	DefaultHTTPTimeoutMS  = 30_000
	DefaultExpiryMarginMS = 300_000
	DefaultKeepBackup     = true
	DefaultOpenBrowser    = true
	MaxTimeoutMS          = 60 * 60 * 1000
	MaxTokenResponseBytes = 1 << 20
	// FileVersion is the layout version written to the credential file.
	FileVersion = 1
)

// defaultAuthorizeParams are sent on the authorize URL in addition to the standard OAuth
// parameters. "code=true" makes the page show a copyable code as well as redirect.
func defaultAuthorizeParams() map[string]string {
	return map[string]string{"code": "true"}
}

// reservedAuthorizeParams are set by the tool and cannot be overridden by authorize_params.
var reservedAuthorizeParams = []string{
	"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method",
}
