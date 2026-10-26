# Design: remote JAD calling (client side)

Status: design only, nothing implemented. Items marked **[unverified]** were not confirmed in code. Section 1 was read from code in `backend-go/internal/httpapi` (handler.go, profile_run.go, catalog_files_handlers.go) and `tools/native`.

## 0. Goal
A local JAD is the CLIENT. `profile_message` and `subagent` can target a profile/agent on another JAD over https or a unix socket, async through the existing handle model. `transcripts` / `transcript` list and read remote transcripts read-only, behind their own off-by-default switch. Permission is decided by the REMOTE (token table). No native bridge, no approvals.

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
    "url": "https://host:8443",            // or "unix:///run/jad/api.sock"
    "token_env": "JAD_REMOTE_BUILD_TOKEN",  // exactly one of token_env | token_file
    "ca_file": "/etc/jad/ca.pem",           // optional extra roots, https only
    "timeout_ms": 30000,                    // per HTTP request, capped
    "max_response_bytes": 1048576,          // capped
    "default_session": "main",              // optional
    "profiles": ["worker/coder"]            // optional allow-list on the client side, narrows only
  } }
```
- No inline token. `token_file` must be a regular file, not group/world readable (unix), read at each connection, trimmed. A missing env/file is an error naming the key, never the value.
- `url`: scheme `https` or `unix` only; `http://` is rejected (no insecure option); https needs a host, no userinfo, no query/fragment. Unix path must be absolute. TLS: system roots plus `ca_file`, TLS 1.2 minimum, hostname verified, no skip-verify key exists.
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
- One `http.Client` per remote entry, built from an explicit `http.Transport` (not DefaultTransport): https via TLS config above; unix via `DialContext` to the socket and a fixed dummy host `jad` (URL host not resolved; unix needs the Host header only).
- `Proxy: nil` (no env proxies for remote calls; open question Q6), `DisableCompression` false, HTTP/1.1 + h2 allowed on https.
- `CheckRedirect`: return `http.ErrUseLastResponse`; any 3xx is an error "remote redirected; redirects are not followed" (stops token forwarding and SSRF pivots). Authorization header is set per request, never in the client default.
- Timeouts: dial/TLS/response-header from `timeout_ms` (cap e.g. 5 min); whole-request context from the tool's own deadline; `wait_ms` polls are shorter than `timeout_ms`.
- Response cap: `io.LimitReader(max_response_bytes+1)`; over-cap is an error, never truncated JSON. Content-Type must be JSON/text as expected.
- No connection to loopback/link-local is blocked by default (a remote on localhost or a unix socket is legitimate); the guard against SSRF is that URLs come only from operator-written remote files, never from model input. The model supplies only the remote NAME. A name not in the catalog is `unknown_remote` listing nearest names (like the profile errors).

## 6. Server side: minimal additions (token table)
Server config gets `server.remote_api`: `enabled` (default false), `listen_unix` and/or `tls {cert_file,key_file}` on the existing listener (plain TCP stays for the UI; remote API only when TLS or unix is configured, refused over plain TCP), and a token table:

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
| `GET /transcripts?...` / `GET /transcript?...` -> rendered text | transcripts |
| `GET /profiles?session=` | any |

Run ids are scoped to the token (a token sees only runs it started). **Idempotency:** `request_id` (client UUID) is stored with the run; a repeat returns the same run id instead of a second message. Loop guard: client sends `Jad-Hops: n+1` and the chain; remote rejects when `hops > min(token.max_hops, server limit)` with 508-style error `remote_hop_limit`, and the existing `max_chain` check runs with the received chain.

## 7. Failure modes
- Connection drops before the response of `POST /messages`: retry with the same `request_id` (safe), up to a small configured count; after that the handle ends `lost` with "remote may have started the run; run id unknown, check remote transcripts".
- Drop while polling a run: retry polls with backoff until `timeout_ms * n`; then the local handle becomes `lost` (not failed, not done); `async stop` on a lost handle tries `stop` once, best effort, and always frees the local row.
- Client restart: remote runs continue; the local handle is not rebuilt (v1) **[Q4]**; the async manager already ends owner handles at restart.
- 401/403: error naming remote and action, no token text. 409 busy: same as local `profile_busy`. 5xx: one retry for idempotent GET only.
- Cancellation of the local run stops remote runs started by it (best effort) like children.

## 8. Security review
- SSRF: no model-controlled URL; no redirects; unix path only from files; names validated like profile names.
- Token leakage: only from env/file at request time, in the Authorization header; never logged, never in INFORMATION, error text, transcripts (tool args hold remote name only), debug capture (`httpdebug` must redact Authorization for remote clients **[unverified: check httpdebug redaction before implementing]**), `%v` of config structs (token fields are not stored, only the source name).
- TLS: verification always on, no insecure key; unix socket permissions are the operator's job but the file is checked to be a socket.
- Resource: response cap, timeouts, per-remote concurrency cap (e.g. 8 in flight), poll rate floor.
- Path leakage: remote rendering mode strips file paths and the `transcript` INFORMATION key; `path:` forbidden after `remote:`; remote error messages are passed through the remote's own client-safe layer.
- Prompt injection: remote output is data; it is returned like tool output.
- Confused deputy: the remote token, not the local agent's rights, bounds what the remote does; a local profile with `remote` access can only reach what its token allows; locally the profile's tools list decides whether `profile_message`/`subagent` exist at all, and the entry's `profiles` allow-list narrows.
- Replays: request_id idempotency plus TLS; tokens have expiry and revocation.

## 9. Phase plan (one commit each, tests listed)
1. **Remote entry file + loader**: `kind: remote`, strict schema, secret sources, URL validation, catalog/`transcripts` listing as `remote` (no network). Tests: strict keys, inline token rejected, http:// rejected, token never in `String()`/errors, schema agreement.
2. **Client transport**: `remote` package: http.Client factory (https+CA, unix), no redirects, caps, timeouts, Authorization header, fake server tests (httptest TLS + unix), redirect refusal, oversize, goroutine leaks, token redaction in errors/debug.
3. **Server remote API skeleton**: TLS/unix listener option, token table with hashes, scopes, revocation, hop header, `/profiles`, `/runs/{id}` status/stop; tests per token table row, 403 matrix, revoked token.
4. **Remote profile_message**: `POST /messages` with request_id idempotency + RemoteMessenger + async driver; tests: busy, retry no duplicate, drop -> lost, stop, chain/hops guard, handle wait/status/stop.
5. **Remote subagent**: `POST /subagents`, local run record + driver; tests as 4 plus depth limits unchanged.
6. **Remote transcripts**: server `GET /transcripts|/transcript` using the real tools in no-paths mode; client `remote:` prefix grammar in `transcript`/`transcripts`, `allow_remote` switches; tests: grammar, path stripping, switch off, token without transcripts action, cursor pass-through, emitted strings round-trip.
7. **Docs/schema/sample/DECISIONS.md**.
Each phase: gofmt, vet, diff --check, full tests, `-race -count=3`.

## 10. Open questions (need the user)
Q1. Directory: remote files in `profiles/` (same listing, so `kind` must be checked on every profile load) or in a sibling `remotes/` folder? Proposal: the same folders as requested, with `kind: "remote"` but a profile loader that skips them; agents folder also allowed. Cost: every loader/validator must learn to skip `remote`; a sibling dir is simpler and safer.
Q2. Must remote runs be addressable by `session/category/profile` only (user decision) even for `subagent`? A sub-agent is started from an AGENT file, not a profile. Proposal: `subagent` target `remote:<name>/<session>/<category>/<agent>` where the agent is a file in the remote; the session picks the remote's context (cwd, settings).
Q3. Poll (`wait_ms` long-poll) vs SSE re-attach for remote runs? Recommended: poll, no stream held.
Q4. After client restart, rebuild remote handles from a small local record, or let them end (as local async handles do)? Recommended: let them end, state it.
Q5. Server side in scope for v1 as the minimal routes of section 6, or should the first release only target a server that already has them? Without them nothing works (section 1).
Q6. Honour HTTPS_PROXY for remotes? Proposed no (explicit `proxy` key later).
Q7. Should the remote answer for sessions that do not exist yet (create), or only existing ones? Proposed: existing only.
Q8. Hop limit default and whether a remote may call back into the first JAD (allowed, bounded by hops).
