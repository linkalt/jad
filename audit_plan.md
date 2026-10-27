# Audit plan: backend-go code quality and performance

Status: plan only, no review done. Prepared at HEAD `4fb4d51`, go1.27.3 linux/amd64, 12 CPUs. Line counts in section 3 were measured by the command in section 3.1.
Reviewers never change code. The user approves every fix (section 6).

## 1. Scope, goals, "done"

- **In scope:** all 37 packages under `backend-go/cmd` (4) and `backend-go/internal` (33), 79.6k non-test lines (377 files), 96120 test lines (measured by the section 3.4 check and 3.1 command).
- **Out of scope:** `frontend/`, `frontend_new/`, generated files (for example `html-to-markdown-rs_cgo_link.go`), `testdata/`, `sample/` content except where a setting is defined. Tests are review targets only for test quality (lens 4), and as evidence (lens 1 to 3).
- **Goals:** find defects (lens 1, 2), measured cost (lens 3) and change-resistance problems (lens 4), rank them, and give the user a bounded fix backlog.
- **Done** = for each of the 8 phases (and sub-passes) all 4 lens files exist, every finding meets the evidence rule (section 5), the merge file is written (section 6), and the user has approved, rejected or deferred every finding. Fixes are a later stage, not part of "done" for the audit.

### 1.1 Prior material (read first; do not re-report)

| File | What it holds | State |
|---|---|---|
| `audit_backend.md` | F-01..F-29, Track P (performance) and Track C (design); benchmark baseline table, Fixes table | Fixed in batches up to 13 (commits in its Fixes table and Log). Its Findings status column is partly stale ("open" next to a fixed commit): trust the Fixes table and `git log`. F-24 closed with no code change. |
| `audit_design.md` | A-1..A-18 (backend and frontend) | Superseded in parts by `audit_audit.md`. |
| `audit_design0.md`, `audit_design1.md` | D-1..D-11, B-*, F-* | Verdicts in `audit_audit.md`. |
| `audit_audit.md` | ACCEPT / CORRECT / REJECT verdicts on the two design audits; Batches 8 and 9 outcomes | Accepted items fixed: panic recovery (D-1), session delete (D-2), shutdown deadline (D-3), http_debug (D-5), content-cache cap (D-6 backend half), fetch SSRF gap, ripgrep and glob timing keys. REJECTED, do not re-raise without new evidence: B-C2 (25 ms lock poll), B-C4, B-C7, D-8, F-S1, F-S3, X-1, X-2. |
| `audit_performance_plan.md` | P-001..P-05x performance findings, batches 0-14, prior-art map (section 3) | Batches 0-13 done with commits (section 8 status table); batch 14 (P-026) skipped; P-024, P-029 skipped; P-005, P-007 deferred; P-011 no repo-side fix. Prior-art map lists A-4 (rest), A-5, A-6, A-8, B-C1, B-C5, B-C8, B-C9 (rest) as "still open" at its writing: re-check each against the current tree before reporting it. Its quiet-machine re-run and P-036 are still deferred. |
| `backend-go/DECISIONS.md` | 72 decision records, newest: remote calling, transcripts v1/v2, `provider_network`, async latest block | A behaviour recorded there is not a finding. A reviewer who disagrees with a decision says so as a "decision challenge" and does not file it as a defect. |

Rule for reviewers: before filing, grep these files for the area and `git log --oneline -- <file>` for recent fixes. A repeat of a fixed item is dropped. A repeat of an open item cites its old id ("re-confirmed A-6").

**Not yet independently reviewed** (must be reviewed inside their phase, section 3): remote work, `f0e8b78`, `decide_systemone`, `exec` alias and rework, transcript v1/v2, the `transcripts` tool, the async "latest" block, asyncmgr Close fix, toolset fix, `provider_network`, `tool_smith` listing.

## 2. Baseline (run once before any review; record in `audit_findings/baseline.md`)

Run on an otherwise idle machine, one command at a time, from `backend-go/`. Never `go test ./...`. Never restart or kill the running backend. Scratch in `/tmp`, removed afterwards.

```
go build ./cmd/... ./internal/...
go vet ./cmd/... ./internal/...
gofmt -l cmd internal
go test -count=1 ./internal/... ./cmd/...            > /tmp/base_test.log
go test -race -count=1 ./internal/... ./cmd/...      > /tmp/base_race.log   # long: use the process tool
git diff --check
```

Record: exit codes, per-package ok/FAIL and time, the section 3.1 line-count table, `go version`, and whether `staticcheck` or `golangci-lint` is installed (at plan time: neither is; do not install; `go vet` only).

**Known flaky tests** (record the observed rate; do not report as new findings):
- `TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks`, `TestRemoteSubagentCloseLeavesNoGoroutines` (tools/native): `.async: directory not empty` on temp-dir cleanup. Root cause was `asyncmgr.Manager.Close` not waiting for `Complete` writes; fixed in `176fd7d`. If it still appears, it is a finding (lens 1).
- `TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher` (httpapi): test timing assumption, fixed in `4fb4d51`.
- A failure of any other test in the baseline is itself a lens 1 finding with the log attached.

**Benchmarks for lens 3.** 100 benchmarks already exist (`grep -rhE '^func Benchmark' --include=*_test.go internal cmd`), in app 7, asyncmgr 1 file (3), httpapi 10 files, session 4, sessionfs 2, tools/native 15, transcript 11, others. Run the ones for the phase's packages with `-run '^$' -bench . -benchmem -count=10` and compare to numbers in `audit_backend.md` and `audit_performance_plan.md` (differences under 5% are noise; `backend-go/README.md:25-31`; `benchstat` only if present, else compare medians by hand). Run them alone, never beside another build or test.

Existing candidates by hot path:

| Hot path | Existing benchmark | Package |
|---|---|---|
| Session load, list | `BenchmarkListSessions`, `BenchmarkListSessionsProfiles`, `BenchmarkListManySessions`, `BenchmarkLoadSessionConfig` | app, sessionfs |
| History read and append | `BenchmarkJournalAppend`, `BenchmarkCommitLargeTranscript`, `BenchmarkReadInputItems1000Records`, `BenchmarkHistoryReadMemory`, `BenchmarkRunRoundsHistory`, `BenchmarkBuildRequestWithLargeHistory` | transcript, session |
| Transcript compaction | `BenchmarkCompactHistory1000Turns`, `BenchmarkFrontCompactRecoverable1000Turns` | transcript |
| httpapi streaming | `BenchmarkPublishMessage`, `BenchmarkPublishFanOut`, `BenchmarkSSEDeltaEventEncoding`, `BenchmarkRunRouteStreamingDeltas`, `BenchmarkLateSubscribe`, `BenchmarkStalledSubscriberMemory`, `BenchmarkLongRunHeap`, `BenchmarkFinishedRunRetention` | httpapi |
| asyncmgr | `BenchmarkOpenFromIndex`, `BenchmarkOpenRebuild`, `BenchmarkListPage10Of10kRows` | asyncmgr |
| Tool output sizing | `BenchmarkBashLargeOutputPeak`, `BenchmarkPlanBashSpillLargeDirectory`, `BenchmarkNativeReadLargeFile` | tools/native |
| Process and sub-agent managers | `BenchmarkProcessStartToExit`, `BenchmarkProcessListManyFinished`, `BenchmarkSubagentListManyFinished`, `BenchmarkPersistManifestParallel` | tools/native |
| Locks, images, MCP | `BenchmarkAcquireWaiters`, `BenchmarkOpenFromIndex`, `BenchmarkLoadToolsServers` | sessionlock, imagestore, tools/mcp |

Gaps lens 3 should fill with a new benchmark in `/tmp` or as a proposed test (not committed by the reviewer): `internal/catalog` Scan (no benchmark), the async latest builder (`tools/native/async_latest.go`), `transcript`/`transcripts` tool list and detail on a large session, `remote` client round trip against an `httptest` server, `anthropicadapter` stream translation, `webui` increments, `tools` catalog build per run. Profiles (`-cpuprofile`, `-memprofile`, `-mutexprofile`, `-blockprofile`) only on a benchmark that shows a cost.

## 3. Phases

### 3.1 Line counts (non-test / test), measured at `4fb4d51`

Command (counts `*.go` in the directory itself, not subdirectories):
```
cd backend-go; for d in $(find cmd internal -type d|sort); do n=$(ls $d/*.go 2>/dev/null|grep -v _test.go|xargs -r cat|wc -l); t=$(ls $d/*_test.go 2>/dev/null|xargs -r cat|wc -l); [ -n "$(ls $d/*.go 2>/dev/null)" ] && echo "$d $n $t"; done
```

### 3.2 Phase table

Each reviewer gets one phase (or sub-pass) and one lens. Package lists in the `phasemap` block are the single source of truth for the check in 3.4.

| Phase | Packages (non-test lines) | Total | Why this position |
|---|---|---|---|
| 1 Foundation | cmd/anthropic-adapter 77, cmd/claude-auth 306, cmd/jad 139, cmd/openai-auth 329, app 6933, rootsetting 45, serverconfig 92, logging 102, catalog 426, sessionlock 224 | 8,673 | Wiring, config load, shutdown. Everything else hangs off `app.Resolver`. |
| 2 State (sub-passes 2a, 2b) | 2a: sessionfs `native_*` settings 4,845 + sessionfs/layouttest 355; 2b: sessionfs rest 7,936 + session 1,692 | 14,828 | Settings and on-disk layout. Needed to judge validation and path handling in later phases. |
| 3 Server surface | httpapi 5656, webui 1840, httpdebug 475, interactions 362, memorytrace 340 | 8,673 | HTTP boundary, SSE, run registry. |
| 4 Async and remote | asyncmgr 1962, remote 1147, remoteapi 1002, checkpoint 1135 | 5,246 | Concurrency and security heavy; consumed by phase 6 tools. Most unreviewed code. |
| 5 Providers | anthropicadapter 3233, openaiapi 538, openaiauth 1226, claudeauth 1234, openaioauth 229, responses 62 | 6,522 | Network clients, tokens, OAuth callbacks. Independent of 3 and 4. |
| 6 Tools part 1 (sub-passes 6a, 6b, 6c) | 6a: tools 1435, tools/extension 886, tools/mcp 1575; 6b native command tools: exec, process, tool_smith, async, shell_async, child_groups, scratch_sweep (29 files, 5,545); 6c native agent tools: subagent*, profile_message*, statecontext_counts (19 files, 4,248) | 13,689 | Process lifetimes, approval, remote hops. After 4 (uses asyncmgr, remote). |
| 7 Tools part 2 (sub-passes 7a, 7b) | 7a file and search tools: bash*, path*, read*, write, edit*, mutate*, file_change, image_file, glob, grep, ripgrep, fetch*, html-to-markdown link (27 files, 6,835); 7b transcript tools and misc: transcript* (10 files, 2,516), memory*, task*, hooks*, skill, catalog, request_draft (12 files, 5,342) | 14,693 | Path confinement and SSRF in 7a; transcript tools read other sessions (7b). |
| 8 Transcripts and media | transcript 5119, imagestore 638, imageresize 558, imagedata 72, contentlibrary 909 | 7,296 | Journal and image storage. Last because phases 2, 3, 6 and 7 are its callers. |

Native file lists: the `phasemap` block uses globs; the check proves each of the 97 `tools/native` non-test files matches exactly one sub-pass. Sub-pass sizes (7a 6.8k, 7b 7.9k, 6b 5.5k, 6c 4.2k, 2a 5.2k, 2b 9.6k) are within the 6-8k guide except 2b; a reviewer may split 2b into `session*.go, roots.go` and the rest.

```phasemap
p1  cmd/anthropic-adapter cmd/claude-auth cmd/jad cmd/openai-auth internal/app internal/rootsetting internal/serverconfig internal/logging internal/catalog internal/sessionlock
p2a internal/sessionfs:native_*  internal/sessionfs/layouttest
p2b internal/sessionfs:!native_* internal/session
p3  internal/httpapi internal/webui internal/httpdebug internal/interactions internal/memorytrace
p4  internal/asyncmgr internal/remote internal/remoteapi internal/checkpoint
p5  internal/anthropicadapter internal/openaiapi internal/openaiauth internal/claudeauth internal/openaioauth internal/responses
p6a internal/tools internal/tools/extension internal/tools/mcp
p6b internal/tools/native:process* internal/tools/native:exec* internal/tools/native:tool_smith* internal/tools/native:async_* internal/tools/native:shell_async.go internal/tools/native:child_groups.go internal/tools/native:scratch_sweep.go
p6c internal/tools/native:subagent* internal/tools/native:profile_message* internal/tools/native:statecontext_counts.go
p7a internal/tools/native:bash* internal/tools/native:path* internal/tools/native:read* internal/tools/native:write.go internal/tools/native:edit* internal/tools/native:mutate* internal/tools/native:file_change.go internal/tools/native:image_file.go internal/tools/native:glob.go internal/tools/native:grep.go internal/tools/native:ripgrep.go internal/tools/native:fetch* internal/tools/native:html-to-markdown*
p7b internal/tools/native:transcript* internal/tools/native:memory* internal/tools/native:task* internal/tools/native:hooks* internal/tools/native:skill.go internal/tools/native:catalog.go internal/tools/native:request_draft.go
p8  internal/transcript internal/imagestore internal/imageresize internal/imagedata internal/contentlibrary
```

Syntax: `pkg` = all non-test `.go` files directly in the package; `pkg:glob` = those matching the glob; `pkg:!glob` = those not matching it. Assignment is per file, so `sessionfs` and `tools/native` are split without overlap. `tools/native/exec_templates` and `testdata` hold no Go files.

### 3.3 Unreviewed commits and known-risk areas, by phase

Commit ranges are inclusive. "Remote range" = `git log f001094^..HEAD --grep=remote` (design `f001094` through `338dfab`, 25 commits mention remote), which includes `f5a76d8..64e407b`, `be4af1d..a21104b`, `8028576`, `a57403b`.

| Phase | Unreviewed commits touching it | Known-risk areas |
|---|---|---|
| 1 | Remote range (app: `remote_listen.go`, `remote_api.go`, `remote_transcripts.go`, `service.go`, `umask_*.go`, `config.go`, `profile_message*.go`; cmd/jad `main.go`: `be4af1d..a21104b`, `8028576`, `a57403b`, `f5a76d8`); `f0e8b78` (`app/profile_message*.go`); `0bf03a1..35b9b66` provider_network (`app/provider_network.go`, `resolver.go`); `0c37f87..ad1018c` (`app/transcript_profiles.go`) | `Resolver.mu` and shared managers (B-C9 rest); shutdown order and deadline (D-3); unix socket permissions and umask; listen-address handling; env and token handling in cmd/*; session lock polling (rejected B-C2: do not re-raise). |
| 2 | Settings added by: remote (`remote_api_settings.go`, `native_subagent_settings.go`, `native_profile_message_settings.go`, `native_transcript(s)_settings.go`, `roots.go`, `layout_load.go`, `session.go`: `be4af1d..a21104b`, `a57403b`); `f0e8b78` (`check.go`); `a095d7e..b8a2967` (`extension_decide*_settings.go`, `tool_shape.go`); `2828a68..010e616`; `86552a4` (`native_async_settings.go`); `0bf03a1..35b9b66` (`provider_network_settings.go`) | Validation of every new key (bounds, defaults, unknown keys), `session.go` (1.3k) as a god file, 21 `Native*Settings` types (F-22), settings stamp coverage (P-001), path handling for session and profile names, atomic writes and fsync in layout files. |
| 3 | `be4af1d..a21104b` (`httpapi/executor.go`, `profile_run.go`, `registry.go`) | Run registry retention (B-C1), SSE queues and slow subscribers (fixed `4fb4d51` was test-only: check publisher), `registry.go` lock scope (P-0xx), CORS and token checks, body limits, http_debug redaction (D-5 fixed). |
| 4 | Remote range (`remote/*`, `remoteapi/*`: `f5a76d8..64e407b`, `be4af1d..a21104b`, `8028576`, `a57403b`); async latest (`86552a4`, `3e33c97`, `4eb3586`, `8563272`, `5230adf`: `asyncmgr/config.go`, `manager.go`); `176fd7d` (Close waits for Complete) | Lifetimes: Close vs `Complete`/`finishing`/`completing`; progress cursor state; per-token run cap; client: TLS, redirects, proxy, caps, retries, token scrubbing; server: auth, path/ID validation, error leakage (`a6150e3`); unix socket mode; checkpoint git subprocess handling. |
| 5 | none by SHA; `provider_network` (`0bf03a1`) changes how providers dial (code in app, phase 1): confirm adapters honour it | Token files and refresh races, OAuth callback binding, retries and idle guards (A-4 rest), stream parsing limits, secret logging, `anthropicadapter` non-loopback relay (A-17). |
| 6a | `a095d7e..b8a2967` (`extension/decide_systemone*.go`, `decide_backend.go`, `decide_image.go`); remote (`tools/catalog.go`) | MCP discovery per run under one mutex (A-6), panic recovery (D-1), tool name collisions, extension process spawn. |
| 6b | exec: `5371620` alias, exec rework (`4c5f289..623fbcb`, `c51f86c..9d8558c` templates and `fn.<name>`); tool_smith (`c4308e0..acb3d98` listing); async latest (`3e33c97`: `async_latest.go`, `async_tool.go`, `process*.go`, `shell_async.go`); `5230adf` | `async_latest.go` cursor and scrubbing; process env inheritance (A-2); orphan/kill logic and recycled pgid (B-C3); command construction and quoting for exec templates; env_file handling; `process_manager.go` (1.1k) and `tool_smith_bridge.go`. |
| 6c | `f0e8b78`, `91c92e6` (`subagent.go`); remote range (`subagent_remote.go`, `subagent_external.go`, `subagent_call.go`, `profile_message_remote.go`, `profile_message_async.go`, `async_host.go`: `be4af1d..a21104b`, `f5a76d8..64e407b`); `2828a68..010e616` (`subagent_manager.go`); `86552a4..8563272` (`subagent_async.go`) | Cross-session addressing `<session>/<category>/<profile>`; remote run lifetime and cancel; nested approval; manifest recovery; `subagent_manager.go` (914) and `subagent.go` (726) size. |
| 7a | async latest touches `bash.go`, `bash_output.go` (`3e33c97`) | Path confinement and symlinks (`path.go`, `mutate_path.go`), TOCTOU on edit/write, fetch SSRF (fixed gap in batch 9: verify), ripgrep/glob timeouts, whole-file reads (F-12), bash spill pruning (F-11). |
| 7b | Transcript v1 and v2 (`2b3e224`, `857b774`, `b94fd7f..c789a9f`: `transcript.go`, `transcript_steps.go`, `transcript_detail.go`, `transcript_cut.go`, `transcript_render.go`); `transcripts` (`2828a68..010e616`: `transcripts.go`, `transcripts_list.go`); remote transcripts (`1bf45fe`, `64e407b`, `a6150e3`: `transcript_remote.go`, `transcript_path.go`); `0fda7ef` and `91c92e6..` catalog registration | Reading another session's files: path parsing, cursor forgery, scope leaks, messages.jsonl reasoning exposure; memory store per-write scans (F-13); `memory_store.go` (1.1k). |
| 8 | none by SHA in `transcript`; `imagedata` changed by `a095d7e..b8a2967` | Journal append/fsync (P-012..P-014 done: re-verify), `appendFileIfAbsent` scan (F-07), hash per round (F-02), recovery, `messages.jsonl` golden contract (any proposed change goes through `messages-golden-diff`), image quota scan and mutex across I/O (B-C5), content cache fallback (A-8 fixed: verify). |

Ordering: 1 and 2 first (config and validation vocabulary), then 4 before 6 (lifetimes used by tools), 3 and 5 in parallel with them (independent), 7 and 8 last. Read-only lens work for different phases may run in parallel. Any measuring (lens 3 benchmarks, `-race` runs) runs alone, one at a time. Lens 1 may run `-race -count=3` only on its own phase's packages, and only when no benchmark is running.

### 3.4 Coverage check (run after any edit of the phase map)

```
python3 - <<'PY'
import re,glob,os,fnmatch,subprocess,sys
os.chdir('backend-go')
plan=open('../audit_plan.md').read()
blk=re.search(r'```phasemap\n(.*?)```',plan,re.S).group(1)
pk=[p for p in subprocess.check_output(['go','list','-f','{{.Dir}}','./cmd/...','./internal/...'],text=True).split()]
pk=[os.path.relpath(p) for p in pk]
files={}                      # file -> phases
for line in blk.strip().splitlines():
    ph,*items=line.split()
    for it in items:
        d,_,g=it.partition(':')
        for f in glob.glob(d+'/*.go'):
            b=os.path.basename(f)
            if b.endswith('_test.go'): continue
            ok = True if not g else (not fnmatch.fnmatch(b,g[1:]) if g[0]=='!' else fnmatch.fnmatch(b,g))
            if ok: files.setdefault(f,[]).append(ph)
allf=[f for p in pk for f in glob.glob(p+'/*.go') if not f.endswith('_test.go')]
miss=[f for f in allf if f not in files]; dup={f:v for f,v in files.items() if len(v)>1}
phpk={}
for f,v in files.items(): phpk.setdefault(v[0],set()).add(os.path.dirname(f))
lines={}
for f,v in files.items(): lines[v[0]]=lines.get(v[0],0)+sum(1 for _ in open(f))
print('packages',len(pk),'files',len(allf),'assigned',len(files),'missing',miss,'duplicated',dup)
for ph in sorted(lines): print(ph,len(phpk[ph]),'pkgs',lines[ph],'lines')
print('total',sum(lines.values()))
sys.exit(1 if miss or dup else 0)
PY
```
Expected: `missing [] duplicated {}`, exit 0.

## 4. Lens briefs

Common to all lenses: read the phase's files in full for the lens; use the code graph / `grep` to find callers; cite `file:line` read in this session; verify against source before filing; check section 1.1 first; do not change code, do not run `git add/commit`, do not restart the backend, do not run `go test ./...`, never kill processes by name. Findings about code outside the phase go in a "cross-phase leads" section with file:line, not as findings.

### Lens 1: correctness and concurrency
- **Look for:** data races and unsynchronised shared state; goroutines without an owner or a stop path; `Close`/shutdown that returns before work stops (the `176fd7d` class); locks held across I/O or callbacks; lock-order inversions; channel misuse (send on closed, never closed, blocking send under lock); `context` not propagated or leaked cancel; timers (`AfterFunc`) outliving their owner; error paths that drop errors, leave partial files or leave a handle `running` forever; integer and slice bounds; time and cursor arithmetic; panics outside a `recover` boundary (D-1); idempotency of recovery after crash; behaviour on restart with half-written state.
- **Evidence:** a failing or flaky test, a `-race` report, a deterministic interleaving written as a test sketch (in `/tmp`), or a traced call path with line numbers. Use `go vet`, `go test -race -count=3 <pkgs>`, `go test -run X -count=50 -cpu 1,4` under parallel load for flake hunts.
- **Do not:** report style, report theoretical races guarded by a lock you did not read, or add sleeps/retries to hide a flake.

### Lens 2: security and boundaries
- **Look for:** every input that crosses a trust boundary (HTTP bodies, query and path params, remote tokens, manifests and settings files on disk, tool arguments from the model, MCP/extension output, env); path traversal, symlink and TOCTOU on files and directories (`..`, absolute, name validation, `O_NOFOLLOW`, `SameFile`); SSRF and redirects; command and argument injection in spawned processes; env inheritance; secrets in logs, errors, results, transcripts, http_debug, process arguments; file and socket modes and umask; auth (constant-time compare, missing checks on a route, scope/globs enforcement for remote tokens, error text leaking server paths or profile names); resource limits on untrusted sizes (body, line, depth, count); scrubbing consistency.
- **Evidence:** a concrete input and the line where it is accepted, a test or small program in `/tmp` demonstrating it against the real function (`httptest`, temp dirs), or the missing check shown by comparing with the sibling route/tool that has it. Rate by who can reach it (local user, token holder, model, remote peer).
- **Do not:** run anything against real networks or real credentials, read real secret files, or file hardening wishes without a reachable path (put those under "hardening, unreachable").

### Lens 3: performance
- **Look for:** repeated I/O (re-stat, re-read, re-parse per request or per round); O(n^2) on turns, lines or sessions; whole-file reads for partial needs; unbounded maps, caches, slices, queues, retained runs; allocation on hot paths (string/[]byte churn, `fmt` on success path, repeated `json.Marshal`/`Unmarshal` of the same value, reflection, `append` without capacity); locks held too long and contention (mutex/block profile); polling instead of events; fsync counts; goroutine-per-item fan-out without a ceiling; subprocess spawn per call. Start from the hot-path table in section 2.
- **Evidence:** a benchmark (`-bench -benchmem -count=10`, run alone) or a profile with the top frames; or reasoning explicitly labelled `unmeasured` with a stated workload size. State workload (sessions, turns, bytes) with every number. Do not claim improvement ranges without a measured prototype in `/tmp`.
- **Do not:** micro-optimise cold code, propose caches without an invalidation story, run benchmarks while another is running, commit benchmark files (propose them), or report differences under 5% as findings.

### Lens 4: maintainability
- **Look for:** duplicated logic across files and packages (candidates already known: F-26 items); dead code and unused exported symbols (`grep -rn`, `go vet`, graph in/out degree zero); files above about 700 lines and functions above about 80 lines with mixed responsibilities; names that mislead; settings read in more than one place; patterns that diverge from the neighbours (settings validation, error wrapping, path helpers, async handle kinds, scrubbing); comments that restate code or are stale; test quality: tests that assert nothing, sleep-based sync, shared global state, missing failure-path tests for the recently added code, coverage of the new commits (`go test -cover` per package), fakes that diverge from the real type; fit with `backend-go/CLAUDE.md` and `DECISIONS.md`.
- **Evidence:** the two or more locations with `file:line` (duplication), a grep showing no callers (dead code), line counts, or a test that passes with the code mutated (`mutate-check.sh` on a copy is not allowed to leave changes; describe the mutation instead).
- **Do not:** propose rewrites, new abstractions for fewer than three uses, renames without a reader-facing reason, or style that `gofmt`/`go vet` already settle.

## 5. Per-phase procedure and findings format

1. Coordinator runs section 2 once; records `audit_findings/baseline.md`.
2. For phase P, four reviewers start in parallel (read-only; measurement tasks serialised by the coordinator).
3. Each reads section 1.1 material for the phase, then the files, then writes its file.
4. Coordinator checks the file against the format (reject files with findings lacking `file:line`).

**File convention:** `audit_findings/<phase>_<lens>.md`, where phase is `p1`..`p8` with sub-pass suffix (`p2a`, `p6c`) and lens is `correctness`, `security`, `performance`, `maintainability`. Example `audit_findings/p4_security.md`. The directory is created by the first reviewer. Merge outputs: `audit_findings/merged_<phase>.md` and `audit_findings/MERGED.md`. (`/audit_*.md` in `.gitignore` matches only root files; whether `audit_findings/` is committed is an open question, section 8.)

**File layout:** header (phase, lens, HEAD sha, files read, commands run with exit codes, time), then findings, then "cross-phase leads", then "checked and sound" (one line each, so the next reviewer does not redo it), then "not reviewed" (what was skipped and why).

**Finding format** (one block per finding):
```
### <PHASE>-<LENSLETTER><nn>  <one-line title>
- severity: critical | high | medium | low
- lens: correctness | security | performance | maintainability
- location: path/file.go:123 (more locations listed the same way)
- evidence: <reproduction, test, benchmark with numbers, or reasoning>   [measured | read | unmeasured]
- suggested fix: <smallest change; name the files>
- effort: S (under 1 h) | M (under 1 day) | L
- prior art: <old id and status, or "new">
```
Lens letters: C, S, P, M. Severity: critical = data loss, remote or unauthenticated compromise, or crash/hang of the whole server in normal use; high = reachable bug or leak with real impact, or a measured cost above 20% on a hot path; medium = real but narrow or conditional; low = small cost, hygiene, or unreachable hardening.

**Evidence rule:** no finding without `file:line`. A claim without a reproduction, test or measurement is labelled `[unmeasured]` or `[read]` and cannot be rated above medium (security: above medium only with a concrete input shown). Numbers always carry workload and command.

## 6. Merge, approval gate, fixes

- **Merge** (coordinator, per phase then global): collect the four files; dedupe findings pointing to the same `file:line` or cause (keep the highest severity, list all lens ids); verify every critical/high by reading the source or re-running the reproduction (reviewer claims are leads); drop findings that repeat fixed prior art; rank by severity, then evidence (`measured` > `read` > `unmeasured`), then effort (S first). Output `merged_<phase>.md` and a ranked `MERGED.md` with a status column (`open`, `approved`, `rejected`, `deferred`, `fixed <sha>`).
- **Approval gate:** the user decides each finding (or each batch) before any code changes. Nothing is fixed without an `approved` status.
- **Fixes:** a separate stage by an implementing agent, not the reviewers. One bounded commit per fix or small related group; every bug fix with a regression test proven to fail without the fix (`mutate-check.sh`); performance fixes with a benchmark before and after; changes that can touch `messages.jsonl` run `messages-golden-diff`; settings follow the config rules (defaults, schema, sample, DECISIONS.md). Per batch: `gofmt -l`, `go vet`, `git diff --check`, affected package tests; at the end `verify-go.sh` with `--race` on touched packages. No push.

## 7. Prompt templates (paste one per reviewer; fill the `<...>`)

Shared acceptance (included in each): the report file exists in the section 5 layout; every finding has `file:line`, severity, evidence tag, fix, effort; prior art checked; no code, git state or running processes changed.

### 7.1 Correctness and concurrency
```
You are reviewer 1 (correctness and concurrency) in a read-only audit of backend-go/.
Goal: find defects in <PHASE NAME> (lens: races, lifetimes, error paths, shutdown, crash recovery).
Target files: <FILE LIST from audit_plan.md phasemap for <PHASE>> (non-test; read tests only as evidence).
Newly added, never independently reviewed, review with priority: <COMMITS and files from audit_plan.md 3.3 for <PHASE>>.
Context: read audit_plan.md sections 1.1, 4 (Lens 1) and 5; backend-go/CLAUDE.md; the DECISIONS.md entries for the area; then grep the prior audits (audit_backend.md, audit_performance_plan.md, audit_audit.md, audit_design*.md) for the area. Do not repeat fixed items; REJECTED items need new evidence.
Constraints: read-only. No edits, no git add/commit, no restart or kill of any process, no `go test ./...` (use `go test ./internal/<pkg>/`), scratch in /tmp only and removed afterwards. Race runs: `go test -race -count=3` on this phase's packages only, one at a time.
Acceptance: write audit_findings/<phase>_correctness.md in the audit_plan.md section 5 format. No finding without file:line; unmeasured claims labelled and not above medium.
Report: file path, counts of findings by severity, commands run with exit codes, areas not covered.
```

### 7.2 Security and boundaries
```
You are reviewer 2 (security and boundaries) in a read-only audit of backend-go/.
Goal: find reachable security defects in <PHASE NAME>: input validation, path handling, secrets, access rules, resource limits.
Target files: <FILE LIST from audit_plan.md phasemap for <PHASE>>.
Newly added, never independently reviewed, review with priority: <COMMITS and files from audit_plan.md 3.3 for <PHASE>>.
Context: read audit_plan.md sections 1.1, 4 (Lens 2) and 5; backend-go/CLAUDE.md; DECISIONS.md for the area; prior audits (fetch SSRF, process env, http_debug, remote token scope were handled before: verify, do not repeat).
Constraints: read-only. Use only httptest, temp dirs and fake credentials; no real network, no real secrets, no edits, no git add/commit, no process kills or backend restart, no `go test ./...`. Scratch in /tmp, removed afterwards.
Acceptance: write audit_findings/<phase>_security.md in the section 5 format. Each finding names who can reach it (local user, token holder, model, remote peer) and the exact input; unreachable hardening goes in its own section.
Report: file path, findings by severity, commands run with exit codes, areas not covered.
```

### 7.3 Performance
```
You are reviewer 3 (performance) in a read-only audit of backend-go/.
Goal: find measured or well-reasoned costs in <PHASE NAME>: repeated I/O, allocations on hot paths, long lock holds, unbounded growth.
Target files: <FILE LIST from audit_plan.md phasemap for <PHASE>>; hot-path benchmarks: <ROWS from audit_plan.md section 2 table for this phase>.
Newly added, never independently reviewed, review with priority: <COMMITS and files from audit_plan.md 3.3 for <PHASE>>.
Context: read audit_plan.md sections 1.1, 2, 4 (Lens 3) and 5; backend-go/CLAUDE.md; backend-go/README.md benchmark section; audit_backend.md baseline table and audit_performance_plan.md (P-ids) so numbers can be compared and fixed items are not repeated.
Constraints: read-only for the repo. Benchmarks and profiles run one at a time, alone, `-run '^$' -bench <name> -benchmem -count=10`; new benchmarks live in /tmp or are proposed in the report, not committed. No edits, no git add/commit, no process kills, no `go test ./...`. Scratch in /tmp, removed afterwards.
Acceptance: write audit_findings/<phase>_performance.md in the section 5 format. Every number has workload size and command; unmeasured items labelled `unmeasured`; differences under 5% not reported.
Report: file path, findings by severity, benchmarks run (command, exit code, key numbers), proposed new benchmarks, areas not covered.
```

### 7.4 Maintainability
```
You are reviewer 4 (maintainability) in a read-only audit of backend-go/.
Goal: find duplication, dead code, misleading names, divergence from existing patterns and weak tests in <PHASE NAME>.
Target files: <FILE LIST from audit_plan.md phasemap for <PHASE>> and their _test.go files (test quality only).
Newly added, never independently reviewed, review with priority: <COMMITS and files from audit_plan.md 3.3 for <PHASE>>.
Context: read audit_plan.md sections 1.1, 4 (Lens 4) and 5; backend-go/CLAUDE.md; DECISIONS.md for the area; prior audits (F-22, F-24, F-26 were handled: check status in audit_backend.md Log).
Constraints: read-only. Propose no rewrites and no new abstractions for fewer than three uses. No edits, no mutation left in the tree, no git add/commit, no process kills, no `go test ./...`. Scratch in /tmp, removed afterwards.
Acceptance: write audit_findings/<phase>_maintainability.md in the section 5 format; duplication findings list every location; dead-code findings include the grep command and its empty result; test-quality findings name the test and what it fails to check.
Report: file path, findings by severity, commands run with exit codes, `go test -cover` numbers for the phase packages if run, areas not covered.
```

## 8. Risks and open questions

Risks:
- Phase 6 (13.7k) and 7 (14.7k) are large; sub-passes keep each reviewer near 5-8k lines, but a reviewer may still skim. The "not reviewed" section is mandatory so gaps are visible.
- Reviewer claims are leads: the merge step must re-verify critical/high; budget about as much coordinator time as one reviewer pass per phase.
- Lens 3 numbers are noisy on a shared machine (12 CPUs, background jobs); the previous quiet-machine re-run is still deferred. Measure alone or label results.
- Prior-art "still open" entries in `audit_performance_plan.md` section 3 predate its fix batches; some are fixed. Reviewers must check the tree.
- Flaky tests can look like lens 1 findings; use the baseline record and `-count`/`-cpu` runs.
- `audit_*.md` at the repo root is git-ignored: this plan was committed with `git add -f`.

Open questions for the user:
1. Are `audit_findings/` files committed (they are not ignored) or kept local like `audit_*.md`?
2. Should phase 2b be split further, and may 6 and 7 sub-passes run as separate reviewer sessions (assumed yes)?
3. Is a quiet-machine slot available for lens 3 and the `-race` baseline (the process tool, one at a time)?
4. Should lens 3 include end-to-end latency against a real provider (plan assumes `httptest` fakes only)?
5. Severity bar for "critical" in security: is a local-user-only issue on a loopback server ever above medium?
