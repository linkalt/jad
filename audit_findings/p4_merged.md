# Phase p4 merged findings — async and remote

- phase: `p4` (`internal/asyncmgr`, `internal/remote`, `internal/remoteapi`, `internal/checkpoint`), HEAD `ab86bef`, go1.27.3 linux/amd64.
- inputs: `p4_r1.md` (273 lines, 18 findings), `p4_r2.md` (248, 12), `p4_r3.md` (204, 10), `p4_r4.md` (138, 8). All four finished before this merge was opened (no reviewer file was read before that).
- coordinator work: merge + verification pass only. **No repository file was changed** (`git status --short internal/{asyncmgr,remote,remoteapi,checkpoint}` empty, `git stash list` empty; the only worktree changes are the pre-existing `M EXECUTOR.md`, `M MASTER.md` and untracked `audit_findings/`, `nodir/`). No benchmark, no `-race`, no full suite (section 6.3 measurement step still pending).
- verification: every finding was re-read at the cited `file:line`; every medium-or-higher and every unique finding is marked `confirmed` / `refuted` / `unverified` with the evidence below. Two cheap `/tmp` reproductions were run (git object pruning, `.gitattributes` round-trip); everything else is a source trace at this HEAD.
- load average while merging: 1.05 0.69 0.53 (12 CPUs).

## 1. Counts

| measure | count |
|---|---|
| findings merged (from 48 reviewer findings) | 33 |
| consensus, filed as a finding by 2+ reviewers | 12 |
| consensus by mixed classification (one finding + one perf hypothesis on the same code) | 2 (M06, M16) → 14 consensus ids in total |
| unique (1 reviewer) | 19 |
| contradictions recorded and resolved | 4 items (2 factual C1/C2, 2 severity groups C3/C4 covering 4 findings) |
| severity: critical / high / medium / low | 0 / 0 / 13 / 20 |
| verification of every unique and every medium-or-higher finding (26 distinct: 19 unique + 13 medium − 6 overlap) | 26 confirmed, 0 refuted, 0 unverified; 2 needed a correction (M19 five copies, not four; M07 r3's 0700 mitigation does not exist) |
| performance hypotheses after dedupe | 20 |
| repeats of fixed prior art | 0 (1 explicit decision challenge: M09) |

Per-reviewer contribution: r1 18 findings (9 consensus + 9 unique), r2 12 (8 + 4), r3 10 (6 + 3 + 1 mixed), r4 8 (5 + 3 + 1 mixed). Nothing r1..r4 filed was dropped except duplicates that map onto another merged id; nothing was added that no reviewer filed.

## 2. Merged findings

`found by` = the reviewer whose finding maps here. `status` is the verification outcome (§3-§4).

| id | title | sev | lens | location | found by | status |
|---|---|---|---|---|---|---|
| M01 | A crash inside the index-flush delay leaves a finished handle live (`running`) forever | medium | A | `asyncmgr/recover.go:18-33,54-69`; `store.go:127-145`; `manager.go:361-428` | r1, r4 | confirmed |
| M02 | A restore that fails after its first workspace write leaves a half-restored tree and no undo record | medium | A | `checkpoint/service.go:655-695` | r1, r2, r4 | confirmed |
| M03 | Restore path containment is lexical for removals and absent for `checkout` | low | B | `checkpoint/service.go:662-674`; `git.go:289-300` | r2, r3 | confirmed |
| M04 | `Close` does not cover every writer of the `.async` folder (`Complete` after close, the flush-timer callback, uncounted `applyWork` callers) | medium | A | `asyncmgr/manager.go:361-428,566-623`; `store.go:127-145`; `wait.go:43,68,90`; `prune.go:29-44`; `query.go:248-258` | r1, r2, r3 | confirmed |
| M05 | Checkpoint git subprocesses ignore the caller's context, so a stop or shutdown cannot cancel them | medium | A | `checkpoint/git.go:143-145`; `service.go:503-564,631`; `run.go:84-88` | r1, r2 | confirmed |
| M06 | `max_files` is checked only after the whole workspace was staged and written to the object store | medium | C/A | `checkpoint/service.go:444-451`; `git.go:208-221` | r1, r4 (H01) | confirmed |
| M07 | Checkpoint and `.async` directories, and the shadow repository's objects, are world-readable | medium | B | `checkpoint/git.go:193-203`; `service.go:332`; `asyncmgr/osutil.go:5` | r1, r3 | confirmed |
| M08 | The unix-socket check ignores group-writable parents and is check-then-dial | medium | B | `remote/socket_unix.go:17-32`; `remote/client.go:96-108` | r1, r4 | confirmed |
| M09 | Pruning checkpoints deletes refs only, so `max_total_bytes` is eventually reached for good | medium | C/A | `checkpoint/service.go:471-482,440-442`; `git.go:79` | r1 | confirmed |
| M10 | `max_total_bytes` is only compared before a snapshot: one snapshot can overshoot the cap by its whole delta | medium | B | `checkpoint/service.go:432-457` | r4 (r3 H3) | confirmed |
| M11 | A reset or delete that runs while a `Complete` is in flight misses the row (`finishing` is skipped) | medium | A | `asyncmgr/manager.go:361-380,442-467,470-489` | r4 | confirmed |
| M12 | A later `CancelOwner` overwrites the end action set by reset or delete | low | A | `asyncmgr/manager.go:470-484` | r1 | confirmed |
| M13 | A stop is starved while every in-flight client slot is held by follower polls | medium | A | `remote/client.go:156-161,116`; `run.go:126-142` | r1 | confirmed |
| M14 | A workspace `.gitattributes` changes the bytes a restore writes back | medium | A | `checkpoint/git.go:71-81,191-204` | r1 | confirmed |
| M15 | Remote references reject dotted category/profile names that local profile IDs allow | medium | A | `remote/target.go:29-36,86-88`; `remoteapi/server.go:484-487` | r3 | confirmed |
| M16 | Checkpoint `Begin`/`Finish`/`Restore` hold the staging mutex across git subprocesses | low | A | `checkpoint/service.go:169-183,503-529,532-564,631` | r3 (r2 H04) | confirmed |
| M17 | Package-level `locks`/`repoSizes` maps keep one entry per staging path for the process lifetime | low | C | `checkpoint/service.go:161-170` | r2, r3, r4 | confirmed |
| M18 | Dead exported API in the phase | low | D | `asyncmgr/types.go:63,manager.go:438,495,prune.go:153,remote/glob.go:9-16,remote/entry.go:59` | r1, r2 | confirmed |
| M19 | Five copies of the rune-aware cut helper across five packages, plus two control-strip variants | low | D | `asyncmgr/types.go:192`; `remoteapi/server.go:608,619`; `remote/client.go:276`; `tools/native/skill.go:144`; `tools/extension/text.go:26`; `webui/hosted_tool.go:204` | r1, r2 | confirmed (5 copies, not 4) |
| M20 | `evictFinishedLocked`'s victim choice is a correct but unreadable precedence puzzle | low | D | `remoteapi/server.go:454-471` | r2, r3 | confirmed |
| M21 | Owner stats with `dropped > 0` are never removed from `m.owners` | low | A/C | `asyncmgr/manager.go:231-238,454-467`; `prune.go:60-90`; `wait.go:62-69` | r3, r4 | confirmed |
| M22 | `MarkRunning` can overwrite a terminal record with a stale `running` copy | low | A | `asyncmgr/manager.go:343-357` vs `:361-428` | r1 | confirmed |
| M23 | `retention_days` / `unread_ttl_days` overflow `time.Duration` and drop everything at once | low | A/B | `asyncmgr/prune.go:84,114,138`; `config.go:97` | r1 | confirmed |
| M24 | A transient token-table read failure ends followed runs as permanent failures and leaves them running remotely | low | A | `remoteapi/tokens.go:253-259`; `remote/run.go:148-154` | r1 | confirmed |
| M25 | `Status` reads `r.ok` outside the lock | low | A | `asyncmgr/query.go:160-168` vs `manager.go:404-407` | r2 | confirmed |
| M26 | `Wait` consumes a signal in memory when its context is cancelled after the timeout pop | low | A | `asyncmgr/wait.go:79-91` vs doc at `:24-26` | r1 | confirmed |
| M27 | `GET /profiles` tells any token holder whether a session exists | low | B | `remoteapi/server.go:283-292`; `app/remote_api.go:33-46` | r2 | confirmed |
| M28 | Two mechanisms decide "this call can change files" and must be kept in sync | low | D | `checkpoint/run.go:12-39`; `tools/catalog.go:256-286`; `session/runner_tools.go:176` | r2 | confirmed |
| M29 | A symlinked remote entry file works when named but is invisible in `List` | low | D | `remote/entry.go:302-315` vs `:322-344` | r2 | confirmed |
| M30 | The ignored `crypto/rand` error in `begin` can mint a colliding run id | low | A | `remoteapi/server.go:419-421` | r3 | confirmed |
| M31 | The token-file check is a `Stat`-then-`ReadFile` TOCTOU | low | B | `remote/entry.go:262-277` | r3 | confirmed |
| M32 | The `176fd7d` Close regression test has a scheduling false-pass window | low | D | `asyncmgr/manager_test.go:411-452` | r4 | confirmed |
| M33 | Async records and the index are renamed without `fsync` (divergence from the checkpoint writer) | low | A/D | `asyncmgr/store.go:46-62` vs `checkpoint/service.go:338-369` | r1 | confirmed |

Severity rule applied per `audit_plan.md` 6.2: highest evidence, median severity. Where the two reviewers disagreed on severity the disagreement is kept in §5.

## 3. Consensus findings (2+ reviewers) — evidence and verification

Each entry: the normalised defect, which reviewer said what, what the source shows at `ab86bef`, and the merged verdict.

### M01 (medium, A) A crash inside the index-flush delay leaves a finished handle live — r1 P4-R1-02 (+ r4 P4-R4-03)
- r1: `[measured]` in a `/tmp` copy: register, `Complete` with a long flush delay, kill/reopen without `Close` → `Counts.Active = 1`, `List` shows `running`, survives a second clean restart; only another `Complete` (which nobody makes) clears it. r4: `[read]`, same conclusion, adds that `Status` then disagrees with `List`/`Wait`.
- source: `Complete` writes the terminal record at `manager.go:399` and the index only through the delayed `writeIndex` (`store.go:127-145`, default `index_flush_ms` 150). On restart `recover` builds the row from the **index** (`recover.go:19-20`), and the reconciliation loop at `:55-58` does `if rec.State.Terminal() { continue }` for a record it has just read — so the row keeps the index's non-terminal state, keeps counting in `liveTotal`/`max_handles`, and never gets its signal.
- verdict: **confirmed** (r1's measurement + this trace). Window = `index_flush_ms` after every state change. Fix: in that loop, adopt the record's state/ok/ended/unread into the row for a non-terminal index row before the `continue`; add the restart test.

### M02 (medium, A) A failed restore leaves a half-restored workspace and no undo record — r1 P4-R1-05, r2 P4-R2-03, r4 P4-R4-04
- r1: `[measured]` type-change case `d` (directory) → symlink: `Restore` returned `remove .../ws/d: directory not empty` with an empty result, the workspace had already been rewritten by `checkout-index`, and no `pre_restore` record or `UndoCheckpointID` was produced. r2/r4: `[read]`, same ordering.
- source: `keep(undo.ID,"before",…)` at `service.go:659` only writes a ref; `checkout` at `:662`, the `os.Remove` loop at `:666-674` and the second `capture()` at `:675` all run before `writeAll(append(readAll(), undo))` at `:691`. Every error between 662 and 691 returns with the workspace already changed and the only pre-restore trace an orphan `refs/checkpoints/<undo>-before` that no code path can name (prune deletes refs only for *recorded* checkpoints, `:471-482`).
- verdict: **confirmed**. Note for ranking: the pre-restore state becomes unreachable through the product (recoverable only by hand with git), which is the reason this is the top item; all three reviewers stayed at medium because the original checkpoint survives and re-running the restore recovers.

### M03 (low, B) Restore containment is lexical for removals and absent for `checkout` — r2 P4-R2-04 (+ r3 P4-R3-10)
- r2: `[read]`, `filepath.Join` + `strings.HasPrefix` passes for `<ws>/link/etc` when `link` is a symlink, so `os.Remove` follows the parent symlink. r3: `[read]`, `result.Restored` is passed to `git.checkout` with no containment check at all.
- source: both true. `service.go:667-668` is a lexical prefix test with no `EvalSymlinks`/`Lstat` walk; `:662` hands `paths(changes.restore)` straight to `read-tree`/`checkout-index --force` (`git.go:289-300`), i.e. containment rests on git refusing `..`/absolute tree entries — untested in this repository. Reach: same-user, model-influenced names, and the genuinely reachable window is the TOCTOU between `capture` (`:636`) and the removals (`:662`), with a still-running `bash` handle swapping a directory for a symlink.
- verdict: **confirmed**; merged low (both reviewers low). This is also a contradiction with r1's "checked and sound" claim — see §5 C1.

### M04 (medium, A) `Close` does not cover every writer of the `.async` folder — r1 P4-R1-12, r2 P4-R2-01, r3 P4-R3-01
- r3: `[read]`, with `stop_wait_ms = 0` the whole wait is skipped (`manager.go:594` tests `m.opts.StopWait > 0`), and `Config.Validate` allows 0 (`config.go:97`). r1: `[read]`, `Complete` has no `closed` check, and the flush-timer callback can pass its `indexClosed` test before `Close` sets it. r2: `[read]`, every `applyWork` caller outside `Complete` (`Wait`, `Prune`, `consume`, `DropUnread`, `DeleteOwnersWithPrefix`) is not counted by `m.completing`, and its final barrier only waits on `indexMu`.
- source: all three hold. `Close` waits `liveTotal + m.completing` (`:597-611`) and only under `StopWait > 0`; after that it stores `indexClosed` (`:612`), flushes, and locks/unlocks `indexMu` (`:619-622`). The timer callback (`store.go:137-144`) checks `indexClosed` **outside** `indexMu`, so a callback descheduled after that check runs `flushIndexNow()` after `Close` has returned. `Register` refuses after close (`:269`) but `Complete` does not.
- verdict: **confirmed** (r1 explicitly did not reproduce it; the trace is unambiguous). Merged medium (r1 medium, r3 medium, r2 low — see §5 C4). Effect in the tree today: harmless at process exit, the `.async: directory not empty` flake class in tests (baseline: 0/2 failures now), and a real second writer if the same folder is reopened.

### M05 (medium, A) Checkpoint git subprocesses cannot be cancelled — r1 P4-R1-11, r2 P4-R2-07
- both `[read]`. Source: `shadowGit.run` builds `context.WithTimeout(context.Background(), g.timeout)` (`git.go:143-145`); no `context.Context` reaches `Begin`/`Finish`/`Restore`; `Finish` is the deferred end of the run (`run.go:84-88`) and holds `s.mu` (`service.go:532-564`). A stop or shutdown during it waits for `git add`/`write-tree`/`ls-files` (bounded only by `timeout_ms`, 120 s default, plus the 5 s `killWait`).
- verdict: **confirmed**. Merged medium (r1 medium, r2 low — §5 C4). The package documents per-command timeouts, so this is "context not propagated", not an accident.

### M06 (medium, C/A) `max_files` is checked after the whole workspace was staged — r1 P4-R1-08, r4 H01
- r1: `[read]`, r4 listed the same cost as performance hypothesis H01 and as a note inside P4-R4-05. Source: `capture` runs `s.git.snapshot()` (`add --all`, `write-tree`, `ls-files -z`, `service.go:445`) and only then compares `fileCount` with `MaxFiles` (`:449-451`); the objects stay in the private repository (see M09).
- verdict: **confirmed**. Merged medium: the guard meant to prevent the cost runs after it, on every `Begin` and `Finish`.

### M07 (medium, B) Private directories and objects are world-readable — r1 P4-R1-06, r3 P4-R3-08
- r1: `[measured]` under umask 022, a 0600 workspace file became `-r--r--r--` loose objects in `checkpoints.git/objects` with 0755 dirs; only `checkpoints.json` is 0600. r3: `[read]`, same sites plus `asyncmgr/osutil.go:5`, and noted the staging parent might already be 0700 (unverified).
- source: `prepare` creates `checkpoints.git` and `info` with 0o755 and `info/exclude` with 0o644 (`git.go:193-203`); `writeAll` creates the metadata parent with 0o755 (`service.go:332`); `mkdirAll` for `.async` is 0o755 (`osutil.go:5`). The staging directory itself is created 0o755 at `transcript/turn.go:163`, and no `0o700` creation exists anywhere in `internal/app`/`internal/sessionfs` for state or staging dirs (grep: the only 0700 is `app/remote_listen.go:61` for the socket dir). So r3's mitigation does **not** apply in this tree.
- verdict: **confirmed**, r3's caveat resolved against the mitigation; merged medium (r1 medium [measured], r3 low). Reach: any local user who can traverse the parent chain — private key files, `.env`, model output and tool results copied by `add --all`.

### M08 (medium, B) The unix-socket check ignores group-writable parents and is check-then-dial — r1 P4-R1-15, r4 P4-R4-01
- both `[read]`. Source: `checkSocketOwner` refuses a parent only when `Perm()&0o002 != 0` without the sticky bit (`socket_unix.go:29`) — group write (0o020) is not tested, although the function's own comment says "a directory other users can write to". `client.go:96-107` `Lstat`s, checks, then dials by path (`dialer.DialContext`), so a replacement between the two sends the bearer token to another listener.
- verdict: **confirmed**; merged medium (r4 medium, r1 low — §5 C4). Mitigation in the default topology: jad binds its own listener inside a fresh 0700 directory (`app/remote_listen.go:61`), so this bites only a hand-configured `unix:` remote whose directory is group-writable. Contradicts r2's "checked and sound" — §5 C2.

### Consensus lows (short)
- **M16** r3 P4-R3-06 (+ r2 H04 as a hypothesis): `s.mu` (`service.go:169-183`) is held across `capture`/`keep`/`restore` (`:504,533,585`), i.e. across ~14 git processes. Serialization is required for the shared `snapshot.index`/`restore.index`, so this is a documented-cost item. **confirmed**.
- **M17** r2/r3/r4: `repoSizes` and `locks` are package-level `sync.Map`s written only by `LoadOrStore` (`service.go:161-170`), never deleted; one mutex plus one `repoSize` per staging path ever seen. **confirmed** (three independent reviewers, same two lines).
- **M18** r1/r2 dead API union: `asyncmgr.ErrNotLive` (`types.go:63`, definition only), `remote.MatchGlobs` (`glob.go:9`, definition only, and `matchGlob` beside it), `remote.Entry.Host` (written at `entry.go:227`, never read outside tests), `Manager.Prune` (`prune.go:153`, no non-test caller), `Manager.Counts` (`manager.go:512`, no caller at all), `Manager.CancelOwnersWithPrefix` (`manager.go:438`, no caller; `SubagentManager` has its own), `DropUnread` (used only in-package at `:447`). **confirmed** by grep at this HEAD.
- **M19** r1/r2: rune-aware cut/clean helpers — `asyncmgr/types.go:192`, `remoteapi/server.go:608` (identical) and `:619` (`safe`), `remote/client.go:276` (`safeText`), `tools/native/skill.go:144`, `tools/extension/text.go:26`, and a sixth copy `webui/hosted_tool.go:204` that neither reviewer listed. **confirmed**; correction: five `cutUTF8` bodies, not four.
- **M20** r2/r3: `server.go:460-461` is `oldest == nil || (own != oldestOwn && own) || (own == oldestOwn && older)`, which does implement "own finished first, then oldest" (both reviewers enumerated the four combinations) but is unreadable. **confirmed** as behaviour-correct, hygiene only.
- **M21** r3/r4: `forgetLocked` deletes the owner only when `s.total == 0 && s.dropped == 0` (`manager.go:235`); `enforceUnreadLocked` and TTL expiry increment `dropped` (`prune.go:69,89`); only `Wait` (delivery/pending==0, `wait.go:64-66`) and `DropUnread` (`manager.go:504`) reset it. An owner that overflowed and is only `List`ed keeps `total == 0, dropped > 0` forever, and a reused owner key inherits the stale count. **confirmed**.

## 4. Unique findings (1 reviewer) — verification pass

Every one was re-read at the cited lines; the two `/tmp` reproductions are marked. Outcome vocabulary per `audit_plan.md` 6.2: confirmed / refuted / unverified.

### M09 (medium, C/A) Pruned checkpoints free no disk — r1 P4-R1-07 — **confirmed**
- r1: `[measured]` in a scratch repo. Coordinator reproduction (`/tmp/p4c`, git 2.56.0): bare repo, one staged commit at `refs/checkpoints/x-after`, `git update-ref -d` → `count-objects -v` `count: 3 size: 12` before **and** after, `du` unchanged.
- source: `prune` deletes refs only (`service.go:471-482`); `gc.auto=0`, `maintenance.auto=false` (`git.go:79-80`) and the package runs no `gc`/`prune`/`repack` (grep). `capture` then skips every snapshot while the cached size is over `MaxTotalBytes` (`:440-442`), so undo stops for good.
- merged medium; the finding is a **decision challenge** on the recorded "no pruning" answer (`audit_performance_plan.md:521` item 4, `:947`), not a P-015 repeat.

### M10 (medium, B) `max_total_bytes` can be overshot by one snapshot — r4 P4-R4-05 — **confirmed**
- source: limit compared at `service.go:440` **before** `snapshot()`; the measured post-snapshot size at `:452-456` is stored but never compared. Overshoot = the delta of one snapshot.
- r3 flagged the same risk inside its perf hypothesis H3 ("allowance undercounts"), no separate finding. Merged medium; magnitudes are bounded by one snapshot and the feature is off by default.

### M11 (medium, A) A reset or delete misses a row whose `Complete` is in flight — r4 P4-R4-02 — **confirmed**
- source: `Complete` claims `finishing` and copies `after` under the lock (`manager.go:376-380`); `cancel` skips `finishing` (`:478`); `DeleteOwnersWithPrefix`'s removal pass skips it too (`:460`). The captured `endKeep` then applies, so a reset leaves an unread signal and a delete leaves the row and its record.
- verdict confirmed by trace; the window is one `writeRecord`. Kept medium (a deleted session keeps a persisted record) but a re-rate to low is defensible.

### M12 (low, A) A later `CancelOwner` overwrites the action set by reset or delete — r1 P4-R1-14 — **confirmed**
- source: `cancel` assigns `r.after = after` for every live match with no ranking (`manager.go:478-482`). `ResetOwner` → `endRead`, `DeleteOwnersWithPrefix` → `endRemove`, then a user stop (`CancelOwner` → `endKeep`) downgrades it: the reset session gets an unread signal and the deleted session keeps its row/record. Needs reset/delete and stop inside the driver's stop time.

### M13 (medium, A) A stop is starved while follower polls hold every slot — r1 P4-R1-01 — **confirmed**
- r1: `[measured]` in a `/tmp` copy: `max_in_flight=2`, two `Follow` goroutines against a server that holds the poll open, `StopRun` with a 3 s context returned `context deadline exceeded` and the server saw no stop request.
- source: `Do` takes a slot from `c.slots` before the request and releases it by `defer` (`client.go:156-161`); `Follow` → `PollRun` passes `wait = PollMs` (`run.go:131`), so a poll holds its slot for up to `poll_ms` (default 20 s). `StopRun` uses the same pool (`run.go:111-121`), so with `max_in_flight` (default 8) concurrent followed runs of one remote, a stop waits for a poll to finish; the cross-phase caller uses a 3 s stop timeout (`tools/native/profile_message_remote.go:224-234`), so the stop is dropped and the remote run keeps going.
- merged medium (only reviewer, but `[measured]` plus this trace).

### M14 (medium, A) A workspace `.gitattributes` changes the bytes a restore writes — r1 P4-R1-10 — **confirmed**
- r1: `[measured]` (`*.txt text eol=crlf`, `*.id ident` → restored `a.txt` had CRLF, `b.id` was ident-expanded).
- coordinator reproduction (`/tmp/p4a`, git 2.56.0, the package's own config flags): blob in the tree is `line1\nline2\n` and `$Id$`; after `read-tree` + `checkout-index --force -z --stdin` the work tree holds `line1\r\nline2\r\n` and `$Id: 055c8729… $`. **Reproduced exactly.**
- source: `fixedConfig` pins `core.autocrlf=false` but nothing overrides work-tree attributes (`git.go:71-81`); `prepare` writes only `info/exclude` (`:191-204`). Fix: write `* -text -ident -filter -working-tree-encoding` to `<gitdir>/info/attributes`.
- merged medium: a restore then differs from what was checkpointed and the next diff reports it as changed.

### M15 (medium, A) Remote references reject dotted category/profile names — r3 P4-R3-03 — **confirmed**
- source: `NamePattern = ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (`remote/entry.go:42`) is what `segmentsOK` (`target.go:29-36`) and the server's `validTarget` (`remoteapi/server.go:484-487`) apply, so no `.` in a category or profile segment. Local profile IDs allow dots: `profileMessageIDPattern = ^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$` (`sessionfs/native_profile_message_settings.go:34,181`). Agent names do **not** allow dots (`native_subagent_settings.go:62`), so the gap is category/profile only.
- verdict confirmed as an availability defect (no scope escape: both sides reject consistently, and `SessionPattern` already allows dots). Merged medium.

### M22 (low, A) `MarkRunning` can overwrite a terminal record with a stale copy — r1 P4-R1-03 — **confirmed**
- source: `MarkRunning` copies `rec` under `mu`, unlocks, then `_ = m.writeRecord(rec)` and drops the error (`manager.go:343-357`). A `Complete` that runs entirely in between writes the terminal record first; the stale copy then rewrites it as `running`, so a restart marks the handle `lost` and the real result is gone. `MarkRunning` does check `StateQueued`/`finishing` before claiming, which bounds but does not close the window. Callers: `subagent_remote.go:160`, `profile_message_async.go:176`, `subagent_async.go:33`.

### M23 (low, A/B) `retention_days` / `unread_ttl_days` overflow — r1 P4-R1-04 — **confirmed**
- source: `time.Duration(days) * 24 * time.Hour` at `prune.go:84,114,138` overflows past 106,751 days; `Config.Validate` only rejects negatives (`config.go:97-99`) and the settings loader allows up to `maxNativeAsyncValue` = 10,000,000 (`sessionfs/native_async_settings.go:10`). r1 measured (in a copy) that 10,000,000 drops every signal and record at once (negative duration → cutoff in the future). Merged low: needs an absurd operator value, but a "never" value silently destroys history.

### M24 (low, A) A transient token-table failure ends followed runs permanently — r1 P4-R1-09 — **confirmed**
- source: `refresh` on any read error sets `t.tokens, t.broken = nil, true` (`tokens.go:253-259`) and `Authenticate` then answers `errBadToken` for every request (`:281-283`) until a later successful read (interval 250 ms). `Follow` treats any non-5xx (including 401) as permanent and returns at once (`run.go:148-154`), so every follower of that remote ends `remote_failed` while the remote runs continue unstoppable. Reach: an operator/editor replacing the table non-atomically, or a transient mode/size error.

### M25 (low, A) `Status` reads `r.ok` outside the lock — r2 P4-R2-02 — **confirmed**
- source: `Status` copies `state`/`changed` under `mu` (`query.go:160`), then on the "no in-memory record" path builds the fallback `Entry` from `r.kind`, `r.ok`, `r.label` outside it (`:167`), while `Complete` writes `r.ok` inside `mutateLocked` (`manager.go:404-407`). `kind`/`label` are immutable, so exactly one unsynchronised field. `[read]` only (no `-race` in this step); the effect is a torn/stale label, not memory unsafety.

### M26 (low, A) `Wait` consumes a signal when the context is cancelled after the timeout pop — r1 P4-R1-13 — **confirmed**
- source: the timeout branch calls `popLocked` (which marks the signal read in memory and returns the flips) at `wait.go:81`, **then** checks `ctx.Err()` at `:82-85` and returns without `applyWork` — so the row is read in memory, the record file still says unread, and the caller got nothing. The doc at `:24-26` promises a cancelled context consumes nothing; the non-timeout path honours it (check at `:54` before `popLocked`). r1 measured it with a forced interleaving.

### M27 (low, B) `GET /profiles` leaks session existence — r2 P4-R2-05 — **confirmed**
- source: `handleProfiles` calls `Backend.Profiles` **before** applying the token's globs (`server.go:288-298`); `backendError` maps `ErrNoSession` to `404 no_session` (`:509-510`) while an existing-but-invisible session is `200 {"profiles": []}`. `app/remote_api.go:33-46` maps every `sessionfs.ResolveError` and every non-layout location to `ErrNoSession`, so the answer distinguishes "no such session" from "nothing you may see" — the oracle `a6150e3` removed for runs. Reach: any valid token with `message`, `subagent` or `transcripts`.

### M28 (low, D) Two mechanisms decide "this call can change files" — r2 P4-R2-10 — **confirmed**
- source: `checkpoint.CallChangesWorkspace` hard-codes `write`/`edit`/`bash`/`exec`/`process`+`command` and `source == "mcp"` (`run.go:12-39`); `catalog.CallMayChangeFiles` uses a per-tool hook and answers false without one (`tools/catalog.go:256-286`); the run ORs them (`session/runner_tools.go:176`). Every currently registered write-capable native tool is covered by one of the two, so this is latent, as r2 said.

### M29 (low, D) A symlinked entry file works when named but is invisible in `List` — r2 P4-R2-12 — **confirmed**
- source: `List` skips any directory entry whose `Type()` is not regular (`entry.go:332`, `os.ReadDir` uses `lstat`), while `Load` `os.ReadFile`s the path with no mode check (`:302-315`). Not a privilege boundary (the remotes folder is operator config), a validation divergence inside one package — note the token-file path in the same file *does* check `Mode().IsRegular()` (`:267`).

### M30 (low, A) The ignored `crypto/rand` error can mint a colliding run id — r3 P4-R3-04 — **confirmed**
- source: `random := make([]byte, 8); _, _ = rand.Read(random)` then `"rr_" + hex.EncodeToString(random)` (`server.go:419-421`); on failure every start mints `rr_0000000000000000` and `s.runs[entry.id]` is overwritten. The client's `newRequestID` returns its error (`remote/run.go:73-76`), so the server side is the outlier.

### M31 (low, B) Token-file `Stat`-then-`ReadFile` TOCTOU — r3 P4-R3-09 — **confirmed**
- source: `os.Stat` at `entry.go:263`, `IsRegular`/size/0600 checks at `:267-272`, `os.ReadFile` at `:273`. A writer with access to the token-file directory can swap the path in between; the error texts name the path but never a value, so no additional leak.

### M32 (low, D) The `176fd7d` Close test has a false-pass window — r4 P4-R4-08 — **confirmed**
- source: the test proves `completing > 0` before starting `Close` (`manager_test.go:422-435`), then asserts with `case <-time.After(150 * time.Millisecond)` that `Close` has not returned (`:441-446`) — no handshake that the `Close` goroutine was scheduled, so a `Close` that returns immediately can pass when the goroutine is delayed. Test quality only; the implementation is correct for this site.

### M33 (low, A/D) Async records are renamed without `fsync` — r1 P4-R1-16 — **confirmed**
- source: `writeFileAtomic` in `asyncmgr` writes, closes and renames with no `Sync` (`store.go:46-62`), while `checkpoint.writeFileAtomic` honours `durability` (fsync unless `process`, directory sync on `strict`, `service.go:338-369`). Process crashes are safe (rename is atomic in the page cache); a power loss can leave a zero-length/old record, and `recover` then logs "async record unreadable; dropped" (`recover.go:25-29`) and loses the handle, including a `lost` signal. Cost of adding the sync is one fsync per record write `[unmeasured]` — belongs to the measurement list (P20).

## 5. Contradictions and disagreements

Recorded with both positions; resolved by the source at `ab86bef` (§6).

### C1 (factual) Restore path containment — r1 "checked and sound" vs r2 P4-R2-04 + r3 P4-R3-10
- r1 §6: "restore targets stay under the workspace root (service.go:665-673)" — listed as sound.
- r2: the check is lexical, so a symlinked parent escapes; r3: the checked-out paths are not checked at all.
- source: the `remove` loop is `filepath.Join` + `strings.HasPrefix` with no `EvalSymlinks`/`lstat` walk (`service.go:667-668`); `checkout` receives `result.Restored` unfiltered (`:662` → `git.go:289-300`).
- **resolution: r2 and r3 are right**; r1's claim holds only for lexically simple paths. Filed as M03 (low).

### C2 (factual) Unix-socket boundary — r2 "checked and sound" vs r1 P4-R1-15 + r4 P4-R4-01
- r2 §6: "`Lstat` + `ModeSocket` + owner/root + non-world-writable parent before every dial — sound".
- r1/r4: the parent test only rejects `0o002`, not group write (`0o020`), and check-then-dial is a TOCTOU.
- source: `socket_unix.go:29` tests `Perm()&0o002 != 0 && !sticky`; nothing looks at `0o020`; the dial happens after the check (`client.go:96-107`).
- **resolution: r1 and r4 are right**; r2's "non-world-writable" is exactly what the code does, and it is not enough. Filed as M08 (medium; see C4 for severity).

### C3 (severity) M07 world-readable private directories — r1 medium `[measured]` vs r3 low `[read]`
- r3 justified low by "the staging parent may already be 0700 … not verified here".
- source: the staging directory is created 0o755 (`transcript/turn.go:163`); `writeAll` creates the metadata parent 0o755 (`service.go:332`); `prepare` creates the repository 0o755 (`git.go:193`); a grep for `0o700` in `internal/app` and `internal/sessionfs` finds only the socket directory (`app/remote_listen.go:61`). The mitigation does not exist in this tree.
- **resolution: medium** (r1's measurement plus this trace).

### C4 (severity) M04, M05, M08 — r2 low vs r1/r3/r4 medium
- r2 rated the `Close`-writer finding and the checkpoint-context finding low, arguing "no impact observed today" and "bounded and deliberate-looking" (both true); r4 rated the socket finding medium, r1 low.
- **resolution**: M04 medium (a *record* and `index.json` written into a folder the caller believes is closed is a second writer after shutdown, plus the documented `StopWait == 0` opt-out); M05 medium (the run holds its lock and the shutdown deadline for up to `timeout_ms`, 120 s default); M08 medium as filed, with the note that jad's own listener lives in a fresh 0700 directory (`app/remote_listen.go:61`), so the reaching topology is a hand-configured `unix:` remote in a group-writable directory.

### C5 (apparent, not a contradiction) "accounting is exact" vs the owner-map leak and the `r.ok` read
- r1 §6 ("capacity counters stay exact through `mutateLocked`") and r2 §6 ("`track`/`untrack` keep … exact on every transition") are about **counters**; M21 is about `m.owners` never being deleted when `dropped > 0`, and M25 is one field read outside the lock. Verified: the counters are exact on every path I read (`track`/`untrack`/`forgetLocked`/`mutateLocked`); neither claim covers the two findings. No reviewer needs to be overruled here; recorded so a later reader does not treat §6 as a refutation.

### C6 (noted, not filed) r3 hypothesis H12
- r3 listed "`Wait` duplicate delivery on a crash between record write and index write" as `unverified` and did not file it. It is the same crash window as M01; M01 (filed by r1 and r4, measured by r1) is the durable form of it. No separate id.

## 6. Reviewer-pair agreement

Counts are findings co-filed by both reviewers of the pair (33 merged findings; 12 consensus, 2 mixed finding+hypothesis, 19 unique).

| pair | agreed findings | list |
|---|---|---|
| r1–r2 | 5 | M02, M04, M05, M18, M19 |
| r1–r3 | 2 | M04, M07 |
| r1–r4 | 4 | M01, M02, M06, M08 |
| r2–r3 | 4 (+1) | M03, M04, M17, M20; M16 if r2's H04 hypothesis counts |
| r2–r4 | 2 | M02, M17 |
| r3–r4 | 2 | M17, M21 |

| reviewer | findings | consensus / unique | medium / low (as filed) | reproductions of its own |
|---|---|---|---|---|
| r1 | 18 | 9 / 9 | 9 / 9 | 7 scratch Go tests (/tmp copies) + 1 git script |
| r2 | 12 | 8 / 4 | 1 / 11 | 0 |
| r3 | 10 | 6 (+1 mixed) / 3 | 2 / 8 | 0 |
| r4 | 8 | 5 (+1 mixed) / 3 | 5 / 3 | 0 |
| all | 48 | 29 mapped onto 14 consensus ids (12 pure + 2 mixed) / 19 | 17 mapped onto 13 merged ids / 31 onto 20 | 8 |

Reading: r1 is the only reviewer that built `/tmp` reproductions (7 scratch tests plus one git script) and they underpin M01, M02, M07, M13, M14 and M23; its extra volume is otherwise low-severity hygiene. r2 and r4 found the review-quality items nobody else did (M25, M27, M29; M10, M11, M32). M17 was found independently by three reviewers on the same two lines — the strongest consensus signal in this phase. No reviewer-pair disagreement required overruling a *finding* (only C1/C2, where a reviewer's "checked and sound" claim lost to two others' findings).

## 7. Prior art handling

- **P-015** (checkpoint size cache, fixed `b27c663`): verified present (`service.go:148-159,432-457`) and **not** re-reported. M09 is a different consequence (refs deleted, objects retained) and is filed as a **decision challenge** on the recorded answer "pruning is separate and not approved" (`audit_performance_plan.md:519` question 4, `:521` item 4, `:947` "no pruning"). If the user keeps that decision, M09 becomes a documentation item ("undo stops at `max_total_bytes`, run `git gc` by hand").
- **P-053** batch 0 (`checkpoint/service.go:345` directory sync, `_ =` audit): the site is still `_ =` best-effort; M02/M33 do not repeat it.
- **176fd7d** (Close waits for `Complete`): verified in the tree — the wait and the `notifyLocked` on decrement exist (`manager.go:597-611,425`). M04 and M32 are *extensions* (other writers; the regression test's synchronisation), not repeats.
- **559ca62** (socket owner check), **f648d14** (token table 0600 + reload), **a6150e3** (no oracle for runs/profiles), **c95d84c** (request-id reuse), **f5a76d8** (per-token cap), **64e407b** (hop guard): all verified as still in force and used as the standard M08/M24/M27 are measured against, not re-reported.
- Rejected prior art (B-C2, B-C4, D-8, F-S1, F-S3, X-1, X-2): not re-raised by any reviewer and nothing found here needs new evidence on them.
- Exposure by configuration. **On the default path** (`asyncmgr`/resolver, no feature flag): M01, M04, M11, M12, M21, M22, M23, M25, M26, M33, plus the hygiene items M18/M19. **Behind `checkpoint.enabled` (default false)**: M02, M03, M05, M06, M09, M10, M14, M16, M17, M28. **Behind `server.remote_api` plus a configured remote entry**: M08, M13, M15, M20, M24, M27, M29, M30, M31 (M32 is a test). M07 spans both: the `.async` record directory is default, the `checkpoints.git` objects are opt-in.

## 8. Ranked list (severity, then evidence, then consensus count, then effort)

Ranking rule from `audit_plan.md` 6.4. `effort`: S under 1 h, M under 1 day. No critical or high finding exists in this phase.

| # | id | sev | file:line | suggested fix | effort |
|---|---|---|---|---|---|
| 1 | M02 | medium | `checkpoint/service.go:655-695` | write the `pre_restore` record as `open` with `BeforeTree` **before** the first workspace write, then finalize it to `complete` (or `failed` + `Warning`), so a partial restore is visible and undoable; add a failure-injection test on `checkout` | M |
| 2 | M01 | medium | `asyncmgr/recover.go:55-58` | for a non-terminal index row whose record is terminal, adopt the record's state/ok/ended/unread into the row instead of `continue`; add the restart test | S |
| 3 | M07 | medium | `checkpoint/git.go:193-203`; `service.go:332`; `asyncmgr/osutil.go:5` | create `checkpoints.git`, `info`, the metadata parent and `.async` with `0700`, `info/exclude` with `0600` (a directory mode is enough; no process-wide umask) | S |
| 4 | M13 | medium | `remote/client.go:156-161,116` | give stop a small reserve (a second semaphore) or let `StopRun` bypass `slots` | S |
| 5 | M14 | medium | `checkpoint/git.go:203` | write `* -text -ident -filter -working-tree-encoding` to `<gitdir>/info/attributes` in `prepare`; add the round-trip test | S |
| 6 | M09 | medium | `checkpoint/service.go:471-482,440-442` | after `prune` dropped refs, `git gc --prune=now` (or `prune --expire=now` + `repack -d`) when the measured size crosses a fraction of the limit — **reopens the recorded "no pruning" decision**; if refused, document that undo stops at the cap and needs a manual `git gc` | M |
| 7 | M04 | medium | `asyncmgr/manager.go:361-428,566-623`; `store.go:137-145` | make `Complete` after `closed` mutate memory only (no record/index write); take `flushMu` across the `indexClosed` check and `flushIndexNow()` in the timer callback; count `applyWork` in the same in-flight counter `Complete` uses | S |
| 8 | M08 | medium | `remote/socket_unix.go:29` | also refuse a group-writable parent without the sticky bit (and, if the check stays path-based, document that the check/dial gap remains) | S |
| 9 | M05 | medium | `checkpoint/git.go:143-145` | thread a `context.Context` through `Begin`/`Finish`/`Restore` → `capture`/`keep`/`checkout` → `shadowGit.run` and combine it with the per-command timeout; callers pass the run/shutdown context | M |
| 10 | M06 | medium | `checkpoint/service.go:444-451` | count first (a bounded `WalkDir` honouring the exclude list, stop at `max_files+1`) or run `add` with a file budget, so the guard runs before the cost | M |
| 11 | M11 | low* | `asyncmgr/manager.go:361-380,442-467` | apply the end action at commit time (re-read `r.after` under `mu` after the record write) and let reset/delete mark a `finishing` row for the action instead of skipping it; add the in-flight test | M |
| 12 | M10 | medium | `checkpoint/service.go:452-457` | compare the post-snapshot size to `MaxTotalBytes` and refuse/roll back that snapshot, or document the one-snapshot overshoot explicitly instead of calling the value a maximum | M |
| 13 | M15 | medium | `remote/target.go:29-36`; `remoteapi/server.go:484-487` | allow `.` in category/profile segments (the local pattern already does), keeping the `..`/empty/dot/`/` guards | S |
| 14 | M17 | low | `checkpoint/service.go:161-170` | drop the `locks`/`repoSizes` entries when a session is deleted, or hold both in the per-session object | S |
| 15 | M21 | low | `asyncmgr/manager.go:231-238,454-467` | delete zero-row owner stats regardless of `dropped` on delete/reset, or reset `dropped` when it is reported once | S |
| 16 | M18 | low | `asyncmgr/types.go:63`; `remote/glob.go:9-16`; `remote/entry.go:59`; `manager.go:438,495,512`; `prune.go:153` | delete the three dead symbols and the two uncalled manager methods; unexport `DropUnread`; keep or drop `Entry.Host` | S |
| 17 | M19 | low | `asyncmgr/types.go:192`; `remoteapi/server.go:608,619`; `remote/client.go:276`; `tools/native/skill.go:144`; `tools/extension/text.go:26`; `webui/hosted_tool.go:204` | one exported cut helper plus the control-strip variant (a small `internal/textcut`), used by all five | S |
| 18 | M20 | low | `remoteapi/server.go:460-461` | extract `prefer(held, oldest, tokenID) bool` with a comment; add a preference-order assertion to the existing cap test | S |
| 19 | M16 | low | `checkpoint/service.go:169-183,503-529,532-564` | document the serialization, or give each capture its own temp `GIT_INDEX_FILE` and keep the mutex only around `readAll`/`writeAll`/`keep` | M |
| 20 | M23 | low | `asyncmgr/config.go:97`; `prune.go:84,114,138` | cap `retention_days`/`unread_ttl_days` (for example 36,500) in `Config.Validate` and in the settings loader | S |
| 21 | M27 | low | `remoteapi/server.go:288-292` | answer `200 {"profiles": []}` when the backend reports `ErrNoSession`, so "missing" and "invisible" are one answer | S |
| 22 | M12 | low | `asyncmgr/manager.go:478-482` | only raise `after` (`endKeep` < `endRead` < `endRemove`) | S |
| 23 | M25 | low | `asyncmgr/query.go:160-168` | capture `ok` with the other fields under the lock and use the copy in the fallback entry | S |
| 24 | M26 | low | `asyncmgr/wait.go:79-91` | move the `ctx.Err()` check above the `popLocked` call in the timeout branch | S |
| 25 | M22 | low | `asyncmgr/manager.go:343-357` | write the record under `ioMu` after re-checking the row is still queued and not `finishing`, or use `updateRecord` with a state guard | S |
| 26 | M24 | low | `remoteapi/tokens.go:253-259`; `remote/run.go:148-154` | keep the last good table for a short grace period after a failed read (fail closed for new starts), or treat 401 as transient in `Follow` until `lost_after_ms` | S |
| 27 | M33 | low | `asyncmgr/store.go:46-62` | reuse one atomic-write helper with a `durability` argument (see M05's package for the existing one); measure the fsync cost first (P20) | S |
| 28 | M28 | low | `checkpoint/run.go:12-39`; `tools/catalog.go:256-286` | make the catalog the single source (a `Writes`/`ReadOnly` flag or `CallMayChangeFiles` on every tool) and keep only the MCP policy in `checkpoint`; or add a test asserting every registered native tool answers true to one of the two | S |
| 29 | M29 | low | `remote/entry.go:302-315` | apply the same `Lstat`/`IsRegular` check in `Load` (refuse a symlinked entry), matching `List` | S |
| 30 | M30 | low | `remoteapi/server.go:419-421` | return `unavailable`/`internal` when `rand.Read` fails instead of ignoring it | S |
| 31 | M31 | low | `remote/entry.go:262-277` | open with `O_NOFOLLOW`, `fstat` the handle, then read from it | S |
| 32 | M32 | low | `asyncmgr/manager_test.go:411-452` | synchronise on the driver's `Stop`/a test hook before asserting that `Close` is blocked (drop the fixed-delay-only assertion) | S |
| 33 | M03 | low | `checkpoint/service.go:662-674`; `git.go:289-300` | re-resolve the parent with `EvalSymlinks` (or `lstat` each component) before `os.Remove`, and apply the same containment filter to `result.Restored` before `checkout` | S |

*M11 was medium at merge time; the user rated it **low** after the measurement (§13.6), so it belongs with the low block. M25 was "low" and is not confirmed as a reachable race (§13.5).

Dependency notes for the fix stage: M01 and M04 share `asyncmgr` files; M22/M25/M26/M12/M23 touch the same two files (`manager.go`, `wait.go`/`query.go`) and can be one bounded batch; M02/M05/M09/M10/M14/M16/M17/M03 all touch `checkpoint/service.go`/`git.go` and should be split into at least two commits (restore correctness vs repository lifecycle). Nothing here changes `messages.jsonl`; the async record/index/`checkpoints.json` formats are separate, so no `messages-golden-diff` run is implied by these fixes (M01's recover change and M04's write suppression touch `asyncmgr` record-file contents only).

## 9. Performance hypotheses for the measurement step (deduped; results in §13)

20 hypotheses: 19 from 29 reviewer entries (r1 H01-H10, r2 H01-H08, r3 H1-H6, r4 H01-H05) plus one that only exists once M33 is considered (P20, the cost of making async record writes durable). r3's H6 covers both the popLocked remarshal and the Status re-cut; it is counted once, in P04. No `-bench`, `-race` or profile was run in the merge step (the measurement step's results are in §13). `new` = the benchmark must be written into a `/tmp` copy; existing benchmarks are in the named `_test.go` files at this HEAD.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| P01 | every index flush sorts and marshals all rows | `asyncmgr/store.go:150-188` | 10,000 retained handles, `index_flush_ms` 150, handles completing | new `BenchmarkIndexFlush10kRows` (modelled on `BenchmarkOpenRebuild`) | r1 H01, r2 H02 |
| P02 | `pruneLocked` + `recomputeExpiryLocked` scan every row/owner when the guard opens | `asyncmgr/prune.go:77-150`; `manager.go:418` | 10,000 retained rows, `retention_days`/`unread_ttl_days` set, `Complete`/`Wait` traffic | new `BenchmarkPrune10kRows` / `BenchmarkCompletePruneManyOwners` | r1 H01, r2 H03, r3 H2 |
| P03 | `List` with a non-active filter copies and sorts every row of every owner under `mu` | `asyncmgr/query.go:94-139` (`:110`, sort `:114`) | `state=all`, 10,000–100,000 retained rows, many owners | existing `BenchmarkListPage10Of10kRows` (`asyncmgr/scale_test.go:635`) at 1 vs N owners, plus 100k rows | r1 H03, r2 H01, r3 H1 |
| P04 | `Wait`/`popLocked` sorts all unread rows and `encodedSize` marshals each entry; every completion wakes all waiters | `asyncmgr/wait.go:100-141`; `entry.go:40-68`; `manager.go:240-243` | `unread_max` 100, `wait_max_signals` 10, 50 concurrent waiters | new `BenchmarkWaitManyWaiters` / `BenchmarkWaitPopFullPage` | r1 H02, r3 H6 |
| P05 | every remote call re-reads and re-parses the entry file (`Directory.Client`) | `remote/hub.go:50-65`; `entry.go:302-315` | one tool call or transcript read per request | new `BenchmarkDirectoryClient` (with the parse hoisted, in `/tmp`) — no `remote` benchmark exists | r1 H05, r2 H06 |
| P06 | every remote request re-`Stat`s and re-reads the token file | `remote/client.go:152`; `entry.go:251-285` | 8–64 followed runs, `poll_ms` 20 s and the 100 ms floor | new `BenchmarkClientRoundTrip` against `httptest` (entry/token files in `b.TempDir()`), `token_file` vs `token_env` | r1 H04, r2 H06 |
| P07 | `begin`/`lookup` sweep and scan all held runs under the server mutex | `remoteapi/server.go:409-414,473-482,519-531` | `max_runs` 256 (default) and 10,000 (cap), clients polling | new `BenchmarkStatusPoll256Runs` / `BenchmarkRemoteAPIPoll` at 256 / 10,000 held runs | r1 H06, r2 H07 |
| P08 | `Authenticate` compares against every token and `refresh` re-reads + re-hashes the whole file every 250 ms under the write lock | `remoteapi/tokens.go:215-266,271-302` | 1 and 1,000 tokens; a table at the 1 MiB cap; 1 request/s | new `BenchmarkAuthenticate1000Tokens` (`StaticTokenTable` for the compare, file-backed for `refresh`) | r1 H07, r2 H08 |
| P09 | 14 git processes per run on top of a full `add --all` twice per run | `checkpoint/service.go:422-458`; `git.go:208-221` | 1,000 / 20,000 files, one edit per run (P-015 baseline: 200 ms at 20k, 62% in `count-objects`) | existing `BenchmarkBeginFinishSteady` (`checkpoint/steady_benchmark_test.go`) | r1 H08 |
| P10 | `count-objects` runs twice per capture (P-015 residue) | `checkpoint/service.go:422-457`; `git.go:208` | same as P09 | existing steady benchmark + `strace -e execve` count | r3 H3 |
| P11 | `readAll`/`writeAll` re-read and rewrite `checkpoints.json` with fsync per `Begin`/`Finish` | `checkpoint/service.go:306-369` | 20 records, durability `turn` and `strict` | existing checkpoint benchmarks with durability set | r1 H09 |
| P12 | pruned objects are never collected, so `repositoryBytes`/`count-objects` time and disk grow with history | `checkpoint/service.go:471-482`; `git.go:79` | 500 runs of a 20,000-file workspace with small edits | script over the steady benchmark, reporting `count-objects -v` size and time per capture (no Go benchmark) | r1 H10 |
| P13 | one staging mutex serializes concurrent captures (2+ runs/profiles of one session) | `checkpoint/service.go:503,532,631` | 2 and 8 concurrent `Begin`/`Finish` on a 20,000-file workspace | new concurrent `BeginFinishSteady` variant (2/8 goroutines, same staging dir) | r2 H04, r3 H4 |
| P14 | `max_files` is enforced only after the full staging cost | `checkpoint/service.go:445-450` | 100,000 files with `max_files=20,000` | new `BenchmarkCaptureOverMaxFiles` against the existing fixtures | r4 H01 (= M06) |
| P15 | one snapshot can exceed `max_total_bytes` by its whole delta | `checkpoint/service.go:439-457` | repository just below the cap, one large new file | script (record repo bytes and peak disk), not a benchmark | r4 H02 (= M10) |
| P16 | the package-level `locks`/`repoSizes` maps grow with deleted staging paths | `checkpoint/service.go:161-170` | 10,000 unique staging paths created then deleted | new `BenchmarkNewServicePathChurn`, recording entries and heap | r4 H03 (= M17) |
| P17 | owner stats with `dropped > 0` survive deletion | `asyncmgr/manager.go:231-237,454-467` | many owners with an expired/overflowed signal, then deletion | new `BenchmarkDeleteOwnersAfterDroppedSignals`, recording `len(owners)` and heap | r4 H04 (= M21) |
| P18 | `Hub.clients` keeps one client per historical (directory, name) and follower goroutines scale with remote runs | `remote/hub.go:44-72,80-114` | 100/1,000 concurrent follows; churn of remotes folders | new `BenchmarkHubClientChurn` + `BenchmarkFollowManyRuns` against `httptest` | r3 H5, r4 H05 |
| P19 | `CallChangesWorkspace` parses the call JSON up to three times per tool call | `checkpoint/run.go:20-37`; `session/runner_tools.go:176` | a round of 64 calls (`BenchmarkRunToolBatch` fixture), checkpoints off | new `BenchmarkCallChangesWorkspace` + `BenchmarkRunToolBatch` 1/8/64 | r2 H05 |
| P20 | `fsync` per async record write (the M33 fix candidate) would cost one sync per state change | `asyncmgr/store.go:46-62` vs `checkpoint/service.go:338-369` | completion rate 20/s, 10,000 rows | `/tmp` prototype of the durability-aware writer vs the current one, plus an fsync count via `strace -c` | merge (dependency of M33) |

Measurement order (per 6.3): P01, P03, P02, P04 in `asyncmgr` first (largest expected effect, one package, one `-race` run at the end), then P09/P10/P13/P14 in `checkpoint`, then P05-P08/P18 in `remote`/`remoteapi`, then the script-only P12/P15/P16/P17/P20. `checkpoint.enabled` and the remote API are off by default; P01-P04 and P19 are on the default path.

## 10. Verification log (what was actually run)

Repo state check (before any read of code): `git -C backend-go rev-parse --short HEAD` → `ab86bef`; `git status --short internal/{asyncmgr,remote,remoteapi,checkpoint}` → empty (exit 0); `git stash list` → 0 entries; worktree changes are the pre-existing `M EXECUTOR.md`, `M MASTER.md` and untracked `audit_findings/`, `nodir/` — untouched by this step.

Source reads at `ab86bef` (whole or cited ranges, in this order): `asyncmgr/manager.go` (40-90, 120-145, 225-300, 315-358, 355-430, 430-530, 555-660), `asyncmgr/recover.go`, `prune.go`, `store.go` (30-200), `wait.go` (1-140), `query.go` (85-180), `osutil.go`, `config.go` (1-60, 95-131), `manager_test.go` (405-455); `remote/socket_unix.go`, `client.go` (80-175), `entry.go` (240-350), `target.go`, `run.go` (72-175); `remoteapi/server.go` (200-300, 300-440, 440-540), `tokens.go` (200-300); `checkpoint/service.go` (140-200, 300-380, 400-470, 470-600, 600-741), `git.go` (55-150, 150-235, 250-305), `run.go`; cross-checks `tools/catalog.go` (250-290), `app/remote_api.go` (25-50), `app/remote_listen.go` (55-70), `transcript/turn.go` (155-170).

Greps that decided findings: dead-symbol grep for `ErrNotLive`, `MatchGlobs`, `.Counts(`, `.Prune(`, `CancelOwnersWithPrefix`, `DropUnread`, `e.Host` over `internal/` + `cmd/` excluding `_test.go` (M18); `cutUTF8`/`safetext` bodies (M19); `NamePattern`/`SessionPattern`/`profileMessageIDPattern`/`subagentNamePattern` (M15); `0o700|0700` in `internal/app` + `internal/sessionfs` (M07); `MkdirAll` sites (M07); prior-art grep of `audit_performance_plan.md` (`P-015`, `P-021`, `P-053`, `P-055`, decisions block) and `git log` for the phase commits.

Reproductions (both in `/tmp`, removed at the end; no repository file touched):

| scratch | hypothesis | command shape | observed | verdict |
|---|---|---|---|---|
| `/tmp/p4c` | M09: deleting checkpoint refs frees nothing | bare repo → `add --all` → `write-tree` → `commit-tree` → `update-ref refs/checkpoints/x-after` → `count-objects -v` → `update-ref -d` → `count-objects -v` | `count: 3 size: 12 …` before and after; `du -sk` 96 KiB unchanged | **confirmed** |
| `/tmp/p4a` | M14: work-tree `.gitattributes` rewrites restored bytes | `.gitattributes` with `*.txt text eol=crlf` + `*.id ident`; stage with the package's `fixedConfig` flags; `cat-file` the blobs; `read-tree` + `checkout-index --force -z --stdin` | blobs are `line1\nline2\n` and `$Id$`; the work tree afterwards has `line1\r\nline2\r\n` and `$Id: 055c8729… $` | **confirmed** (matches r1 exactly) |

Not run, deliberately: no `go build`/`go vet`/`go test`/`-bench`/`-race`/profile (this step is read-only and the measurement step is separate; no code changed, so there is nothing new to vet). Baseline `ab86bef` results are taken from `audit_findings/baseline.md`. `uptime` at the start of the merge: load 1.05 0.69 0.53.

## 11. Unverified, limits and open items

*(Status after the measurement step: see §13. The two `/tmp` reproductions here were extended by P12-P18; M25 was probed under `-race` and not confirmed; the unmeasured magnitudes P14/P15 are now measured.)*

Kept in the list but not proven by execution at this HEAD (everything beyond the two coordinator `/tmp` repros and r1's seven scratch tests is `[read]`):

- **Concurrency interleavings without a reproducer**: M04 (both sub-windows), M11, M12, M22, M24, M26. r1 measured M26 and M01 in a copy; M04/M11/M12/M22/M24 rest on the trace. A `-race` run on the phase packages in the measurement step cannot find M11/M12/M22 (they are logic interleavings, not data races) but will confirm or refute M25.
- **M25** is the only data race; `-race` is forbidden in this step and is the natural confirmation.
- **M02**: r1's reproduction was `safe` mode with a directory→symlink type change; the `full`-mode branch and a failing second `capture()` were not exercised. The ordering argument does not depend on the mode, but the exact user-visible result does.
- **M09**: the "undo silently stops after `max_total_bytes`" consequence is arithmetic on `service.go:440-442` plus the un-freed objects, not an end-to-end run to 512 MiB.
- **M10/M06 magnitudes**: how much a single snapshot can overshoot, and the cost of staging an over-`max_files` workspace, are unmeasured (P14/P15).
- **M08**: no socket-replacement attempt was made (out of scope: would need two users/processes); the finding is a source-traced local-user race.
- **M15**: the patterns are verified; that a deployment actually uses a dotted profile id is an operational assumption.
- **M07**: object modes were reproduced by r1 under umask 022; directory modes of the *session storage root* are set outside p4 (`internal/app`), and the grep found no 0700 there, but a restrictive user umask would change the outcome.
- **Cross-phase claims were not verified** beyond what each finding needs: `app/remote_api.go` run timeout, `app/remote_transcripts.go` scope enforcement, `tools/native/async_latest.go:40-68` cursor handoff, `session/runner_tools.go:176`, `transcript/turn.go:163`. They stay as leads for phases 1, 6b/6c, 7b.
- **Coverage gap all four reviewers share**: nobody ran `-race`, nobody exercised a real unix socket across users, nobody ran real git at 20k/100k files, and all four skipped the app-side wiring and Windows. That is the visible blind spot of phase 4; the measurement step closes the `-race` and benchmark parts.
- **Reviewer independence**: the four reports cite disjoint read sets and no report quotes another's id, and the finish order matches the plan; file modification times were not compared (no evidence of cross-reading to report).

## 12. Inputs

| file | lines | findings | hypotheses | prior-art handling |
|---|---|---|---|---|
| `audit_findings/p4_r1.md` | 273 | 18 | 10 | P-015 taken as fixed, F-26 noted |
| `audit_findings/p4_r2.md` | 248 | 12 | 8 | P-015/P-021/P-053/P-055, B-C2/X-2 checked |
| `audit_findings/p4_r3.md` | 204 | 10 | 6 | rejected items (B-C2/B-C4/D-8/F-S1/S3/X-1/X-2) re-checked, no new evidence |
| `audit_findings/p4_r4.md` | 138 | 8 | 5 | P-015 not re-reported |

## 13. Measurement results (step 6.3, P01-P20)

### 13.1 Preconditions and method

- Machine: 12 CPUs (AMD Ryzen 5 3600), go1.27.3 linux/amd64, git 2.56.0, go build cache warm. `/tmp` is **tmpfs** (19 GiB, 1% used), so every file write in these numbers is memory-backed: absolute I/O numbers (fsync, loose objects) understate a real disk by a large factor. Load average 1.0-2.1 throughout (the running jad backend holds it near 1.0); recorded before and after every group.
- No reviewer, build or other test was running; each `go test -bench` was started only after the previous one exited. No process was killed or restarted. No repository file was touched: the graded code is a byte-identical copy at `ab86bef` (`/tmp/p4m/bg`, `diff -r` against `backend-go/internal/{asyncmgr,remote,remoteapi,checkpoint}` exit 0) with four scratch benchmark files added (`p4bench_test.go`, `p4script_test.go`, `p4write_test.go`, `p4count_test.go`; all removed at the end with `/tmp/p4m`).
- Baseline, in the repository (read-only): `go build ./internal/{asyncmgr,remote,remoteapi,checkpoint}/` exit 0; `go vet` exit 0; `gofmt -l` exit 0 (no output); `go test -count=1` per package: asyncmgr ok 0.997s, remote ok 2.311s, remoteapi ok 1.029s, checkpoint ok 2.042s (4/4 ok). One serialized `go test -race -count=1` over the four packages: all ok (2.656 / 3.352 / 2.166 / 3.210 s), **no data race reported**.
- Each benchmark: `go test <pkg> -run '^$' -bench '<name>' -benchmem -benchtime=<0.2-0.3s> -count=10`. n=10 always. `spread = (max-min)/mean`; over 5% = **noisy**, repeated once (both numbers below). Statistics computed from the raw `ns/op` lines.
- Two sizes per hypothesis wherever the hypothesis is about scaling; claims are relative (the ratio between sizes), not absolute.

### 13.2 Results by hypothesis

| id | command (benchmark) | sizes | numbers (mean of 10) | per-op cost | spread | verdict |
|---|---|---|---|---|---|---|
| P01 | `BenchmarkP4P01IndexFlush` | rows 1k / 10k | 1.344 ms / 15.062 ms (repeat 14.486 ms); 523 KiB / 7.6-8.0 MiB, 28 / 42 allocs | one `flushIndex` | 2.7% / 6.3% noisy (repeat 6.1% noisy) | **confirmed (scaling)**; absolute figure noisy — 10.8-11.2× for 10× rows, marshal-bound (see pprof) |
| P02 | `BenchmarkP4P02PruneScan` | rows 1k / 10k | 0.169 ms / 2.448 ms; 18 KiB / 449 KiB | one prune pass that does work | 2.2% / 4.2% | **confirmed**: 14.5× for 10× rows (superlinear: the finished-row sort), paid on a completion or wait that crosses an expiry |
| P03 | `BenchmarkP4P03ListAll` (10 matching rows, 1k vs 10k total rows) | total 1k / 10k | 0.023 ms / 0.151 ms | one `List(state=all)` | 3.0% / 2.7% | **confirmed for the global scan** (6.6× for 10× invisible rows, under `mu`); **not confirmed for the sort** — see 13.3 |
| P04 | `BenchmarkP4P04PopCost` | unread 100 / 1000 | 0.020 ms / 0.146 ms | one waiter's pop, verbatim | 2.8% / 2.4% | **confirmed** per waiter and linear in unread rows; a herd of W waiters costs W of these per completion (multiplication derived, not measured) |
| P05 | `BenchmarkP4P05DirectoryClient` | entry 0 / 100 profile globs | 0.014 ms / 0.036 ms; 42 / 72 allocs | one `Directory.Client` (read+parse+`DeepEqual`) | 3.1% / 1.5% | **confirmed**: a file read plus JSON parse per call, growing with entry size; small in absolute terms |
| P06 | `BenchmarkP4P06RoundTrip` | `token_env` / `token_file` | 0.055 ms / 0.070 ms | one HTTP round trip against `httptest` | 2.2% / 1.2% | **confirmed**: the token file adds 15 µs (+27%) per request; immaterial at the `poll_ms` cadence, visible in a burst |
| P07 | `BenchmarkP4P07StatusPoll` | 256 / 10,000 held runs | 0.008 ms / 0.150 ms | one `GET /runs/<id>` | 2.7% / 2.5% | **confirmed**: O(held runs) under the server mutex; 8 µs at the default cap, 150 µs at the 10,000 cap |
| P08 | `BenchmarkP4P08Authenticate` / `P4P08Refresh` | 10 / 1,000 tokens | 0.5 µs / 0.017 ms; refresh 0.041 ms / 3.583 ms (1.9 MiB, 10,869 allocs) | one `Authenticate`, one `refresh` | 1.7% / 1.0%; 3.6% / 2.8% | **confirmed**: the compare loop is negligible; the every-250 ms re-read+hash+**parse** of the whole table is the real cost (3.6 ms and 1.9 MiB every 250 ms at 1,000 tokens, under the write lock) |
| P09 | `BenchmarkBeginFinishSteady` (existing) | 1,000 / 20,000 files | 72.6 ms / 135.2 ms | one `Begin`+`Finish` | 1.8% / 2.4% | **confirmed as a cost** (135 ms per run at 20k files on tmpfs) but **not the file-count scaling the hypothesis expected**: 20× the files costs 1.86×, so the 12 git processes dominate |
| P10 | `strace -f -e trace=execve` on `TestP4P10Execve` | 200 files, 4 captures in one process | 26 git execs: `init` 1, then per capture `add`,`write-tree`,`ls-files`,`count-objects`,`commit-tree`,`update-ref` | git processes per run | exact counts | **confirmed**: 12 git processes per steady run with **2 `count-objects`** (P-015's fix is present; the cold first run has 3) |
| P11 | `BenchmarkP4P11WriteAll` | durability process / turn / strict, 20 records | 0.034 / 0.034 / 0.038 ms (strict repeat 0.038 ms) | one `writeAll` | 1.9 / 2.8 / 5.2% noisy (repeat 2.3%) | **not confirmed on this filesystem**: the fsync costs ~0-4 µs on tmpfs; the question needs a real disk |
| P12 | `TestP4P12Growth` (script) | 50 runs, 200-file workspace | repo 24 KiB / 6 objects at run 1 → 827 KiB / 202 objects at run 50; records kept 20; per-run 43.8 → 37.8 ms | one run | per-run time flat | **confirmed**: 34× repository growth in 50 runs (4 objects per run) with `max_checkpoints` 20; the measurement cost is still flat at 200 objects (P-015's 31.7 ms was at 20k) |
| P13 | `BenchmarkP4P13ConcurrentBeginFinish` | 1 / 2 / 8 goroutines, same staging dir | 54.085 / 53.695 (repeat 54.116) / 54.131 ms | one run, per-run | 1.5% / 8.1% noisy (repeat 1.6%) / 2.6% | **confirmed**: fully serialized — per-run cost is flat, so 8 concurrent runs take 8× the wall time |
| P14 | `BenchmarkP4P14OverMaxFiles` | 2,000 / 25,000 files, `max_files` 20,000 | cold first `Begin`: 237-250 ms (taken) / **2.54-2.57 s** (skipped); steady `Begin`: 51.2 ms (12.6% noisy, still noisy after a repeat → unmeasured) / 66.6 ms | one `Begin` | cold rows <1%; steady 12.6% noisy / 2.5% | **confirmed**: the first capture of an over-`max_files` workspace stages everything (2.5 s, 25,000 objects written) before reporting `skipped`; every later capture costs 66.6 ms vs 51.2 ms under the limit |
| P15 | `TestP4P15Overshoot` (script) | `max_total_bytes` 64 KiB, one 400 KiB incompressible file | repository after the second `Begin`: **454,656 bytes**, state `open`, overshoot **389,120 bytes** (5.9× the cap) | one `Begin` | exact | **confirmed by reproduction**: M10 is real, not just a reading |
| P16 | `TestP4P16StagingChurn` (script) | 10,000 staging dirs | registry entries 4 → **20,004** (2 per staging dir), unchanged after deleting all 10,000 dirs | `NewService` | exact | **confirmed**: two process-lifetime entries per session ever seen |
| P17 | `TestP4P17DroppedOwners` (script) | 100 owners, 200 rows | after `DeleteOwnersWithPrefix`: rows 0, `owners` **100**, all with `total == 0 && dropped > 0`; `List`/`Counts` do not clear them | one delete | exact | **confirmed**: one retained owner stat per session that ever overflowed |
| P18 | `TestP4P18HubRetention` (script) | 1,000 remotes dirs; 200 follows | 1,000 retained clients, **+1,243 KiB** heap; goroutines 3 → **227** (+224) for 200 follows, back to 3 after `Close`; 601 polls answered in 300 ms | one client / one follow | exact | **confirmed** for both halves: one client per historical (dir,name) until `Close`, one goroutine per followed run, released at `Close` |
| P19 | `BenchmarkP4P19CallChangesWorkspace` | call shapes write / process / read | 0.001 / 0.003 / 0.001 ms (2 / 14 / 2 allocs) | one call | 1.1 / 1.0 / 1.2% | **not confirmed** as a cost: 1-3 µs per call, so 64 parallel calls cost ~0.2 ms per round |
| P20 | `BenchmarkP4P20RecordWrite` | 1 KiB record, process / turn / strict | 0.018 / 0.018 / 0.022 ms | one durable record write | 1.0 / 1.1 / 1.3% | **not confirmed on this filesystem**: on tmpfs an fsync is ~0-4 µs; the answer needs a real disk. The prototype writer exists (P11/P20 share it) |

Verdicts: **confirmed 15** (P01-P10, P12-P18 with the caveats in the table), **confirmed with a corrected mechanism 1** (P03), **not confirmed 4** (P11, P19, P20; P03's sort half). Noisy and still noisy after a repeat (kept unmeasured): P01's absolute 10k figure, P14's steady 2,000-file row. Noisy once, quiet on repeat: P11 strict, P13 2-goroutine.

### 13.3 What the measurement corrected

- **P03**: `List` with `state != active` scans **every row of every owner** (confirmed: 6.6× for 10× invisible rows) but `collect` filters by owner first (`query.go:95-99`), so the **sort** is over the queried owner's rows only. A first fixture of mine (10k rows in one owner vs 10k rows spread over 100 owners) produced 2.181 ms vs 0.168 ms — a 13× difference caused by the *matched* row count, not by the owner count. M06's claim about sorting "every row of every owner" is narrowed to scanning; H01's "a page of 10 costs the same sort as a page of 10,000" is confirmed (2.181 ms for a page of 10 out of 10,000 of the caller's own rows).
- **P09/P14**: on tmpfs the git subprocess count, not the workspace scan, dominates a steady capture (20× files = 1.86× time), so "scales with file count" holds only for the first, cold snapshot (2.5 s at 25,000 files).
- **P11/P20**: the fsync question cannot be answered on tmpfs; both stay open with a prototype benchmark ready.
- **P13 harness note**: the first 8-goroutine run printed 403 ms/op because my loop divided by `b.N` while spawning `ceil(b.N/8)` runs per worker. After splitting `b.N` exactly across the goroutines the row is flat at 54 ms; the artifact is recorded here so the 403 ms number is not reused.

### 13.4 pprof (only where a benchmark showed a cost)

`go test ./internal/asyncmgr/ -run '^$' -bench 'P4P01IndexFlush/10000' -benchtime=2s -cpuprofile /tmp/p4m/p01.cpu` — process-wide profile (it also covers the untimed fixture), focused on `flushIndex`: 22.2% of samples, dominated by `encoding/json/v2.makeStructArshaler.func2` (27% of the focused samples) and `memmove` (6.6%). So the per-flush cost is the **marshal of all rows** (as hypothesised), not the sort or the rename. No other benchmark was profiled: P09/P13 are subprocess-bound (a Go CPU profile shows `Syscall6`/`wait`), P08's 3.6 ms is already attributed by the 10,869 allocations of one parse, and P15-P18 are exact-count probes.

### 13.5 M25 data-race confirmation (one `-race` probe)

- `TestP4M25StatusRace` (scratch, in the asyncmgr copy): register and complete a handle, let `Wait` consume the signal so `row.rec` becomes nil, remove `<id>.json` so `readRecord` fails — the fixture first asserts `row.rec == nil`, i.e. `Status` really is on its fallback path — then 10,000 `Status("s1","a")` calls run concurrently with 10,000 `Complete("a", …)` calls under `-race`.
- Result: **PASS, no data race reported** (`go test -race -count=1 -run '^TestP4M25StatusRace$'`).
- Reason, from the invariants: the fallback is reachable only when `row.rec == nil`, which holds only for a terminal-read row (or a removed one). `Complete` refuses a terminal or `finishing` row, and it writes `r.ok` inside the same critical section that sets `r.rec` (`manager.go:403-411`), so any write of `r.ok` for that row happens before `Status` takes `mu` (`query.go:148`) — the read at `query.go:167` is ordered by the mutex.
- **Verdict: M25 not confirmed as a reachable data race.** The unsynchronised read remains in the source and the two-line fix is still harmless hygiene, but the finding is downgraded from "data race" to "read outside the lock on an unreachable path". `-race` on the four packages (13.1) reported nothing else, so the phase has no measured race.

### 13.6 Ratings and status after measurement

Defaults recorded from the user (they override the merge-time text where they differ):

| id | decision now | basis |
|---|---|---|
| M11 | **low** (was medium) | user default; the window is one record write and the effect is one stale row/signal |
| M08 | **medium, hand-configured topology only** | user default; jad's own listener binds inside a fresh 0700 directory (`app/remote_listen.go:61`), so only a hand-written `unix:` remote in a group-writable directory is reachable |
| M09 | **open**, blocked on real-disk numbers | P12 shows 34× repository growth with no reclamation; whether `max_total_bytes` is reached "for good" in practice depends on the growth rate on a real workspace and disk. `git gc` remains a decision challenge on the recorded "no pruning" answer |
| M06 | confirmed, keep medium | P14: 2.5 s and 25,000 objects written before `skipped` |
| M10 | confirmed, keep medium | P15 reproduced: 389 KiB overshoot of a 64 KiB cap |
| M21, M17 | confirmed growth, keep low | P17 (100 retained owner stats), P16 (20,000 registry entries for 10,000 sessions) |
| M25 | **not confirmed as a race** | 13.5; recommend downgrading to hygiene, fix optional |
| P11, P20 | open (environment) | tmpfs; needs one run on a real disk |
| P19 | not confirmed | 1-3 µs per call; close it |
| P03 sort half | not confirmed | 13.3 |

*Follow-up on a real disk: §14 re-runs M09 and the checkpoint I/O hypotheses (P09-P12, P15, P11/P20) on btrfs. Verdict changes: P11 and P20 are confirmed there; M09 gets disk numbers plus a new sub-finding (M09b, a manual gc does not unstick a live session).*

## 14. Real-disk measurement (M09 and the checkpoint I/O hypotheses)

### 14.1 Preconditions and method

- **Filesystem used:** `/dev/sda2` on `/home/www/Downloads`, `stat -f` type **btrfs**, mounted `rw,noatime,compress=zstd:3,ssd,space_cache=v2`, `/sys/block/sda/queue/rotational` = 0 (SSD), 233 GiB free. This is a real disk, not tmpfs; everything below was written under `/home/www/Downloads/.p4disk` (removed at the end).
- **Two btrfs caveats** for reading the numbers: (a) `compress=zstd:3` means `du -sk` and `git count-objects -v`'s `size` report **block-allocated** KiB, so a small loose object costs a full 4 KiB block and the same bytes compress on disk; (b) `du -sb` (apparent) is the logical byte sum. Both are reported for every state so neither is misread. `git count-objects -v` `size` is what `repositoryBytes()` (`checkpoint/git.go:241-253`) adds up, so the `size_kib` column is exactly the number the `max_total_bytes` gate compares.
- Machine: 12 CPUs (AMD Ryzen 5 3600), go1.27.3 linux/amd64, git 2.56.0, same HEAD `ab86bef`. Graded code is a byte-identical copy (`diff -r` of `internal/checkpoint` and `internal/asyncmgr` against `backend-go`, both exit 0) at `/home/www/Downloads/.p4disk/bg` with two scratch test files added (`p4disk_test.go`, `p4dstale_test.go`); both removed with the scratch directory. **No repository file was created, edited or deleted.**
- `TMPDIR` pointed at the btrfs scratch for every run, so `testing.B.TempDir()`/`t.TempDir()` fixtures were on the real disk, not tmpfs. The package's own suite passes there: `go test ./internal/checkpoint/ -count=1 -skip P4D` → `ok 2.149s` (tmpfs: 2.042 s).
- Serialized, one command at a time. Load average before/after each group: 3.05→2.96 (P09), 2.51→2.04 (P10), 2.07→2.04 and 1.88→1.88 (P11/P20 + repeat), 1.93→1.94 (P12), 1.72→1.72 (P15), 1.82→1.95 (M09), 1.49→1.79→2.66 (P10-large, P09 repeat). The machine was **not idle**: the desktop Firefox (`firefox-bin` 25% CPU, one content process 23%) and the running jad backend (~1%) were up the whole time and cannot be stopped here. This is the source of the outlier samples below; the load was the same for every variant of a comparison, so ratios stay usable.
- 10 samples per benchmark (`-count=10`), `-benchtime=1x` for the run-level benchmark (one `Begin`+`Finish` per sample) and `-benchtime=0.3s` for per-command and per-write benchmarks. **`spread = (max-min)/mean`; > 5% is marked noisy and the pair is repeated once.** Because real-disk samples are noisy by nature, the headline number is the **median**; the mean, min and max are kept in the tables so nothing is hidden.
- Scratch harness: `/tmp/p4stats.py` (parses the raw `ns/op` lines into median/mean/min/max/spread), raw outputs kept in `…/.p4disk/out/`.

### 14.2 Results by hypothesis

**P09 — one `Begin`+`Finish` (the package's `BenchmarkBeginFinishSteady`, TMPDIR on btrfs)**

| files | median | mean | min | max | spread | repeat median | tmpfs (§13) | delta |
|---|---|---|---|---|---|---|---|---|
| 1,000 | **83.1 ms** | 75.3 | 48.7 | 87.1 | 51.0% noisy | 84.3 ms | 72.6 ms | +15% |
| 20,000 | **162.1 ms** | 192.5 | 147.7 | 472.9 | 169.0% noisy (one 472.9 ms outlier) | 161.0 ms (spread 3.9%, clean) | 135.2 ms | +20% |

Verdict **confirmed, and the real disk adds only 15-20%**: the run is not disk-bound, it is subprocess-bound. The two 47-49 ms samples in the 1,000-file row are outliers (page cache / background load), the median is stable at 83-84 ms across the repeat. The second 20,000-file row came out clean, so only the 1,000-file row stays marked noisy.

**P10 — cost of each git subprocess of a steady run (through `shadowGit.run`, warm repo)**

| command | 200 files | 20,000 files | per steady run |
|---|---|---|---|
| `count-objects -v` (the size gate) | 3.75 ms | **40.10 ms** | ×2 |
| `add --all -- .` | 3.00 ms | 14.37 ms | ×2 |
| `write-tree` | 2.09 ms | 3.35 ms (5.4% noisy) | ×2 |
| `ls-files -z` | 1.85 ms | 4.39 ms | ×2 |
| `commit-tree` | 2.80 ms | — | ×2 |
| `update-ref` | 2.91 ms | — | ×2 |
| `git --version` (process floor) | 1.62 ms | — | — |
| **sum of the 12 execs of one run** | **32.8 ms** | **135.8 ms** | |

All rows except `write-tree` at 20,000 files were clean (spread 0.7-4.4%). The 20,000-file sum borrows the 200-file cost of `commit-tree`/`update-ref` (not re-measured: both are size-independent, ~2.8-2.9 ms).

- Verdict **confirmed and now quantified on a real disk**: on tmpfs §13 could only count the execs (12 per steady run, 2 of them `count-objects`); here each has a price. The **process floor is 1.62 ms**, so 12 execs are ~19.4 ms before git does any work.
- **The most expensive subprocess is the size gate, not the staging**: `count-objects -v` costs 40.1 ms at 20,000 files (it stats every loose object, and a fresh 20,000-file repository has ~20,000 blobs) against 14.4 ms for the whole `add --all`. Called twice per run (`service.go:433-456`: once for the gate, once after the snapshot) it is **80.2 ms of the 161 ms (50%)** measured for P09 at 20,000 files, while all 12 execs together are 135.8 ms (84%). This refines P02/M06: the `commitObjectAllowance` cache (`service.go:158-159`) avoids a third call but does not avoid the two.
- At 200 files the whole 12-exec budget is 32.8 ms, i.e. ~56% of P12's 58.9 ms run — consistent with §13's strace attribution.

**P11 / P20 — durable metadata writes (fsync on a real disk)**

`BenchmarkP4D_P11WriteAll` is `writeAll` of **20 records** (~5 KiB); `BenchmarkP4D_P20RecordWrite` is one **1 KiB** `writeFileAtomic`. Medians, n=10, two runs each:

| durability | P11 run 1 / repeat | P20 run 1 / repeat | tmpfs (§13) | what the delta is |
|---|---|---|---|---|
| `process` (no fsync) | 0.14 / 0.15 ms | 0.08 / 0.08 ms | 0.034 / 0.018 ms | CPU only (spread 2-30%) |
| `turn` (file fsync) | **5.71 / 6.13 ms** | **5.93 / 6.29 ms** | 0.034 / 0.018 ms | one `file.Sync()` ≈ **5.8 ms** |
| `strict` (file + dir fsync) | **13.68 / 12.93 ms** | **13.61 / 11.90 ms** | 0.038 / 0.022 ms | `Sync` + directory `Sync` ≈ **13 ms** |

- Both were **not confirmed on tmpfs and are confirmed here**: an fsync costs ~0-4 µs on tmpfs and **5.8 ms on this disk — about 170×**, while `strict`'s extra directory sync adds a further ~7 ms (btrfs transaction commit), i.e. **~350×** the tmpfs figure.
- The cost is **fsync-dominated and payload-independent**: a 5 KiB / 20-record write and a 1 KiB / 1-record write cost the same to within 0.3 ms. So `writeAll`'s JSON marshal is irrelevant next to its durability setting.
- Noise: `turn` 26-61%, `strict` 60-147% (one 30.9 ms sample); medians agree across the repeat within 7%, so the numbers are usable and both rows stay marked noisy.
- Consequence for a run: `writeAll` runs on every `Begin` (`service.go:520`) and every `Finish` (`service.go:561`), so a `turn` session pays **~11.6 ms** and a `strict` session **~26 ms** of metadata fsync per checkpointed run, on top of P12's 58.9 ms.

**P12 — repository growth over 50 runs (200-file workspace, 1 file changed per run, `max_checkpoints` 20)**

| | run 1 | run 50 | growth | per run |
|---|---|---|---|---|
| objects (`count-objects` count) | 306 | 551 | 1.80× | +5.0 |
| counted size (what the cap sees) | 1,224 KiB | 2,204 KiB | 1.80× | **+20.0 KiB** |
| apparent bytes | 65,057 | 210,002 | 3.23× | +2.96 KiB |
| allocated (`du -sk`) | 1,332 KiB | 2,464 KiB | 1.85× | +23.1 KiB |
| run cost | 107.6 ms (cold) | 64.0 ms | flat | median 58.9 ms, min 50.2, max 66.0 |

- Verdict **confirmed**: a 200-file session that keeps 20 checkpoints grows its repository 1.8× in 50 runs and **never shrinks**, at **+20 KiB of gated size per run** (5 objects × a 4 KiB block each; the real delta is 2.96 KiB). The per-run *cost* stays flat over 50 runs (58.9 ms median on the real disk vs 43.8→37.8 ms on tmpfs), so growth is a disk-usage problem, not a speed problem.
- Arithmetic for M09 (not an end-to-end run): at +20 KiB per run the default `max_total_bytes` of 512 MiB is reached after **~25,000 runs** at this workspace shape; a bigger per-run delta reaches it proportionally sooner.

**P15 — one snapshot overshooting `max_total_bytes` (cap 64 KiB, one 400 KiB incompressible file)**

| state | counted size | apparent | allocated | record |
|---|---|---|---|---|
| after the first run | **442,368 B** | 459,319 B | 520 KiB | `complete`, 3 objects |
| after the second `Begin` | 442,368 B | 459,319 B | 520 KiB | `skipped`, "…holds 442368 bytes, over checkpoint.max_total_bytes (65536)" |

- Verdict **confirmed on the real disk, identically to §13**: overshoot **376,832 B = 5.75× the cap**, and from then on every capture is `skipped`. Deterministic across 3 runs (`-count=3`, identical numbers), so no noise marking applies. The §13 tmpfs figure (454,656 B) is exactly 3 blocks (12 KiB) larger, i.e. that fixture had one more capture's objects; the mechanism is identical.

### 14.3 M09 — what pruning frees, and what gc would free and cost

Method: a 200-file workspace, one file changed per run, `Durability: turn`. Phase A runs `N` checkpoints with `max_checkpoints = N` so nothing prunes. Phase B opens a second `Service` on the **same staging dir** with the default `max_checkpoints = 20` and runs once, so `prune` (`service.go:471-482`) deletes the refs of `N-20` records. Each reclamation variant then runs on its **own copy** of that post-prune repository (`cp -a`), so all three start from identical state. `reachable objects` = `git rev-list --objects --all | wc -l`, i.e. what the 20 kept checkpoints actually need.

| N | refs before → after prune | objects before → after prune | counted KiB before → after prune | allocated KiB before → after | reachable objects |
|---|---|---|---|---|---|
| 40 | 80 → 40 | 501 → 506 | 2,004 → 2,024 | 2,424 → 2,284 | 401 |
| 200 | 400 → 40 | 1,301 → 1,306 | 5,204 → 5,224 | 6,904 → 5,484 | 401 |
| 1000 | 2000 → 40 | 5,301 → 5,306 | 21,204 → 21,224 | 29,304 → 21,484 | 401 |

**Deleting the refs frees nothing.** At N=1000, 1,960 refs are deleted and the repository still counts 21,224 KiB (it even *rises* by 20 KiB because phase B's own run added one checkpoint's objects). This is M09 reproduced on a real disk, with the cap-relevant number visible: the kept 20 checkpoints need **401 objects / 33 KiB packed**, while the gate sees **21,224 KiB — 640×** the live state. The only thing the ref deletion actually removed is the 1,960 ref *files* themselves: the apparent bytes fall by 77 KiB (= 1,960 x 40 B) and nothing else. The post-prune state is measured after phase B's own run, which is why objects and counted bytes are 5/20 KiB higher than before the prune.

| variant (on a copy of the post-prune repo) | N=40 (506 obj) | N=200 (1,306 obj) | N=1000 (5,306 obj) | result at N=1000 |
|---|---|---|---|---|
| `git gc` (default prune expiry) | 215 ms → 48 KiB pack | 365 ms → 135 KiB | **1,130 ms → 549 KiB** (5,306 in pack) | unreachable objects survive in a **cruft pack**: 549 KiB vs 33 KiB needed |
| `git gc --prune=now` | 168 ms → **33 KiB** | 244 ms → 33 KiB | **548 ms → 33 KiB** (401 in pack, 0 loose) | exactly the reachable set: **frees 21,191 KiB of the 21,224 KiB counted (99.8%)** |
| `git prune --expire=now` (no repack) | 34 ms → 1,604 KiB loose | 99 ms → 1,604 KiB | **417 ms → 1,604 KiB** (401 loose objects) | frees 19,620 KiB (92%), but leaves 401 loose objects = 1,604 KiB of block padding |

- Cost shape: `gc --prune=now` is **~0.1 ms per object** with a ~100 ms floor (168 ms / 506, 244 / 1,306, 548 / 5,306), i.e. **~26 ms per MiB of counted size** at the largest point. Its cost scales with the number of objects, most of which are garbage, so deferring gc makes each individual gc more expensive.
- `git gc` alone is the wrong command here: it keeps the unreachable objects in a cruft pack (549 KiB at N=1000 vs 33 KiB), because the default `gc.pruneExpire` is two weeks and the deleted refs are seconds old. **`--prune=now` is what frees the bytes.**
- After `git gc` / `git gc --prune=now` the repository is one pack (`count=0, size=0`), so `repositoryBytes()` reads `size-pack` — the 33 or 549 KiB column, and the gate would see a 640x or 38x smaller repository. `git prune --expire=now` alone changes nothing for the gate unless a repack follows: it still reads 1,604 KiB of loose objects.

### 14.4 M09b (new, found by this measurement): a manual gc does not unstick a live session

The measurement above shows gc reclaims 99.8%. It does **not** show that the service notices. `TestP4D_M09StaleCacheAfterGC` (scratch) drives a 200-file session with `max_total_bytes` 1.5 MiB until the gate trips, then runs `git gc --prune=now` in the staging repository:

| step | counted size | cached `repoSize.bytes` | `Begin` result |
|---|---|---|---|
| run 17 trips the gate | 1,581,056 B (386 objects) | 1,581,056 | `skipped`, "holds 1581056 bytes, over … (1572864)" |
| after `git gc --prune=now` | 0 loose, 32,768 B packed (**48× under the cap**) | 1,581,056 (unchanged) | — |
| next `Begin`, same service | 32,768 | 1,581,056 | **`skipped`**, same stale message |
| next `Begin`, a **fresh `Service`** for the same staging dir | 32,768 | 1,581,056 | **`skipped`**, same stale message |

- **Confirmed:** the gate reads the cached size (`service.go:432-442`), and a refusal returns *before* the re-measure at `:452`, so the cache can never correct itself downwards. Because the cache lives in the package-level `repoSizes` map keyed by staging path (`service.go:161-185`), even constructing a new `Service` for the directory inherits it; only a process restart, or deleting the repository (`capture` sets `known = false` when `HEAD` is missing, `:426-428`), resets it.
- This makes M09's consequence sharper than "the cap is reached for good": *an operator who notices and runs gc by hand still cannot recover the session*, which also means any automatic gc fix must invalidate `repoSizes` (or re-measure once when the gate trips) to have any effect.

### 14.5 What this changes against §13

| id | §13 verdict (tmpfs) | §14 verdict (real disk) | what changed |
|---|---|---|---|
| P09 | confirmed as a cost, subprocess-bound | **confirmed**, +15-20% over tmpfs | the disk is not the bottleneck; the 12 git execs are (84% at 20k files) |
| P10 | confirmed (exec counts only) | **confirmed and priced** | 1.62 ms process floor; 32.8 ms (200 files) to 135.8 ms (20k files) per run |
| P02/M06 | `count-objects` counted, not priced | **priced: 40 ms each at 20k files, ×2 per run = 50% of the run** | the size gate is the dominant subprocess, the staging is not |
| P11 | not confirmed (tmpfs) | **confirmed**: 5.8 ms per fsync, 13 ms with the directory sync | ~170× / ~350× the tmpfs figure; payload-independent |
| P20 | not confirmed (tmpfs) | **confirmed**: 5.9 ms (turn) / 12-14 ms (strict) per record write | same mechanism as P11 |
| P12 | confirmed, 34× in 50 runs (6 → 202 objects) | **confirmed, 1.8× counted / 3.2× apparent in 50 runs** (306 → 551 objects) at +20 KiB gated per run | the two multipliers differ only in the baseline: §13's fixture changed no file between runs (4 objects/run from 6), mine changes one file per run (5 objects/run from a first snapshot that already stores 200 blobs). Per-run growth agrees in order of magnitude; the real-disk number that matters for the cap is +20 KiB gated per run |
| P15 | confirmed, 389 KiB overshoot | **confirmed, 376,832 B overshoot (5.75×)** | same mechanism, exact and deterministic |
| M09 | confirmed; consequence = arithmetic | **confirmed with disk numbers**: refs free 0 B, gc --prune=now frees 99.8%, plain gc leaves a cruft pack | plus the new M09b stale-cache result above |

### 14.6 Recommendation: should pruning run gc, and when?

**Yes — but `git gc --prune=now`, after the run has finished, serialized with captures, and only when a prune actually dropped records.**

1. **Which command.** `git prune --expire=now` alone is the cheapest correct reclaim (34-417 ms) and drops the count from 21,224 KiB to 1,604 KiB, but it leaves 401 loose objects whose 4 KiB blocks are what the gate counts. `git gc --prune=now` costs 168-548 ms and reaches 33 KiB — a further 48× reduction in the number the `max_total_bytes` gate compares. Plain `git gc` is not enough: it keeps the fresh unreachable objects in a cruft pack (549 KiB vs 33 KiB at N=1000), because `gc.pruneExpire` defaults to two weeks.
2. **When.** After a `prune` that removed at least one record, i.e. at most once per run, and only when the amount of garbage justifies it: the measured cost is ~0.1 ms/object (~26 ms/MiB), so gc at every prune would add 168-548 ms to every run whose history is already pruned — 3-9× the 59 ms run itself (P12). A reasonable trigger, expressible in the existing settings: run gc when the *dropped* objects since the last gc exceed the objects the kept records need (here: 20 kept records ≈ 401 objects ≈ 33 KiB packed), or when the cached size exceeds ~25% of `max_total_bytes`.
3. **Where it must not run.** Not inside `Begin`/`Finish` while the staging mutex is held (`service.go:169-183`): gc is 0.2-1.1 s here and M16 already shows the mutex blocks the session. Off the critical path (after `Finish` returned, or from a small background worker serialized on the same mutex) keeps the run cost unchanged.
4. **It must invalidate the cache.** M09b: without clearing `repoSizes` (or forcing one re-measure when the gate trips), gc frees the bytes but the session stays stuck forever — the fix would be invisible in practice.
5. **Safety.** `gc.auto=0` and `maintenance.auto=false` are already in `fixedConfig` (`git.go:72-81`), so nothing runs gc on its own today; explicit `gc`/`prune` invocations are unaffected by those keys. Shadow repositories carry no reflogs and `update-ref -d` leaves none, so `--prune=now` has nothing to resurrect.

### 14.7 Limits, what was not measured, and reproducibility

**Not measured (and why):**

- **A true idle machine.** The desktop's Firefox (25% + 23% CPU) and the running jad backend were up throughout; no process was killed. This is what makes the real-disk rows noisy (up to 169% on P09's first 20,000-file row from one 472.9 ms sample). Medians were stable across repeats (within 7%), which is why the headline numbers are medians; the raw samples are in the tables.
- **A non-btrfs filesystem.** This machine has exactly one real disk (`/dev/sda2`, btrfs, `compress=zstd`) and tmpfs. Both btrfs effects are visible in the tables (a 4 KiB block per loose object inflates `size_kib` against apparent bytes; compression shrinks the allocated column after a repack), so an ext4/xfs host will differ in the *allocated* numbers and in the gc cost by some factor. The mechanisms and the orders of magnitude will not change; the exact ms and KiB will.
- **Concurrent runs.** Every measurement is serialized by design (§14.1); the interaction of a background gc with a capture, and the mutex contention of M16 under gc, were not exercised.
- **gc with a live service mid-run.** Only the "operator runs gc, then the service tries again" sequence was tested (14.4). What a gc during an in-flight `add --all` does was not tested (it would need the fix to exist first).
- **P13/P14/P16-P18 on the real disk.** Out of the requested scope; their §13 tmpfs numbers stand (they are count- or CPU-bound, except P14's 2.5 s cold staging, which would be slower on a real disk).
- **The `full` restore path, M02, and the `-race` probe.** Unchanged from §13; no `-race` run was repeated here because nothing in this step touches concurrency.

**Reproducibility.** Scratch files (removed at the end, none inside the repository): `…/.p4disk/bg/internal/checkpoint/p4disk_test.go` (`BenchmarkP4D_P10GitCommand`, `BenchmarkP4D_P10GitCommandLarge`, `BenchmarkP4D_P11WriteAll`, `BenchmarkP4D_P20RecordWrite`, `TestP4D_P12Growth`, `TestP4D_P15Overshoot`, `TestP4D_M09Prune`) and `p4dstale_test.go` (`TestP4D_M09StaleCacheAfterGC`). Command shapes:

```
TMPDIR=<btrfs-scratch> go test ./internal/checkpoint/ -run '^$' -bench '<name>' -benchmem -benchtime=1x|0.3s -count=10
TMPDIR=<btrfs-scratch> go test ./internal/checkpoint/ -run '^TestP4D_(P12Growth|P15Overshoot|M09Prune|M09StaleCacheAfterGC)$' -v -count=1
```

Both scratch files were `gofmt`-clean and the package `go vet`-clean before use. The btrfs scratch directory is deleted; the two scratch test files and the raw benchmark outputs are kept in `/tmp/p4disk-harness/` so the fix step can re-run them (they compile only inside a copy of `internal/checkpoint`). The graded sources were never edited: `diff -r` against `backend-go/internal/checkpoint` and `internal/asyncmgr` was clean when the copy was made, and no file under `backend-go/` was opened for writing at any point in this step.

### 14.8 Status after the real-disk measurement

| id | status now | basis | rating |
|---|---|---|---|
| M09 | **confirmed with disk numbers** (was "open, blocked on real-disk numbers") | refs free 0 B; `gc --prune=now` frees 99.8% in 168-548 ms; the gate sees 640× the live state; default cap reached after ~25k runs at +20 KiB/run (arithmetic on P12) | no change proposed here; the recorded "no pruning" decision is the open question, now with the cost and the reclaim ratio attached |
| M09b | **new, confirmed** (this step) | a manual `git gc --prune=now` does not unstick a live session: the cached size in `repoSizes` is never invalidated downwards, and a fresh `Service` inherits it (§14.4) | low severity on its own (it only delays a recovery), but it is a **precondition for any gc fix** — without it the fix reclaims bytes the gate will not see |
| P11, P20 | **closed: confirmed** | 5.8 ms per fsync, 13 ms with the directory sync, payload-independent (§14.2) | — |
| P02/M06 | confirmed, magnitude now attributed | the size gate (`count-objects -v`, 2× per run) is 50% of a 20k-file run, not the staging | M06 keeps medium |

Everything else in §13.6 stands unchanged, including the user's ratings from the previous step (M11 low, M08 medium in a hand-configured topology only).
