# openai-auth

Signs in to a ChatGPT account, refreshes its tokens and reports the state of the credential file that the Go backend reads for `auth_mode: "openai_oauth"` in `model.json`. The backend never writes that file; this command is its only writer.

```sh
go build -o /tmp/openai-auth ./cmd/openai-auth
/tmp/openai-auth login            # first sign-in
/tmp/openai-auth status           # what is stored; never prints a token
/tmp/openai-auth refresh          # new tokens from the stored refresh token
```

Add `--config <path>` to any command to use a config file (see below). Without it the defaults apply, and the credential file is `~/.jad/auth/openai-codex.json`, the backend's default `oauth_auth_file`. If `model.json` names another file, set the same path in `auth_file`.

## Commands

| Command | What it does | Exit status |
| --- | --- | --- |
| `login` | Starts a loopback server on `localhost:1455`, prints the authorization URL, opens a browser (unless `--no-browser` or `open_browser: false`), waits for the redirect, exchanges the code (PKCE, S256) and stores the tokens. Refuses when a credential file exists unless `--force`. Gives up after `login_timeout_ms`. | 0 signed in, 1 failed |
| `refresh` | Sends the stored refresh token to the issuer and rewrites the file. `--if-older-than 55m` does nothing when the tokens were refreshed more recently than that, so a timer can call it often. | 0 refreshed or not due, 1 failed, 3 sign in again |
| `status` | Prints path, file mode, `auth_mode`, a shortened account id, whether a refresh token is present, the age of the last refresh, and for the access token and id token when they were issued and when their expiry claim says they expire. `--json` prints the same as JSON. | 0 usable by claims, 1 file missing or unusable, 2 access token expired by its claim |

Exit status 3 from `refresh` means the issuer refused the stored refresh token (a 4xx other than 408 and 429) or the file has none. Repeating the command cannot help; run `login --force`. A 5xx, a 429 or a network error exits 1: try again later.

`login` writes its progress, including the URL, to stderr and the final line to stdout. Nothing the command prints contains a token, an authorization code or a refresh token.

## What `status` can and cannot tell you

`status` reads the tokens' own claims. It makes no request. It cannot tell whether the server accepts the token: on 2026-10-08 the Codex endpoint answered `401 token_expired` for an access token issued 83 hours earlier whose `exp` claim was still 6.5 days away. So "usable by claims: yes" means only "not expired by its own claim". How often to refresh is not known from the claims; see Keeping tokens fresh.

## The credential file

```json
{
  "auth_mode": "chatgpt",
  "tokens": { "id_token": "…", "access_token": "…", "refresh_token": "…", "account_id": "…" },
  "last_refresh": "2026-10-08T12:00:00.000Z"
}
```

- This is the layout the Codex CLI and the `openai-oauth` library use. Keys this command does not know are kept when it rewrites the file.
- It is written to a private temporary file in the same directory (mode 0600), synced, and renamed over the old one, so the backend, which reads the file on every request, never sees half a file.
- `keep_backup` (default true) keeps the previous file as `<file>.bak`, mode 0600, one generation. A refresh replaces the only refresh token, so the backup is the way back if the new file is wrong.
- A lock file (`.<file>.jad-lock`, next to the credential file) makes two runs wait for each other. This matters because the issuer normally rotates the refresh token: a second refresh with the already spent token fails and can invalidate the login.
- A new file is checked before it is written, with the same rule the backend uses: it needs an access token and an account id. A response without them leaves the old file as it was.
- `login --force` starts from nothing: the old access, id and refresh tokens and the account id are not carried over, because the new sign-in may be another account. Other keys are kept.

## Keeping tokens fresh

Nothing runs in the background. Use a timer, for example a systemd timer or cron every 30 minutes:

```sh
/tmp/openai-auth refresh --if-older-than 55m
```

55 minutes is the interval the `openai-oauth` library uses (`REFRESH_INTERVAL_MS`). I read it in that library's source. I have not measured how long the Codex endpoint accepts an access token, so treat the number as a starting point and lower it if you see `token_expired` again. The backend does not guess a token age: it refuses a token only when the token's own expiry claim has passed, or comes within `openai_oauth.expiry_margin_ms` (`setting.json`) of it.

## Config

Schema: `sample/schemes/openai-auth.schema.json`. A starting point: `sample/openai-auth.json`. Unknown keys are rejected. Every key is optional.

| Key | Default | Meaning |
| --- | --- | --- |
| `auth_file` | `~/.jad/auth/openai-codex.json` | Credential file. Absolute or starting with `~/`. |
| `issuer` | `https://auth.openai.com` | `/oauth/authorize` and `/oauth/token` are appended. |
| `client_id` | `app_EMoamEEZ73f0CkXaXp7hrann` | The Codex CLI's public client id. |
| `scope` | `openid profile email offline_access` | Requested scopes. |
| `authorize_params` | `id_token_add_organizations: "true"`, `codex_cli_simplified_flow: "true"` | Extra query parameters on the authorize URL. A value here replaces the default set. The standard OAuth parameters cannot be listed. |
| `redirect_host` | `localhost` | Host in the redirect URI. The server listens on `127.0.0.1` and `::1`. |
| `login_port` | `1455` | Callback port. `0` picks a free one, but the issuer may accept only the registered port. |
| `login_timeout_ms` | `300000` | How long `login` waits for the browser. |
| `open_browser` | `true` | `--no-browser` turns it off for one run. |
| `http_timeout_ms` | `30000` | Deadline of one token request. |
| `expiry_margin_ms` | `300000` | Margin `status` applies to the expiry claim. Keep it equal to `setting.json` `openai_oauth.expiry_margin_ms`. |
| `keep_backup` | `true` | See above. |

## What is and is not known

- **The sign-in is not documented by OpenAI.** The issuer, client id, scope, the two extra authorize parameters, the `1455` callback and the request shapes (form-encoded code exchange, JSON refresh) were read from the source of the `openai-oauth` npm library (version 2.0.0 in `backend/node_modules`), which describes itself as unofficial. OpenAI's Codex authentication page says only that the CLI signs in through a browser. All of them are config keys.
- **Not run against the real issuer.** The tests use a fake issuer. They show the command does what the library does, not that `auth.openai.com` still accepts it. `status` is the only command that has run against the real credential file, and it did not change it.
- **Terms.** I found no OpenAI text allowing or forbidding another tool to use the Codex client id. Using it is your decision; `client_id` is a key so you can change it.
- **No browser:** on a machine without one, run `login` on a machine that has one and copy the file (mode 0600). There is no device-code flow because I found none in the library.
- **No `logout`/`clear`:** delete the file yourself. That does not revoke the tokens at the issuer.
- **Windows and macOS:** nothing ran there. The lock and the rename use the same code as the session locks, which has Windows and macOS builds.
