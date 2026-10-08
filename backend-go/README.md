# Go backend

## Build native.fetch

The `native.fetch` HTML converter uses Xberg's cgo-backed Go binding and its platform-specific Rust FFI library. Use Go 1.26 or newer, with cgo enabled and a C toolchain installed.

From this directory, run the setup generator on each build host before building or testing for that host platform:

```sh
go generate ./internal/tools/native
```

This downloads and verifies the host-platform native library in the user cache and writes a machine-local cgo link shim. The shim (and the Windows DLL, if generated) is ignored by Git. Re-run the generator after upgrading the Xberg package. The generator detects the host platform; cross-compilation requires provisioning the matching target library and cgo link flags separately, and deployments must include the native runtime library.

## ChatGPT sign-in: `openai-auth`

`cmd/openai-auth` signs in to a ChatGPT account, refreshes its tokens and reports the credential file that a session with `auth_mode: "openai_oauth"` reads. The backend never writes that file. See `cmd/openai-auth/README.md`.

## Claude models: `anthropic-adapter`

`cmd/anthropic-adapter` serves the Responses API on top of the Anthropic Messages API, so a session can use a Claude model by changing `base_url` in `model.json`. See `cmd/anthropic-adapter/README.md`.

## Claude subscription sign-in: `claude-auth`

`cmd/claude-auth` signs in to a Claude account (Pro, Max, Team or Enterprise), refreshes its tokens and reports the credential file that the adapter reads when `upstream.credential_file` is set. Unofficial and untested against Anthropic; read its README before using it. See `cmd/claude-auth/README.md`.
