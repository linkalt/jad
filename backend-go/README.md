# Go backend

## Build native.fetch

The `native.fetch` HTML converter uses Xberg's cgo-backed Go binding and its platform-specific Rust FFI library. Use Go 1.26 or newer, with cgo enabled and a C toolchain installed.

From this directory, run the setup generator on each build host before building or testing for that host platform:

```sh
go generate ./internal/tools/native
```

This downloads and verifies the host-platform native library in the user cache and writes a machine-local cgo link shim. The shim (and the Windows DLL, if generated) is ignored by Git. Re-run the generator after upgrading the Xberg package. The generator detects the host platform; cross-compilation requires provisioning the matching target library and cgo link flags separately, and deployments must include the native runtime library.

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

`jad run` runs one turn of a session without a server and exits, for scripts and CI. It reads the same root setting as the server (`JAD_ROOT_SETTING_PATH`), needs no `JAD_API_TOKEN`, and goes through the same run route in-process, so the transcript and locks are the server's.

```sh
jad run --session local:demo --profile dev/coder "Fix the failing test in sum.py"
echo "Summarize CHANGELOG.md" | jad run --session demo --format json -
jad run --session demo --format jsonl --timeout 10m "..." > events.jsonl
```

`--format text` prints the final answer; `json` one result object; `jsonl` every run-stream event, then `{"event":"result",...}`. Exit status: 0 completed, 1 failed or refused, 2 usage error, 124 `--timeout` reached, 130 interrupted. Logs go to `server.log_file` or stderr (`--log-level` overrides `server.log_level`).

## Evals: `jad eval`

`jad eval suite.json` runs a suite of tasks (format: `sample/schemes/eval-suite.schema.json`, example: `sample/evals/smoke/suite.json`). Each attempt gets a fresh copy of the case's workspace and its own session (category `eval`), one headless turn, then the case's `checks` (shell commands that must exit 0) and `expect_text` patterns. The JSON report goes to stdout or `--out`; progress and a summary go to stderr; the exit status is 0 only when every attempt passed.

```sh
jad eval --out report.json ../sample/evals/smoke/suite.json
jad eval --cases '^fix-' --repeat 5 --parallel 2 suite.json
```

The case sessions are kept, so each transcript can be opened in the web UI. Keep `parallel` at or below `server.max_concurrent_runs`.

## Tracing

Set `server.tracing.enabled` in the root setting to export OpenTelemetry spans (one per run, model request and tool call, with GenAI attributes and token usage, never prompt or output text) over OTLP/HTTP with the JSON encoding to `server.tracing.endpoint` (`<endpoint>/v1/traces`), or to `OTEL_EXPORTER_OTLP_ENDPOINT`. Collector headers come from `OTEL_EXPORTER_OTLP_HEADERS`. See `DECISIONS.md` for the span shape.

```json
{"server": {"tracing": {"enabled": true, "endpoint": "http://127.0.0.1:4318"}}}
```

## ChatGPT sign-in: `openai-auth`

`cmd/openai-auth` signs in to a ChatGPT account, refreshes its tokens and reports the credential file that a session with `auth_mode: "openai_oauth"` reads. The backend never writes that file. See `cmd/openai-auth/README.md`.

## Claude models: `anthropic-adapter`

`cmd/anthropic-adapter` serves the Responses API on top of the Anthropic Messages API, so a session can use a Claude model by changing `base_url` in `model.json`. See `cmd/anthropic-adapter/README.md`.

## Claude subscription sign-in: `claude-auth`

`cmd/claude-auth` signs in to a Claude account (Pro, Max, Team or Enterprise), refreshes its tokens and reports the credential file that the adapter reads when `upstream.credential_file` is set. Unofficial and untested against Anthropic; read its README before using it. See `cmd/claude-auth/README.md`.
