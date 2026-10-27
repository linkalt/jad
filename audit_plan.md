# Audit plan: backend-go code quality and performance

Status: plan only, no review done. Prepared at HEAD `4fb4d51`, go1.27.3 linux/amd64, 12 CPUs. Line counts in section 3 were measured by the command in section 3.1.
Model: four different reviewer agents (section 4.1) run on ONE machine; ALL FOUR do a FULL review of the SAME phase, independently. A coordinator compares the four reports, one serialized step measures, the user approves, then fixes. Reviewers never change code.

## 1. Scope, goals, "done"

- **In scope:** all 37 packages under `backend-go/cmd` (4) and `backend-go/internal` (33), 79.6k non-test lines (377 files), 96120 test lines (measured by the section 3.4 check and 3.1 command).
- **Out of scope:** `frontend/`, `frontend_new/`, generated files (for example `html-to-markdown-rs_cgo_link.go`), `testdata/`, `sample/` content except where a setting is defined. Tests are review targets only for test quality (checklist D) and as evidence.
- **Goals:** per phase, four independent full reviews of the same files, compared to separate consensus from single-reviewer claims; a measured, ranked, bounded fix backlog for the user.
- **Done** = for each of the 8 phases (and sub-passes) all 4 reviewer files exist, every finding meets the evidence rule (section 5), the merge file records which reviewers found what (section 6), measurements of the performance hypotheses are done, and the user has approved, rejected or deferred every finding. Fixes are a later stage and not part of "done" for the audit.

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

## 2. Baseline (run ONCE by the coordinator, serialized, before any review; record in `audit_findings/baseline.md`)

One command at a time, from `backend-go/`, while no reviewer is running. The machine is shared and not assumed quiet: record `uptime` load before and after. Never `go test ./...`. Never restart or kill the running backend. Scratch in `/tmp`, removed afterwards.

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
- `TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks`, `TestRemoteSubagentCloseLeavesNoGoroutines` (tools/native): `.async: directory not empty` on temp-dir cleanup. Root cause was `asyncmgr.Manager.Close` not waiting for `Complete` writes; fixed in `176fd7d`. If it still appears, it is a finding (checklist A).
- `TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher` (httpapi): test timing assumption, fixed in `4fb4d51`.
- A failure of any other test in the baseline is itself a correctness finding with the log attached. Reviewers are told the baseline result and flaky list in their prompt, so they do not rerun the suite.

**Benchmarks for the measurement step (section 6.3).** Reviewers do NOT run benchmarks, pprof or `-race`. 100 benchmarks already exist (`grep -rhE '^func Benchmark' --include=*_test.go internal cmd`), in app 7, asyncmgr 1 file (3), httpapi 10 files, session 4, sessionfs 2, tools/native 15, transcript 11, others. The measurement step runs those the reviewers' hypotheses point to with `-run '^$' -bench <name> -benchmem -count=10`, one at a time. Because the machine is not quiet, each claim needs a relative measurement (baseline and candidate interleaved in one session, or the same benchmark at two workload sizes), the load average, and a noisy label when the spread of the 10 runs exceeds 5% (`backend-go/README.md:25-31`; `benchstat` only if present, else medians by hand). Absolute numbers from `audit_backend.md` are context only.

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

Gaps where reviewers may propose a new benchmark (written to `/tmp` by the measurement step, not committed): `internal/catalog` Scan (no benchmark), the async latest builder (`tools/native/async_latest.go`), `transcript`/`transcripts` tool list and detail on a large session, `remote` client round trip against an `httptest` server, `anthropicadapter` stream translation, `webui` increments, `tools` catalog build per run. Profiles (`-cpuprofile`, `-memprofile`, `-mutexprofile`, `-blockprofile`) only on a benchmark that shows a cost, in the measurement step. Provider behaviour uses `httptest` fakes only.

## 3. Phases

### 3.1 Line counts (non-test / test), measured at `4fb4d51`

Command (counts `*.go` in the directory itself, not subdirectories):
```
cd backend-go; for d in $(find cmd internal -type d|sort); do n=$(ls $d/*.go 2>/dev/null|grep -v _test.go|xargs -r cat|wc -l); t=$(ls $d/*_test.go 2>/dev/null|xargs -r cat|wc -l); [ -n "$(ls $d/*.go 2>/dev/null)" ] && echo "$d $n $t"; done
```

### 3.2 Phase table

All four reviewers get the same phase (or sub-pass) and review all of it. Package lists in the `phasemap` block are the single source of truth for the check in 3.4.

| Phase | Packages (non-test lines) | Total | Why this position |
|---|---|---|---|
| 1 Foundation | cmd/anthropic-adapter 77, cmd/claude-auth 306, cmd/jad 139, cmd/openai-auth 329, app 6933, rootsetting 45, serverconfig 92, logging 102, catalog 426, sessionlock 224 | 8,673 | Wiring, config load, shutdown. Everything else hangs off `app.Resolver`. |
| 2 State (sub-passes 2a, 2b1, 2b2) | 2a: sessionfs `native_*` tool settings 4,845 + layouttest 355 = 5,200 (tool settings types and validation). 2b1: sessionfs core 4,933 (`session.go` config model and loader 1,305, `roots.go` roots and resolve 840, `layout_*` layout loading 1,317, `catalog_*` scans 696, `check.go`, `settings_stamp.go`, `settings_inputs.go`, `profile_*`, `model_effort.go`, `removed_keys.go`). 2b2: sessionfs non-tool settings and file helpers 3,003 + session runner 1,692 = 4,695 (`file_settings`, `files`, `session_files`, `storage_paths`, `tool_*`, `state_context_settings`, `hooks`, `mcp`, `provider_*`, `image_*`, `checkpoint`, `http_debug`, `remote_api`, `openai_oauth`, `extension_decide_systemone` settings; `session/` runner, response, tool batch) | 14,828 | Settings and on-disk layout. Needed to judge validation and path handling in later phases. 2b1 is the loader every setting passes through; 2b2 holds the leaf settings plus the `session` runner that consumes them (`session` imports `sessionfs`). |
| 3 Server surface | httpapi 5656, webui 1840, httpdebug 475, interactions 362, memorytrace 340 | 8,673 | HTTP boundary, SSE, run registry. |
| 4 Async and remote | asyncmgr 1962, remote 1147, remoteapi 1002, checkpoint 1135 | 5,246 | Concurrency and security heavy; consumed by phase 6 tools. Most unreviewed code. |
| 5 Providers | anthropicadapter 3233, openaiapi 538, openaiauth 1226, claudeauth 1234, openaioauth 229, responses 62 | 6,522 | Network clients, tokens, OAuth callbacks. Independent of 3 and 4. |
| 6 Tools part 1 (sub-passes 6a, 6b, 6c) | 6a: tools 1435, tools/extension 886, tools/mcp 1575; 6b native command tools: exec, process, tool_smith, async, shell_async, child_groups, scratch_sweep (29 files, 5,545); 6c native agent tools: subagent*, profile_message*, statecontext_counts (19 files, 4,248) | 13,689 | Process lifetimes, approval, remote hops. After 4 (uses asyncmgr, remote). |
| 7 Tools part 2 (sub-passes 7a, 7b) | 7a file and search tools: bash*, path*, read*, write, edit*, mutate*, file_change, image_file, glob, grep, ripgrep, fetch*, html-to-markdown link (27 files, 6,835); 7b transcript tools and misc: transcript* (10 files, 2,516), memory*, task*, hooks*, skill, catalog, request_draft (12 files, 5,342) | 14,693 | Path confinement and SSRF in 7a; transcript tools read other sessions (7b). |
| 8 Transcripts and media | transcript 5119, imagestore 638, imageresize 558, imagedata 72, contentlibrary 909 | 7,296 | Journal and image storage. Last because phases 2, 3, 6 and 7 are its callers. |

Native file lists: the `phasemap` block uses globs; the check proves each of the 97 `tools/native` non-test files matches exactly one sub-pass. Sub-pass sizes (7a 6.8k, 7b 7.9k, 6b 5.5k, 6c 4.2k, 2a 5.2k, 2b1 4.9k, 2b2 4.7k) are all between 4k and 8k.

```phasemap
p1  cmd/anthropic-adapter cmd/claude-auth cmd/jad cmd/openai-auth internal/app internal/rootsetting internal/serverconfig internal/logging internal/catalog internal/sessionlock
p2a internal/sessionfs:native_*  internal/sessionfs/layouttest
p2b1 internal/sessionfs:session.go internal/sessionfs:roots.go internal/sessionfs:layout_* internal/sessionfs:catalog_* internal/sessionfs:check.go internal/sessionfs:removed_keys.go internal/sessionfs:settings_inputs.go internal/sessionfs:settings_stamp.go internal/sessionfs:profile_* internal/sessionfs:model_effort.go
p2b2 internal/sessionfs:file_settings.go internal/sessionfs:files.go internal/sessionfs:session_files.go internal/sessionfs:storage_paths.go internal/sessionfs:tool_* internal/sessionfs:state_context_settings.go internal/sessionfs:checkpoint_settings.go internal/sessionfs:extension_decide_systemone_settings.go internal/sessionfs:hooks_settings.go internal/sessionfs:http_debug_settings.go internal/sessionfs:image_* internal/sessionfs:mcp_settings.go internal/sessionfs:openai_oauth_settings.go internal/sessionfs:provider_* internal/sessionfs:remote_api_settings.go internal/session
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
| 2a | `native_subagent_settings.go`, `native_profile_message_settings.go`, `native_transcript(s)_settings.go` (`be4af1d..a21104b`, `a57403b`, `2828a68..010e616`); `native_async_settings.go` (`86552a4`) | Validation of every new key (bounds, defaults, unknown keys), 21 `Native*Settings` types (F-22), duplicated validation patterns across `native_*` files. |
| 2b1 | `roots.go`, `layout_load.go`, `session.go` (`be4af1d..a21104b`, `a57403b`); `check.go` (`f0e8b78`); `layout_files.go` (`0bf03a1..35b9b66`); | `session.go` (1.3k) as a god file, unknown-key and removed-key handling, settings stamp coverage (P-001), path handling for session, profile and root names, atomic writes and fsync in layout files, catalog scans following symlinks (B-C4 rejected: do not re-raise without new evidence). |
| 2b2 | `remote_api_settings.go` (`be4af1d..a21104b`, `a57403b`); `extension_decide_systemone_settings.go`, `tool_shape.go` (`a095d7e..b8a2967`, `2828a68..010e616`); `provider_network_settings.go` (`0bf03a1..35b9b66`); none in `session/` | Validation of the leaf settings (bounds, defaults), `files.go`/`session_files.go` directory listing limits and symlinks, `storage_paths.go` path selection; `session/runner.go` (980 lines): run loop lifetimes, tool batch workers and panic recovery (D-1 fixed: verify), token limits, no global ceiling on tool goroutines (B-C8 open). |
| 3 | `be4af1d..a21104b` (`httpapi/executor.go`, `profile_run.go`, `registry.go`) | Run registry retention (B-C1), SSE queues and slow subscribers (fixed `4fb4d51` was test-only: check publisher), `registry.go` lock scope (P-0xx), CORS and token checks, body limits, http_debug redaction (D-5 fixed). |
| 4 | Remote range (`remote/*`, `remoteapi/*`: `f5a76d8..64e407b`, `be4af1d..a21104b`, `8028576`, `a57403b`); async latest (`86552a4`, `3e33c97`, `4eb3586`, `8563272`, `5230adf`: `asyncmgr/config.go`, `manager.go`); `176fd7d` (Close waits for Complete) | Lifetimes: Close vs `Complete`/`finishing`/`completing`; progress cursor state; per-token run cap; client: TLS, redirects, proxy, caps, retries, token scrubbing; server: auth, path/ID validation, error leakage (`a6150e3`); unix socket mode; checkpoint git subprocess handling. |
| 5 | none by SHA; `provider_network` (`0bf03a1`) changes how providers dial (code in app, phase 1): confirm adapters honour it | Token files and refresh races, OAuth callback binding, retries and idle guards (A-4 rest), stream parsing limits, secret logging, `anthropicadapter` non-loopback relay (A-17). |
| 6a | `a095d7e..b8a2967` (`extension/decide_systemone*.go`, `decide_backend.go`, `decide_image.go`); remote (`tools/catalog.go`) | MCP discovery per run under one mutex (A-6), panic recovery (D-1), tool name collisions, extension process spawn. |
| 6b | exec: `5371620` alias, exec rework (`4c5f289..623fbcb`, `c51f86c..9d8558c` templates and `fn.<name>`); tool_smith (`c4308e0..acb3d98` listing); async latest (`3e33c97`: `async_latest.go`, `async_tool.go`, `process*.go`, `shell_async.go`); `5230adf` | `async_latest.go` cursor and scrubbing; process env inheritance (A-2); orphan/kill logic and recycled pgid (B-C3); command construction and quoting for exec templates; env_file handling; `process_manager.go` (1.1k) and `tool_smith_bridge.go`. |
| 6c | `f0e8b78`, `91c92e6` (`subagent.go`); remote range (`subagent_remote.go`, `subagent_external.go`, `subagent_call.go`, `profile_message_remote.go`, `profile_message_async.go`, `async_host.go`: `be4af1d..a21104b`, `f5a76d8..64e407b`); `2828a68..010e616` (`subagent_manager.go`); `86552a4..8563272` (`subagent_async.go`) | Cross-session addressing `<session>/<category>/<profile>`; remote run lifetime and cancel; nested approval; manifest recovery; `subagent_manager.go` (914) and `subagent.go` (726) size. |
| 7a | async latest touches `bash.go`, `bash_output.go` (`3e33c97`) | Path confinement and symlinks (`path.go`, `mutate_path.go`), TOCTOU on edit/write, fetch SSRF (fixed gap in batch 9: verify), ripgrep/glob timeouts, whole-file reads (F-12), bash spill pruning (F-11). |
| 7b | Transcript v1 and v2 (`2b3e224`, `857b774`, `b94fd7f..c789a9f`: `transcript.go`, `transcript_steps.go`, `transcript_detail.go`, `transcript_cut.go`, `transcript_render.go`); `transcripts` (`2828a68..010e616`: `transcripts.go`, `transcripts_list.go`); remote transcripts (`1bf45fe`, `64e407b`, `a6150e3`: `transcript_remote.go`, `transcript_path.go`); `0fda7ef` and `91c92e6..` catalog registration | Reading another session's files: path parsing, cursor forgery, scope leaks, messages.jsonl reasoning exposure; memory store per-write scans (F-13); `memory_store.go` (1.1k). |
| 8 | none by SHA in `transcript`; `imagedata` changed by `a095d7e..b8a2967` | Journal append/fsync (P-012..P-014 done: re-verify), `appendFileIfAbsent` scan (F-07), hash per round (F-02), recovery, `messages.jsonl` golden contract (any proposed change goes through `messages-golden-diff`), image quota scan and mutex across I/O (B-C5), content cache fallback (A-8 fixed: verify). |

Ordering (one phase at a time): 1, 2 (config and validation vocabulary), 4 (lifetimes used by tools), 3, 5, 6, 7, 8 (callers last). One phase at a time: all four reviewers finish phase N (and the merge and measurement for it are done or running) before phase N+1 reviews start. Nothing else runs during a measurement step, so reviewers of the next phase wait for it.

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

## 4. Reviewers, checklist, workflow

### 4.1 Reviewers (fill before the first phase; ids are not known to the plan author)

| Slot | Profile / model id | Notes (strengths, known limits) |
|---|---|---|
| R1 | `jad/reviewer` (id `r1`) | |
| R2 | `jad/reviewer1` (id `r2`) | |
| R3 | `jad/reviewer2` (id `r3`) | |
| R4 | `jad/reviewer3` (id `r4`) | |

The reviewer id used in file names and finding records is the slot (`r1`..`r4`), mapped to the profile here. Slots must be different profiles/models; the mapping is not shown to a reviewer.

### 4.2 Lens checklist (every reviewer applies ALL of it to the WHOLE phase)

Lenses are checklist headings, not assignments. Use them to avoid blind spots; a finding carries the lens letter it fits best.

- **A. Correctness and concurrency.** Data races and shared state without a guard; goroutines without owner or stop path; `Close`/shutdown that returns before work stops (the `176fd7d` class); locks held across I/O or callbacks, lock-order inversions; channel misuse; `context` not propagated; timers outliving their owner; error paths that drop errors, leave partial files or leave a handle `running`; bounds and cursor/time arithmetic; panics outside a `recover` boundary (D-1); crash/restart with half-written state; idempotent recovery.
- **B. Security and boundaries.** Every trust boundary (HTTP bodies and params, remote tokens, files on disk, model tool arguments, MCP/extension output, env): validation, path traversal, symlinks and TOCTOU, SSRF and redirects, argument/command injection, env inheritance, secrets in logs/errors/results/transcripts, file and socket modes, auth (constant-time compare, scope/globs enforcement, error text leaking server paths), limits on untrusted sizes, scrubbing consistency. Name who can reach it (local user, token holder, model, remote peer).
- **C. Performance.** Repeated I/O (re-stat, re-read, re-parse per request or round); O(n^2) on turns, lines, sessions; whole-file reads for partial needs; unbounded maps, caches, queues, retained runs; hot-path allocation (string/[]byte churn, repeated marshal, reflection, `append` without capacity); locks held too long; polling instead of events; fsync counts; goroutine-per-item without ceiling; subprocess per call. Reviewers do NOT measure: they list hypotheses (section 5.3) with workload size and the benchmark to run.
- **D. Maintainability.** Duplication (list all locations), dead code (grep with empty result), files above about 700 lines and functions above about 80 with mixed duties, misleading names, settings read in more than one place, divergence from neighbouring patterns (validation, error wrapping, path helpers, async kinds, scrubbing), stale or restating comments, test quality (asserts nothing, sleep-based sync, shared globals, missing failure-path tests for the new commits, fakes diverging from real types), fit with `backend-go/CLAUDE.md` and `DECISIONS.md`.

**Not to do (all lenses):** style that `gofmt`/`go vet` settle; rewrites; abstractions for fewer than three uses; hardening wishes without a reachable path (put under "hardening, unreachable"); repeats of fixed prior art (section 1.1); speculative reading outside the phase (note it as a cross-phase lead with `file:line`).

### 4.3 Workflow per reviewer (the `feature-development` skill, adapted to review)

Source: `.jad/prompts/skills/feature-development/SKILL.md`. Steps 1-4 map directly; step 5 ("Implement and verify") becomes "Report" in the audit. The skill's execution rules apply unchanged: explore for the objective only, targeted reads, no re-reading of understood code, the initial plan is provisional, every extra investigation answers a specific question, report unrelated bugs without fixing them, never claim a test passed unless it was run and passed.

1. **Explore and understand the phase.** Entry points, data flow, state and lifecycle, configuration and persistence, constraints; the prior-audit entries and DECISIONS records for the area (section 1.1); the unreviewed commits of the phase (section 3.3). Output: a short understanding note (in the report header).
2. **Draft the review plan and hypotheses.** What to check per checklist lens, with assumptions marked "assumed" vs "verified". No findings yet.
3. **Trace impacted code to verify each hypothesis.** Callers/callees, shared state, lifecycle and shutdown, error paths, configuration, existing tests. Run only build/vet and targeted tests with `-count=1` where a hypothesis needs it (section 4.4). No speculative reading outside the phase; expand only when a trace reveals an important dependency.
4. **Revise and finalize the findings.** Drop disproved hypotheses (list them under "checked and sound", one line each), keep the evidence of the rest, set severity and effort.
5. **Report (replaces implement and verify).** Write the findings file (section 5). Every finding is evidence-backed (`file:line` plus reproduction, test or trace) or labelled `unmeasured`. State what was not covered and which commands ran with exit codes.

Fixes follow the skill's step 5 in full later (section 6.5), as separate bounded commits after the user approves the merged list.

### 4.4 Independence and same-machine rules (reviewers)

- Write only your own file `audit_findings/<phase>_<reviewer-id>.md`. Do NOT open any other `audit_findings/` file, other reviewers' notes or the merge files until your own file is finished and you have reported. Do not discuss findings with other reviewers.
- Never change code, config or git state: no edit, `git add/commit/stash/checkout/reset/clean`, `go clean`, `go mod tidy`, no `go generate`. The worktree and the Go build/test cache are shared with the other three reviewers and with the running backend.
- Allowed commands: reading tools, `grep`, `git log/show/diff/blame`, `go build`, `go vet`, `gofmt -l`, and targeted `go test ./internal/<pkg>/ -run <Name> -count=1` for a hypothesis. Not allowed: `-race`, `-bench`, `-cpuprofile` and other profiles, `-count>1` loops, `-cpu` sweeps, `go test ./...`, anything that binds a fixed port or leaves files outside `/tmp`. Write scratch files only under `/tmp/<reviewer-id>/` and remove them at the end.
- Never restart or kill the backend or any process; never kill by name or pattern.
- Do not use the network or real credentials; tests use `httptest`, temp dirs and fakes.
- A test that fails when you run it may be a known flaky test (section 2) or a collision with another reviewer's run: rerun once, and record both results; do not report it as a defect without a cause.

## 5. Findings: file, format, evidence

**File convention:** `audit_findings/<phase>_<reviewer-id>.md`, phase `p1`..`p8` with sub-pass suffix (`p2a`, `p6c`), reviewer id `r1`..`r4`. Example `audit_findings/p4_r2.md`. Coordinator outputs: `baseline.md`, `<phase>_merged.md`, `<phase>_measure.md`, `MERGED.md`. The directory is local and uncommitted, like the other root `audit_*` files (decided); created by the coordinator before the first phase.

**File layout:** header (phase, reviewer id, HEAD sha, files read, commands with exit codes, date); 1 understanding note; 2 hypotheses list (id, statement, status verified/disproved/unverified); 3 findings; 4 performance hypotheses for measurement (5.3); 5 cross-phase leads; 6 checked and sound; 7 not covered and why.

### 5.1 Finding format (one block per finding)
```
### <PHASE>-<REVIEWER>-<nn>  <one-line title>
- severity: critical | high | medium | low
- lens: A correctness | B security | C performance | D maintainability
- location: path/file.go:123 (more locations the same way)
- evidence: <reproduction, test, trace or benchmark>   [measured | read | unmeasured]
- suggested fix: <smallest change; name the files>
- effort: S (under 1 h) | M (under 1 day) | L
- prior art: <old id and status, or "new">
```
Severity: critical = data loss, remote or unauthenticated compromise, or hang/crash of the whole server in normal use; high = reachable bug or leak with real impact, or a measured cost above 20% on a hot path; medium = real but narrow or conditional; low = small cost, hygiene, unreachable hardening.

### 5.2 Evidence rule
No finding without `file:line`. A claim without reproduction, test or trace is labelled `[unmeasured]` or `[read]` and cannot be rated above medium (security: above medium only with a concrete input shown). Numbers carry workload and command. Because reviewers do not benchmark, a performance finding from a reviewer is `[read]`/`[unmeasured]` until the measurement step; the measured value replaces the label there.

### 5.3 Performance hypothesis list (section 4 of the reviewer file)
```
- H<nn>: <what is slow/unbounded>, file:line
  workload: <sessions/turns/bytes/concurrency that make it matter>
  benchmark: <existing name from section 2, or the proposed new one and its fixture>
  expected: <relative effect, e.g. "scales with turns", "extra stat per call">
```

## 6. Merge, measurement, approval, fixes

### 6.1 Per phase order
1. Reviewers R1-R4 run in parallel on the same phase (section 4.3). 2. Coordinator merges (6.2). 3. Coordinator runs the serialized measurement step (6.3). 4. Phase results are added to `MERGED.md`. 5. After all phases, the ranked list goes to the user (6.4). 6. Fixes (6.5).

### 6.2 Merge (coordinator; reads all four files only after all four are finished)
- Normalise each finding to a key: cause plus `file:line` (same defect described differently is one finding). Record per finding which reviewers found it (`found by: r1, r3`).
- **Consensus** (2+ reviewers): higher confidence; take the highest evidence and the median severity; still verify any critical/high by reading the source.
- **Unique** (1 reviewer): no ranking until the coordinator runs a verification pass (read the source, reproduce with a test in `/tmp`); outcome `confirmed`, `disproved` or `unverified`. Unverified stays in the list, flagged.
- **Contradictions** (one reviewer lists the code under "checked and sound" or disputes severity, another files a bug): resolve with evidence (source reading or reproduction) and record both positions and the result.
- Drop repeats of fixed prior art; mark re-confirmed open items with the old id. Record disagreement counts per reviewer pair in `<phase>_merged.md` (useful to judge reviewer reliability). Output: `<phase>_merged.md` with a table (id, title, severity, lens, location, found by, status) and the evidence for every non-consensus decision.

### 6.3 Measurement step (coordinator/executor, serialized)
Runs after the merge, alone on the machine (no reviewer, build or test running; check `uptime` before and after). Inputs: the performance hypotheses (5.3) from all four files, merged and deduplicated, plus performance findings that need a number. For each: run the named benchmark (`-run '^$' -bench <name> -benchmem -count=10`), relative measurement (two workload sizes, or before/after of a `/tmp` prototype, never both in different sessions), `-race -count=1` only on the phase's packages once, pprof only where a benchmark shows a cost. Label results `noisy` when the 10-run spread exceeds 5%; repeat once; if still noisy keep the finding `unmeasured`. Output `<phase>_measure.md`: hypothesis, command, load average, numbers, verdict (confirmed / not confirmed / noisy). No repo files are changed (prototypes and new benchmarks in `/tmp`, removed afterwards).

### 6.4 Approval gate
`MERGED.md` ranks all findings by severity, then evidence (`measured` > `confirmed read` > `unmeasured`), then consensus count, then effort (S first), with a status column (`open`, `approved`, `rejected`, `deferred`, `fixed <sha>`). The user decides each finding or batch. Nothing is fixed without `approved`.

### 6.5 Fixes
A separate stage by an implementing agent following `feature-development` steps 1-5 per batch (explore, plan, trace impact, revise, implement and verify). One bounded commit per fix or small related group. Every bug fix has a regression test proven to fail without the fix (`mutate-check.sh`); performance fixes have a benchmark before and after; changes that can touch `messages.jsonl` run `messages-golden-diff`; settings follow the config rules (default, schema, sample, DECISIONS.md). Per batch: `gofmt -l`, `go vet`, `git diff --check`, affected package tests; at the end `verify-go.sh` with `--race` on touched packages. No push. Reviewers do not fix.

## 7. Prompt templates

Fill the `<...>` fields. File lists come from the `phasemap` block (3.2), commits from 3.3, baseline facts from `baseline.md`.

### 7.1 Reviewer prompt (identical for R1..R4 except `<reviewer-id>`)
```
You are reviewer <reviewer-id> in an independent, read-only audit of backend-go/ (code quality and performance). Three other agents review the SAME phase independently; your report is compared with theirs, so be complete and evidence-based, and do not look at their work.

Goal: a FULL review of phase <PHASE> (<name>) applying the whole checklist below: correctness and concurrency, security and boundaries, performance (as hypotheses), maintainability.
Scope (non-test files; read tests only as evidence and for test quality): <FILE LIST for the phase from audit_plan.md phasemap>. Stay inside it; note outside issues as cross-phase leads with file:line.
Context to read first: audit_plan.md sections 1.1, 4 and 5; backend-go/CLAUDE.md; DECISIONS.md entries for this area; prior audits (audit_backend.md, audit_performance_plan.md, audit_audit.md, audit_design.md, audit_design0.md, audit_design1.md): do not repeat fixed items, REJECTED items need new evidence, open items are cited by old id.
Never independently reviewed, check these with priority: <COMMITS and files for the phase from audit_plan.md 3.3>. Known risk areas: <risk column>.
Baseline (coordinator already ran it; do not rerun the suite): <build/vet/gofmt/test results and known flaky tests from baseline.md>.
Checklist (apply every item to the whole phase): <paste audit_plan.md 4.2 A-D>.
Workflow (skill .jad/prompts/skills/feature-development/SKILL.md, adapted; read it): 1 explore and understand the phase; 2 draft review hypotheses, assumed vs verified; 3 trace code to verify each hypothesis (callers, callees, shared state, lifecycle, errors, config, tests), no speculative reading outside the phase; 4 revise: drop disproved, keep evidence; 5 report instead of implement. Keep the skill's execution rules; report unrelated bugs; never claim a test passed unless you ran it.
Independence and machine rules: write only audit_findings/<PHASE>_<reviewer-id>.md; do not open any other file in audit_findings/ before finishing and reporting; never change code or git state (no edit, add, commit, stash, checkout, reset, clean, go clean); the worktree and the Go cache are shared with three other agents. Allowed: reading, grep, git log/show/diff, go build, go vet, gofmt -l, and targeted `go test ./internal/<pkg>/ -run <Name> -count=1`. Forbidden: -race, -bench, profiles, -count>1, -cpu, `go test ./...`, fixed ports, network, real credentials, killing or restarting any process. Scratch only in /tmp/<reviewer-id>/, removed at the end.
Output format: audit_plan.md sections 5 and 5.1-5.3 (file layout, finding blocks, performance hypothesis list). No finding without file:line; claims without reproduction/test/trace are labelled [read] or [unmeasured] and not rated above medium; list "checked and sound" and "not covered".
Report back (short): file path, finding counts by severity and lens, hypotheses count, commands run with exit codes, areas not covered.
```

### 7.2 Coordinator prompt (merge)
```
You are the coordinator for phase <PHASE>. The four independent reports are audit_findings/<PHASE>_r1.md .. _r4.md (all finished). Read audit_plan.md sections 5 and 6.2.
Task: write audit_findings/<PHASE>_merged.md. Normalise findings by cause + file:line; mark for each which reviewers found it. Split into consensus (2+), unique (1), contradictions. Verify every unique finding and every critical/high by reading the source or reproducing in /tmp (outcome confirmed / disproved / unverified). Resolve contradictions with evidence and record both positions. Drop repeats of fixed prior art (grep the prior audits and git log). Merge the performance hypotheses (dedupe) into a measurement list. Add per reviewer-pair agreement counts.
Constraints: read-only for the repo; no code changes; no benchmarks or -race in this step; no deletion of reviewer files.
Report: merged file path, counts (consensus/unique/contradictions, by severity), verification outcomes, the measurement list size, unresolved items.
```

### 7.3 Measurement prompt (serialized)
```
You are the executor for the measurement step of phase <PHASE>. Read audit_plan.md section 6.3 and audit_findings/<PHASE>_merged.md (measurement list).
Preconditions: no reviewer or other build/test is running; record `uptime` before and after.
Task: for each hypothesis run the named benchmark one at a time (`-run '^$' -bench <Name> -benchmem -count=10`), as a relative measurement (two workload sizes or a /tmp prototype versus current in the same session), then `go test -race -count=1` once on the phase's packages; pprof only for a benchmark that shows a cost. Mark results noisy when the spread exceeds 5%, repeat once, else keep `unmeasured`. Machine is shared and not quiet: no absolute claims.
Constraints: no repo changes (prototypes and new benchmarks in /tmp, removed at the end); never kill or restart processes; never `go test ./...`.
Output: audit_findings/<PHASE>_measure.md (hypothesis, command, load average, numbers, verdict). Report: path, verdict counts, commands with exit codes, noisy items.
```

## 8. Risks and open questions

Risks:
- Reviewer disagreement: four agents will differ in depth and severity. The merge records agreement and verifies unique and contradicted items instead of voting; consensus alone is not proof (they may share a blind spot or all read the same stale audit entry).
- Shared worktree and build cache: a reviewer that edits, stashes, checks out or cleans breaks the other three. The rules in 4.4 forbid it; the coordinator checks `git status --short` and `git stash list` after each review round and discards a round if the tree changed.
- Concurrent reviewer test runs can collide on temp dirs/ports and trigger the known flaky tests; reviewers are limited to `-count=1` targeted tests and told to record collisions rather than report them.
- Independence can leak: a reviewer may open another's file. The prompt forbids it; the coordinator compares file modification times with the finish order and notes any suspicion.
- Phases 6 and 7 are large (sub-passes of 4-8k lines); a reviewer may skim. "Not covered" is mandatory so gaps are visible, and uncovered parts are visible when all four list the same gap.
- Reviewer claims are leads: merge must re-verify critical/high and all unique findings; budget about one reviewer pass of coordinator time per phase.
- The machine is shared and noisy: only relative measurements, serialized, with load recorded; the earlier quiet-machine re-run (deferred in `audit_performance_plan.md`) is still open.
- Prior-art "still open" entries in `audit_performance_plan.md` section 3 predate its fix batches; some are fixed. Check the tree.
- Flaky tests can look like correctness findings; use the baseline list and rerun once.
- `audit_*.md` at the repo root is git-ignored: this plan is tracked only because it was added with `git add -f`; `audit_findings/` stays untracked and local.

Decided (not open): findings stay local and uncommitted; performance uses `httptest` fakes only; the machine is shared so measurement is serialized and relative.

Open questions for the user:
1. Reviewer slots R1-R4: which four profiles/models (table 4.1)?
2. Severity bar for security "critical": may a local-user-only issue on a loopback server ever exceed medium? (Assumed: no.)
3. Phase order is 1, 2 (2a, 2b1, 2b2), 4, 3, 5, 6, 7, 8 (section 3.3); confirm.
