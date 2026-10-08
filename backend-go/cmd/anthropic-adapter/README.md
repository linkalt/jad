# anthropic-adapter

Serves the OpenAI **Responses API** (`POST /v1/responses`, streaming and not) on top of the **Anthropic Messages API**. A session uses a Claude model by pointing its `model.json` `base_url` at the adapter. Neither backend (TypeScript or Go) changes.

```
backend (TS or Go)  --Responses-->  anthropic-adapter  --Messages-->  api.anthropic.com
```

## Run it

```sh
cd backend-go
go build -o /tmp/anthropic-adapter ./cmd/anthropic-adapter
/tmp/anthropic-adapter --config ../sample/anthropic-adapter.json
```

Without `--config` the defaults apply: listen on `127.0.0.1:8788`, upstream `https://api.anthropic.com`, no thinking parameter, no caching.

Point a session at it, in that session's `model.json`:

```json
{
  "schema_version": 1,
  "provider": "anthropic",
  "model": "claude-opus-5-5",
  "api_key_env": "ANTHROPIC_API_KEY",
  "base_url": "http://127.0.0.1:8788/v1"
}
```

The adapter holds no credential. The backend sends the token from `api_key_env` as `Authorization: Bearer ...` (an `x-api-key` header from the caller is accepted too) and the adapter forwards it upstream as `Authorization: Bearer ...`, the form Anthropic documents. `upstream.auth_header: "x-api-key"` sends the legacy header instead, for a proxy that expects it. A request with neither header gets 401 and never reaches Anthropic.

**Credentials.** By default the adapter passes the token through and does not know what kind it is, so an API key (`sk-ant-api...`) and a short-lived Workload Identity Federation access token both work. Getting and refreshing a WIF token is a job for a separate tool: put the current token in the environment variable named by `api_key_env`. With `upstream.credential_file` set, it reads a credential file instead (next section).

**Claude subscription (Pro, Max, Team, Enterprise) sign-in: `upstream.credential_file`.** Unofficial, off by default. Set `upstream.credential_file` to a file written by `claude-auth` (`cmd/claude-auth/README.md`). The adapter then reads the access token from it on every request, so a refresh by `claude-auth` needs no restart, sends it upstream in the header chosen by `auth_header`, and ignores the credential the caller sent (the backend still needs some `api_key_env` value, which is never forwarded). The adapter never writes the file. An unusable file (missing, no token, no valid expiry, or a token whose stored expiry is within `upstream.expiry_margin_ms`) answers 401 `credential_unusable` with a message that names the file and the reason, and never reaches Anthropic.

Anthropic's Claude Code legal page (code.claude.com/docs/en/legal-and-compliance, read 2026-10-08) limits subscription credentials to ordinary use of Claude Code and other native Anthropic applications, and says developers may not collect, store or intermediate Claude.ai credentials. The Messages API authentication page documents API keys, Workload Identity Federation and App Attest only. Whether your own account in your own tool is allowed is not settled by those pages; the risk to the account is the account owner's. **Nothing here has been sent to Anthropic with a subscription token.** Two things may be needed that this adapter does not do and that no one has tried: the headers Claude Code sends and a Claude Code identity in the system prompt (the pi agent sends `anthropic-beta: claude-code-20250219,oauth-2025-04-20`, `x-app: cli`, a `claude-cli/…` user agent, and a first system block reading "You are Claude Code, Anthropic's official CLI for Claude."). `upstream.beta` and `upstream.extra_headers` can carry the headers; the system block and tool-name changes are not built. Add them only if a response says they are required.

## Configuration

Schema: `sample/schemes/anthropic-adapter.schema.json`. A working example: `sample/anthropic-adapter.json`. Unknown keys are rejected, so a misspelled key cannot fall back to a default silently. Every key below is optional.

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8788` | Address to listen on. |
| `upstream.base_url` | `https://api.anthropic.com` | `/v1/messages` is appended. |
| `upstream.anthropic_version` | `2023-06-01` | `anthropic-version` header. |
| `upstream.auth_header` | `bearer` | `bearer` sends `Authorization: Bearer <token>`; `x-api-key` sends the legacy header. |
| `upstream.beta` | `[]` | Joined into `anthropic-beta`. |
| `upstream.credential_file` | `""` | A `claude-auth` credential file (absolute or `~/…`). When set, its access token is sent upstream and the caller's credential is ignored. Empty forwards the caller's credential. |
| `upstream.expiry_margin_ms` | `300000` | With `credential_file`: a token whose stored expiry is this close is refused with 401 before the request. |
| `upstream.extra_headers` | `{}` | Headers added to every upstream request. `authorization`, `x-api-key`, `anthropic-version`, `anthropic-beta`, `content-type`, `content-length` and `host` are refused here: they come from other keys. |
| `upstream.request_timeout_ms` | `600000` | Deadline of one upstream request, the whole stream included. |
| `upstream.stream_retry.max_retries` | `0` | Retries of a stream whose first event is an error. `0` is off. |
| `upstream.stream_retry.delay_ms` | `1000` | Wait before each retry. |
| `upstream.stream_retry.error_types` | `overloaded_error`, `api_error` | Error types that are retried. |
| `default_max_tokens` | `8192` | Sent when the request has no `max_output_tokens` (Anthropic requires it). |
| `max_request_bytes` | `67108864` | Largest accepted body. |
| `log.upstream_errors` | `true` | One line on stderr per failed upstream call: an HTTP error status, an error event in a stream, a non-stream body for a stream request, or an error body behind HTTP 200. Success is not logged. |
| `log.body_bytes` | `2048` | How much of the response body (or error message) is logged, on one line. `0` logs none. |
| `log.headers` | `request-id`, `retry-after`, `x-should-retry`, `anthropic-ratelimit-*`, `content-type`, `server`, `cf-ray` | Response header names to log; matched without regard to case; a trailing `*` is a prefix. The line also lists the **names** of the request headers the adapter sent (`sent_header_names`), never their values, so the credential cannot reach the log. |
| `strip_schema_keywords` | `oneOf`, `allOf`, `anyOf` | Removed from the **top level** of every tool's `input_schema`, because Anthropic answers 400 `input_schema does not support oneOf, allOf, or anyOf at the top level`. The harness's `fetch`, `task`, `memory` and `process` tools use them. Nested schemas are not touched, and a schema with none of them is sent byte for byte. The caller still validates each call against its full schema; the model no longer sees the "one of these fields is required" rule. Each removal is listed in `X-Adapter-Ignored-Fields` as `tools.<name>.<keyword>`. `[]` sends schemas unchanged. |
| `unsupported_fields` | `ignore` | `ignore` drops a field with no Anthropic equivalent and lists it in the `X-Adapter-Ignored-Fields` response header; `reject` answers 400. |
| `synthesize_missing_tool_results` | `true` | Answers a `function_call` that has no output with an error `tool_result`. |
| `cache.automatic` / `tools` / `system` / `history` | all `false` | Prompt-caching breakpoints, see below. |
| `cache.ttl` | `""` | `""` (5 minutes) or `"1h"`. |
| `default_profile`, `models[]` | thinking `none` | Per-model rules, see below. |

### Model profiles

Which thinking parameters a Claude model accepts differs per model, and changes with releases, so it is data. `models` is a list; the **first** entry whose `match` regular expression fits the request's `model` wins, otherwise `default_profile` applies. Put the more specific pattern first.

| Key | Meaning |
| --- | --- |
| `thinking` | `none` sends no thinking parameter. `adaptive` sends `thinking: {type: adaptive}` and `output_config.effort`. `budget` sends `thinking: {type: enabled, budget_tokens}` for models that still need it. |
| `display` | `thinking.display` to send: `summarized`, `omitted` or empty. Many current models default to `omitted`, which returns the signature without any text. |
| `default_effort` | Used when the request has no `reasoning.effort`. `none` turns thinking off. |
| `effort_map` | Responses effort to `output_config.effort`. Missing keys use the defaults. |
| `effort_budget` | Responses effort to `budget_tokens` in `budget` mode. Missing keys use the defaults. |
| `mid_conversation_system` | Send later system and developer messages as `role: system` instead of folding them into a user message. |

`sample/anthropic-adapter.json` encodes the per-model table as read from the Anthropic documentation on the day it was written. A test checks that it loads, validates against the schema, and gives each family the profile it names, but **the table itself has not been checked against the live API**. Check it against the current documentation before relying on it.

### Prompt caching

All off by default. A cache write costs 1.25x (5 minutes) or 2x (1 hour) the input price, so caching pays only when the same prefix is reused within the TTL. Anthropic allows four breakpoints per request; the four switches use at most four.

- `automatic` sends the top-level `cache_control`, which marks the last cacheable block of the request.
- `tools` marks the last tool definition. `system` marks the last system block, and the cached prefix then covers the tools too.
- `history` marks the last block that comes from a real conversation item, **skipping system or developer text that follows it**. The agent backends append changing state context at the end of every request. `automatic` would put its breakpoint on that text, which differs on every request, and never hit. Use `history` for these backends.

## What is translated

- **Input.** User, assistant, system and developer messages. `input_text`, `input_image` (data URL or `https` URL) and `input_file` (PDF or text as `file_data`, or `file_url`). `function_call` becomes `tool_use` and `function_call_output` becomes `tool_result`. Every result goes into the one user message right after its call, results first. Leading system text goes to the top-level `system`.
- **Reasoning.** Anthropic's `thinking` and `redacted_thinking` blocks, signature included, travel JSON-encoded in the Responses `reasoning` item's `encrypted_content`, and are decoded and sent back unchanged on the next request. Anthropic requires this when tools are used. A reasoning item the adapter did not produce, or a block without a signature, is dropped.
- **Tools.** `function` tools, `tool_choice` (`auto`, `required`, `none`, a named function) and `parallel_tool_calls: false`.
- **Output.** Text, function calls and reasoning, as streamed events (`response.output_item.added`, `output_text.delta`, `function_call_arguments.delta`, `reasoning_text.delta`, ..., `response.completed`) or as one response object. The terminal event carries the full `output` list, which the TypeScript backend rebuilds from.
- **Stop reasons.** `end_turn`, `tool_use` and `stop_sequence` complete. `max_tokens` and `model_context_window_exceeded` give `response.incomplete` (`max_output_tokens`), `refusal` gives `incomplete` (`content_filter`).
- **Usage.** Cache read and creation tokens are added into `input_tokens`, as the Responses API counts them, and reported in `input_tokens_details`. Reasoning tokens are not reported separately: Anthropic does not split them out.
- **Errors.** An HTTP error from Anthropic is returned with its status and `Retry-After`, before any stream byte, so the backend's own `provider_retry` handles it. An `error` event inside a stream, or a stream that ends before `message_stop`, becomes `response.failed` with the Anthropic error type as `code`. A half-built item is never closed.

## What is not supported

| Request | Answer |
| --- | --- |
| `previous_response_id`, `conversation`, `background: true` | 501. The adapter keeps no state; send the full `input`. |
| `GET /v1/responses/{id}` and other stored-response routes | 501. |
| Hosted tools (`file_search`, `web_search`, `code_interpreter`, `computer`, ...) | 400. They have no equivalent. |
| `file_id` images or files | 400. Send `image_url`, `file_data` or `file_url`. |
| Other content (audio, files of other types) | 400. |
| `text.format` other than text (structured output), `top_logprobs`, `logprobs`, ... | Dropped and reported in `X-Adapter-Ignored-Fields`, or 400 with `unsupported_fields: reject`. |
| `store`, `include`, `metadata`, `service_tier`, `prompt_cache_key`, `truncation`, ... | Dropped silently; they do not change the generated text. |

`pause_turn` (a server-tool loop that hit its limit) maps to `incomplete` and is **not** continued. It cannot occur here today, because the adapter forwards only client `function` tools and Anthropic returns `pause_turn` only for server tools.

## Not verified

- **Nothing has run against the real Anthropic API.** The tests use a fake upstream written from the Anthropic documentation. A live key could show differences in field shapes, the model table, or error behavior.
- Both agent backends were run through the adapter against that fake, for a tool-call turn with reasoning and an error event. No model answered.
- Nothing was run on Windows.
