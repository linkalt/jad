# Backend authentication

## Local HTTP API

Set a private, randomly generated `AGENT_API_TOKEN` (at least 32 characters) in the backend **process environment before starting it**. The server refuses to start without one; do not put the token in `setting.json`, frontend source, or a public environment file. For example, generate one with `openssl rand -hex 32` and supply it through your private process manager. The current backend process is unaffected until you restart it yourself.

Every `/api/sessions` route requires `Authorization: Bearer <token>`; `/api/health` remains public. Browser clients additionally require their exact origin in root `setting.json` under `server.allowed_origins` (for example `http://localhost:5173` for the Vite dev proxy). The default is `[]`, which denies cross-origin browser calls. Configure the actual frontend origin if its host or port differs; no wildcards are supported. The frontend prompts for the token on the first 401 and keeps it only in that tab's session storage. Direct API clients must add the header themselves. Keep the API on loopback or behind TLS and a trusted reverse proxy; bearer tokens are not safe over unencrypted remote HTTP. Do not grant untrusted users access to the frontend origin, since code running on that origin can access its tab's token.

Legacy model configurations default to API-key authentication. To use the optional local Codex OAuth adapter, sign in with the backend CLI and select `auth_mode: "openai_oauth"` in that session's `model.json`:

```json
{
  "schema_version": 1,
  "provider": "openai-codex",
  "model": "gpt-5.4",
  "auth_mode": "openai_oauth",
  "oauth_auth_file": "~/.jad/auth/openai-codex.json",
  "request": {}
}
```

`oauth_auth_file` is optional; its default is `~/.jad/auth/openai-codex.json`. It must be absolute or start with `~/`. Do not set `base_url` in OAuth mode; the adapter selects its Codex endpoint. No frontend OAuth or token forwarding is used.

From `backend/`, run:

```sh
npm run auth -- login
npm run auth -- status
```

Use `npm run auth -- login --oauth-file <same-path>` if the model config uses a non-default file. Existing credentials require terminal confirmation before replacement; `--force` is available for non-interactive use. `npm run auth -- clear` deletes only the local file and does not revoke the provider-side grant.

The OAuth credentials are stored separately from the standard Codex CLI auth file. Keep the file private and out of source control. `openai-oauth` is an unofficial, community-maintained integration; its upstream service behavior can change. Use the adapter only for the user's own account and do not share credentials or bypass restrictions. The OAuth adapter is optional; removing `auth_mode` or setting it to `api_key` restores the existing API-key path (`api_key_env`, defaulting to `OPENAI_API_KEY`).
