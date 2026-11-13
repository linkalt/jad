# Go backend

## Build

The backend builds with plain `go build ./cmd/jad`: no cgo, no generator step. (Until native.fetch was removed it needed `go generate ./internal/tools/native` for Xberg's HTML converter; a leftover `internal/tools/native/html-to-markdown-rs_cgo_link.go` from that step now breaks the build and can be deleted.)

## Performance regressions

Timings differ between machines and runs, so none is stored in a test. Two checks split the work:

- **Allocations, in `go test`.** `TestSessionListAllocationsPerProfile` fails when a session list allocates much more per extra profile, as it would if a 304 loaded each profile again. Allocation counts are stable, so a failure is a real change in the work done.
- **Timings, by hand, on one machine.** Run the benchmark on the old code and on the new code and compare with `benchstat`:

```sh
cd backend-go
go test ./internal/app -run '^$' -bench ListSessionsProfiles -benchtime 50x -count=10 > /tmp/old.txt
# switch to the code under test, then:
go test ./internal/app -run '^$' -bench ListSessionsProfiles -benchtime 50x -count=10 > /tmp/new.txt
(cd /tmp && go run golang.org/x/perf/cmd/benchstat@latest old.txt new.txt)
```

Treat a difference under about 5% as noise: two runs of the same code showed +4% at p=0.009 for the 304 path. Do not commit the result files.

## Headless runs: `jad run`

`jad run` runs one turn of a session without a server and exits, for scripts and CI. It starts in a workspace (the current directory must have `.jad/workspace.json`; `JAD_GLOBAL_DIR` names the global folder, default `~/.jad`) and reads the same settings as the server, needs no `JAD_API_TOKEN`, and goes through the same run route in-process, so the transcript and locks are the server's.

```sh
jad run --session local:demo --profile dev/coder "Fix the failing test in sum.py"
echo "Summarize CHANGELOG.md" | jad run --session demo --format json -
jad run --session demo --format jsonl --timeout 10m "..." > events.jsonl
```

`--format text` prints the final answer; `json` one result object; `jsonl` every run-stream event, then `{"event":"result",...}`. Exit status: 0 completed, 1 failed or refused, 2 usage error, 124 `--timeout` reached, 130 interrupted. Logs go to `server.log_file` or stderr (`--log-level` overrides `server.log_level`).

## Evals: `jad eval`

`jad eval suite.json` runs a suite of tasks (format: `sample/schemes/eval-suite.schema.json`, example: `sample/evals/smoke/suite.json`). Each attempt gets a fresh copy of the case's workspace and its own session, one headless turn, then the case's `checks` (shell commands that must exit 0) and `expect_text` patterns. The JSON report goes to stdout or `--out`; progress and a summary go to stderr; the exit status is 0 only when every attempt passed.

```sh
jad eval --out report.json ../sample/evals/smoke/suite.json
jad eval --cases '^fix-' --repeat 5 --parallel 2 suite.json
```

The case sessions are kept, so each transcript can be opened in the web UI. Keep `parallel` at or below `server.max_concurrent_runs`.

## Tracing

Set `server.tracing.enabled` in `global.json` or `workspace.json` to export OpenTelemetry spans (one per run, model request and tool call, with GenAI attributes and token usage, never prompt or output text) over OTLP/HTTP with the JSON encoding to `server.tracing.endpoint` (`<endpoint>/v1/traces`), or to `OTEL_EXPORTER_OTLP_ENDPOINT`. Collector headers come from `OTEL_EXPORTER_OTLP_HEADERS`. See `DECISIONS.md` for the span shape.

```json
{"server": {"tracing": {"enabled": true, "endpoint": "http://127.0.0.1:4318"}}}
```

## Subscription sign-in was removed

Signing in with a ChatGPT or Claude subscription is not supported: the `openai-auth` and `claude-auth` commands, `auth_mode: "openai_oauth"`, `oauth_auth_file`, the `openai_oauth` setting and `anthropic.upstream.credential_file` were removed. A model file or setting that still has one of them fails to load with an error that says so; nothing falls back to an API key. Use an API key in the environment variable named by `api_key_env`. A credential file an earlier sign-in wrote (`~/.jad/auth/`) is not read or deleted. MCP server sign-in (`jad mcp login`) is separate and unchanged. See `DECISIONS.md`.

## Other provider protocols: `api` in `model.json`

jad speaks the Responses API. A model whose provider speaks another protocol names it in `api`, and jad translates in its own process: no adapter to start, no port, no second credential hop. The translation sits behind the ordinary Responses client (`internal/inprocess` answers its requests with the translator's handler), so `provider_retry`, the idle timeout, HTTP debug logging (of the real upstream call), tracing and `messages.jsonl` work as for any model. Neither protocol can run `hosted_tools`.

**Claude: `"api": "anthropic_messages"`.** The Anthropic translator (`internal/anthropicadapter`), run in process. `base_url` is the Anthropic API root (default `https://api.anthropic.com`), `api_key_env` defaults to `ANTHROPIC_API_KEY`, and the optional `anthropic` object takes the adapter's own config keys (thinking and effort per model, prompt caching, `stream_retry`, `extra_body`, `log`, ...), except `listen`, `caller_token_env` and `upstream.base_url`. Example: `sample/jad/models/cheap/haiku.json`. The adapter's behaviour and limits are in `internal/anthropicadapter/README.md`. The standalone `anthropic-adapter` command was removed (see `DECISIONS.md`).

```json
{"schema_version": 1, "api": "anthropic_messages", "model": "claude-sonnet-5-5",
 "request": {"max_output_tokens": 16384, "reasoning": {"effort": "high"}},
 "anthropic": {"cache": {"system": true, "history": true},
               "default_profile": {"thinking": "adaptive", "display": "summarized", "fixed_sampling": true}}}
```

**OpenAI-compatible servers: `"api": "chat_completions"`.** `internal/chatadapter` calls `{base_url}/chat/completions` (vLLM, Ollama, llama.cpp, LM Studio, DeepSeek, Groq, gateways) and streams the answer back as Responses events. Function tools, tool rounds, images and files in user messages, reasoning text (`reasoning_content` or `reasoning` deltas become a reasoning item), structured output (`text.format`) and usage are translated. The optional `chat_completions` object sets `max_tokens_field`, `include_usage`, `system_role`, `reasoning_fields`, `echo_reasoning_field`, `missing_tool_result_text`, `request_timeout_ms`, `max_event_bytes`, `max_request_bytes`, `extra_headers` and `extra_body`; the schema has their defaults. Example: `sample/model-chat-completions.json` (Ollama).
