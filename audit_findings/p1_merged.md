# Phase p1 merged findings — foundation (app, cmd, catalog, sessionlock, logging, rootsetting, serverconfig)

- phase: `p1`, reviewed at HEAD `a5a2626` (go1.27.3 linux/amd64). The baseline ran at `ab86bef`; 12 commits landed in between (the p4 fixes for M01, M02, M04-M09, M13, M14 and the M05 context plumbing `65fdb84`), so reviewers saw the fixed tree.
- inputs: `p1_r1.md` (299 lines, 21 findings, 9 hypotheses), `p1_r2.md` (179, 6, 6), `p1_r3.md` (177, 10, 7), `p1_r4.md` (177, 4, 6). All four finished before this merge opened any of them. File mtimes (r3 23:13, r2 23:16, r1 00:33, r4 01:46) are consistent with independent work; no reviewer cites another.
- coordinator work: merge and verification only. **No repository file was changed** (`git status --short` shows only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` empty). This section describes the merge step alone: it ran no benchmark, no `-race` and no full suite (step 6.3 did, afterwards, in §13). Reproductions ran in a copy of the tree under `/tmp/coord/bg` (scratch tests, kept in `/tmp/p1-merge-repro/`, see §10).
- load average: 0.61 at start, 8.05 right after the first reproduction builds, 0.25 at the end (12 CPUs). Nothing here is a timing claim.
- **Scope anomaly (needs a decision, §11):** `phase1_prompt.md` lists the p4 packages (`asyncmgr`, `remote`, `remoteapi`, `checkpoint`, 4,246 lines) inside its scope although its header says "8,673 lines in 10 packages". Only r1 read them; r2/r3/r4 did not (r4 says so). Several r1 findings therefore overlap `p4_merged.md` (N02 = M03, N11 = residual of M08, part of N19 = M18, N20 = M19). Reviewers were not shown `p4_merged.md`.

## 1. Counts

| measure | count |
|---|---|
| reviewer findings filed | 41 (r1 21, r2 6, r3 10, r4 4) |
| merged findings | 35 (30 from filed findings + 2 promoted from reviewer cross-phase leads, N31 and N32, + 3 filed from the measurement, N33-N35) |
| filed from the measurement (§13) | 3: N33, N34, N35 (no reviewer filed them; §9's H-E, H-G, H-J describe the areas) |
| reviewer findings not carried | 3: r3-04 (prior art D-7), r3-06 (disproved), r3-10 (informational) |
| consensus (2+ reviewers) | 6: N07, N14 (3 finders each), N19, N20, N21, N26 (2 each) |
| unique (1 reviewer) | 26 |
| by number of finders | 3 finders: 2 · 2 finders: 4 · 1 finder: 26 |
| severity proposed: critical / high / medium / low | 0 / 0 / 7 / 28 |
| severity as filed (reviewer findings): critical / high / medium / low | 0 / 0 / 16 / 25 |
| verification: reproduced in `/tmp` / confirmed by reading source / measured by benchmark (§13) / disproved or dropped | 11 / 21 / 3 / 0 merged (3 reviewer findings dropped) |
| "checked and sound" claims refuted (fully or partly) | 5 (C1, C2, C4, C5, C6 in §5) |
| contradictions recorded | 8 (C1-C8) |
| performance hypotheses | 28 raw (r1 9, r2 6, r3 7, r4 6) → 14 for measurement; 3 (r1 H06-H08) were measured in p4 as P07, P08, P09/P10 and are closed |
| repeats of fixed prior art | 1 (r3-04 = D-7, dropped); 2 re-confirmations of open items (N02 = p4 M03, N07 = B-C9 rest) |

Per reviewer: r1 21 findings (5 consensus, 16 unique; 11 reproduced by me), r2 6 (2 consensus, 4 unique), r3 10 (4 consensus, 2 unique, 3 not carried, 1 folded into N01), r4 4 (1 consensus, 3 unique).

## 2. Merged findings

`found by` lists the reviewer findings that map here. `sev` is proposed; `filed` is what the reviewer(s) said. Status: R = reproduced in `/tmp` (for N33-N35: measured by the §13 benchmark named in `found by`), S = confirmed by reading source. A `-` in `filed` means no reviewer filed the entry.

| id | title | sev | filed | lens | location | found by | status |
|---|---|---|---|---|---|---|---|
| N01 | `HOST=0`, `0.0`, `0x0`, `00`, `0.0.0` pass the wildcard check and bind every interface under the cgo resolver | medium | m | B | `app/config.go:141-175` | r1-01 (r3-07 adjacent) | R |
| N02 | Checkpoint restore deletes files outside the workspace through a symlink it has just recreated (safe mode too) | medium | m | B | `checkpoint/service.go:765-778` | r1-02 (= p4 M03) | R |
| N03 | A killed git (timeout or caller cancel) leaves `snapshot.index.lock`; every later capture and restore of that session fails | medium | m | A | `checkpoint/git.go:159-178,213-235` | r1-03 | R |
| N04 | Restore of a path that changed file↔directory fails after doing its work and skips the remaining removals | medium | m | A | `checkpoint/service.go:769-778` | r1-04 | R |
| N05 | Nested git repositories become one gitlink: restore is silently a no-op, and an empty nested repo fails the whole snapshot | medium | m | A | `checkpoint/git.go:263-276` | r1-05 | R |
| N06 | `Restore` never starts the gc its prune asked for (DECISIONS says it does) | low | l | A | `checkpoint/service.go:673-718,803-816` | r1-06 | S |
| N07 | `Resolver.AsyncHost` opens (and recovers) an async manager under `Resolver.mu` | medium | m,m,hyp | A/C | `app/resolver.go:602-618`; `asyncmgr/manager.go:127` | r1-07, r2-02, r4-H02 | S |
| N08 | `targetQueue.try` is held across `StartProfileRun`, which runs the compaction `BeforeRun` | low | l | A | `app/profile_message_queue.go:230-277`; `httpapi/profile_run.go:119-121` | r1-08 | S |
| N09 | Every sub-agent run builds its own transport when `provider_network` is not tcp | low | l | C | `app/subagent.go:62`; `resolver.go:481-515` | r1-09 | S |
| N10 | A remote entry's `token_file` may name any readable file, `token_env` is limited to `JAD_REMOTE_*` | low | m | B | `remote/entry.go:143-145,264-280` | r1-10 | S (reach unverified) |
| N11 | Unix-socket client checks the socket owner before dialing, not the peer it reaches | low | l | B | `remote/client.go:96-109`; `socket_unix.go:17-33` | r1-11 | S |
| N12 | `ListenUnix` binds a longer staging path, so socket paths of about 99+ bytes are refused | low | l | A | `app/remote_listen.go:63-71` | r1-12 | R |
| N13 | `StartSubagent` select can report a started-and-finished run as "did not start" | low | l | A | `app/remote_api.go:216-239` | r1-13 | S (reach narrow) |
| N14 | Shutdown: new runs are admitted while `Service.Close` waits for storage recovery | low | m,m,l | A | `cmd/jad/main.go:122-130`; `app/service.go:107-128`; `storage_recovery.go:53-54,155-164` | r4-04, r3-03, r1-15 | S |
| N15 | Shutdown has no total bound: async hosts close one after another, two Shutdown deadlines | low | l | A | `app/resolver.go:971-990`; `asyncmgr/manager.go:597-614`; `cmd/jad/main.go:125-131` | r1-14 | S |
| N16 | A failed usage-cache write fails the session list, the snapshot, compaction and every run start | medium | m | A | `app/catalog.go:621-626`; `bound_runner.go:37-46`; `compaction_usage.go:33,56` | r2-01 | R |
| N17 | Session delete holds the resolver-wide `settingsMu` across sequential process-kill waits | low | m | A | `app/session_catalog_files.go:71-84,100-154`; `tools/native/process_manager.go:482-549` | r2-03 | S |
| N18 | The session list creates the state directory of sessions that never ran | low | l | A | `app/catalog.go:234`; `resolver.go:905-908` | r2-04 | R |
| N19 | Dead and test-only code, stale comment | low | l | D | see §3 | r1-19, r2-05 | S |
| N20 | Duplicated helpers: `openBrowser` (2), glob matching (3), atomic writers | low | l | D | `cmd/*/browser.go`; `remote/glob.go:6-16`; `remoteapi/tokens.go:150-162` | r1-20, r3-08 | S |
| N21 | `resolver.go` 1122 lines, `catalog.go` 768, long functions, doc comment on the wrong type | low | l | D | `app/resolver.go:36-38` | r1-18, r3-09 | S |
| N22 | Test gaps for the new commits and sleep-based synchronisation | low | l | D | see §4 | r1-21 | S |
| N23 | `sharedStore` and sub-agent build waiters cannot be cancelled | low | m,m | A | `app/resolver.go:880,661` | r3-01, r3-02 | S |
| N24 | Remote transcript reads have no per-token concurrency bound and rescan the file per page | low | l | B/C | `remoteapi/server.go:647-699`; `app/remote_transcripts.go:88-122` | r1-16 | S |
| N25 | Multi-root: transcript view and message start match different spellings of the session name | low | l | A | `app/remote_transcripts.go:28-47`; `remoteapi/server.go:351-358` | r1-17 | S |
| N26 | `listen_unix_mode` accepts up to 0666 | low | l | B | `sessionfs/remote_api_settings.go:116`; `app/remote_listen.go:73` | r3-05, r2 lead | S |
| N27 | Two concurrent `ListenUnix` calls on one path both succeed; closing one unlinks the other's socket | low | m | A | `app/remote_listen.go:47-80,93-98`; `cmd/jad/main.go:128` | r4-01 | R |
| N28 | An existing log file keeps its mode (0644 stays 0644) | low | l | B | `logging/logging.go:72` | r4-02 | R |
| N29 | The adapter startup line prints `upstream.base_url` verbatim, userinfo and query included | low | l | B | `cmd/anthropic-adapter/main.go:53`; `anthropicadapter/config.go:270-274` | r4-03 | S |
| N30 | `readTurnStarts` holds `turnStartsMu` across a journal read | low | l | A/C | `app/turn_starts.go:41-92,174-181` | r2-06 | S |
| N31 | A forged transcript cursor with a negative `b` panics `pageOfText` (cross-phase, `tools/native`) | low | lead | A | `tools/native/transcript.go:146-152,318`; `transcript_detail.go:79-84` | r1 lead L2 | R (tool level) |
| N32 | `RestoreCheckpoint` runs git on the HTTP request context: a client disconnect kills a restore part-way | low | lead | A | `app/checkpoints.go:25-39`; `httpapi/checkpoint_handlers.go:122` | r2 lead | S |
| N33 | The history block re-reads and re-decodes the whole `messages.backup.jsonl` on every snapshot, keeping only the last `backup_turns` turns | low | - | A | `app/history.go:162-178,222-240,242-317`; `app/catalog.go:398` | §13 H-G | R |
| N34 | Every remote transcript request rebuilds the tool and re-indexes the whole transcript; nothing bounds one request or the number of callers | low | - | A/C | `app/remote_transcripts.go:88-122`; `tools/native/transcript.go:296-306` | §13 H-J (adjacent to N24) | R |
| N35 | The state-context trim re-renders the whole document after every single removed item | low | - | A | `app/state_context.go:207-238` | §13 H-E | R |

Not carried: r3-04 (adapter without caller auth on non-loopback = prior art D-7, §7), r3-06 (disproved, §4), r3-10 (informational, no defect), r3-08/r3-09/r1-18/r1-20 folded into N20/N21.

N33-N35 were not filed by any reviewer: they are the three measured costs in §13 that had no owner finding, filed at low because each is a bounded latency/allocation cost with no data at risk. Every other measured cost belongs to an existing id (H-B → N9, H-D → the N16 area, H-F → N30, H-H → N17, H-L → N15, H-J → N24 for the missing bound, see N34), and H-C, H-I, H-K, H-M, H-N are not worth a dedicated entry (13.3, 13.5). Their evidence is a benchmark rather than a reproduction, which is why their status is R (§13 says which run) and not S.

Severity rule applied: highest evidence, median severity (`audit_plan.md` 6.2), with two deliberate departures: N14 (median would be medium; lowered, see C3) and N10/N23/N27/N17 (lowered from a reviewer's medium for lack of a reachable path, see C8).

## 3. Verification, consensus findings

### N07 (medium, A/C) `AsyncHost` opens the async manager under `Resolver.mu` — r1-07, r2-02, r4-H02 — **confirmed**
- source: `resolver.go:604-617` holds `r.mu` (deferred unlock) across `native.NewAsyncHost` → `asyncmgr.Open` → `m.recover()` (`asyncmgr/manager.go:127`; mkdir, index read, one record read per live/unread row, rewrites). `sharedStore` (`:857-880`) and `sharedSubagentManager` (`:647-668`) already build outside the lock, so this is the one site left from B-C9. `2b9f62c` (2026-10-22) added it after the audit that closed B-C9 (`audit_performance_plan.md:683`, 2026-10-21 notes the constructors had no I/O). r1 and r2 also checked the other constructors (process/task/memory/fetch/mcp): no I/O, agreed.
- severity: medium (three finders). Cost is unmeasured; it is per storage directory, once per process, so I would rate it by the measurement (H-A).
- fix: copy the `subagentBuilds` pattern (S, ~30 lines).

### N14 (low, A) Admission stays open while shutdown waits for storage recovery — r4-04, r3-03, r1-15 — **confirmed, narrower than filed**
- r4: new runs can start after SIGTERM while `Service.Close` waits for the recovery scan (medium). r3: "managers close before the HTTP drain" (medium). r1: close waits for an uninterruptible recovery step (low).
- source: `cmd/jad/main.go:125` calls `service.Close()` first, and `Shutdown` after (`:127-130`). `Service.Close` (`service.go:112-127`) cancels recovery, **waits** `<-s.recovering`, and only then closes `s.remote` and `s.api`. `RecoverStorages` checks `ctx` between sessions and storages only (`storage_recovery.go:53-54,76,156`), and `manager.Recover()` (`:164`) is not cancellable. So the window exists, but it only opens when recovery is still running at SIGTERM and lasts for one storage's recovery.
- r3's wider claim ("managers close while `Serve` still accepts") is true but intended and bounded: `api.Close` is `runRegistry.close` (`httpapi/registry.go:444-455`), which rejects new starts first (`:323-328`) and cancels runs; the resolver then answers 503 (`resolver.go:858-860`). The residual defect is only the ordering inside `Service.Close`.
- severity: **low** (see C3). fix (r4's, S): close `remote` and `api` immediately after `stopRecovery()`, then join the scan.

### N19 (low, D) Dead code — r1-19, r2-05 — **confirmed, list corrected**
- zero non-test references: `usage.go:37 readUsageCache`, `usage.go:123 stringField`, `history.go:319 var _ = transcript.TurnStore{}` (the import is used at `:289`), `remote/glob.go:9 MatchGlobs`, `asyncmgr/types.go:63 ErrNotLive` (no reference anywhere, tests included), `remoteapi/server.go:588 view(..., withText)` (parameter unused), `asyncmgr/prune.go:153 Prune` (grep for `.Prune()` finds nothing outside tests).
- test-only: `asyncmgr/manager.go:441 CancelOwnersWithPrefix`, `:523 Snapshot` (production uses `SnapshotLive`), `app/storage_recovery.go:39 RecoverProcesses` (3 test callers).
- p4 overlap: `ErrNotLive`, `MatchGlobs`, `CancelOwnersWithPrefix`, `Prune` are already p4 **M18**; not new. New in p1: `readUsageCache`, `stringField`, `var _`, `RecoverProcesses`, `view` parameter. r1's "manager.go:67 says it starts no goroutine" is correct: `store.go:146` uses `time.AfterFunc`.

### N20 (low, D) Duplicates — r1-20, r3-08 — **confirmed**
- `cmd/claude-auth/browser.go` and `cmd/openai-auth/browser.go`: `diff` is empty. Glob matching: `remote/glob.go:6`, `remote/entry.go:291-300`, `remoteapi/tokens.go:150-162` (the last rejects `""`, `.`, `..` segments, the others do not; that difference is intentional-looking and untested). Atomic writers: 12 files call `os.Rename` (list in r1-20 plus `task_store`, `memory_store`, `fetch_store`, `process_*`, `subagent_store`); p4 **M19/M33** already cover the asyncmgr/checkpoint pair. With two uses only, no abstraction is warranted for the auth commands.

### N21 (low, D) Size — r1-18, r3-09 — **confirmed**
- `resolver.go` 1122, `catalog.go` 768 (`wc -l`); misplaced comment at `resolver.go:36-38` (the "Resolver composes ..." text sits on `CatalogConfig`; the real `type Resolver` is at `:50`). r4 says the size concern is F-23; `audit_performance_plan.md` P-054 says no whole-file split. Only the comment fix is worth doing.

### N26 (low, B) `listen_unix_mode` up to 0666 — r3-05 (+ r2 cross-phase lead) — **confirmed**
- `sessionfs/remote_api_settings.go:116` range `0o600..0o666`, default `0o600` (`:51`); `ListenUnix` chmods exactly that (`remote_listen.go:73`). An operator who sets 0666 lets any local user reach a token-taking socket; they still need a token. Hardening of an explicit operator choice: low. r2 additionally notes combinations such as 0641 pass the range check.

## 4. Verification, unique findings

### N01 (medium, B) Wildcard bypass `HOST=0` — r1-01 — **confirmed and reproduced**
- Reproduction (`/tmp/p1-merge-repro/host_wildcard_main.go`, plus the real function in a copy of `app/config.go`): `listensOnEveryInterface` returns **false** for `0`, `0.0`, `0x0`, `00`, `0.0.0`, `127.1`, and **true** only for `0.0.0.0`. `net.Listen("tcp", "<h>:0")` with the default Go resolver fails for all of them ("no such host"), but with `GODEBUG=netdns=cgo` it **binds `[::]`** for `0`, `0.0`, `0x0`, `00`, `0.0.0`. The `0x0` default-resolver run even sent a DNS query to 8.8.8.8 for the literal.
- reach: `CGO_ENABLED=1` here (`go env`; `go version -m jad` shows `CGO_ENABLED=1`); `/etc/nsswitch.conf` is `files dns`, and the Go resolver picks cgo on many Linux set-ups and by default on macOS. Remote API enabled, no `listen_unix`, `HOST=0` (or `server.host:"0"`) → remote API and its clear-text bearer tokens on every interface: exactly what `checkRemoteListenHost` exists to refuse (`config.go:167-175`, DECISIONS "Plain http only").
- caveat on severity: operator mistake, not attacker-controlled input; the sample/test table (`config_test.go:129-130`) lists 11 wildcard spellings but none of these. The default pure-Go resolver fails closed. Keep **medium** (r1's rating) because the failure is silent exposure of a token-taking API.
- fix: treat a host made only of digits, dots and `x`/hex as an address candidate: refuse unless it parses with `netip.ParseAddr`; add the five spellings to the table.
- r3-07 (host/port validation only when remote is on) is the same code and is subsumed: with remote off a bad `HOST` fails at `net.Listen`, no exposure.

### N02 (medium, B) Restore deletes outside the workspace — r1-02 — **confirmed, reproduced end to end**
- r1 reproduced only the git/os primitives and left severity open ("promote to high if a Service-level test confirms"). I ran it through `checkpoint.Service` (`/tmp/p1-merge-repro/checkpoint_safe_mode_escape_test.go`, `checkpoint_copy_into_symlink_test.go`): workspace `data` = symlink to `../outside`; run replaces it by a real directory with `x y z`; `Restore` with **default options (mode safe)** returns `Deleted:[data/x data/y data/z]`, err nil, and the three files in the directory **outside the workspace are gone** (0 left). Full mode does the same (`TestCoordSymlinkEscape`).
- mechanism: `restore` (`service.go:765`) runs `checkout-index --force` which replaces directory `data` by the symlink, then `os.Remove(filepath.Join(workspace, change.Path))` (`:769-776`) with only a lexical `strings.HasPrefix` guard (`:771`), so the remove follows the new symlink.
- reach: the model needs write tools plus an existing file at the same relative name outside the workspace; the user presses restore (checkpoints are off by default: `checkpoint.enabled=false`). The delete uses the server's rights and bypasses `PathScope` (DECISIONS: path lists are "not a sandbox"; bash can already delete). Rated **medium**, not high: same-user process, opt-in feature, and bash/exec reach the same files anyway. It is nevertheless a destructive action by the restore button on data the user never asked to touch.
- this is p4 **M03** (low, "restore containment is lexical"), now with a reproduction that shows real deletion. I propose raising M03/N02 to medium; the p4 merge rated it low from a `[read]` trace.
- fix: `Lstat`/`EvalSymlinks` the parent before each remove, or remove before the symlink is checked out; add the test.

### N03 (medium, A) Stale `snapshot.index.lock` disables checkpoints for the session — r1-03 — **confirmed, reproduced through the Service**
- `/tmp/p1-merge-repro/checkpoint_restore_lock_nested_test.go` `TestCoordStaleLock`: with the lock file present, `Begin` returns "" and records `failed: git add exited with 128: ... Unable to create '.../snapshot.index.lock': File exists`; 3 further `Begin`s fail the same; `Restore` fails with the same text.
- reachability (the open question): a Service-level run with a 120 ms context on a 15,000-file workspace (`checkpoint_cancel_lock_test.go`) left the lock behind **twice in two runs**, and the next `Begin` with a live context failed with "File exists". A plain `kill -SIGTERM` of `git add` leaves no lock (git cleans up on TERM); SIGKILL leaves it (checked with a 60,000-file scratch repo). `exec.CommandContext` kills with SIGKILL, which is what a timeout (`checkpoint.timeout_ms`, 120 s) or a stop/disconnect now does since `65fdb84`.
- nothing removes the lock: `prepare` (`git.go:213-235`) does not, and grep for `.lock` in `checkpoint/` finds none. The session then keeps failing until someone deletes the file, with a git message as the only hint.
- note: r1 predicted that `65fdb84` makes this "more frequent"; true, it made the cancel path reachable (before it, only the timeout killed git).
- fix (S): in `prepare`, under the staging mutex, remove `snapshot.index.lock` and `restore.index.lock` (all in-process services share that mutex; two servers on one staging dir are already unsupported).

### N04 (medium, A) Restore of a file↔directory change fails after the work — r1-04 — **confirmed, reproduced**
- `TestCoordFileDirSwap`: `a/x` replaced by file `a`; `Restore(full)` returned `remove .../ws/a: directory not empty` with an empty result, **after** `checkout-index` had already recreated `a/x` (it is readable). A `pre_restore` record with `state=failed` and the warning "failed part-way ... this checkpoint holds the state before it" exists (that part is the p4 M02 fix, working as intended), but the API caller gets an error although the workspace is restored, and any remaining removes are skipped.
- cause: the error filter at `service.go:775` accepts `IsNotExist` and `ENOTDIR` but not "now a directory".
- fix: before removing, `Lstat` and skip a target that is now a directory or is no longer the planned type; test with a file↔dir swap. r1 predicted this correctly.

### N05 (medium, A) Nested git repositories — r1-05 — **confirmed, and the real behaviour is worse in one case**
- `TestCoordNested` with a **committed** nested repo `vend/`: `Begin` succeeds, the run edits `vend/f.txt` and adds `vend/new.txt`, `Finish`, `Restore(full)` returns `Restored:[] Deleted:[]`, err nil: **nothing is restored** (`f.txt` still `agent`, `new.txt` still there), the record says `complete` with no warning. This matches r1 (the nested repo is one `160000` entry).
- extra: with an **empty** nested repo (`git init` only, no commit) `git add --all` fails ("`vend/` does not have a commit checked out", exit 128), so the whole `Begin` becomes `failed` and checkpoints are off for that workspace. r1 did not report this variant.
- no test, DECISIONS or design text mentions nested/embedded/submodule (grep). Reach: vendored clones, `node_modules/.git`-style trees, git worktrees inside the workspace: common for a coding agent. Medium is right: silent no-op undo.
- fix: detect mode `160000` after `snapshot` (already one `ls-files -s -z` away), put a record `Warning`, document.

### N06 (low, A) `Restore` never starts its gc — r1-06 — **confirmed**
- `startPendingReclaim` is called only at `service.go:576` (Begin) and `:621` (Finish). `Restore` appends the `pre_restore` record through `storeRecord` (`:803-816`), which can set `gcPending` (`:813`), and returns without starting the gc. The service is rebuilt per request (`app/checkpoints.go:39`), so the flag is lost with it. DECISIONS (`:150`) says the gc starts "from `Finish`, from `Restore`, or from a `Begin` ...". Doc/code divergence in a feature that landed in `a6dff86` after the p4 merge; low.

### N08 (low, A) `q.try` held across the compaction hook — r1-08 — **confirmed**
- `profile_message_queue.go:231` `q.try.Lock()` … `:258 StartProfileRun(ctx, ...)` … `:277 Unlock`; `StartProfileRun` runs `request.BeforeRun(ctx)` synchronously (`httpapi/profile_run.go:119-121`), and `BeforeRun` is the compaction (`profile_message.go:145-155`), a provider call. A second `Start` for that target blocks on `q.try.Lock()`, which takes no context. Bounded by one compaction; only with `compact: on`. Low.

### N09 (low, C) Per-child transport — r1-09 — **confirmed**
- `subagent.go:62` calls `newProviderStreamer` per run; `providerHTTPClient` (`resolver.go:490-515`) builds `newNetworkTransport` (a clone of the default transport, `provider_network.go:18`) each time for `tcp4/tcp6`; nothing calls `CloseIdleConnections` (grep: only `fetch` does). Main runs cache the runner by fingerprint (`resolver.go:439-454`). With the default network the client is nil and the shared default client is used, so only `provider_network: tcp4/tcp6`, OAuth or `http_debug` pay. Idle connections persist to the 90 s idle timeout. Low until measured (H-B).

### N10 (low, B) `token_file` can name any file — r1-10 — **confirmed as design asymmetry; reach not demonstrated**
- `remote/entry.go:44-47` restricts `token_env` to `JAD_REMOTE_*` explicitly "so it must not be able to name a provider key", while `token_file` (`:143-145`) accepts any path; `Token` (`:264-280`) requires a regular file ≤4096 bytes, mode `0600`-like, ≥32 printable bytes, and the file is re-read on every call. The entry also chooses the destination URL (`:224-230`), so whoever can write `<remotes>/<name>.json` can send a token-shaped file (a one-line API-key file) to a host of their choice.
- the open question was whether the model can write that folder. Not shown: the remotes folder is `.jad/remotes` beside the root setting (`layout_root.go:18`), the HTTP catalog API handles only profiles, models, toolsets and mcp (`catalog_files.go:13-18`), and file tools are limited by `paths` rules (cwd allowed by default, DECISIONS:562) which I did not trace against that folder. DECISIONS also states that `paths` lists are "not a sandbox" and bash/exec can write anywhere the server user can. So the model can already do worse by itself; the incremental risk is a human with write access to the folder only. I downgrade r1's medium to **low** (a [read] hardening with no demonstrated actor); it stays a decision challenge on the asymmetry.

### N11 (low, B) Owner check before dial — r1-11 — **confirmed**
- `remote/client.go:97-109`: `Lstat` + `checkSocketOwner` then a separate `DialContext`. Since `eaa4ea6` the parent may not be group/other-writable (`socket_unix.go:29`), so an attacker needs to own the parent directory; and the dialled path is a hand-written `unix:` remote. The residual is the TOCTOU already recorded in p4 M08 ("medium, hand-configured topology only"). Low; `SO_PEERCRED` after dial is the clean fix.

### N12 (low, A) Staging path too long — r1-12 — **confirmed, reproduced**
- `ListenUnix` binds `<dir>/.jsNNNNNNNNN/s` (`remote_listen.go:63-69`). With the real function: final path length 90 → ok; **99, 100, 104, 107 → `bind: invalid argument`** (`/tmp/p1-merge-repro/app_usage_cache_test.go`, `TestCoordSocketLen`). r1 said 99-107; the threshold is about 8 bytes under the OS limit. The error says `invalid argument`, not "path too long". Low.

### N13 (low, A) `StartSubagent` select race — r1-13 — **confirmed by reading, reach narrow**
- `remote_api.go:216-239`: after the tool goroutine ends with a start failure OR a very fast complete run, `select` over `observed` and `finished` is not prioritised. In `subagent_call.go:132-144`, `observe(...)` runs before `await` and so before `finished`, both channels are buffered (1), so both can be ready when the selecting goroutine is scheduled; Go then picks randomly. A pick of `finished` for a run that did start returns "the subagent tool did not start a run" (`remote_api.go:261`) → 500, `begin` deletes the dedupe entry (`remoteapi/server.go:428-433`), and a client retry with the same `request_id` starts a second sub-agent. Needs a child that ends within a scheduling quantum. Not reproduced; low.

### N15 (low, A) Shutdown total — r1-14 — **confirmed**
- `resolver.go:982-984` closes async hosts sequentially, each up to `StopWait` (default 2000 ms, `asyncmgr/config.go:71`; wait loop `manager.go:597-614`); `cmd/jad/main.go:127,130` applies `ShutdownTimeout` to the socket server and again to the TCP server; `service.Close` has no deadline. Worst case is (hosts × 2 s) + 2 × `shutdown_timeout`. D-3 bounded only `Server.Shutdown`. Low; hypothesis H-L covers the cost.

### N16 (medium, A) Usage-cache write failure is fatal — r2-01 — **confirmed, reproduced**
- `TestCoordUsageCacheWriteFailure` (`usage-cache.json` made a directory): `ReadSessionSnapshot` returns `write session usage cache: replace usage cache: rename ...: file exists`, and `ListSessions` returns the same error for the whole list. r2's chain for the run path also holds: `bound_runner.go:37-46` `nextSessionTurn` → `scanTranscriptIndex` → `writeUsageCacheCheckpoint` error → `session.PersistInputWithImages` returns `read next session turn: ...` (`session/runner.go:195-198`) → the run does not start and the input is not stored, contradicting the invariant stated at `bound_runner.go:47-50`. `compaction_usage.go:33-35,56-58` return an error **after** the transcript was already rewritten.
- cause is real but the trigger is narrow: the cache file replaced by a directory, wrong permissions on that one file, ENOSPC at the wrong moment (the transcript append can succeed while the cache write fails), or a read-only state tree (list works only after state exists). The read side already treats the cache as optional (`readUsageCacheState` ignores any invalid cache). Medium: a cache failure takes down listing and runs of that session, and a one-line fix exists. Never decided: the text dates from the initial port (r2's `git log -S`).
- fix (S): log and continue in `scanTranscriptIndex`; same in the two compaction wrappers.

### N17 (low, A) `settingsMu` across process stops — r2-03 — **confirmed, rated low**
- `session_catalog_files.go:71-72` takes `settingsMu`; `releaseSessionChildren` (`:82`, body `:100-154`) closes MCP and process managers while still holding it; `ManagedProcessManager.Close` (`process_manager.go:525-549`) stops running processes one at a time, `Stop` waits `kill_grace` for TERM and again after KILL (`:482-522`, default 2000 ms, max 60000). A process that ignores TERM, started by the model through the `process` tool, makes a session delete block every settings/catalog write for 4 s per process (up to 120 s at the maximum). Other holders: `session_settings.go:23`, `catalog_files.go:79,93`, `session_catalog_files.go:47`.
- r2 rated it medium. I lower it: the stalled actions are rare settings writes and the cause is a model-started TERM-ignoring process combined with a delete; no data is at risk. Low, S fix (release after `DeleteSessionFile`).

### N18 (low, A) List creates state dirs — r2-04 — **confirmed, reproduced**
- `TestCoordListCreatesStateDir`: with the session's state directory removed, `ListSessions` returns nil and the directory exists afterwards (`sharedStore` → `openSharedSession` → `os.MkdirAll`, `resolver.go:905-908`). DECISIONS:793 says the list must not create state for non-default profiles; the default profile does. Effect: stray empty folders; on a read-only state root the list fails. Low.

### N22 (low, D) Test gaps — r1-21 — **confirmed in the sense that every missing test r1 lists is also a defect I reproduced**
- Missing tests that would have caught defects: N01 (table lacks the five spellings), N03, N04, N05, N02, N12, N13, N16, N18, N27, N28. Sleep counts verified: `remoteapi/server_test.go` 6, `remote/client_test.go` 4, `asyncmgr/manager_test.go` 4, `app/profile_message_async_test.go` 2, `profile_message_goroutines_test.go` 2 (r1 cited lines, not totals). Package vars `providerDial`/`netListen` swapped by tests: present.

### N23 (low, A) Uncancellable build waiters — r3-01, r3-02 (both r3) — **confirmed, rated low**
- `resolver.go:880` `<-entry.ready` (no `select` on `ctx.Done()`) and `:661` `<-build.done`. Waiters block until the one build ends. Builds are bounded file work (store open, manager recovery) and release on panic, so the wait is a latency, not a hang. r3 rated both medium; I rate one low finding. Note N07 is what makes the async side of this build slow.

### N24 (low, B/C) Remote transcript reads unbounded per token — r1-16 — **confirmed**
- `handleTranscript` (`remoteapi/server.go:647-699`) sits outside `MaxRuns/MaxRunsPerToken`; `RemoteAPITranscriptSettings` uses the tool defaults (`ScanMaxBytes` 256 MiB, `sessionfs/native_transcript_settings.go:12`), and each call re-indexes the whole file (`tools/native/transcript.go:296-306`). Authenticated caller, own CPU/disk: low.

### N25 (low, A) Multi-root glob spelling — r1-17 — **confirmed**
- `remoteapi/server.go:351-358` builds the scope from the raw `body.Session`; `tokenProfiles.Profile` checks raw, then canonical (`remote_transcripts.go:28-39`) while `Candidates` ORs both (`:47`). With more than one root the canonical form is `root:session` (`sessionfs/roots.go:makeLocation`), so a glob written for one spelling can start a message but not read the transcript, or list a profile that `Profile` then hides. Single-root deployments are unaffected. Low.

### N27 (low, A) Concurrent `ListenUnix` — r4-01 — **confirmed, reproduced, rated low**
- 200 rounds of two concurrent `ListenUnix` calls on one path (`TestCoordConcurrentListen`): **both succeeded in 193 of 200 rounds** (the later `Rename` silently replaces the earlier listener's path); `unixListener.Close` then removes whatever socket sits at the path (`remote_listen.go:93-97`, checks only `ModeSocket`), and `main.go:128` removes it again. Two `jad` processes started against one config at the same moment is an operator error; the second would also have failed on port bind in the normal TCP case (the TCP bind happens first, `main.go:81`, so a second server dies before reaching `ListenUnix` unless it uses another port). r4 rated medium; I rate low: availability only, needs two servers sharing a socket path and different TCP ports.

### N28 (low, B) Existing log file keeps its mode — r4-02 — **confirmed, reproduced**
- `TestCoordExistingMode`: an existing 0644 file stays `-rw-r--r--` after `logging.New`. `os.OpenFile(..., 0o600)` (`logging.go:72`) sets the mode only on create. DECISIONS:98 says "opened append-only with mode 0600". The log holds provider failure causes and URLs. A local user could read it only if the file was already readable (created by the operator or an earlier version). Low. Fix: `file.Chmod(0o600)` after open.

### N29 (low, B) Adapter startup log prints the upstream URL — r4-03 — **confirmed by reading**
- `cmd/anthropic-adapter/main.go:53` prints `config.Upstream.BaseURL`; `config.go:270-274` accepts any parseable http(s) URL with a host, userinfo and query included. Only an operator who embeds a credential in the URL leaks it, to their own stderr. Low; log only the host.

### N30 (low, A/C) `readTurnStarts` lock scope — r2-06 — **confirmed**
- `turn_starts.go:41` locks `shared.turnStartsMu`, held through `ReadCompleteRecordsRange` with a per-record `json.Unmarshal` (`:56-92`); `invalidateTurnStarts` (`:174-181`) takes the same lock. Per-store, incremental work; r2 rated low. Measure with H-F before touching it.

### N31 (low, A) Forged transcript cursor panics — r1 lead L2 — **confirmed at tool level**
- `decodeTranscriptCursor` rejects negative `Offset` and `End` but not `Bytes` (`tools/native/transcript.go:146-152`); a detail cursor carries `b`, which is passed straight as `offset` (`:318`) into `pageOfText`, whose `text[offset]` indexes negative (`transcript_detail.go:79-84`). Reproduced: a cursor re-encoded with `"b":-5` makes the real tool panic `index out of range [-5]` (`/tmp/p1-merge-repro/native_negative_cursor_test.go`). The tool path is wrapped by `recoverToolPanic` (`tools/catalog.go:406`) when called as a model tool. The remote HTTP route `GET /transcript?cursor=` calls `tool.Execute` directly from `remoteBackend.Transcript` (`app/remote_transcripts.go:122`) with **no** recover, and the remote handler is mounted outside `observeRequest` (`app/remote_listen.go:34-40`), so a token holder with the `transcripts` action can panic the handler; `net/http` recovers per connection, so the server survives, the client gets a dropped connection, and nothing is logged. Not reproduced over HTTP; severity low (authenticated, no server effect). Cross-phase: belongs to `tools/native` (phase 6) for the fix.

### N32 (low, A) Restore uses the request context — r2 lead — **confirmed by reading**
- `httpapi/checkpoint_handlers.go:122` passes `r.Context()`; `app/checkpoints.go:39` hands it to `Restore`; since `65fdb84` every git call is a `CommandContext`. A client that disconnects mid-restore SIGKILLs `git checkout-index`, leaving a half-restored tree (the `pre_restore` record now exists and is marked `failed`, so it is recoverable) and, through N03, a possible stale lock. r2 asked phase 4; p4 M05 was about the opposite (stop/shutdown could not cancel). Low (user closes the browser tab during a restore); the safe shape is to run the restore on a detached context with `timeout_ms`, as `Finish` does (`service.go:616-618`).

### N33 (low, A) Backup tail is re-read and re-decoded in full — §13 H-G (measurement) — **confirmed**
- `readBackupTail` (`history.go:242-317`) streams the whole `messages.backup.jsonl`: every line is JSON-decoded twice (`:281` into a `map[string]json.RawMessage`, then `role`/`type`/`batch` at `:284-295`), and on each eviction the surviving records are **copied into a new slice** and `turnStarts` is rebuilt (`:301-309`). The caller only wants the last `backup_turns` turns (`history.go:162-178`).
- measured (§13 H-G): 500 turns **5.8 ms / 7.4 MB allocated**, 5000 turns **60.1 ms / 77 MB**, i.e. 12 µs per turn and 10.4x the time for 10x the turns — linear in the file, not in the window. The read is unconditional: `readHistoryBlock` (`:222-240`) reads before it computes the ETag, so a 304 costs the same, and every snapshot without a `before` cursor reaches it through `catalog.go:398` (also `ReadSessionHistory` `:46` and `ReadMessageImage` `:147`). Only while `backup_turns` > 0 and a backup exists.
- bounded (60 ms and 77 MB for the longest session in the fixture), no data at risk, but it is a per-request cost on a session the UI polls. Low.
- fix direction (S): read the file backwards in blocks and stop as soon as `backup_turns` turn starts are found, instead of decoding the whole file and copying the window per eviction.

### N34 (low, A/C) Every remote transcript request re-indexes the whole transcript, unbounded — §13 H-J (measurement) — **confirmed**
- `remoteBackend.Transcript` (`remote_transcripts.go:88-122`) reloads both transcript settings (`:97`), constructs the whole tool (`native.NewTranscriptTool` / `NewTranscriptsTool`, `:103-107`), validates the arguments and executes it; the tool stats the file and runs `indexTranscriptTurns` over its entire `size` (`tools/native/transcript.go:296-306`) under the tool defaults (`scan_max_bytes` 256 MiB, `sessionfs/native_transcript_settings.go:12`). A cursor page repeats all of it, so a 20-page walk is 20 full scans.
- measured (§13 H-J, btrfs): **15.5 ms / 9 MB allocated** at 1 MiB, **280 ms / 161 MB** at 20 MiB, **1.36 s / 798 MB** (3.2 M allocs) at 100 MiB — about 13 µs per KiB of transcript and **8x the file size allocated per request**. 8 concurrent requests completed in 31 / 481 / 2253 ms per batch: all of them ran in parallel, with no queue and no refusal.
- relation to N24: N24 files the missing per-token concurrency bound and the per-page rescan; this entry files what **one** request costs, which is what concurrent callers multiply. Same two functions — fix together, and folding N34 into N24 at the fix stage is a reasonable simplification. Authenticated caller spending its own CPU, disk and memory, no cross-tenant effect: low.
- fix direction (S): keep the turn index in a cache keyed by the transcript path and the existing `transcriptIdentity` stamp, and give the remote route its own `scan_max_bytes` instead of the 256 MiB tool default.

### N35 (low, A) State-context trim re-renders the whole document per removed item — §13 H-E (measurement) — **confirmed**
- `state_context.go:207-238`: while the rendered document exceeds `max_bytes`, the loop picks the largest trimmer by re-scanning the provider list (`:210-220`), removes **one** item (`TrimStateContextItem`) or one oldest query (`:222-233`) and re-renders the complete document (`:234`), which marshals every provider again. Cost is therefore items × removed items.
- measured (§13 H-E, `max_bytes` set to 1/5 of the document so about 80% of the items are removed): 10 items **172 µs**, 100 items **10.1 ms**, 1000 items **933 ms** with **1.05 GB allocated** for one snapshot; about 1.1 ms per removed item at 1000 items (≈800 renders for 1000 items).
- needs a document over `max_bytes` to enter the loop at all (configuration), and the browse ends in a failed snapshot when even the query trim cannot shrink it; the item count is the process provider's. Low.
- fix direction (S): trim in batches — derive how many items to drop from the size ratio (or bisect the kept count) and drop that many per pass, so the render count is logarithmic instead of one per item.

## 5. Contradictions and disagreements

Both positions are recorded; the source decides.

### C1 (factual) The remote wildcard-host check — r2, r3, r4 "sound" vs r1-01
- r2 §6: "the wildcard-host refusal parses the address (`netip`, ...) and rejects anything that is not an address or a plain hostname". r3 §6: "`checkRemoteListenHost` fail-closed on wildcard/odd HOST ... sound". r4 §6: "wildcard/invalid `HOST` is rejected for TCP remote API".
- source/repro: `0`, `0.0`, `0x0`, `00`, `0.0.0` are not addresses to `netip` but match `listenHostPattern`, so they count as plain hostnames (`config.go:141,156-159`); under the cgo resolver they bind `[::]`.
- **resolution: r1 is right; three "sound" claims are refuted.** Their claim holds for the pure-Go resolver and for every spelling in the existing test table. N01.

### C2 (factual, partial) `ListenUnix` — r1, r2, r3, r4 "sound" vs r1-12 and r4-01
- all four list the staging-directory design as sound (0700 staging dir, chmod before rename, no umask change); I agree for the mode and umask questions (`remote_listen.go:61-81`).
- but r1-12 (path length) and r4-01 (concurrent publish, close unlinks another listener's socket) are real: reproduced (N12: 99-107 byte paths fail with `invalid argument`; N27: 193 of 200 concurrent rounds both succeeded).
- **resolution: the permission/umask claims stand; the "sound" label does not cover path length or concurrency.** r1 listed both its own finding and the "sound" item, so r1 is internally consistent (it limited the claim to mode/umask).

### C3 (severity) Shutdown ordering — r3-03, r4-04 medium vs r1-15 low vs r2 "Close order sound"
- r3: close-before-drain breaks in-flight runs; r4: admission stays open while recovery is waited on; r1: `Close` waits for an uninterruptible step; r2: order sound, double close harmless (agree: all nested `Close` are idempotent, `resolver.go:935`).
- resolution: the effect is one recovery-storage long, only when SIGTERM arrives during start-up recovery, and runs started in that window are stopped by `api.Close` right after (`registry.go:444-455`). **low** (N14). r2's "sound" holds for what it examined; r3's wider "wrong order" is the intended design (D-3 decided the order: cancel runs, then drain).

### C4 (factual) Usage-cache write — r3 "usage.go ... sound", r2 §6 "scanTranscriptIndex sound" vs r2-01
- r3's list says the checkpoint validation, atomic temp+rename and durability-gated fsync are sound: true for what it names. r2 §6 calls `scanTranscriptIndex` sound and files r2-01 against it: an internal inconsistency inside r2 (its §6 item is about the incremental stamping, r2-01 about error handling).
- repro: a cache path that cannot be written fails the list, the snapshot and the run start (N16).
- **resolution: r2-01 stands; r3's sound claim is correct only for validation and write ordering, not for error handling.**

### C5 (factual) "B-C9 closed" — r3-01 (prior art line) vs r1-07, r2-02, r4-H02
- r3-01 says B-C9's "rest" was "closed by Phase 4 measurement". `audit_performance_plan.md:683` closed it for the constructors **that existed on 2026-10-21**; `AsyncHost` came on 2026-10-22 (`2b9f62c`) and does file I/O under the lock (`resolver.go:602-618`).
- **resolution: r1, r2, r4 are right; r3's prior-art statement is refuted for AsyncHost.**

### C6 (factual, partial) Checkpoint context plumbing — r3 "M05 addressed in phase" vs r2 lead, N03, N32
- r3 §6: `Restore(ctx, ...)` takes the caller ctx, "M05 addressed". True: the parameter exists. But `65fdb84` also means a cancelled request context SIGKILLs git (`git.go:167`, `CommandContext`) mid-write, which leaves the stale lock (N03) and a half-restored tree (N32), and r3 did not check that.
- **resolution: plumbing is correct and necessary; its consequences were not examined by r3. Not a refutation of M05, but "addressed" is incomplete.**

### C7 (factual) Remote sub-agent detached context — r3-06 low vs behaviour
- r3-06: the run is detached from the caller's ctx and relies on the remote server to `Stop`. Checked: `remote_api.go:216` uses `context.Background()` on purpose (the POST that starts a run returns while the run continues; `begin` passes the request context only to the start call, `remoteapi/server.go:426`). Stop comes from `/runs/<id>/stop` (`:571`), `Server.Close` (`:188-204`), and the agent's own `timeout_ms` (`subagent_manager.go:604`, default 600000 ms per `native_subagent_settings.go:21`), not "1h `runTimeout`" as r3 says (that is the message path, `remote_api.go:109-113`).
- **resolution: disproved as a defect; not carried.** r3's own "no goroutine leak" remark is right.

### C8 (severity) Departures from the filed rating
- raised: N02 low (p4 M03) → medium (end-to-end deletion shown, default mode).
- lowered: N10 medium → low (no demonstrated actor, DECISIONS says path lists are not a sandbox); N17 medium → low (settings writes stall, nothing is lost, trigger needs a TERM-ignoring model process); N23 two mediums → one low (bounded latency, not a hang); N27 medium → low (needs two servers sharing a socket path; second TCP bind usually fails first); N14 medium → low (C3).
- kept: N07 medium (3 finders), N16 medium (reproduced, run start blocked), N01 medium.

## 6. Reviewer-pair agreement

Counts are merged findings co-filed by both reviewers (a perf hypothesis on the same code counts when no finding exists).

| pair | agreed | ids |
|---|---|---|
| r1–r2 | 2 | N07, N19 |
| r1–r3 | 3 | N14, N20, N21 |
| r1–r4 | 2 | N07 (hypothesis), N14 |
| r2–r3 | 0 (+1) | N26 (r2 only as a cross-phase lead) |
| r2–r4 | 1 | N07 (hypothesis) |
| r3–r4 | 1 | N14 |

| reviewer | findings | consensus / unique | rated medium / low | own reproductions | refuted by this merge |
|---|---|---|---|---|---|
| r1 | 21 | 5 / 16 | 7 / 14 | yes (`/tmp/r1`: resolver bind test, socket length, git lock, symlink, nested repo, panic) | 0 findings; "sound" items fine |
| r2 | 6 | 2 / 4 | 3 / 3 | none | 0 |
| r3 | 10 | 4 / 2 (+1 adjacent, 3 not carried) | 4 / 6 | none | r3-06 disproved (C7); C5, C6; "wildcard sound" (C1) |
| r4 | 4 | 1 / 3 | 2 / 2 | none | "wildcard sound" (C1) |

Reading: r1 produced all five medium reproducible defects in the checkpoint/config area that nobody else saw (N01-N05), by building scratch programs; r1 read the p4 packages the other three did not. r2 found the one app-internal medium correctness defect nobody else saw (N16, error path, the cache invariant) and was the most accurate on severity. r3 filed the most `[read]` mediums (5) and two findings were prior-art confusions (r3-01 B-C9, r3-04 D-7). r4 had the thinnest coverage (below) but its two security items (N28, N29) are valid and N27 is real. All three "sound" refutations (C1) came from reviewers who did not construct the input. Consensus did not mean correctness here: three reviewers called the wildcard check sound together (a shared blind spot, as `audit_plan.md` §8 warned).

## 7. Prior art handling

- **D-7** (adapter open relay, ACCEPT-LOW; fix: startup warning, which exists at `cmd/anthropic-adapter/main.go:68-77`, `f0ecac6`). r3-04 asks again for a refusal or caller token; `audit_audit.md` §11 already rejected the refusal (breaks a container bind) and recommended an opt-in token, not yet done. **Dropped as a repeat**; N29 is a different sink.
- **B-C9 rest**: N07 (re-confirmed, new site, cited by old id).
- **B-C2** (session lock poll), **B-C4** (catalog symlinks): rejected; not re-raised by anyone.
- **D-3** (shutdown deadline): fixed; N14/N15 are different (admission during recovery; total duration).
- **p4 M03** (restore containment): N02 supersedes with a reproduction; **p4 M08** (socket TOCTOU): N11 is the residual; **p4 M18/M19/M33**: N19/N20 overlap (listed).
- **P-051** dead code (fixed `77b7213` for `stringFieldRaw`): N19 lists the remaining items. **F-23/P-054** (no file split): N21 agrees, only the comment fix.
- **DECISIONS.md:793** (list creates no state for other profiles): N18 extends it to the default profile.

## 8. Ranked list (severity, then evidence, then finders, then effort)

Evidence order per `audit_plan.md` 6.4: reproduced > read-confirmed > unmeasured. No critical or high item.

| # | id | sev | evidence | finders | fix | effort |
|---|---|---|---|---|---|---|
| 1 | N02 | medium | reproduced, default mode | 1 (+p4 M03: 2) | `Lstat`/`EvalSymlinks` the parent before each `os.Remove`; test with a swapped symlink | S |
| 2 | N03 | medium | reproduced (Service-level cancel) | 1 | remove stale `*.index.lock` in `prepare`, under the staging mutex | S |
| 3 | N05 | medium | reproduced | 1 | detect `160000`, warn, document; handle the empty-repo `add` failure | S |
| 4 | N04 | medium | reproduced | 1 | skip a remove whose target is now a directory | S |
| 5 | N16 | medium | reproduced | 1 | log and continue on a failed cache write (3 sites) | S |
| 6 | N01 | medium | reproduced (cgo) | 1 (+r3-07 adjacent) | refuse digit/hex/dot hosts that are not parseable addresses; extend the table | S |
| 7 | N07 | medium | read, 3 finders | 3 | `subagentBuilds` pattern for `AsyncHost` | S |
| 8 | N06 | low | read, doc/code divergence | 1 | `defer s.startPendingReclaim()` in `Restore` | S |
| 9 | N14 | low | read | 3 | close admission before joining the scan | S |
| 10 | N31 | low | reproduced (tool level) | 1 | reject negative `Bytes` in `decodeTranscriptCursor`; test | S |
| 11 | N28 | low | reproduced | 1 | `file.Chmod(0o600)` | S |
| 12 | N18 | low | reproduced | 1 | no `MkdirAll` for a session that never ran | S |
| 13 | N12 | low | reproduced | 1 | short staging name or an up-front length check with a clear message | S |
| 14 | N32 | low | read | 1 (+lead) | restore on a detached, `timeout_ms`-bounded context | S |
| 15 | N17 | low | read | 1 | narrow `settingsMu` | S |
| 16 | N27 | low | reproduced | 1 | no-replace publication; own-inode check on close | M |
| 17 | N10 | low | read | 1 | confine `token_file` or decide the asymmetry is intended | S |
| 18 | N13 | low | read | 1 | prefer `observed` before `finished` | S |
| 19 | N23, N08, N09, N11, N15, N24, N25, N26, N29, N30 | low | read | 1-2 each | see §4 | S-M |
| 20 | N19, N20, N21, N22 | low | read | 1-2 each | delete dead code; fix the comment; add the tests above | S |
| 21 | N33, N34, N35 | low | measured (§13) | none (§9 H-G, H-J, H-E) | see §4 for each fix direction: backward tail read, index cache + remote `scan_max_bytes`, batched trim | S |

Dependency notes for the fix stage: N02, N03, N04, N05, N06, N32 all edit `checkpoint/service.go` and `git.go` (split as in p4: restore correctness vs repository lifecycle); N01 and N26 are the only config-load changes (schema/sample/DECISIONS rules apply); N07 and N23 both touch `resolver.go` waiter code; N14 and N15 touch `cmd/jad/main.go` and `service.go`. The three measured entries arrived after the ranking was made and are placed on evidence, not severity (row 21): N33 and N35 are self-contained (`history.go`, `state_context.go`), N34 shares its code with N24 and should be fixed in the same change. None of these changes `messages.jsonl`. N16 changes behaviour on a read path: `messages-golden-diff` is not implied (the cache file format is unchanged), but the usage-cache tests should be re-run.

## 9. Performance hypotheses for the measurement step (deduped; results in §13)

14 hypotheses. Existing benchmark names were checked in the tree at this HEAD; `new` means it has to be written in a `/tmp` copy.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| H-A | `AsyncHost` open serialises unrelated lookups behind async recovery | `app/resolver.go:602-618`; `asyncmgr/recover.go` | 100 sessions × 2 `.async` folders, 100 unread + 200 terminal rows each; 8 concurrent `Resolve`; also the rebuild case (index deleted) | existing `BenchmarkOpenFromIndex` / `BenchmarkOpenRebuild` (`asyncmgr/scale_test.go:592-593`) + new resolver-level contention benchmark | r1 H01, r2 H5, r4 H02 |
| H-B | per-child provider transport and handshake with `provider_network` tcp4/tcp6 | `app/subagent.go:62`; `resolver.go:481-515` | 50 sequential sub-agent runs against an `httptest` TLS server | new, count accepted connections | r1 H02 |
| H-C | session-list poll residual: stat per stamped path, model-file reads and parses (`model_options`, active model twice), per-profile stat/scan on stamp change | `app/catalog.go:216,245,277-291,298-340,630-654,726-747` | 200 sessions × 3 profiles × 3-25 listed models, conditional poll at 5 s | existing `BenchmarkListManySessions` (conditional/200), `BenchmarkListSessionsProfiles`, `BenchmarkListSessions`; extend the fixture with listed models | r2 H1/H4/H6, r3 H01/H02, r4 H01/H11 |
| H-D | the read path writes and fsyncs `usage-cache.json` whenever the transcript grew | `app/catalog.go:621-626`; `usage.go:129-212` | 200 sessions each with one new turn per poll; durability `turn` and `strict` | existing `BenchmarkScanTranscriptIndexRebuild` (`app/usage_scan_benchmark_test.go:14`) + fsync count; p4 measured 5.8 ms per fsync on btrfs | r2 H2, r3 H04/H07 |
| H-E | state-context trim loop re-renders the whole document per removed item | `app/state_context.go:207-238` | provider documents 4-10× `max_bytes` with 10/100/1000 items | new `BenchmarkBuildStateContextSnapshot` | r2 H3 |
| H-F | `readTurnStarts` serialises concurrent readers of one session | `app/turn_starts.go:41-92` | list + snapshot + message page on one session, 100 turns appended | existing scan benchmarks, concurrent variant | r2-06 (N30) |
| H-G | `readBackupTail` reads the whole backup every call and copies the window per evicted turn | `app/history.go:242-317,126-177` | backup of 5,000 turns, `backup_turns` 50, N image reads | new `BenchmarkReadBackupTail` | r3 H03, r4 H09 |
| H-H | `settingsMu` held across catalog/session file writes and across process-kill waits | `app/catalog_files.go:79,93`; `session_catalog_files.go:47,71` | 2-8 concurrent writers; delete with a TERM-ignoring process | new, direct `Resolver` calls | r3 H05 (N17) |
| H-I | per-run tool catalog build, MCP `LoadTools`, `fingerprintRunner` marshal+sha256 | `app/resolver.go:426-455` | back-to-back runs, 5 vs 50 tools | existing `BenchmarkRunStartLockAndResolve` (`app/runstart_benchmark_test.go:12`), `BenchmarkLoadTools` (`tools/mcp/manager_benchmark_test.go:42`) | r3 H06 |
| H-J | each remote transcript request rebuilds settings and the tool schema, and rescans the file per page | `app/remote_transcripts.go:97-122`; `tools/native/transcript.go:296-306` | 10 GET/s, 1 MiB and 100 MiB transcript, 20 cursor pages | new `BenchmarkRemoteTranscriptRequest` | r1 H03/H04 |
| H-K | `profileMessenger.Profile` loads full session settings once per profile; `transcripts` listing calls it N times | `app/transcript_profiles.go:35`; `tools/native/transcripts_list.go:198` | session with 20 profiles, 10 listings | new | r1 H05 |
| H-L | async hosts close sequentially, each up to `stop_wait_ms` | `app/resolver.go:982-984`; `asyncmgr/manager.go:597-614` | 50 hosts with one handle whose driver never reports | new `BenchmarkResolverCloseStuckHandles` (stub driver) | r1 H09 (N15) |
| H-M | double settings preflight per `profile_message` start (Resolve in `Prepare`, again in `StartProfileRun`) | `app/profile_message.go:84-90`; `httpapi/profile_run.go:89-113` | burst of starts to many targets with a large catalog | new, fake starter | r4 H10 |
| H-N | OAuth source `Load()` on every outbound request | `app/oauth_client.go:18-45` | OAuth run with 100 provider rounds | existing `BenchmarkSourceLoad` (`openaioauth/oauth_benchmark_test.go:22`) | r4 H12 |

Closed by p4 (not re-measured): r1 H06 = P07, r1 H07 = P08, r1 H08 = P09/P10. Owner findings: H-A → N07, H-B → N9, H-D → the N16 area, H-F → N30, H-H → N17, H-J → N24, H-L → N15; **H-E, H-G and H-J had no owner finding**, so the measurement filed them as N35, N33 and N34 (§2, §4). H-C, H-I, H-K, H-M, H-N are costs without a finding and stay that way (§13.3, §13.5).

Suggested order (all serialised, relative, `-race` once on `internal/app` and `internal/checkpoint` at the end): H-A, H-C, H-D, H-G first (on the default path, polled by the UI), then H-I, H-F, H-E, then the remote ones (H-J, H-K) and H-B, H-L, H-H, H-M, H-N last (cold paths).

## 10. Verification log (what I ran)

Repo state: `git status --short` before and after: only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` 0. No code, config or git state was changed.

Reproductions, all in a copy of the module under `/tmp/coord/bg` (binaries and original tests excluded, original tests copied where they provide fixtures), scratch tests saved in `/tmp/p1-merge-repro/` for the fix step:
- `host_wildcard_main.go` (standalone, default and `GODEBUG=netdns=cgo`) + `TestCoordRealWildcard` against the real `listensOnEveryInterface` (N01).
- `checkpoint_*_test.go`, six tests through `checkpoint.Service`: symlink escape in full and in default (safe) mode, copy-into-replaced-symlink, file↔directory swap, stale lock (3 `Begin`s + `Restore`), cancel-with-120 ms-context on 15,000 files, committed and empty nested repo (N02-N05, N32 context). Plus a 60,000-file scratch `git add` killed with TERM (no lock) and KILL (lock left).
- `app_usage_cache_test.go`: usage-cache-as-directory on `ReadSessionSnapshot` and `ListSessions`; state-dir creation by `ListSessions`; `ListenUnix` at final path lengths 90/99/100/104/107; 200 rounds of two concurrent `ListenUnix` (N16, N18, N12, N27).
- `native_negative_cursor_test.go`: forged `"b":-5` detail cursor against the real transcript tool (N31); `pageOfText` direct.
- logging: existing 0644 file after `logging.New` (N28).
Everything above was run with `go test ... -count=1` on one package at a time, no `-race`, no `-bench`. The first compile after copying took about 17 s (load average peaked at 8).

Source reads at `a5a2626`: `app/config.go:50-180`, `remote_listen.go` (whole), `remote_api.go:60-275`, `remote_transcripts.go:20-125`, `service.go:60-135`, `shutdown.go`, `resolver.go:436-458,478-520,598-720,840-995`, `catalog.go:226-335,600-630`, `usage.go:125-215`, `bound_runner.go:30-60`, `compaction_usage.go:14-62`, `session_catalog_files.go:66-155`, `history.go:242-317`, `turn_starts.go:36-100`, `profile_message_queue.go:218-290`, `profile_message.go:80-160`, `storage_recovery.go:36-90,150-175`, `subagent.go:50-75`, `transcript_profiles.go:16-79`; `checkpoint/service.go:100-215,513-535,566-624,673-830`, `git.go:150-300`; `remote/entry.go:36-52,135-150,255-300`, `socket_unix.go`, `client.go:90-115`; `remoteapi/server.go:385-445,545-575,585-700`, `tokens.go:145-160`; `asyncmgr/manager.go:569-620`, `recover.go:1-40`; `tools/native/transcript.go:127-160,200-330`, `transcript_detail.go:79-140`, `subagent_call.go:85-200`, `process_manager.go:480-552`; `logging/logging.go:55-95`; `cmd/jad/main.go:55-139`, `cmd/anthropic-adapter/main.go:25-77`; `anthropicadapter/credential.go:112-126`, `config.go:270-280`; `sessionfs/layout_root.go`, `file_settings.go:101-175`, `native_transcript_settings.go:138-166`; `httpapi/registry.go:444-455`, `request_observer.go:70-100`, `profile_run.go:86-135`, `checkpoint_handlers.go:118-130`. Prior-art greps in `audit_audit.md`, `audit_performance_plan.md`, `DECISIONS.md` (lines 98, 140-150, 793, 825-832, 1037-1053).

## 11. Coverage gaps, unverified items and decisions

### Coverage gaps
- **r4 coverage is thin**: 4 findings; its report is three appended passes (sections 3, 8 and 9 repeat each other), it says `remoteapi` internals were not audited, and it has no mention of `rootsetting` or `serverconfig` (0 hits) and none of `bound_runner`, `checkpoints`, `compaction_usage`, `front_compact`, `maintenance`, `profile_tools`, `run_mutex`, `locked_config`, `session_catalog_files`, `session_settings`, `skipped_sessions`, `state_context*`, `storage_recovery` beyond one line. It ran the most targeted tests (all exit 0) but found nothing in the area r1 covered best.
- **r1 left app files unread** (its §7): `usage.go`, `front_compact.go`, `turn_starts.go`, `state_context.go`, `state_context_user.go`, `maintenance.go`, `check.go`, `provider_retry.go`, `image_resize.go`, `filestat*.go` (except `filestat.go`), `skipped_sessions.go`, `session_settings.go`, `catalog_files.go`, `compaction_usage.go`, `history.go` beyond line 140, most of `catalog.go`, `run_hooks.go`, the windows/other build variants of `remote_listen.go`, `sessionlock` windows/unsupported, `serverconfig/defaults.go`, `rootsetting`, `catalog` beyond two files, and `cmd/openai-auth` (compared by `diff` only). r2 and r3 claim to have read all of `internal/app`, so the files are covered at least twice except as below.
- **Files no reviewer discusses in a finding or a sound-list**: `resolver_options.go` (20 lines, I read it: nothing), `image_resize.go` (19, I read: nothing), `run_hooks.go` (53, I read: nothing; the hook runs `native.FireRunEvent` with `config.Model.APIKeyEnv`, which is outside this phase), `state_context_user.go` (61, not read by me), `filestat_linux.go`/`filestat_other.go` (not read by me), `front_compact.go`, `skipped_sessions.go` (r2 only), `compaction_usage.go` (r2 only). `windows`/non-unix build tags of `sessionlock` and `remote_listen`: nobody.
- **Priority range**: `transcript_profiles.go` (0c37f87..ad1018c) was read by r1 only (I checked `Profile`/`Candidates`: no defect; the glob handling in `remote_transcripts.go` is N25). `umask_*.go` no longer exist (r1). The p4 packages inside the p1 prompt were read by r1 only.
- **No reviewer or I ran the app tests under the race detector** (not allowed); no data race claim in this file is `-race` backed.

### Unverified (stays in the list, flagged)
- N10: whether the model can write `.jad/remotes` through its default tool scope (file tools allow the cwd by default; I did not trace the path rules against that folder).
- N13: not reproduced (needs a child that ends inside one scheduling quantum).
- N14: the admission window was read, not provoked (no concurrency test).
- N17: the delete stall was traced, not timed.
- N31: reproduced at the tool, not over HTTP; the missing recover on the remote route is read, not executed.
- N32, N09, N15, N24, N25, N29, N30: read only at merge time; §13.4 measured or reproduced every one of them.
- N01: reach depends on the resolver mode of the actual deployment host (cgo vs pure Go) and on an operator typing a short host; I confirmed the binding behaviour on this machine only (`nsswitch hosts: files dns`, `CGO_ENABLED=1`).
- N05: the empty-nested-repo failure is mine; r1 did not see it. Both cases were reproduced only with git 2.56.0.
- None of the 14 performance hypotheses was measured at merge time; §13 measures all of them (13 confirmed, H-C partial) and checks every item in this list: N9 via H-B, N13, N14, N15 via H-L, N17, N24 via H-J, N25, N29, N30 via H-F, N31 over HTTP, N32, and N10 resolved to "the model can write its own `.jad/remotes` but nothing outside the workspace".

### Decisions needed from the user
1. **Scope overlap.** `phase1_prompt.md` put the p4 packages in p1 scope. Do N02/N11/N19/N20 stay in the p1 list, or are they folded into the p4 ids (M03, M08, M18, M19)? My proposal: keep N02 as the new, higher-rated entry for M03 and fold the other three.
2. **N02 rating.** Raise the restore-through-symlink item (p4 M03) from low to medium on the reproduction, or keep low because checkpoints are opt-in and bash can do the same?
3. **N01 behaviour.** Refuse any host that is not a parseable address or a name with a letter, or only add the five spellings to the table? (The first is stricter and closes `127.1`-style forms too.)
4. **N16 design.** May a failed usage-cache write be logged and ignored (the cache is optional on read)? It changes error behaviour of the list, snapshot, compaction and run start.
5. **N10 asymmetry.** Confine `token_file` to the remotes folder (or a dedicated tokens folder), or record "any file the server can read, by design" in DECISIONS?
6. **N26.** Cap `listen_unix_mode` at 0660 or keep 0600..0666 with a startup warning?
7. **N05.** Warn and document nested repositories only, or also snapshot them (a bigger design change)?
8. **Measurement go-ahead** for the 14 hypotheses in §9 — answered after this section was written: all 14 were run and reported in §13. What is still open is only whether a machine with a quieter disk/network is wanted for the two numbers this one cannot give (a TLS handshake per child run, and a non-btrfs fsync).

## 13. Measurement results (step 6.3, H-A to H-N)

*(§12 is skipped: `p4_merged.md` uses it for an input table, which this file does not carry; the inputs are in the §1 head.)*

### 13.1 Preconditions and method

- Machine: 12 CPUs (AMD Ryzen 5 3600), go1.27.3 linux/amd64, git 2.56.0, go build cache warm. HEAD `a5a2626` (unchanged; `git status --short` before and after: only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` empty).
- **Filesystems:** `/tmp` is **tmpfs** (19 GiB), the workspace disk is **btrfs** (`/dev/sda2` on `/`, `rw,noatime,compress=zstd:3,ssd,space_cache=v2`, SSD per `/sys/block/sda/queue/rotational` = 0). CPU- and lock-bound hypotheses ran on tmpfs; every I/O-sensitive one (H-D, H-J, plus the asyncmgr fixtures) ran with `TMPDIR=/home/www/Downloads/.p1disk/...` on btrfs, i.e. fixtures and writes on a real disk, as §14 of `p4_merged.md` did. The btrfs scratch directory is deleted.
- **Load average** (12 CPUs, recorded around every group): 0.85 / 2.16 at the baseline, 0.8-2.4 during the tmpfs benchmarks, 1.1-3.8 during the disk runs, 1.9 at the end. Every figure below is relative (ratios, per-item costs) and none is a hard real-time claim; the fastest/slowest spread of the accepted rows is 1.0-5.7% (H-G/500-turns is the only row above 5%; see the per-row notes).
- **No repository file was changed.** The graded tree was copied byte-identically to `/tmp/p1m/bg` (`rsync -a`, `diff -r` against `backend-go/` clean; three prebuilt binaries excluded) and the fourteen scratch benchmark/probe files live only there: `internal/app/p1m_app_bench_test.go`, `p1m_io_bench_test.go`, `p1m_checks_test.go`, `p1m_n25_test.go`, `p1m_fill_test.go`, `p1m_probe_test.go`, `p1m_probe_hd_test.go`, `p1m_probe_hd2_test.go`, `p1m_probe_hc_test.go`; `internal/asyncmgr/p1mbench_test.go`; `internal/tools/native/p1m_n10_test.go`, `p1m_n17_test.go`; `internal/checkpoint/p1m_n32_test.go`; `cmd/anthropic-adapter/p1m_n29_test.go`. All `gofmt`- and `go vet`-clean before use. No process was killed or restarted; nothing listened on a fixed port (only `httptest` ephemeral ports and `127.0.0.1:1` as a dead upstream).
- Benchmarks: `go test <pkg> -run '^$' -bench '<name>' -benchmem -benchtime=0.3s -count=10`, one at a time, n=10 unless a row says otherwise; statistics from the raw `ns/op` lines (`spread = (max-min)/mean`). The `H-A` rows use `-benchtime=3x` and the 100 MiB `H-J` row `-benchtime=2x` (one iteration costs a second). Syscall counts come from `strace -f -c` on a prebuilt `go test -c` binary; no profiler was used.
- Baseline (in the repository, read-only, one command at a time), all at `a5a2626` over the 14 phase-1 packages (`internal/{app,asyncmgr,catalog,checkpoint,logging,remote,remoteapi,rootsetting,serverconfig,sessionlock}` + `cmd/{jad,anthropic-adapter,claude-auth,openai-auth}`):

| command | exit | wall | result |
|---|---|---|---|
| `go build <14 pkgs>` | 0 | 14.0 s | empty |
| `go vet <14 pkgs>` | 0 | 3.4 s | empty |
| `gofmt -l cmd internal` | 0 | 1 s | 0 files |
| `go test -count=1 <14 pkgs>` | 0 | 45.0 s | 13 ok, 1 no test files (`serverconfig`), 0 FAIL |
| `go test -race -count=1 <14 pkgs>` | 0 | 118.6 s | 13 ok, 1 no test files, 0 FAIL, **0 `DATA RACE`** (`grep -c` = 0) |
| `git diff --check` | 0 | - | empty |

  Per-package plain / `-race`: app 40.6/61.5 s, asyncmgr 1.0/2.8, catalog 0.01/1.1, checkpoint 5.4/6.8, logging 0.01/1.0, remote 2.3/3.4, remoteapi 1.0/2.2, rootsetting 0.01/1.0, sessionlock 0.3/2.3, cmd/jad 0.02/1.1, cmd/anthropic-adapter 0.01/1.0, cmd/claude-auth 0.09/1.1, cmd/openai-auth 0.12/1.1.

### 13.2 Results by hypothesis

Verdicts use the §9 wording; "confirmed" means the mechanism and the direction of its cost are measured, not that the absolute number travels to another machine.

| id | workload measured (n=10 unless noted) | numbers | per-item cost | verdict |
|---|---|---|---|---|
| H-A | `BenchmarkP1HAAsyncHostContention`: 8 `.async` folders with 100 unread + 200 terminal rows, each opened through `Resolver.AsyncHost` on a **fresh** resolver; 8 folders × 300 records, sequential and 8-concurrent, and 8 concurrent on 8 resolvers (3x, n=3) | sequential 52.7 ms; 8 concurrent on **one** resolver 52.4 ms; 8 concurrent on **8** resolvers 11.4 ms (4.6x faster) | one folder open = **6.6 ms** (probe: 301 files, 6.5-6.8 ms) | **confirmed**: `Resolver.mu` is held for the whole open, so 8 unrelated opens gain nothing from concurrency; with a lock per resolver they finish in the time of one |
| H-A (asyncmgr level) | `BenchmarkP1HAOpenAcrossDirs`: 100 sessions × 2 folders, 200 dirs × 300 rows | open-from-index 200 dirs **500 ms** sequential vs **178 ms** with 8 goroutines; rebuild (index removed) **763 ms** sequential | 2.5 ms per folder from the index, 3.8 ms per folder rebuilt | **confirmed** as a cost: a restart that opens every session's folder pays 0.5-0.8 s, and each *single* open is 2.5-6.6 ms under the resolver lock |
| H-B / N9 | `TestP1HBChildTransportConnections` + `BenchmarkP1HBChildTransport`: 50 sequential requests to an `httptest` server, one `providerHTTPClient` per "child run" (`provider_network.network` = tcp4) vs one shared client | fresh client per run: **50 accepted connections** for 50 runs, 0.15 ms/run; one client: **1 connection**, 0.05 ms/run. Benchmark 147 µs vs 50.5 µs (spread 2-6%) | +0.10 ms and +1 connection per child run, on loopback **plain HTTP** | **confirmed**: no connection is reused across child runs; the TLS handshake is not in this number (self-signed fixture), so on a real provider the delta is larger |
| H-C | `BenchmarkP1HCList`: 200 sessions × 3 profiles × 3 or 25 listed models, polled conditional (ETag) and unconditional; plus `strace -f -c` on one poll of each kind | conditional **10.7 ms** (3 models) / **17.3 ms** (25 models); unconditional **39.2 ms** / **105.0 ms**; 19-262 k allocs per poll | per session on the 304 path: **16 `newfstatat` + 4 `statx`** (measured: +3,200 fstat and +800 statx for 200 sessions); on the rebuild path additionally 8 `openat` + 10 `read` per session | **partial**: the mechanism is exactly as filed (a stat per stamped path, every model file read and parsed twice per session, per-profile stamp/scan), but at a 5 s poll it is 0.2-0.4% of one core; it is not a load problem at 200 sessions, and 25 listed models triple the rebuild path |
| H-D | `BenchmarkP1HUsageCacheWrite` on btrfs (`TMPDIR` on the disk): 20 sessions, one new turn appended to each before every poll, durability `process` / `turn` / `strict`; `strace -f -c` counts `fsync`/`renameat` | per poll of 20 grown sessions: **66.7 ms** / **181.8 ms** / **413.4 ms**; fsync per poll 140 / 160 / 180; renameat 140 per poll | **+5.75 ms per usage cache** (`turn`), **+11.6 ms** more for the directory sync (`strict`); 7 renames + 7 fsyncs per session per poll in total | **confirmed on a real disk**, and it is the dominant cost of a grown poll: scaled to 200 sessions a single poll costs **1.8 s** (`turn`) or **4.1 s** (`strict`) of metadata I/O every 5 s, against 0.67 s with no fsync at all |
| H-E / N35 | `BenchmarkP1HEStateContextTrim`: process provider with 10 / 100 / 1000 items, `max_bytes` set to 1/5 of the rendered document so ~80% of the items are removed one at a time | 10 items **172 µs**; 100 items **10.1 ms** (59x for 10x); 1000 items **933 ms** (92x for 10x more) | ≈1.1 ms per removed item at 1000 items (about 800 renders for 1000 items); **1.05 GB allocated** for one snapshot (spread 2.4-3.4%) | **confirmed as superlinear**: the whole document is re-marshalled after every removed item, so cost grows with items × removed items |
| H-F / N30 | `BenchmarkP1HFTurnStarts`: one session, 100 turns, `readTurnStarts` serial vs 8 concurrent readers of the same store | serial **105.8 µs**; 8 concurrent **878.9 µs wall** (8.3x) | 106 µs per call, 110 µs per call with contention | **confirmed**: `turnStartsMu` serialises readers; the absolute cost (106 µs for an *incremental* read) is the surprise, not the contention |
| H-G / N33 | `BenchmarkP1HGBackupTail`: 500 and 5000-turn `messages.backup.jsonl`, `backup_turns` 50 | 500 turns **5.8 ms**, 7.4 MB allocated; 5000 turns **60.1 ms**, 77 MB allocated (10x turns = 10.4x time) | 12 µs and 12 µs per turn: the whole file is read and every record JSON-decoded, only the last 50 turns are kept | **confirmed**: linear in the whole backup, not in the window; a 5000-turn session pays 60 ms and 77 MB on every snapshot that carries a history block |
| H-H | `BenchmarkP1HHSettingsMuWriters`: 1 vs 8 writers doing `WriteSessionConfig` on distinct sessions (`settingsMu`); `TestP1N17CloseStall`: `ManagedProcessManager.Close` with `N` processes that ignore TERM | 1 writer **239 µs**; 8 writers **2.00 ms** per write with 8 in flight (8.4x, no throughput gain). Close: 3 processes at `kill_grace` 200 ms → **603 ms**; at the default 2000 ms → **6.00 s** | one settings write 239 µs; one TERM-ignoring process ≈ **2 s** (grace) plus kill | **confirmed** for both halves; the combination (delete holds `settingsMu` across those waits) is derived from H-H + N17, not measured end to end |
| H-I | existing `BenchmarkRunStartLockAndResolve` (app) and `BenchmarkLoadTools` / `BenchmarkLoadToolsServers` (tools/mcp) | run start lock + resolve **248 µs**; MCP `LoadTools` cold **1.32 ms** (5 tools) → **2.32 ms** (50 tools), 1.76x for 10x tools; warm 8 servers with 5 ms latency **6.2 ms** | 248 µs per run start, 1.3-2.3 ms per tool catalog build | **confirmed** as a per-run cost that does not grow much with tool count; the marshal+sha256 fingerprint is inside the 248 µs and was not separated |
| H-J / N24, N34 | `BenchmarkP1HJRemoteTranscript` on btrfs: one `GET /transcript` (tool `transcript`, path `profile:demo/dev/main`, page 1) against a 1 / 20 / 100 MiB transcript, serial and 8 concurrent | **15.5 ms** (1 MiB, 9 MB allocated), **280 ms** (20 MiB, 161 MB), **1.36 s** (100 MiB, **798 MB** allocated, 3.2 M allocs); 8 concurrent: 31 / 481 / 2253 ms per batch, i.e. no queueing and no refusal | ~13 µs per KiB of transcript, 8x the file size allocated per request | **confirmed**: every request re-opens, re-scans and re-indexes the whole transcript (the `scan_max_bytes` cap for remote calls is 256 MiB); a 20-page walk costs 20 of these, and nothing bounds the number of concurrent callers |
| H-K | `BenchmarkP1HKProfileLoad`: one session with 20 profiles, `profileMessenger.Profile` once and 20 times (one `transcripts` listing) | one profile **1.11 ms**, 426 KB allocated; 20 profiles **22.0 ms**, 8.5 MB allocated | 1.1 ms and 0.43 MB per profile per listing | **confirmed**: the listing loads every profile's full session settings |
| H-L / N15 | `BenchmarkP1HLCloseStuck` (asyncmgr): 1 / 10 / 50 managers, each with one handle whose driver never reports, `StopWait` 50 ms | 50.2 ms / 502 ms / **2.51 s**, exactly linear (3x, identical to 0.3%) | one stuck host = one `StopWait`, hosts closed one after another | **confirmed, and N15 with it**: at the default `stop_wait_ms` 2000 the same shutdown takes **100 s** for 50 hosts, with no total bound |
| H-M | `BenchmarkP1HMResolve`: `Resolver.Resolve` for a session of a 3- and a 25-model catalog | **175 µs** (3 models, 53 KB) / **266 µs** (25 models, 92 KB) | twice per `profile_message` start | **confirmed** as a doubled preflight: the cost is real and small; that `StartProfileRun` resolves again is read (`profile_message.go:84-90`, `httpapi/profile_run.go:89-113`), not instrumented |
| H-N | existing `BenchmarkSourceLoad` (openaioauth) | **11.2 µs** per `Source.Load` (2.6 KB, 31 allocs) | 11.2 µs per outbound provider request | **confirmed**: a file read plus two ES256/JWT decodes per request; 1.1 ms over a 100-round run, immaterial next to the request |

Verdicts: **confirmed 13** (H-A, B, D, E, F, G, H, I, J, K, L, M, N), **partial 1** (H-C: mechanism confirmed, cost below 0.5% of a core). Nothing was "not confirmed".

### 13.3 What the measurement corrected

- **H-C is a code smell, not a load problem.** The stat-and-parse-per-poll pattern is exactly as described, and it triples on the rebuild path with 25 listed models, but 200 sessions polled every 5 s cost 2.1-3.5 ms/s. Fix it with a stamp cache if it is touched, not as a performance item.
- **H-D needed the real disk.** On tmpfs an fsync is ~0-4 µs (as p4's P11/P20 found), so the same benchmark there shows no difference at all. On btrfs the delta is 5.75 ms per cache write and 11.6 ms with the directory sync — p4's 5.8 ms / 13 ms to within noise. The `process` mode is *not* free either: 7 renames and 7 fsyncs per session per poll happen regardless (`usage-cache.json` is only one of the writers).
- **H-B measured plain HTTP.** The per-child transport costs one TCP connection and 0.10 ms on loopback; the TLS handshake that a real provider adds is outside the fixture (the adapter needed to trust a self-signed cert would have to be injected into a cloned `http.DefaultTransport`).
- **N31's logging claim was wrong.** §4/N31 says a panicking remote transcript request is not logged; the HTTP measurement shows `net/http` printing `http: panic serving 127.0.0.1:…: runtime error: index out of range [-5]` plus a stack to the server's error log. The connection still drops with no response and the server survives, so the finding stands with a corrected detail.
- **N32's record text needs a caveat.** With a 25 ms deadline the restore failed before any file was restored and **no `pre_restore` record was written** (only the `complete` capture is listed) — the "record exists and is marked failed" wording in §4/N32 assumes the failure happens later in the restore.
- **H-F's absolute cost**: 106 µs per *incremental* `readTurnStarts` call on a 100-turn transcript is far more than the lock scope suggests; the contention (8.3x wall for 8 readers) is real but the per-call work dominates. Worth a second look before the N30 fix, not a rewrite.
- **H-A's per-folder price is the recovery, not the index**: the same 301-record folder costs 6.6 ms on first open and 0.7 µs once the host is cached, and the resolver holds its lock across the 6.6 ms. The 2.5 ms measured at the asyncmgr level is the same folder *without* orphan recovery (`ServerAlive` nil); `native.NewAsyncHost` passes it, so every restart pays the orphan scan, which is the 6.6 ms and the realistic number.

### 13.4 The unverified items, checked where it was cheap

| item | check | result |
|---|---|---|
| N9 (per-child transport) | H-B: connection count at a real server | **confirmed**: 50 runs, 50 connections; one shared client, 1 connection |
| N13 (select can report a finished run as "did not start") | `TestP1N13SelectBothReady`: both `observed` and `finished` buffered and ready, 2000 rounds of the same two-case select | **mechanism confirmed**: the finished case won **1021/2000 (51.0%)**, so once a run reports and finishes before the select runs the outcome is a coin flip. The real timing (a child that ends inside one scheduling quantum) was not provoked |
| N14 (admission open during recovery) | `TestP1N14RecoveryWindow`: `RecoverStorages` over 200 sessions of the fixture | **bounded and small on an idle tree**: 34.4 ms for 200 sessions (settings loads only, 0 storages); the window is one storage recovery, 6.6 ms for a 300-row async folder (H-A). Ordering (close admission before joining the scan) remains read-only, no concurrency test |
| N15 (no total shutdown bound) | H-L at 50 hosts × 50 ms, linear | **confirmed**: 100 s at the default `stop_wait_ms` |
| N17 (delete holds `settingsMu` across kill waits) | `TestP1N17CloseStall`: `ManagedProcessManager.Close` with TERM-ignoring processes | **confirmed**: 603 ms for 3 processes at `kill_grace` 200 ms, **6.00 s** at the default 2000 ms; H-H adds that settings writes are fully serialised behind the same lock |
| N24 (no per-token bound, rescan per page) | H-J serial vs 8 concurrent | **confirmed**: 8 concurrent requests ran in parallel with no queue or refusal, each rescanning the whole file |
| N25 (multi-root spelling) | `TestP1N25MultiRootSpelling`: two session roots, canonical reference `two:demo`, a token glob in each spelling | **confirmed**: a glob written for the raw spelling `demo/dev/main` lists the profile (`Candidates("demo") = [dev/main]`) but `Profile` refuses it (hidden); a glob for `two:demo/dev/main` reads the profile but `Candidates("two:demo")` returns nothing. One spelling each, exactly as filed |
| N29 (upstream URL printed) | `TestP1N29UpstreamURLInStartupLine`: `LoadConfig` on a URL with userinfo and a query, then the `main.go:53` format | **confirmed**: the URL validates and the startup line is `… upstream https://user:s3cret@gateway.example.test/v1?api_key=topsecret` |
| N30 (`turnStartsMu` scope) | H-F | **confirmed**, 8.3x wall for 8 readers |
| N31 over HTTP | `TestP1N31ForgedCursorOverHTTP`: real `remoteapi.Server` + token table + `remoteBackend`, `GET …/transcript`, a real detail cursor re-encoded with `"b":-5` | **confirmed**: the handler panics (`index out of range [-5]`), the client gets **EOF with status 0**, the server survives (next request 200), and `net/http` logs the panic + stack (see 13.3) |
| N32 (restore on the request context) | `TestP1N32RestoreCancelledMidway`: 4,000-file capture, 2,000 files edited, `Restore` with a 25 ms context | **confirmed**: restore returned `context deadline exceeded` after 26 ms with **0 of 2,000** files restored, and the killed git left `snapshot.index.lock` behind (N03). No `pre_restore` record was written (see 13.3) |
| N10 (default write scope) | `TestP1N10DefaultWriteScope`, `TestP1N10WriteOutsideWorkspace`: the real `write` tool, empty policy, `WorkspaceRoot` = the fixture cwd | **resolved**: a write to `<cwd>/.jad/remotes/evil.json` is **allowed** (`"status":"ok"`, file created), a write outside the workspace is refused (`"status":"forbidden"`). The remotes folder is `<cwd>/.jad/remotes` (`sessionfs/layout_root.go:18`), so under the default layout the model does reach the entry files of its own workspace; it cannot reach files outside it, which keeps N10 at low |

### 13.5 Ratings and status after measurement

| id | change from §2/§8 | basis |
|---|---|---|
| N07 | **stays medium, measured**: 6.6 ms of `Resolver.mu` per opened folder, once per process per folder | H-A; the cost is the recovery of the folder, not the index read |
| N16 | unaffected by this step; the H-D fsync numbers (5.75 ms / 11.6 ms) are what a *failed* write would also pay | H-D |
| N17 | **stays low**, now with numbers: 6.0 s of a shared lock for 3 TERM-ignoring processes | N17 check; H-H |
| N30 | **stays low**, and the per-call cost (106 µs) is worth a look before narrowing the lock | H-F |
| N31 | **stays low**, reach over HTTP now reproduced; "nothing is logged" corrected | N31 check |
| N32 | **stays low**, reproduced; the record wording corrected | N32 check |
| N33 (new) | **filed low** on H-G: 60 ms and 77 MB per snapshot for a 5000-turn backup, linear in the file rather than the window | H-G |
| N34 (new) | **filed low** on H-J: 13 µs per KiB, 798 MB allocated at 100 MiB, and 8 concurrent callers all served — the per-request half of N24 | H-J |
| N35 (new) | **filed low** on H-E: 933 ms and 1.05 GB for a 1000-item trim, superlinear in items × removed items | H-E |
| N24 | **stays low**: one request costs 13 µs/KiB and up to 798 MB of allocation at 100 MiB, with no bound, for an authenticated caller | H-J |
| N15 | **stays low**, quantified: 100 s at 50 stuck hosts | H-L |
| N10 | **stays low, now resolved**: the model can write its own `.jad/remotes` entries but nothing outside the workspace | N10 check |
| H-C, H-I, H-M, H-N | not worth a dedicated fix; fold into whichever change touches the code | 13.2 |
| H-D, H-E, H-F, H-G, H-J, H-K, H-L | **real costs**; H-D, H-F, H-L, and H-J's missing bound belong to existing findings (N16 area, N30, N15, N24), H-E, H-G and H-J's per-request cost had none and are now **N35, N33, N34** (low, §2/§4), H-K stays a cost without a finding | 13.2 |

### 13.6 Limits, what was not measured, and reproducibility

- **One machine, one filesystem.** btrfs with `compress=zstd:3` inflates or deflates nothing here except the 100 MiB transcript (which compresses), so its 1.36 s cold read is a page-cache/CPU number, not a disk-throughput number. An ext4/xfs host will differ in the absolute ms and in the fsync cost by a factor; the ratios (per KiB, per session, per host) will not.
- **No TLS in H-B** (self-signed `httptest`), no real provider, no real network latency. H-J and H-K used `httptest` only where the route needed an HTTP hop; the transcript cost is local file work.
- **H-M's doubling is read, not instrumented**: `Resolve` was measured as a unit; the second call inside `StartProfileRun` was not counted at runtime.
- **H-H's delete stall is composed**: it needs a session whose process manager holds a TERM-ignoring process *and* a concurrent settings write; each half is measured, the composition is not.
- **No profiler runs**: the costs are attributed by workload shape and allocation counts (H-E, H-G, H-J) rather than by pprof.
- **The `-race` pass covers the test suite only**, not the benchmarks; no data race was reported anywhere in this step.
- Scratch material: `/tmp/p1m/bg` (byte-identical copy + the fourteen scratch files) and `/tmp/p1m/*.txt` (raw benchmark and strace output) are kept for the fix step; `/home/www/Downloads/.p1disk` is deleted. The graded tree, `sample/`, `DECISIONS.md` and git state are untouched.
