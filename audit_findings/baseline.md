# Baseline (audit_plan.md section 2)

HEAD at run: ab86bef (plan table commit) on top of 4fb4d51 code; run from `backend-go/`, one command at a time, no reviewer running, no other build/test started by the coordinator. Date: 2026-10-27 (machine clock).
go version: go1.27.3 linux/amd64. Linters: `staticcheck` NOT installed, `golangci-lint` NOT installed (`go vet` only).

## Commands and exit codes

| Command | Exit | Wall | Output |
|---|---|---|---|
| `go build ./cmd/... ./internal/...` | 0 | 30 s (cold cache) | empty |
| `go vet ./cmd/... ./internal/...` | 0 | 3 s | empty |
| `gofmt -l cmd internal` | 0 | 1 s | empty (no unformatted files) |
| `go test -count=1 ./internal/... ./cmd/...` | 0 | 103 s | 34 ok, 3 no test files, 0 FAIL |
| `go test -race -count=1 ./internal/... ./cmd/...` | 0 | 161 s | 34 ok, 3 no test files, 0 FAIL |
| `git diff --check` | 0 | - | empty |

No-test-file packages: internal/responses, internal/serverconfig, internal/sessionfs/layouttest.

## Load average (uptime)
- before everything: 0.53 0.68 0.57
- after build/vet/gofmt: 8.18 2.82 1.31 (build spike)
- before test: 7.52 2.77 1.30; after test: 2.58 2.86 1.53
- before race: 2.37 2.82 1.52; after race: 1.85 3.10 1.89
- Machine otherwise near idle (12 CPUs); numbers are context, not benchmark-grade.

## Failures and flaky tests
- Failing tests: none, in either run. No log excerpt needed.
- Known flaky tests (`TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks`, `TestRemoteSubagentCloseLeavesNoGoroutines`, `TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher`): 0 failures in 2 observations each (plain and -race, one run each) = 0/2. A rate cannot be estimated from 2 runs; absence of failure is not proof the flakiness is gone. Fixed state at 176fd7d / 4fb4d51 is consistent with it.

## Per-package results (plain run / -race run, time as printed by go test)

| Package | go test | go test -race |
|---|---|---|
| cmd/anthropic-adapter | ok 0.005s | ok 1.012s |
| cmd/claude-auth | ok 0.082s | ok 1.104s |
| cmd/jad | ok 0.019s | ok 1.083s |
| cmd/openai-auth | ok 0.122s | ok 1.161s |
| internal/anthropicadapter | ok 0.500s | ok 1.301s |
| internal/app | ok 40.504s | ok 61.940s |
| internal/asyncmgr | ok 1.140s | ok 2.786s |
| internal/catalog | ok 0.010s | ok 1.047s |
| internal/checkpoint | ok 2.695s | ok 3.644s |
| internal/claudeauth | ok 0.120s | ok 1.193s |
| internal/contentlibrary | ok 0.009s | ok 1.067s |
| internal/httpapi | ok 14.140s | ok 43.693s |
| internal/httpdebug | ok 0.023s | ok 1.060s |
| internal/imagedata | ok 0.020s | ok 1.061s |
| internal/imageresize | ok 1.266s | ok 12.634s |
| internal/imagestore | ok 0.088s | ok 1.298s |
| internal/interactions | ok 0.013s | ok 1.071s |
| internal/logging | ok 0.007s | ok 1.011s |
| internal/memorytrace | ok 0.005s | ok 1.012s |
| internal/openaiapi | ok 1.547s | ok 2.592s |
| internal/openaiauth | ok 0.662s | ok 1.742s |
| internal/openaioauth | ok 0.009s | ok 1.023s |
| internal/remote | ok 2.339s | ok 3.360s |
| internal/remoteapi | ok 1.076s | ok 2.218s |
| internal/rootsetting | ok 0.003s | ok 1.012s |
| internal/session | ok 0.907s | ok 3.664s |
| internal/sessionfs | ok 1.024s | ok 4.968s |
| internal/sessionlock | ok 0.259s | ok 2.285s |
| internal/tools | ok 0.009s | ok 1.050s |
| internal/tools/extension | ok 0.193s | ok 1.353s |
| internal/tools/mcp | ok 2.527s | ok 25.137s |
| internal/tools/native | ok 92.636s | ok 99.518s |
| internal/transcript | ok 0.070s | ok 1.406s |
| internal/webui | ok 1.646s | ok 12.539s |

## Line counts (section 3.1 command)
37 directories with Go files; non-test 79,620 lines, test 96,120 lines (matches plan section 1). Per-directory table:

| dir | non-test | test |
|---|---|---|
| cmd/anthropic-adapter | 77 | 33 |
| cmd/claude-auth | 306 | 293 |
| cmd/jad | 139 | 73 |
| cmd/openai-auth | 329 | 325 |
| internal/anthropicadapter | 3233 | 2951 |
| internal/app | 6933 | 21973 |
| internal/asyncmgr | 1962 | 1734 |
| internal/catalog | 426 | 286 |
| internal/checkpoint | 1135 | 923 |
| internal/claudeauth | 1234 | 1024 |
| internal/contentlibrary | 909 | 274 |
| internal/httpapi | 5656 | 7845 |
| internal/httpdebug | 475 | 400 |
| internal/imagedata | 72 | 111 |
| internal/imageresize | 558 | 688 |
| internal/imagestore | 638 | 677 |
| internal/interactions | 362 | 256 |
| internal/logging | 102 | 176 |
| internal/memorytrace | 340 | 96 |
| internal/openaiapi | 538 | 1059 |
| internal/openaiauth | 1226 | 1406 |
| internal/openaioauth | 229 | 303 |
| internal/remote | 1147 | 824 |
| internal/remoteapi | 1002 | 899 |
| internal/responses | 62 | 0 |
| internal/rootsetting | 45 | 82 |
| internal/serverconfig | 92 | 0 |
| internal/session | 1692 | 3240 |
| internal/sessionfs | 12781 | 11587 |
| internal/sessionfs/layouttest | 355 | 0 |
| internal/sessionlock | 224 | 330 |
| internal/tools | 1435 | 731 |
| internal/tools/extension | 886 | 719 |
| internal/tools/mcp | 1575 | 1652 |
| internal/tools/native | 24486 | 25782 |
| internal/transcript | 5119 | 5742 |
| internal/webui | 1840 | 1626 |
