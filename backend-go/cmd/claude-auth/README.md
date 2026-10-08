# claude-auth

Signs in to a Claude account (Pro, Max, Team or Enterprise), refreshes its tokens and reports the state of the credential file that `anthropic-adapter` reads when `upstream.credential_file` is set. The adapter never writes that file; this command is its only writer.

```sh
go build -o /tmp/claude-auth ./cmd/claude-auth
/tmp/claude-auth login            # first sign-in (browser on this machine)
/tmp/claude-auth login --copy-code  # browser on another machine: paste the code the page shows
/tmp/claude-auth status           # what is stored; never prints a token
/tmp/claude-auth refresh          # new tokens from the stored refresh token
```

Add `--config <path>` to any command to use a config file (see below). Without it the defaults apply, and the credential file is `~/.jad/auth/claude-oauth.json`. Put the same path in the adapter's `upstream.credential_file`.

## Read this first

- **Anthropic does not document this sign-in.** The endpoints, the client id and the scopes are the ones Claude Code uses. They were taken from the source of the pi coding agent (`pi-ai`, `auth/oauth/anthropic.js`) and checked against strings in the installed Claude Code binary. Each is a config key.
- **Anthropic's terms limit subscription credentials.** The Claude Code legal page (code.claude.com/docs/en/legal-and-compliance, read 2026-10-08) says they are for ordinary use of Claude Code and other native Anthropic applications, that developers may not "collect, store, or intermediate Claude.ai credentials or session tokens", and that Anthropic may enforce this without notice. This command stores such tokens. Whether using your own account in your own tool is allowed is not settled by that page. The decision and the risk to the account are the account owner's.
- **Nothing has been sent to Anthropic.** All tests use a fake issuer. Whether the token endpoint, the client id and the JSON bodies still work, and whether the Messages API accepts the resulting token from this adapter, is unverified.

## Commands

| Command | What it does | Exit status |
| --- | --- | --- |
| `login` | Starts a loopback server on `localhost:53692`, prints the authorization URL, opens a browser (unless `--no-browser` or `open_browser: false`), waits for the redirect, exchanges the code (PKCE, S256) and stores the tokens. Refuses when a credential file exists unless `--force`. Gives up after `login_timeout_ms`. | 0 signed in, 1 failed |
| `login --copy-code` | Does not listen. The redirect URI is `copy_code_uri` (a page that shows a code). Paste the code (`code#state`, a redirect URL or a bare code) into the terminal. The state is checked when the paste carries one. | 0 signed in, 1 failed |
| `refresh` | Sends the stored refresh token to the issuer and rewrites the file. `--if-older-than 55m` does nothing when the tokens were refreshed more recently. `--if-expires-within 30m` does nothing unless the access token ends within that time. | 0 refreshed or not due, 1 failed, 3 sign in again |
| `status` | Prints path, file mode, scope, when the access token expires (the expiry stored at the last refresh), whether a refresh token is present and the age of the last refresh. `--json` prints the same as JSON. | 0 usable by expiry, 1 file missing or unusable, 2 access token expired |

Exit status 3 from `refresh` means the issuer refused the stored refresh token (a 4xx other than 408 and 429) or the file has none. Repeating the command cannot help; run `login --force`. A 5xx, a 429 or a network error exits 1: try again later.

`login` writes its progress, including the URL, to stderr and the final line to stdout. Nothing the command prints contains a token, an authorization code or a refresh token.

## What `status` can and cannot tell you

Claude access tokens are opaque strings, not JWTs, so the only expiry is the one this command computes from the issuer's `expires_in` when it stores the token. `status` makes no request and cannot tell whether the server accepts the token. For OpenAI's endpoint the server rejected a token whose expiry was still days away; nothing shows Anthropic behaves differently, nothing shows it does not. "usable by expiry: yes" means only "not past the expiry stored with it".

## The credential file

```json
{
  "version": 1,
  "access_token": "…",
  "refresh_token": "…",
  "expires_at": "2026-10-08T20:00:00Z",
  "scope": "…",
  "last_refresh": "2026-10-08T12:00:00Z"
}
```

- This layout is this project's own, not Claude Code's. Claude Code's file is not read or written. Keys this command does not know are kept when it rewrites the file.
- It is written to a private temporary file in the same directory (mode 0600, directory 0700), synced, and renamed over the old one, so the adapter, which reads the file on every request, never sees half a file.
- `keep_backup` (default true) keeps the previous file as `<file>.bak`, mode 0600, one generation. A refresh may replace the only refresh token.
- A lock file (`.<file>.jad-lock`, next to the credential file) makes two runs wait for each other, because a rotating refresh token spent twice can invalidate the login.
- A new file is checked before it is written: it needs an access token and an expiry. A response without `expires_in` writes nothing.
- `login --force` starts from nothing: the old access and refresh tokens and the scope are not carried over, because the new sign-in may be another account. Other keys are kept.

## Keeping tokens fresh

Nothing runs in the background. The lifetime the issuer reports (`expires_in`) is not known here, so use the stored expiry:

```sh
/tmp/claude-auth refresh --if-expires-within 30m
```

from a timer (systemd or cron, for example every 15 minutes). The adapter refuses a token whose stored expiry is within `upstream.expiry_margin_ms` and answers 401 with a message that names the file, so a missed refresh is visible. Combine with `--if-older-than` if the server turns out to end tokens earlier than the stored expiry.

## Config

Schema: `sample/schemes/claude-auth.schema.json`. A starting point, equal to the defaults: `sample/claude-auth.json`. Unknown keys are rejected. Every key is optional.

| Key | Default | Meaning |
| --- | --- | --- |
| `auth_file` | `~/.jad/auth/claude-oauth.json` | Credential file. Absolute or starting with `~/`. |
| `authorize_url` | `https://claude.ai/oauth/authorize` | Full URL of the authorization page. Claude Code's binary also holds `platform.claude.com/oauth/authorize` and `claude.com/cai/oauth/authorize`; which one is canonical is not known. |
| `token_url` | `https://platform.claude.com/v1/oauth/token` | Token endpoint, JSON body. |
| `client_id` | `9d1c250a-e61b-44d9-88ed-5944d1962f5e` | Claude Code's public client id. |
| `scope` | `org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload` | Requested scopes. |
| `authorize_params` | `code: "true"` | Extra query parameters on the authorize URL. A value here replaces the default set. The standard OAuth parameters cannot be listed. |
| `redirect_host` | `localhost` | Host in the redirect URI. The server listens on `127.0.0.1` and `::1`. |
| `login_port` | `53692` | Callback port. `0` picks a free one, but the issuer may accept only the registered port. |
| `callback_path` | `/callback` | Path of the loopback redirect URI. |
| `copy_code_uri` | `https://platform.claude.com/oauth/code/callback` | Redirect URI of `login --copy-code`. |
| `login_timeout_ms` | `300000` | How long `login` waits for the browser or the pasted code. |
| `open_browser` | `true` | `--no-browser` turns it off for one run. |
| `http_timeout_ms` | `30000` | Deadline of one token request. |
| `expiry_margin_ms` | `300000` | Margin `status` applies to the stored expiry. Keep it equal to the adapter's `upstream.expiry_margin_ms`. |
| `keep_backup` | `true` | Keep `<file>.bak`. |

## Where the wire facts come from

- Token endpoint, client id, scopes, PKCE S256, the extra `code=true` authorize parameter, both redirect URIs, the JSON code exchange (`grant_type`, `client_id`, `code`, `state`, `redirect_uri`, `code_verifier`) and the JSON refresh (`grant_type`, `client_id`, `refresh_token`): the pi coding agent's `anthropic.js` (read 2026-10-08).
- The Claude Code 2.1.287 binary on this machine holds the same token URL, client id and scope string. It also sends `scope` on refresh; this command does not, as pi does not.
- Not confirmed by any request: that `claude.ai/oauth/authorize` is the right authorize host, that the refresh response always carries `expires_in`, and that Anthropic accepts these calls from this program.
