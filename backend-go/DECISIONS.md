# Go backend migration: initial dependency decisions

**Status:** The Go backend implements the OpenAI Responses streaming adapter and staged transcript recovery, a multi-round session runner with native and MCP tool execution, explicit and automatic compaction, the core HTTP/SSE routes, effective session tool discovery, and normalized live UI streaming. Native tools and the MCP client are implemented; OpenAI OAuth is intentionally excluded. Full TypeScript API/config parity and a live differential harness remain outstanding, so this document records current boundaries rather than a claim of complete parity.

The module path in `go.mod` (`example.com/agent-harness/backend-go`) is a placeholder until the repository's canonical import path is known.

## HTTP server

Use the Go standard library's `net/http` for the local API; the bounded route slice uses a small injectable `http.Handler` to centralize compatibility behavior. It provides request contexts and SSE flushing without an additional server framework. Do not add Fiber or raw `fasthttp` unless measurements show the HTTP server itself is a bottleneck.

`internal/httpapi.Handler` exposes `GET /api/health`, authenticated `GET /api/library` and `GET /api/library/{kind}/{id}`, session catalog/history/message/profile/model/files/tools routes, run start/attach/stop, and session compact/reset/front-compact. The session tools route returns the selected profile's effective native and MCP definitions. Root-library access is injected; responses are `no-store`, and configured library directories are reloaded on each request. Session lookup/config selection and transcript maintenance are injected; auth, CORS, body-size, SSE-buffer, and run-retention settings are explicit inputs. A disconnected start subscriber does not cancel an accepted run. Compaction retains configured request history through a compaction-cache overlay and resets derived caches; reset clears the transcript, backup, and known per-profile caches. Front-compaction removes the active transcript suffix beginning at a user record; configured recoverable mode archives the removed bytes in `<messages.jsonl>.deleted-history`, while permanent mode does not archive, and both invalidate derived caches. Live UI streaming normalizes reasoning and text deltas, function-call announcements and argument deltas, and tool results into cumulative messages; attach/reconnect streams replay the current message and terminal state. Remaining route/config parity is not claimed. Optional `http_debug` captures bounded provider HTTP request/response details asynchronously and redacts sensitive request headers.

## Filesystem session resolution and bootstrap

Run the current server with `go run ./cmd/agent-backend`. `AGENT_API_TOKEN` is required and must be at least 32 bytes; `HOST` and `PORT` override listener settings. Session model files select their API key through `api_key_env` (default `OPENAI_API_KEY`, or `OPENROUTER_API_KEY` for OpenRouter-compatible settings) and may set a custom `base_url`. API-key auth is supported; OpenAI OAuth is intentionally out of scope, and Go rejects `auth_mode: "openai_oauth"` rather than silently accepting it.

Root discovery follows the TypeScript precedence: `JAD_SESSIONS_DIR` selects one `default` root and bypasses root settings; otherwise `JAD_ROOT_SETTING_PATH` or `setting.json` in the working directory/parent can provide `session_roots` (or legacy `session_paths`), with `.jad/sessions` as the fallback. Multi-root requests use `root-id:session-id`; an unqualified ID is accepted only when it resolves uniquely. With one root, canonical references stay unqualified.

The resolver reads session `setting.json`/`model.json`, path-profile model and storage selection, prompt documents, and persistence settings for the current runner. It shares a single `TurnStore` per transcript path and serializes runs that use shared storage. Cross-process locking, live TypeScript differential tests, other API route families, and full session-config parity remain out of scope.

## OpenAI client

Use the official `github.com/openai/openai-go/v3` SDK behind a small adapter. The existing backend uses the Responses API, including streaming, custom provider base URLs, and function calls; keep SDK-specific types out of session and persistence code.

The v3.66.0 SDK is pinned in `go.mod` and was tested against an offline HTTP/SSE fixture. It provides `Responses.NewStreaming` and `ResponseStreamEventUnion.RawJSON()`, supplying raw event JSON to the compatibility/persistence layer. Its README states v3.45.0 and later require Go 1.25+. The current environment is Go 1.27.1; confirm the deployment Go version before cutover.

Do not reconstruct persisted provider events solely by marshaling typed SDK structs: preserve unknown response fields and verify the raw-event path in tests.

## JSON

Keep JSON operations behind a small internal boundary. Start with `encoding/json` for the first compatibility implementation. Evaluate Sonic against representative transcript/config/stream fixtures and benchmarks before adding it. Sonic's upstream documentation notes behavior differences from the standard library, including HTML escaping; the OpenAI SDK also performs its own JSON handling, so Sonic will not replace the SDK's decoder.

The `messages.jsonl` observable representation is a compatibility contract. Serialization tests must cover ordering, escaping, numeric values, unknown fields, and the exact line shape before changing serializers.

`internal/transcript.Journal` appends caller-supplied JSON objects, repairs an incomplete final line, supports hash-based idempotent append for a rendered file, and serializes transcript reads with writes. It syncs for `turn`/`strict` durability. `TurnStore` stages `response.output_item.done` items in `items.jsonl` plus `items.index.jsonl`, commits them in output-index order, filters minimal/full response metadata, compacts usage, and writes a hash-tracked canonical completion line. It also appends input batches with TypeScript-compatible batch metadata and expands raw input/`response.completed` records back into request-history items. Offline fixtures cover ordering, unknown fields, metadata modes, large items, duplicate commits, and Node-generated input-batch lines.

Recovery repairs partial message/item/index tails, retains unfinished turns without a final record, and completes or removes turns whose final record is present, using its hash to avoid duplicate transcript lines. Retained streaming turns are not yet reopened for writing. The `internal/session.Runner` serializes runs, normalizes and persists inputs, derives the next turn number from transcript records, builds requests from model settings and prompt configuration, streams raw Responses events, and commits completed or incomplete responses. Its provider-neutral stream interface keeps SDK types outside orchestration. Function calls execute through the session `ToolCatalog`; results are persisted and may feed follow-on provider rounds within configured limits. Explicit and automatic compaction are supported, with statecontext refresh after successful automatic compaction. Request history is reconstructed from `messages.jsonl` rather than optional history/usage caches, and a live differential harness against a running TypeScript store remains outstanding.

## Deferred integrations

The MCP client is implemented with session-scoped connection reuse, stdio and Streamable HTTP transports, tool discovery, schema validation, and bounded result conversion. OpenAI OAuth remains intentionally unported; the Go backend supports API-key authentication and custom base URLs, and rejects unsupported OAuth configuration explicitly.

## Compatibility gates

Before cutover, test the Go implementation against the TypeScript baseline for:

- `messages.jsonl` output and recovery behavior;
- Responses stream events, including incomplete/failure events and function calls;
- HTTP JSON and SSE event shapes used by the frontend;
- custom provider base URLs and API-key handling;
- cancellation and session/run lifecycle behavior.

Keep test sessions isolated so the two implementations never concurrently write the same session data.

## Cross-process locking

The TypeScript backend has no cross-process session lock: it assumes one server process per session directory and only guards the partial-response cleanup (`responses.partial.cleanup-lock`). The Go backend goes further. `internal/sessionlock` takes an advisory `flock` (`LockFileEx` on Windows) on a hidden `.<file>.jad-lock` beside `messages.jsonl` and `setting.json`, so two Go processes cannot interleave writes to one session. Lock files are persistent and never removed, and workspace file listings hide them.

This is a deliberate superset, not a parity gap. It is the only on-disk difference the differential harness (`tools/differential`) ignores. It does not make mixed use safe: the TypeScript backend never takes these locks, so a TypeScript and a Go process writing the same session concurrently remain unsupported. Making that safe would require the TypeScript backend to take the same locks, which is out of scope for this port.

## Interactions that ask the user

A tool that must ask the user cannot answer itself and cannot read the run stream, so the question waits in a per-run broker: the tool hands over the dynamic JSON it proposes, the run publishes it as **message state** (`WebUiMessage.pending`, so a reload or a second tab still sees it), and the promise settles when the answer endpoint replies, the deadline passes, or the run stops. The tool result then carries the outcome back to the model as an ordinary `function_call_output`, so an interaction adds no transcript item type and `messages.jsonl` keeps its format.

Keep these rules when porting:

- The **in-code definition is the contract**; the shipped document under `sample/tools/native/` is an enablement manifest whose `inputSchema` must equal the code schema exactly (the loader refuses a divergence, and a test asserts the two are deep-equal).
- Dynamic JSON payloads are accepted with **configured bounds** (`max_draft_bytes`, `max_draft_depth`, `max_title_chars`), measured the same way in both places they are checked: the tool before it publishes a question, and the answer endpoint before it applies the user's edit. The limits travel **with the question**, not in a second config lookup, so the check always follows the question that is waiting.
- The **session config owns the wait**: `timeout_ms` is what the run publishes and the model has no argument that could change it. The user may push the deadline out from the drawer — `extend_ms` per press, the whole wait capped at `max_timeout_ms` — and an extension **republishes the question** with its new `expires_at`/`max_expires_at`, so every client shows the same time left.
- Report what the user changed as `changed` — the **paths** the edit differs at, relative to the proposed draft — so the model reads a large edit as a handful of paths instead of a re-read of the whole payload.

## Run streams and how a client learns a run finished

A run outlives the request that started it, and a browser that loses its stream must still end up in the right state. Two streams carry one run — `POST …/run` (the request that started it) and `GET …/run/stream` (attach, for a reload or a second tab) — and **both begin with a `run` frame** naming that run: `{turn_id, started_at, status}`. A client that later reads a session snapshot has to recognize which run the status it sees belongs to; the snapshot reports `run`/`runs_by_profile` with the same `turn_id`, which is how a client whose stream went silent learns that *its own* turn finished instead of waiting for a frame that may never come.

Keep these rules when porting:

- Terminal events are replayed to a subscriber that arrives after the run finished (`ui.message`, then `completed`, then `done`), and the response **ends** after `done` — on both routes. A finished run is retained for `run_state_retention_ms` and then retired; attaching after that answers a single `idle` frame and ends.
- A client may **replace a stream that goes quiet** (the TypeScript client's `run_stream_stall_timeout_ms`): silence is not the same as slowness, because a connection that stops delivering frames neither errors nor ends. Reconnecting is safe at any point in a run — the attach route replays the newest `ui.message` snapshot, so a client that missed every delta is correct from that frame on, and a client that already saw the terminal events finishes immediately.
- A tool that asks for an interaction (`WebUiMessage.pending`) survives all of this: the question is message state, so a reconnecting or attaching client sees it in the snapshot it replays.

## Parallel tool calls and the SSE keep-alive (TypeScript 479509f)

Tool calls of one provider response run with bounded concurrency (`max_parallel_tool_calls`, default 8, range 1–64; `1` restores strict order). The Go port keeps the TypeScript contract:

- Each result is published to the client the moment its call settles, so the stream follows completion order. `messages.jsonl` is unchanged: every result is persisted once, after the batch, in call order.
- After the first error no further call starts; calls already running settle and every result that completed is persisted. A cancelled run cancels all in-flight calls through the shared context.
- A user-decision tool (`request_draft`) in the response still runs alone; the other calls get the `user_decision_required` placeholder.
- A quiet run stream gets a `: keep-alive` SSE comment every `server.sse_heartbeat_interval_ms` (default 5000, `0` disables, max 300000). It carries no event, so a client parser ignores it and only the stall watchdog is reset.
- Slow clients are bounded by `server.read_header_timeout_ms` (default 60000, max 300000) and `server.idle_timeout_ms` (default 5000, max 3600000); `0` disables either. Both backends read the same keys, and the defaults equal Node's `headersTimeout` and `keepAliveTimeout`. Neither backend sets a whole-request or write timeout, because that would cut long-lived SSE run streams.

The Go backend has a structured server log built on `log/slog` (`internal/logging`). It is Go-only: the keys below live in the shared `server` section, and the TypeScript loader ignores them, so one `setting.json` serves both backends, while TypeScript keeps its `console.*` output.

- `server.log_level` (`debug|info|warn|error|off`, default `info`), `server.log_format` (`text|json`, default `text`) and `server.log_file` (default empty = standard error; relative paths resolve against the setting file's directory). The file is opened append-only with mode 0600; a file that cannot be opened stops startup instead of silently dropping records. There is no rotation: use logrotate or the service manager.
- Components receive a `*slog.Logger` by injection (`httpapi.Config.Logger`, `transcript.TurnStoreOptions.Logger`, `httpdebug.Settings.Logger`, `app.WithLogger`). A nil logger discards records; there is no global logger, so the library packages stay quiet in tests and embedders.
- What is logged: a 5xx response with its cause (the response body stays `internal server error`); a recovered panic in a request or a run, with its stack; the run lifecycle (started, finished with status and duration, failed, aborted with its reason); a session skipped from the list for an invalid setting, once per distinct reason (the frontend polls the list, so every skip would repeat); and the existing persistence and cache warnings. At `debug`, one access line per request.
- What is never logged: request or response bodies, prompts, model output, the `Authorization` header, tokens, environment values or query strings. The access line carries a route template (`/api/sessions/{session}/run`), never the raw URL, and a path outside the known routes is `unmatched`, so a client cannot write text into the log through the URL. Provider failure causes are logged server-side only and may name the provider URL or an upstream message; `http_debug` remains the opt-in way to capture bodies.
- Startup and shutdown failures are always written to standard error, whatever the level, because they explain why the process exited.
- Go's process manager and fetch store were already serialized behind a mutex that spans the capacity check and the write, so the two races TypeScript closed (`max_running`, fetch storage capacity) did not exist here; regression tests lock that in.

Go-specific, accepted differences:

- **Order of near-simultaneous results.** Two calls that finish within microseconds publish in an order that depends on the scheduler (a JavaScript microtask queue and Go goroutines differ). Calls whose duration differs by more than scheduling noise publish in the same order on both backends; the differential harness compares only the set for calls that settle together.
- **Heartbeat skipping.** TypeScript skips a frame while earlier output is still queued in the socket. Go writes each frame from one goroutine that blocks on the write, so there is never queued output for the heartbeat to skip behind; a stalled client back-pressures the loop instead.
