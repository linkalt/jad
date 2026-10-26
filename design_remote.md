# Design: remote JAD calling (client side)

Status: design only, nothing implemented. Items marked **[unverified]** were not confirmed in code. Section 1 was read from code in `backend-go/internal/httpapi` (handler.go, profile_run.go, catalog_files_handlers.go) and `tools/native`.

## 0. Goal
A local JAD is the CLIENT. `profile_message` and `subagent` can target a profile/agent on another JAD over plain http or a unix socket, async through the existing handle model. `transcripts` / `transcript` list and read remote transcripts read-only, behind their own off-by-default switch. Permission is decided by the REMOTE (token table). No native bridge, no approvals.

## 1. Inventory: what the server has today
Auth: ONE static bearer token (`HandlerConfig.APIToken`, >=32 bytes, SHA-256 compared in constant time, handler.go `authorized`). Everything under `/api/sessions`, `/api/catalog`, `/api/library` needs it; `/api/health` does not. A token is all-or-nothing: it can do anything the web UI can (write catalog files, reset, compact). CORS allow-list for browsers only. Listener: `net.Listen("tcp", host:port)` in cmd/jad/main.go, plain HTTP, no TLS, no unix socket.

| Need | Exists? | Endpoint / gap |
|---|---|---|
| health | yes | `GET /api/health` |
| list sessions | yes | `GET /api/sessions`, `GET /api/catalog/sessions...` |
| list profiles | yes | `GET /api/catalog/profile` (files list) |
| profile tools | yes | `GET /api/sessions/{ref}/tools?profile=` |
| send a message to a profile | partly | `POST /api/sessions/{ref}/run?profile=` starts a run and streams SSE on the same response; busy gives 409 (ErrSessionBusy). It is the UI path: no `caller`, no `chain`, no queue-wait option (the in-process `ProfileRunRequest` has Caller/Chain/Wait/WaitLimit; the HTTP route does not) |
| run status / wait | partly | only `GET .../run/stream` re-attaches to the CURRENT run of that profile (no run id; a finished run is gone) |
| stop a run | partly | `POST .../run/stop` stops the profile's current run and cancels the session's children; no run id, so it cannot stop one run of several |
| read transcript | no | `.../history` and `.../messages/{id}` return UI messages, not the messages.jsonl list/detail views of `transcript` |
| list sub-agent runs | no | runs live in `SubagentManager` and `.runs/<id>.json` manifests; no HTTP route |
| start a sub-agent | no | sub-agents start only from a `subagent` tool call inside a run |
| run a catalog `agent` file | no | no route; agent files are not in `/api/catalog` kinds (profile, model, toolset, mcp) |
| scoped tokens | no | single token |
| hop counter | no | profile_message chain exists in-process only (`ProfileRunRequest.Chain`, `max_chain`) |
| TLS / unix socket | no | plain TCP listener |

Conclusion: the client can be built against nothing that exists except the UI run route, and that route cannot carry caller/chain/idempotency, cannot address runs, and cannot do sub-agents. A small server-side "remote API" is needed (section 7).

## 2. Remote entry file
Same folders and naming as profiles/agents (`<category>/<name>.json`). `kind: "remote"` is the discriminator; the loader (strict keys) rejects model/tools/instructions in a remote entry and `remote` in a profile or agent. **[open: profiles and agents are different directories; see Q1.]**

```json
{ "schema_version": 1, "kind": "remote", "description": "build box",
  "remote": {
    "url": "http://host:8787",              // or "unix:///run/jad/api.sock"
    "token_env": "JAD_REMOTE_BUILD_TOKEN",  // exactly one of token_env | token_file
    "timeout_ms": 30000,                    // per HTTP request, capped
    "max_response_bytes": 1048576,          // capped
    "default_session": "main",              // optional
    "profiles": ["worker/coder"]            // optional allow-list on the client side, narrows only
  } }
```
- No inline token. `token_file` must be a regular file, not group/world readable (unix), read at each connection, trimmed. A missing env/file is an error naming the key, never the value.
- `url`: scheme `http` or `unix` only; `https://` is rejected with the reason; http needs a host, no userinfo, no query/fragment. Unix path must be absolute. There is no TLS: the bearer token of an `http` url travels in clear text, so that connection must be a VPN or a private network, and a unix socket is the local option. There is no `ca_file`, `insecure` or `skip_verify` key; all of them are refused by name.
- Addressing in tools: `session/category/profile` goes in the target field next to the remote name: `remote:<name>/<session>/<category>/<profile>`. With `default_session`, `remote:<name>/<category>/<profile>` also works (3 vs 2 segments, same trick as `profile:`).
- Shows in the catalog and in `transcripts` as kind `remote` (listing a remote shows its name, url host (no token, no path of unix socket beyond its basename), and whether reading is enabled).

## 3. Tools
### 3.1 profile_message
Target `remote:<name>/<session>/<category>/<profile>` goes through a new `RemoteMessenger` implementing the same `ProfileMessenger.Prepare` contract: the plan's `Run` posts the message, returns a handle whose driver (kind `profile_message`, as today) maps to a remote run. Local cycle/chain checks stay local; the chain is also sent to the remote (section 6). Results: remote final text, bounded by the existing profile_message result caps; INFORMATION `transcript` key is NOT set for remote (no file path leaks); instead `remote_run: <name>/<run id>`.
### 3.2 subagent
`subagent` with `agent: "remote:<name>/<session>/<category>/<agent>"` starts a sub-agent on the remote (remote needs a start route, 7.3). Local `SubagentManager` keeps a local run record (local run id, kind subagent) whose driver proxies status/wait/stop to the remote run id. Nesting/depth limits apply locally to the local call and the remote enforces its own on its side. The child's own tools and approvals are the remote's business.
### 3.3 async handle mapping
Local handle id stays local (`async wait/status/stop` unchanged). Row `Label` = `remote:<name> <run id short>`. `Stop` -> `POST .../runs/{id}/stop` (idempotent). `Detail` -> remote status text, cut locally. Wait = long-poll `GET .../runs/{id}?wait_ms=` so no SSE is held across `block_ms`; the client never holds a streaming connection (simpler failure handling than the UI route; **decision recommended, see Q3**).

## 4. Transcripts
Grammar: `remote:<name>/` is a PREFIX in front of an existing `kind:` selector, the remote part of the string being exactly what the REMOTE's own `transcripts` would emit, so the string is copy-paste:
- `remote:b/profile:<session>/<category>/<name>`, `remote:b/subagent:<run id>`, `remote:b/subagent:<session>/<category>/<name>/<run id>`.
- Session is always explicit on a remote (the remote has no "current session" for us) unless `default_session` is set. `path:<file>` is NEVER accepted after `remote:`.
- The `transcript` tool resolves `remote:` in `transcript_path.go` before the local kinds; `transcripts` lists a remote as the one path `remote:<name>` (profiles of the remote's session), then the existing levels below it, with every emitted path carrying the `remote:<name>/` prefix.
- The REMOTE runs the real `transcript`/`transcripts` code (list/detail, truncation, caps, cursors, step numbers) and returns the already rendered text. Little crosses the wire; no second implementation of step numbering. Local only forwards the arguments (`step`, `part`, `offset`, `role`, `tool`, `search`, `turns`, `limit`, `cursor`) after validating key names; cursors are opaque to the client and bound by the remote to file identity as today.
- Remote output is sanitised on the client: the full messages.jsonl path lines are removed server-side (remote mode renders with paths off), and the client additionally rejects lines starting with `path`/absolute-path-looking fields only as defence in depth **[unverified: rely on server-side switch, not on filtering]**.
- Own switch: `tools.native.transcript.allow_remote` (default false) and the same in `transcripts`; shared semantic like the other `allow_*`. A per-remote `read_transcripts` is decided by the remote's token (403 -> clear error). Both must allow.
- Remote text is untrusted data: the tools wrap/mark it as the model-visible result like any other tool output; it never becomes instructions to the client harness.

## 5. Transport (client)
- One `http.Client` per remote entry, built from an explicit `http.Transport` (not DefaultTransport): plain http over tcp; unix via `DialContext` to the socket and a fixed dummy host `jad` (URL host not resolved; unix needs the Host header only).
- `Proxy: nil` (no env proxies for remote calls; open question Q6), `DisableCompression` false, HTTP/1.1.
- `CheckRedirect`: return `http.ErrUseLastResponse`; any 3xx is an error "remote redirected; redirects are not followed" (stops token forwarding and SSRF pivots). Authorization header is set per request, never in the client default.
- Timeouts: dial/response-header from `timeout_ms` (cap e.g. 5 min); whole-request context from the tool's own deadline; `wait_ms` polls are shorter than `timeout_ms`.
- Response cap: `io.LimitReader(max_response_bytes+1)`; over-cap is an error, never truncated JSON. Content-Type must be JSON/text as expected.
- No connection to loopback/link-local is blocked by default (a remote on localhost or a unix socket is legitimate); the guard against SSRF is that URLs come only from operator-written remote files, never from model input. The model supplies only the remote NAME. A name not in the catalog is `unknown_remote` listing nearest names (like the profile errors).

## 6. Server side: minimal additions (token table)
Server config gets `server.remote_api`: `enabled` (default false), the routes on the existing plain http listener or, when `listen_unix` is set, only on that unix socket (never both), and a token table:

```json
"tokens": [
 { "id": "build-box", "hash_sha256": "…", "expires": "2026-12-31T00:00:00Z",
   "profiles": ["main/worker/coder"],      // session/category/name globs
   "agents":   ["worker/*"],
   "actions":  ["message","subagent","status","stop","transcripts"],
   "max_hops": 3, "revoked": false } ]
```
Only a hash of the token is stored; reload on file change revokes immediately; every denial is logged with token id (never the token). Remote-API routes (new, under `/api/remote/v1`, separate from the UI token so a UI token cannot be used and a remote token cannot reach `/api/catalog` writes):

| route | token action |
|---|---|
| `POST /messages` {session, profile, message, request_id, hops, chain, caller} -> {run_id} (or 409 busy) | message |
| `POST /subagents` {session, agent, task, request_id, hops, ...} -> {run_id} | subagent |
| `GET /runs/{id}?wait_ms=` -> {state, final text, failure, started} | status |
| `POST /runs/{id}/stop` | stop |
| `GET /transcripts?...` / `GET /transcript?...` -> rendered text (profiles by the token's `profiles` globs; sub-agent runs also by its `agents` globs, a run of an agent outside them answers like one that does not exist) | transcripts |
| `GET /profiles?session=` | any |

Run ids are scoped to the token (a token sees only runs it started). **Idempotency:** `request_id` (client UUID) is stored with the run; a repeat returns the same run id instead of a second message. Loop guard: client sends `Jad-Hops: n+1` and the chain; remote rejects when `hops > min(token.max_hops, server limit)` with 508-style error `remote_hop_limit`, and the existing `max_chain` check runs with the received chain.

**The hop count is caller-supplied.** The server trusts `X-Jad-Hops`: a token holder can send `0` every time, so the limit only bounds peers that are honest (other JAD servers running this client). It is a loop guard, not a security boundary; what a token may do is decided by its actions and globs, and `max_runs_per_token` bounds how much one token can run at once.

## 7. Failure modes
- Connection drops before the response of `POST /messages`: retry with the same `request_id` (safe), up to a small configured count; after that the handle ends `lost` with "remote may have started the run; run id unknown, check remote transcripts".
- Drop while polling a run: retry polls with backoff until `timeout_ms * n`; then the local handle becomes `lost` (not failed, not done); `async stop` on a lost handle tries `stop` once, best effort, and always frees the local row.
- Client restart: remote runs continue; the local handle is not rebuilt (v1) **[Q4]**; the async manager already ends owner handles at restart.
- 401/403: error naming remote and action, no token text. 409 busy: same as local `profile_busy`. 5xx: one retry for idempotent GET only.
- Cancellation of the local run stops remote runs started by it (best effort) like children.

## 8. Security review
- SSRF: no model-controlled URL; no redirects; unix path only from files; names validated like profile names.
- Token leakage: only from env/file at request time, in the Authorization header; never logged, never in INFORMATION, error text, transcripts (tool args hold remote name only), debug capture (`httpdebug` must redact Authorization for remote clients **[unverified: check httpdebug redaction before implementing]**), `%v` of config structs (token fields are not stored, only the source name).
- Clear-text token: the bearer token of an `http` remote is visible to anyone on the path, so the deployment puts that connection on a VPN or a private network, or uses a unix socket; the server refuses a wildcard listen address (see section 11). The unix socket's permissions are the operator's job but the file is checked to be a socket, and a socket owned by another user or in a directory others can write to is refused before the token is sent.
- Resource: response cap, timeouts, per-remote concurrency cap (e.g. 8 in flight), poll rate floor.
- Path leakage: remote rendering mode strips file paths and the `transcript` INFORMATION key; `path:` forbidden after `remote:`; remote error messages are passed through the remote's own client-safe layer.
- Prompt injection: remote output is data; it is returned like tool output.
- Confused deputy: the remote token, not the local agent's rights, bounds what the remote does; a local profile with `remote` access can only reach what its token allows; locally the profile's tools list decides whether `profile_message`/`subagent` exist at all, and the entry's `profiles` allow-list narrows.
- Replays: request_id idempotency; tokens have expiry and revocation.

## 9. Phase plan (one commit each, tests listed)
1. **Remote entry file + loader**: `kind: remote`, strict schema, secret sources, URL validation, catalog/`transcripts` listing as `remote` (no network). Tests: strict keys, inline token rejected, https:// rejected, token never in `String()`/errors, schema agreement.
2. **Client transport**: `remote` package: http.Client factory (http, unix), no redirects, caps, timeouts, Authorization header, fake server tests (httptest + unix), redirect refusal, oversize, goroutine leaks, token redaction in errors/debug.
3. **Server remote API skeleton**: listener option, token table with hashes, scopes, revocation, hop header, `/profiles`, `/runs/{id}` status/stop; tests per token table row, 403 matrix, revoked token.
4. **Remote profile_message**: `POST /messages` with request_id idempotency + RemoteMessenger + async driver; tests: busy, retry no duplicate, drop -> lost, stop, chain/hops guard, handle wait/status/stop.
5. **Remote subagent**: `POST /subagents`, local run record + driver; tests as 4 plus depth limits unchanged.
6. **Remote transcripts**: server `GET /transcripts|/transcript` using the real tools in no-paths mode; client `remote:` prefix grammar in `transcript`/`transcripts`, `allow_remote` switches; tests: grammar, path stripping, switch off, token without transcripts action, cursor pass-through, emitted strings round-trip.
7. **Docs/schema/sample/DECISIONS.md**.
Each phase: gofmt, vet, diff --check, full tests, `-race -count=3`.

## 10. Decisions (final, from the user)
1. Remote entry files live in a sibling `remotes/` folder, not in profiles/agents. Loaders of profiles/agents need no change; `kind: "remote"` is still required in the file.
2. Remote sub-agent target: `remote:<name>/<session>/<category>/<agent>`.
3. Remote runs are followed by polling with `wait_ms`; no held stream.
4. After a client restart remote handles end like local ones; they are not rebuilt.
5. Server routes (`/api/remote/v1`, token table, http listener) are in scope for v1.
6. HTTPS_PROXY and proxy env are ignored for remotes.
7. A remote addresses only sessions that already exist.
8. Hop limit default 3; a remote may call back into the first JAD, bounded by hops.
Section 2 above says "same folders as profiles/agents": read it as the `remotes/` folder. httpdebug check: it already redacts Authorization, api-key, cookie headers and secret-like query keys (httpdebug/transport.go, test at transport_test.go); the remote client does not use httpdebug at all and gets its own redaction test in phase 2.

## 11. HTTPS removed: plain http and unix only (2026-10-30, user decision)

The user runs local -> VPN -> local and asked for the simpler thing: **there is no TLS any more, anywhere in remote calling.** The client's `url` is `http://host[:port]` or `unix:///absolute/path.sock`; an `https://` url is refused by name ("https was removed ..."), as are the TLS keys of a remote file (`ca_file`, `insecure`, `insecure_skip_verify`, `skip_verify`, `tls`). On the server, `server.remote_api` no longer has `tls_cert_file`/`tls_key_file` (both refused by name with the reason) and the main listener is never wrapped in TLS; the `/api/remote/v1` routes answer on the plain listener, or only on the unix socket when `listen_unix` is set: a socket keeps them off the network entirely.

Everything else is unchanged: the bearer token table (hashes, globs, actions, expiry, revocation, reload), `token_env`/`token_file` and their permission checks, the hop guard, `request_id` idempotency, no redirects, no proxy from the environment, the response and body caps, token scrubbing from errors, and the unix socket ownership and directory checks.

- **Clear-text token.** Over tcp the bearer token is readable by anyone on the path. The deployment must put that connection on a VPN or a private network; a unix socket is the local option. This is stated in `DECISIONS.md`, in this file, in the sample remote file (`sample/jad/remotes/build-box.json`) and in the schemas.
- **Listen address.** A wildcard is refused: with `server.remote_api` enabled and no `listen_unix`, `server.host`/`HOST` must name one address (`app.LoadConfig`, which refuses a wildcard host); a socket-only server is never on TCP, so any host is accepted there. The default stays `127.0.0.1`; reaching the API from the VPN peer means writing that VPN address into `server.host`, so exposing the token-taking routes is always deliberate. There is no new listener: the remote routes share the main one.

## 12. Implemented (2026-10-29)
Phases 1-7 are in. Differences from the text above: sub-agent targets are `remote:<name>/<session>/<category>/<profile>/<agent>` (the profile gives the context the agent runs in, as `subagent` runs for a local profile; the earlier `<category>/<agent>` was too short to name it); `transcripts` accepts `remote:<name>` for the default session and `remote:<name>/session:<id>`; the switch is `allow_remote` plus `remotes` in each of the two tools; a cursor from a remote read is `remote:<name>/<cursor>`. See DECISIONS.md, "Remote JAD calling".
