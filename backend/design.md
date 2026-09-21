# Agent Harness Persistence Design

**Status:** Implemented (initial)
**Scope:** OpenAI Responses API response streaming and session persistence
**Implementation target:** TypeScript under `backend/`

## 1. Purpose

The Agent harness stores each session below:

```text
.jad/sessions/<session-id>/
├── messages.jsonl
├── model.json
├── setting.json
├── tools/                  # optional tool definition documents
└── staging/
```

This document defines the first version of the response persistence protocol. The primary goals are:

- preserve the OpenAI Responses API response shape;
- commit only complete responses to `messages.jsonl`;
- bound memory by the largest active output item rather than total response size;
- recover safely after process, stream, or filesystem failures;
- keep the persisted response suitable for deterministic local replay and cache-key generation.

## 2. Core decision

`messages.jsonl` stores one compact JSON event for each completed model response:

```json
{"type":"response.completed","response":{"id":"resp_123","status":"completed","model":"gpt-...","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}}
```

The exact fields inside `response` are those supplied by the Responses API. The example is illustrative; fields must not be invented or removed except that `response.output` is assembled from staged completed output items.

The following events are **not** written to `messages.jsonl`:

- `response.output_text.delta`;
- `response.content_part.added`;
- `response.output_item.added`;
- `response.output_item.done`;
- other intermediate streaming events.

Intermediate events are transport state. The committed `response.completed` event is the durable session state.

### 2.1 Input transcript decision

The same `messages.jsonl` file also stores request-history input items as raw Responses API input items, one object per line and without a harness envelope:

```json
{"type":"message","role":"user","content":[{"type":"input_text","text":"Find the answer"}]}
{"type":"function_call_output","call_id":"call_123","output":"answer"}
```

The transcript therefore has two canonical line categories:

1. raw input items such as `message` and `function_call_output`;
2. completed model events with the exact `response.completed` shape.

The history reader yields raw input items directly and expands each committed `response.completed.response.output` array into input items for the next request. This preserves output order and avoids a custom cache-relevant wrapper. `model.json`, `setting.json`, and tool definition documents remain separate configuration sources.

### 2.2 Configuration loading

`src/config.ts` loads and validates the session configuration. JSON Schema definitions are kept in `sample/schemes/model.schema.json` and `sample/schemes/setting.schema.json`:

- `model.json` is required and contains `provider`, `model`, optional `base_url`, an `api_key_env` name, and request-generation options;
- `setting.json` is optional and defaults to the session directory name, the current working directory, and an empty system prompt;
- API keys are never read from JSON files;
- relative working directories are resolved to absolute paths and must exist as directories;
- unknown fields are preserved for forward compatibility;
- normalized model and setting objects receive canonical SHA-256 hashes;
- `setting.json.http_debug` is disabled by default and controls native request/response diagnostics;
- enabled diagnostics write numbered JSON records to `debug/http/request1`, `debug/http/response1`, and subsequent request/response files in the session directory;
- API-key and authorization headers are redacted, and bodies are opt-in and capped by `max_body_bytes`;
- `tool_definition_paths` selects definition documents relative to `cwd`;
- a tool must also be present in the per-session `tools` policy to be exposed, so omitted tools are disabled;
- native `read` uses `max_bytes` as its output-content cap and defaults its optional `limit` to 250.

Example HTTP debug configuration:

```json
{
  "http_debug": {
    "enabled": false,
    "include_request_headers": false,
    "include_request_body": false,
    "include_response_headers": true,
    "include_response_body": false,
    "max_body_bytes": 65536
  }
}
```

Example model configuration:

```json
{
  "schema_version": 1,
  "provider": "openrouter",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key_env": "OPENROUTER_API_KEY",
  "model": "inclusionai/ling-3.0-flash-fin:free",
  "request": { "max_output_tokens": 4096 }
}
```

The loader rejects runner-owned request fields such as `input`, `instructions`, `stream`, and `tools` inside `model.json`.

### 2.3 Session runner and native tools

`src/session.ts` provides `AgentSession` for the streaming turn path:

1. load and normalize model/settings files;
2. load the selected tool definition documents and compile their input schemas with AJV;
3. normalize text input into a Responses `message` input item;
4. append the input item to `messages.jsonl`;
5. reconstruct all prior input items and completed response output items;
6. build a streaming request with `model`, `input`, `instructions`, model request options, and selected tools;
7. persist the streamed response through the response adapter;
8. execute returned function calls, append `function_call_output` items, and continue until a response has no tool calls.

Runs are serialized per `AgentSession` instance. Tool-call schema failures and execution failures are returned as compact tool output text; they do not cause an unnecessary duplicate provider request.

### 2.4 Local HTTP/SSE bridge

`src/server.ts` exposes the runner to the local frontend without exposing provider credentials to the browser:

- `GET /api/health` checks that the server is available;
- `GET /api/sessions/:id` returns the persisted transcript snapshot;
- `GET /api/sessions/:id/tools` returns the effective provider-neutral tool definitions;
- `POST /api/sessions/:id/run` accepts `{ "input": "..." }` and emits SSE `ui.message`, `completed`, `done`, or `error` events.

The session roots are configured by the workspace-level `setting.json` `session_roots` array. Each root path is resolved relative to that file and contains session subdirectories. `JAD_ROOT_SETTING_PATH` selects another root setting file, while `JAD_SESSIONS_DIR` and the `sessionsDir` server option remain single-root compatibility overrides. The default fallback is `.jad/sessions` (or `../.jad/sessions`). When multiple roots are configured, API session references are namespaced as `root-id:session-id` to avoid collisions. The frontend Vite server proxies `/api` to `127.0.0.1:8787`. Start the local integration with `npm run server --prefix backend` and `npm run dev --prefix frontend` from the project root. The backend owns its TypeScript compiler and Node types; the frontend is only an HTTP/SSE client and is not a backend build dependency.

The browser may abort its fetch, but the first bridge version does not cancel an already-started provider request; the server continues persistence so the transcript remains consistent.

### 2.5 `web-ui-normalized` plugin

`src/plugins/web-ui/` is a transport/view plugin, separate from `messages.jsonl` persistence. The legacy `src/plugins/web-ui-normalized.ts` module remains a compatibility barrel:

- `reasoning-web-ui.ts` normalizes reasoning output and deltas;
- `assistant-output-web-ui.ts` normalizes assistant text output and deltas;
- `tool-read-web-ui.ts` normalizes native read calls, tool-call fallbacks, and tool results;
- `setting-web-ui.ts` maps session configuration into UI metadata;
- `normalize-web-ui.ts` owns ordering, merging, tool-result pairing, and message assembly.

The plugin:

- converts raw session records into UI messages for `GET /api/sessions` and `GET /api/sessions/:id`;
- the session list requires `setting.json`, validates its session settings, and uses `setting.json.session_name` for the display name; model validation remains part of full session operations;
- it converts provider stream events into ordered `ui.message` SSE updates;
- it keeps adjacent reasoning/text chunks merged in the backend and emits tool call/result blocks as structured parts;
- it does not change the raw `messages.jsonl` format or write a UI projection there;
- the frontend receives ready-to-render messages and does not normalize provider events.

Each normalized assistant message contains ordered `parts` with `reasoning`, `text`, `tool_call`, or `tool_result` types, plus aggregate `text` and `reasoningContent` fields for rendering. Tool results retain their bounded output and a normalized success/error status.

## 3. Terminology

- **Turn:** One Responses API request and its resulting stream.
- **Output item:** One item in `response.output`, such as a message, function call, or reasoning item.
- **Staging:** Durable temporary state for a turn that has not yet been committed.
- **Commit:** The operation that appends one complete `response.completed` line to `messages.jsonl`.
- **Canonical:** Compact, deterministic JSON with the API-compatible top-level shape. Canonical does not automatically mean byte-identical to the provider's original SSE serialization.

## 4. Staging layout

Each active turn gets a unique directory:

```text
staging/<turn-id>/
├── manifest.json
├── items.jsonl
├── items.index.jsonl
└── final.jsonl                 # exists only while committing
```

`items.jsonl` contains internal records, one per completed output item:

```json
{"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}
{"output_index":1,"item":{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}}
```

The staging envelope is internal and must never appear in the final `messages.jsonl` record.

`items.index.jsonl` stores the byte offset and length of each item record. It allows the commit phase to emit output items in `output_index` order without loading the whole response into memory. `final.jsonl` is a fully rendered canonical line prepared before it is appended to `messages.jsonl`; it makes commit retry and hash-based idempotency possible.

A manifest contains transaction state, for example:

```json
{
  "schema_version": 1,
  "turn_id": "turn-001",
  "response_id": "resp_123",
  "status": "streaming",
  "item_count": 1,
  "next_sequence": 1
}
```

Recommended manifest states:

```text
streaming -> ready -> committing -> committed
                         \-> failed
```

The manifest may also contain a canonical-line hash after the final record has been rendered. The response ID and hash are used for recovery and duplicate-commit detection, not added to `messages.jsonl`.

## 5. Streaming workflow

### 5.1 Start a turn

1. Allocate a unique `turnId`.
2. Create the staging directory.
3. Create the manifest with `status: "streaming"`.
4. Open the Responses API stream.

Only one writer should commit a session at a time. The default harness should also process turns sequentially because response order affects conversation history and provider prompt caching.

### 5.2 Process deltas

While an output item is incomplete, the stream adapter keeps the current item state in memory. Deltas update that state; they are not appended to `messages.jsonl`.

The normal memory bound is:

```text
size of the largest active output item + stream/parser buffers
```

It is not the size of the whole response.

If a single output item can be very large, the adapter should support a spill threshold. Once the threshold is exceeded, text or argument fragments are written to a turn-local temporary file instead of remaining entirely in memory. This is an optional second phase; the initial implementation may enforce a maximum item size and fail safely when it is exceeded.

### 5.3 Process `response.output_item.done`

When an output item is complete:

1. Validate the item shape.
2. Determine its `output_index` from the API event. If unavailable, assign the next monotonic index.
3. Serialize an internal staging record.
4. Append exactly one complete line to `items.jsonl`.
5. Flush according to the configured durability policy.
6. Update the manifest.
7. Release the in-memory item and its buffers.

The item must be staged before its memory is released. A failed staging write must not advance the manifest or discard the item.

The final output order is determined by `output_index`, not merely by the order in which asynchronous callbacks happen to finish.

### 5.4 Process `response.completed`

When the final event arrives:

1. Verify that its response ID matches the manifest.
2. Verify that all expected output indexes are present and unique.
3. Preserve the final response metadata, including unknown future API fields.
4. Replace or ignore the final event's in-memory `response.output` and rebuild it from staged items in index order.
5. Render one canonical `response.completed` line.
6. Append the line to `messages.jsonl`.
7. Flush the messages file.
8. Mark the manifest `committed`.
9. Remove the staging directory after successful commit confirmation.

If the SDK has already materialized a complete `response.output` array internally, the harness cannot eliminate that SDK-level allocation. The persistence layer must nevertheless avoid retaining or serializing a second full copy.

### 5.5 OpenAI SDK adapter

`src/responses.ts` provides two boundaries:

- `persistResponseStream()` accepts any `AsyncIterable<unknown>` containing Responses-compatible events;
- `persistOpenAIResponse()` accepts the official `openai` Node SDK client and its typed streaming request.

The official SDK owns authentication, HTTP, and SSE decoding. The adapter forwards only `response.output_item.done` and `response.completed` events to the persistence layer. This keeps storage tests offline and keeps persistence independent of SDK-specific object types.

## 6. Memory-efficient final serialization

The implementation must not reconstruct a large response with one unbounded call such as:

```ts
JSON.stringify(fullResponse)
```

Instead, the final line should be rendered incrementally from metadata and staged items:

```text
write {"type":"response.completed","response":<metadata-without-output>,"output":[
write staged item 0
write ,
write staged item 1
write ]}}
write newline
```

The exact serializer must produce valid JSON and must preserve the required property order if byte-level cache keys are used.

Because an append-only file cannot atomically replace an existing file, a crash may leave a partial final line. On startup, the writer must retain complete newline-terminated records and truncate only the incomplete trailing record before resuming.

## 7. Canonical `messages.jsonl` rules

Every committed response record must follow these rules:

1. UTF-8 encoding.
2. One JSON object per physical line.
3. No pretty-print whitespace.
4. A terminating newline after every committed record.
5. Top-level shape exactly:

   ```json
   {"type":"response.completed","response":{}}
   ```

6. No harness-specific `turn_id`, sequence, checksum, or staging fields in the final record.
7. Preserve the API response fields, including fields unknown to the current TypeScript version.
8. Preserve `response.output` array order.
9. Omit no meaningful API field and do not serialize JavaScript `undefined` values as custom placeholders.
10. Use a deterministic serializer. Fixed object construction order is sufficient for one runtime; a canonical JSON algorithm is preferable if multiple runtimes will produce cache keys.

This provides structural API compatibility. If byte-for-byte equality with the original provider payload is required, the raw provider event must also be captured. Re-serializing an SDK object cannot guarantee the provider's original whitespace, property order, or number representation.

## 8. Failure and recovery rules

### Process failure during an item

Previously staged items remain available. The current incomplete item is discarded and must be rebuilt when the stream/request is restarted.

### Failure during an item staging write

The last incomplete `items.jsonl` line is truncated to its previous newline. The item is retried; the manifest is not advanced until the complete line is durable.

### Failure after staging but before `response.completed`

The response is not committed. The staging directory remains for recovery or diagnostics.

### Failure while appending the final message

On startup:

1. truncate any incomplete trailing `messages.jsonl` line;
2. inspect the unfinished staging manifest;
3. compare the response ID and canonical-line hash with the last complete message record;
4. mark the transaction committed if the final line already exists;
5. otherwise retry the append.

This prevents duplicate commits after a crash between the messages flush and staging cleanup.

### Missing `response.completed`

Do not write a synthetic `response.completed` record. Retain the staged items and mark the turn incomplete. A later recovery operation may retrieve the response by ID if supported or restart the request.

### Remote stream failure

The local staging protocol can preserve completed items, but the Responses API stream should not be assumed to support arbitrary item-level resume. If the provider connection is lost, the harness may need to restart the entire request. It must not splice items from a new response into an old response unless response identity and output equivalence are explicitly proven.

## 9. Durability and performance

The writer should support a configurable durability mode:

- **process:** await writes, optimized for process failures but not sudden power loss;
- **turn:** flush staged data and `messages.jsonl` at turn boundaries;
- **strict:** flush each completed staged item and the final committed line.

Recommended initial behavior:

- one sequential writer per session;
- await backpressure from file writes;
- flush each completed item in strict/recovery-sensitive mode;
- always flush the final `messages.jsonl` line before deleting staging;
- never use synchronous filesystem calls on the stream event loop.

A single `items.jsonl` per turn is preferred over one file per item because it reduces inode operations. The trade-off is that recovery must truncate an incomplete trailing line.

## 10. TypeScript boundaries

The implementation should keep transport, staging, and canonical persistence types separate:

```ts
interface StagedOutputItemRecord {
  output_index: number;
  item: Record<string, unknown>;
}

type ResponseInputItem = Record<string, unknown>;

interface TurnManifest {
  schema_version: 1;
  turn_id: string;
  response_id?: string;
  status: "streaming" | "ready" | "committing" | "committed" | "failed";
  item_count: number;
  next_sequence: number;
  canonical_line_sha256?: string;
}

interface CompletedResponseEvent {
  type: "response.completed";
  response: Record<string, unknown> & {
    output: Array<Record<string, unknown>>;
  };
}
```

The public stream adapter also exposes:

```ts
persistResponseStream(store, stream, { turnId });
persistOpenAIResponse(client, store, streamingRequest, { turnId });
```

The official SDK dependency is limited to the adapter; the file format and staging implementation do not import SDK response classes.

The final event type should remain extensible. Unknown API fields must survive persistence so that adding a newer Responses API field does not require a migration before it can be stored.

## 11. Validation and tests

The first implementation should test at least:

- a response with one message output item;
- multiple output items with nontrivial indexes;
- message, function-call, and reasoning output item shapes;
- interleaved stream deltas;
- a large response whose total size exceeds the intended memory budget;
- process failure before and after each staging write;
- a partial trailing staging line;
- a partial trailing `messages.jsonl` line;
- crash after final message append but before staging deletion;
- duplicate response ID recovery;
- missing or duplicate output indexes;
- `response.completed` without staged items;
- unknown fields in the response object;
- validation that `messages.jsonl` contains only complete canonical records.

## 12. Deliberate scope boundaries

This design standardizes assistant response completion records, raw user/tool input items, and the initial native `read` tool. It does not yet implement MCP transport, destructive native tools, or automatic retry strategy.

The design also assumes that cache compatibility means stable structural/serialized response data. Provider prompt-cache hit rate additionally depends on stable input history, model settings, tools, and other request fields; storing the response event alone cannot guarantee a provider cache hit.

## 13. Acceptance criteria

The design is complete when:

- every successful Responses API turn produces exactly one canonical `response.completed` line;
- no intermediate streaming event is committed to `messages.jsonl`;
- completed output items are staged before their memory is released;
- recovery never exposes a partial JSON record as a valid message;
- final response serialization does not require retaining the full response in application memory;
- staging can be safely replayed or discarded without corrupting session history;
- the final record remains compatible with the Responses API event shape.
