# Backend authentication

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
