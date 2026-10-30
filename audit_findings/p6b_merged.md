# Phase p6b merged findings — the native command tools: `internal/tools/native` (exec, process, tool_smith, async, shell_async)

- phase: `p6b`, merged at HEAD `385cc89` (go1.27.3 linux/amd64). Worktree before and after this merge: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`; `git diff --check` reports only one pre-existing trailing-whitespace line in `EXECUTOR.md:21`, inherited and untouched. **No repository file was changed** — no source, config, `sample/` or git state. The only repository file written is this one; probes lived in `/tmp/p6bmerge` (one overlay test file, one overlay JSON) and are removed at the end.
- inputs: `p6b_r1.md` (263 lines, 18 findings, 8 hypotheses), `p6b_r2.md` (199, 10, 6), `p6b_r3.md` (137, 6, 5), `p6b_r4.md` (62, 2, 1). Total **36 filings** and 20 raw performance hypotheses.
- scope (`wc -l`, non-test): 33 files / **5,850 lines** in `backend-go/internal/tools/native/` (3 Go platform stubs, 3 `exec_templates/*`, 1 `.ts`). All four reviewers agree on the scope; no re-measurement was needed.
- **r4 filed only 2 findings and cleared everything else it read** (§11 records the spot-check: ten of the 23 entries below live in files r4 declared read and reported sound, including two it explicitly cleared against r1's evidence). **r1 admits it skipped the mandatory index navigation** (one `search_graph` call, then `sed`/`grep`); every location r1 cites was re-read from source here and every one held (§7, §10).
- **Verification outcome: 36 filings → 25 merged entries; 2 reproduced by execution in this merge (P6BM-02, P6BM-05), 22 confirmed by reading source at `385cc89`, 1 refuted as a defect while the fact holds (P6BM-25), 1 more refuted in part (r2's consequence 4, C2), 0 unverified facts left in the list.** 0 filings dropped as prior art; 8 entries merge 19 filings. Nine claims or severities are corrected (C1-C9) and one framing note recorded (C10).
- the task's explicit decisions, each answered below: bridge `ExecutionContext` severity → **medium** (§3.1, C1); second hand copy at `subagent_call.go:54` → **confirmed, but not a defect: the run-scoped fields are filled out-of-band one call later** (§3.1); exec rewriting a literal `{template}` → **reproduced** (§3.2); missing `recover` in the detached shell/process-wait goroutines → **confirmed** (§3.3); async runs losing `ChildGroups` → **confirmed** (§3.4); recycled pgid in `ChildGroups.sweep` → **confirmed structurally, not reproducible here** (§3.5); `latestBlocks` advancing its cursor past unseen output → **confirmed, and the cut drops the *newest* bytes; but "the cursor moves" is documented and pinned by a test** (§4.1, C5); the 12.8 GiB process-log default → **confirmed arithmetic from the shipped defaults** (§3.6).

## 1. Counts

| measure | count |
|---|---|
| reviewer findings filed | 36 (r1 18, r2 10, r3 6, r4 2) |
| merged entries | 25 — every filing carried, none dropped as prior art |
| entries that merge several filings | 8, holding 19 filings: P6BM-01 (4), P6BM-03 (3), P6BM-04 (2), P6BM-05 (2), P6BM-12 (2), P6BM-17 (2), P6BM-18 (2), P6BM-22 (2). The other 17 entries hold one filing each. |
| consensus (2+ reviewers) | 8: P6BM-01 (r1, r2, r3, r4), P6BM-03 (r1, r2, r3), P6BM-04 (r2, r3), P6BM-05 (r3, r4), P6BM-12 (r1, r2), P6BM-17 (r1, r2), P6BM-18 (r1, r3), P6BM-22 (r1, r2) |
| unique (one reviewer) | 17: r1 **12** (P6BM-02, 06, 07, 08, 09, 10, 11, 19, 20, 21, 23, 24), r2 **4** (P6BM-13, 14, 15, 16), r3 **1** (P6BM-25), r4 **0**. r1 supplies 18 of the 36 filings and every one of them maps to its own entry, so it is by far the widest net — and the one reviewer that admits it skipped the index navigation; each of its 18 locations was re-read here. |
| severity as merged: critical / high / medium / low | 0 / 0 / **6** (P6BM-01, 02, 03, 04, 06, 13) / 19 — **now 7 / 18**: measurement raised P6BM-15 to medium (§12-bis) |
| severity as filed (36 filings) | critical 0 / **high 1** (r2-01) / medium **11** (r1 4, r2 2, r3 3, r4 2) / low-medium 1 (r1-04) / low 23 |
| mediums: kept / lowered / raised | 11 filed → **10 kept**, **1 lowered** (r4-02 → P6BM-05, low), and **1 raised** (r2-03, low → P6BM-04, medium, because it is the same cause as r3-03's medium). r2's single **high** was lowered to medium (P6BM-01, C1). |
| r4's own list (2 medium) | 1 kept (P6BM-01), 1 lowered (P6BM-05) |
| verification: reproduced / read-confirmed / refuted / unverified | **2 executed** (P6BM-02, P6BM-05) / **22 read-confirmed** / **1 fact-confirmed-but-refuted-as-a-defect** (P6BM-25) / **0 facts unverified** — three entries carry an *unreproduced* consequence, named in §11 (P6BM-06 pid wrap, P6BM-07 child survival, P6BM-10 race window) |
| every unique (17) and every filed medium/high (12) verified — 25 distinct entries after overlap | 25 verified: 24 as facts, 1 (P6BM-25) as a fact whose *defect* framing is refused. **0 left unverified as facts.** |
| claims corrected | 9 (C1-C9) |
| contradictions recorded | 5 hard contradictions (C1, C2, C3, C4, C9) + 4 corrections |
| performance hypotheses | 20 raw (r1 8, r2 6, r3 5, r4 1) → **9 groups** for the measurement step (§9); **measured in §12-bis: 6 confirmed, 1 confirmed with its mechanism corrected, 1 refuted, 1 refuted as material** |
| findings added by this merge | 0 |
| repeats of prior art | 3: P6BM-01 is carried-in **P6AM-22**; P6BM-03 is **D-1**'s own follow-up ("audit the other unrecovered goroutines", `audit_audit.md:220`); P6BM-06 is the open remainder of **B-C3**. None is counted as a new class. |
| reviewer-pair overlap | see §6: r1+r2 5, r1+r3 3, r1+r4 1, r2+r3 4, r2+r4 1, r3+r4 1 |

## 2. Merged findings

`found by` lists the reviewer filings that map here. `sev` is the coordinator rating; `filed` is what the reviewers said, one letter per filing in the order of `found by` (h = high, m = medium, lm = low-medium, l = low). Status: **R** = this merge executed a probe that reproduced it, **S** = confirmed by reading source at `385cc89`, **S\*** = confirmed as a fact, refused as a defect. Locations are the ones verified in this merge, not quoted from the filings.

| id | title | sev | filed | lens | location | found by | status |
|---|---|---|---|---|---|---|---|
| P6BM-01 | The `tool_smith` bridge builds the nested `ExecutionContext` by hand and drops `AgentDepth`, `RootRun`, `RemoteHops`, so a script's nested tools run at depth 0 / no root / hop 0 | medium | m,h,m,m | B/A | `tool_smith_bridge.go:224-235` (11 of 17 fields copied); struct `tools/catalog.go:27-71`; consumers `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290`, `transcript_path.go:81`, `profile_message.go:356`, `profile_message_remote.go:80`, `subagent_remote.go:127` | r1-01, r2-01, r3-01, r4-01 | S |
| P6BM-02 | exec substitutes `{template}` across the whole argv *after* `{code}` is in it, so a literal `{template}` inside the model's program is rewritten | medium | m | A | `exec.go:366-375` (`prepare` substitutes `{file}`/`{code}`), `exec_template.go:62-67` (`strings.ReplaceAll(item, "{template}", path)` over every element); node/ruby/ts argv `sessionfs/native_exec_settings.go:115,118,120` | r1-02 | **R** |
| P6BM-03 | The detached goroutines that finish a shell run and a managed process have no `recover`, so a panic there takes down the server and leaves the handle `running` | medium | m,m,m | A | `shell_async.go:167-185` (`go func(){ … run(runCtx) … }`), `process_manager.go:378` (`go manager.waitForProcess`); only recover sites in the package are `subagent_manager.go:645` and `tool_smith_bridge.go:345` (the latter logs nothing) | r1-03, r2-02, r3-02 | S |
| P6BM-04 | An async exec/tool_smith run detaches the whole context value chain, so `kill_children_on_run_end` never covers its descendants | medium | l,m | A | `shell_async.go:162-164` (`context.WithCancel(context.Background())` + `withOutputWatch` only), reader `bash_child.go:163-165` (`childGroupsFrom(ctx)` + `request.sweepAtRunEnd`), set at `bash.go:269` | r2-03, r3-03 | S |
| P6BM-05 | `latestBlocks` cuts a block that overruns the aggregate budget from its end — dropping the **newest** output — and still advances the handle's cursor past it | low | l,m | A | `async_latest.go:57-63` (`cutUTF8(text, budget)` then `h.manager.SetProgress(id, next)`), `next` = full produced offsets from `outputLatest` `:136-137`, `cutUTF8` `skill.go:144-155` keeps the head, `tailUTF8` `process_async.go:89` keeps the tail | r3-05, r4-02 | **R** (direction) + S (cursor) |
| P6BM-06 | `ChildGroups.sweep` can signal a recycled process group: it stores only a pgid at call end and re-checks liveness (often hours later) at run end | medium | m | B/A | `child_groups.go:33-57`, liveness `process_group_unix.go:54-56` (`kill(-pgid,0)`), registration `bash_child.go:165`, run scope `app/bound_runner.go:119`, `app/subagent.go:81` | r1-05 | S (not reproduced) |
| P6BM-07 | `AsyncHost.Close` does not join the shell-run goroutines: it returns while a stubborn child is still in its kill grace, and the goroutine then writes the result file and calls `Complete` on a closed manager | low | lm | A | `async_host.go:84` (`Close` = `manager.Close()`), `asyncmgr/manager.go` `Close` (driver `Stop`, then `StopWait` 2 s), `shell_async.go:167-185`, `kill_grace_ms` default 2 s `sessionfs/native_bash_settings.go:14` | r1-04 | S |
| P6BM-08 | `Stop` and `setLeftRunning` can write a stale `running` manifest over the terminal one `finish` already wrote (memory and disk) | low | l | A | `process_manager.go:495-510` (clone under `entry.mu`, write under `manager.mu` after unlocking), `:716-724` (`setLeftRunning`), contrast `finish` `:637-661` | r1-06 | S (not reproduced) |
| P6BM-09 | `ManagedProcessManager.Close` returns while `prune` is still deleting directories, so a caller that removes the storage dir right after races it | low | l | A | `process_manager.go:667-676` (`close(entry.done)` **before** `prune`), `Close` `:525-552` (waits through `Stop`), `prune` `:1056-1117` | r1-07 | S |
| P6BM-10 | A `Stop` that lands between `Async.Begin` and `command.Start` does not stop the process: it is marked `LeftRunning` and then started anyway | low | l | A | `process_manager.go:482-523` (`pid <= 0 || command == nil` → `setLeftRunning`, return), `:290` (`Begin`), `:348` (`command.Start`) | r1-08 | S (not reproduced) |
| P6BM-11 | exec listing renders every stray top-level library entry and every malformed-file problem with no cap, while `matched` is capped | low | l | B/C | `exec_functions.go:136-166` (`strays` per non-runner entry), `:255-266` (all `problems` rendered), `:283-285` (only `shown` capped); `tool_smith_list.go` same shape | r1-09 | S |
| P6BM-12 | The stale-scratch sweep only looks at `os.TempDir()`, so `tool-smith-*` dirs created by the `/tmp` fallback are never swept | low | l,l | A/D | `scratch_sweep.go:21-25` (`sweepStaleScratch(os.TempDir(), …)`), `tool_smith.go:246-262` (`makeToolSmithWorkDir` falls back to `toolSmithShortTempDir` = `/tmp`) | r1-10, r2-07 | S |
| P6BM-13 | `process` logs can reach ~12.5 GiB (13.4 GB, r2's "12.8 GB") per tool-storage directory at the shipped defaults: `log_cap_bytes` 64 MiB × 2 streams × `max_completed` 100 | medium | m | C/B | defaults `sessionfs/native_process_settings.go:15,18` (64 MiB, 100); two spill writers `process_manager.go:273-274`; prune keeps `max_completed` `:1056-1117`; documented default `sample/schemes/setting.schema.json:1564-1568` | r2-04 | S |
| P6BM-14 | Every `tool_smith` run re-reads every library header and re-copies every valid function, whether or not the script calls `fn` | low | l | C | `tool_smith_fn.go:27-77` (`os.ReadDir`, `readToolSmithListEntry`, `copyToolSmithFunction` per file), caps `list_max_files` 200 / `function_max_bytes` 64 KiB | r2-05 | S |
| P6BM-15 | Each async wait/list/status re-indexes the whole transcript of every live agent handle from offset 0, up to `latest_scan_max_bytes` (8 MiB), before showing anything | low | l | C | `async_latest.go:213-249` (`indexTranscriptTurns(ctx, file, stat.Size(), …)` → `transcript_scan.go:95-119` scans from 0), reachable from `async_tool.go:194,213,255` | r2-06 | S |
| P6BM-16 | `ManagedProcessManager.Close` stops running processes one at a time, each up to 2× `kill_grace_ms` | low | l | A/C | `process_manager.go:525-552` (sequential `Stop`), `Stop` `:479-523` (two grace waits) | r2-08 | S |
| P6BM-17 | The "resolve, inside ×2, regular file, no-follow open" sequence exists three times, and the two library listings duplicate the filter/limit/render block | low | l,l | D | `exec_functions.go:60-83` (`openExecFunction`), `tool_smith_fn.go:76-107` (`copyToolSmithFunction`), `tool_smith_list.go:222-240` (`readToolSmithListEntry`); listings `exec_functions.go:223-337` vs `tool_smith_list.go:72-175` | r1-11, r2-09 | S |
| P6BM-18 | `process_manager.go` is 1,117 lines of mixed duties, `Start` repeats the same failure block three times, and the process env builder re-implements the bash one with a different deny order | low | l,l | D | `process_manager.go:1-1117` (Start `:218-385`), failure block `:308-312,332-337,350-355`, env `:754-787` vs `bash.go:382-420` | r1-12, r3-06 | S |
| P6BM-19 | Production dead code: `executeNativeExec` and `ManagedProcessManager.Directory()` have no non-test callers | low | l | D | `exec.go:181-192` (callers only in `exec_test.go:50`, `exec_input_test.go:189`, `child_groups_test.go:170`); `process_manager.go:191` | r1-13 | S |
| P6BM-20 | A code-file write failure returns an error with an empty call id (`"id":""`) | low | l | A | `exec.go:366` (`tools.ExecutionError("", …)`), rendered by `tools/output.go:47-53` | r1-14 | S |
| P6BM-21 | Process start failures hand the model `err.Error()`, which carries the profile's tool-storage path | low | l | B | `process.go:172` (`process_start_failed`, `err.Error()`), wrapped errors `process_manager.go:262,298-309,883` | r1-15 | S |
| P6BM-22 | Test gaps and sleep-based synchronisation in the new commits: no `AgentDepth`/`RootRun`/`RemoteHops` anywhere in the tool_smith tests, no literal `{template}` in an exec code argument, 6 `time.Sleep` in the bridge tests and 6 in `child_groups_test.go`, no stop/exit-race or start-stop-window test | low | l,l | D | `tool_smith_bridge_test.go` (6 sleeps incl. `:577-580,302,612`), `tool_smith_run_test.go` (2), `child_groups_test.go:53,76,93,160`, `exec_template_test.go` (no literal case), `process_block_cancel_test.go:44` | r1-16, r2-10 | S |
| P6BM-23 | Orphan recovery forces SIGKILL on a pid it re-checked only for existence, not identity | low | l | B | `process_orphan.go:131-157` (`stopOrphanGroups`: `processExists(pid)` only), identity proven once at `:104-129` | r1-17 | S (hardening) |
| P6BM-24 | Retention is enforced only when another process finishes; `Recover` never prunes | low | l | A/C | `process_manager.go:673` (the only `prune` call site), `Recover` `:867-940` | r1-18 | S |
| P6BM-25 | Async `tool_smith` runs nil the interaction hooks that sync runs keep — **the divergence is real and correct; what is missing is the reason in a comment** | low | l | D | `tool_smith_async.go:22-26` (`execution.RequestInteraction/OnInteractionResolved = nil`), sync path forwards them via `tool_smith.go:212-221` | r3-04 | S\* |

Severity rule applied: highest evidence, then the median severity of the filings, every departure named. **Kept at medium:** P6BM-01 (median medium, four finders, a tier-1 guard silently zeroed — r2's high refused in C1), P6BM-02 (reproduced; a silent change to model-supplied code), P6BM-03 (three mediums; a panic kills the server — D-1's own remainder), P6BM-04 (raised from r2's low: same cause and consequence as r3's medium, and a documented setting stops applying), P6BM-06 (an unrelated process group can be signalled; needs pid wrap, so medium not high), P6BM-13 (a model-reachable path to 12.8 GiB of disk from the shipped defaults). **Lowered:** P6BM-05 (from r4's medium: a summary line loses its newest bytes, the full result still exists, and "the cursor moves" is documented at `async_latest.go:36-38` and pinned by `TestLatestTotalIsCappedAcrossHandles`), P6BM-07 (from r1's low-medium: needs a SIGTERM-ignoring child at shutdown), P6BM-25 (r3's low, refuted as a defect).

`sev` summary: **medium** P6BM-01, 02, 03, 04, 06, 13; everything else low. Per reviewer (entries, not filings): **r1** 18 entries from 18 filings — 6 consensus (P6BM-01, 03, 12, 17, 18, 22), 12 unique, 4 medium kept (P6BM-01, 02, 03, 06) and one lowered (P6BM-07); **r2** 10 entries from 10 filings — 6 consensus (P6BM-01, 03, 04, 12, 17, 22), 4 unique, 4 medium (P6BM-01, 03, 04, 13) and its single high lowered; **r3** 6 entries from 6 filings — 5 consensus (P6BM-01, 03, 04, 05, 18), 1 unique, 3 medium (P6BM-01, 03, 04) and 1 refused defect; **r4** 2 entries from 2 filings — 2 consensus, 1 medium kept and 1 lowered.

## 3. Verification of the medium findings

Probes are `go test -overlay` files mapped onto `internal/tools/native`, so nothing was written in the tree. Commands and exit codes are in §10.

### P6BM-01 (medium, B/A) The bridge drops three run-scoped fields — r1-01, r2-01, r3-01, r4-01 — **confirmed by reading, including every consumer r2 named**

The struct has **17 fields**; the bridge literal copies **11** (`tool_smith_bridge.go:224-235`: `WorkspaceRoot, SessionDir, SessionKey, AgentName, PathScope, RequestInteraction, OnInteractionResolved, OnFileChange, ProcessOwner, ProcessOwnerLimit, ProcessAsync`). It correctly omits the three per-call fields (`CallID`, `ToolName`, `RawArguments` — the catalog sets them). It also drops `AgentDepth`, `RootRun`, `RemoteHops`, which are run-scoped. The nested `tools.Catalog` therefore executes with `depth=0, root="", hops=0`. r1's probe printed exactly that (`depth=0 root="" hops=0 owner="run1"`, exit 0); the literal settles it without a probe, so nothing here rests on r1's measurement.

Each consumer r2 listed exists, read at `385cc89`:

1. **child gate skipped** — `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290` refuse only when `AgentDepth > 0 && !AllowChildren`. `transcript` and `transcripts` are **not** in `nativeToolSmithExcludedTools` (`sessionfs/native_tool_smith_settings.go:41`: bash, exec, process, async, subagent, tool_smith, task), so the profile config decides.
2. **own-run selector** — `transcript_path.go:81` (`AgentDepth == 0 || ProcessOwner == ""` → the parent's own transcript instead of `subagent:<run>`).
3. **child call flag** — `profile_message.go:356` (`if execution.AgentDepth > 0 { WithChildCall }`), and `internal/app/profile_message.go:70-83` then cannot find the caller transcript path.
4. **`max_runs_per_root` — REFUTED as a bridge consequence (C2).** `subagent` is in `nativeToolSmithExcludedTools`, so a script cannot start a nested run at all; r2's consequence 4 cannot be reached through the bridge. It is a real property of the *second* hand copy's call site (which passes `rootRun` out-of-band, `subagent_call.go:91`), not of the bridge.
5. **hop counting** — `profile_message_remote.go:80` and `subagent_remote.go:127` send `RemoteHops+1`. Only the `profile_message` half is reachable through the bridge (again: `subagent` is excluded).

**The second hand copy at `subagent_call.go:54-60` — confirmed, and not a defect.** It copies `WorkspaceRoot, SessionDir, SessionKey, AgentName, RemoteHops` only, but the dropped run-scoped fields are assigned one call later in `subagent_manager.go:569-573` (`ProcessOwner = id`, `AgentDepth = spec.depth + 1`, `RootRun = root`, `ProcessOwnerLimit`, `ProcessAsync`, plus `OnFileChange` at `:575`). It is a deliberate narrowing with an out-of-band transfer, not a forgotten copy — but it is hand-written twice in the same package and drifts (the bridge keeps `PathScope` and hooks, the child copy does not), which is the maintainability half of r1/r2/r3's fix proposal.

Severity: **medium**, not r2's high (C1). The mechanism is confirmed and the fix is a small helper, but every affected guard is a config-scoped one whose enabling setting is *not* on by default for the script path (default nested tools are `read, grep, glob`), the escalation requires the local user to add `transcript`/`profile_message` to `tools.native.tool_smith.tools`, and nothing here is reachable unauthenticated or remote. What would justify high: the user configuring `transcript` into the nested list *and* `allow_children: false` — then a child agent reads transcripts it is explicitly forbidden to read. **Decision 1 asks the user to confirm the bar.**

### P6BM-02 (medium, A) exec rewrites `{template}` inside the model's program — r1-02 — **reproduced by execution here**

Probe `TestP6BMExecTemplateRewritesModelCode` (`/tmp/p6bmerge/probe_test.go`, overlay onto `internal/tools/native`, exit 0), calling the product's `attachExecTemplate` with the argv `plan.prepare` produces for node:

    argv before: ["-r" "{template}" "-e" "console.log(\"{template}\")"]
    argv after : ["-r" "/tmp/…/template.js" "-e" "console.log(\"/tmp/…/template.js\")"]
    code element changed: true

The order is fixed in code: `exec.go:366-375` substitutes `{file}` then `{code}` into each flag, then `:229` calls `attachExecTemplate`, which runs `strings.ReplaceAll(item, "{template}", path)` over **every** element (`exec_template.go:62-67`). It applies to the runners that have both a template and arg delivery — `node`, `ruby`, `ts` in the shipped defaults (`native_exec_settings.go:115,118,120`); `{file}` and `bash`/`sh`/`perl` code are unaffected, and file-mode runners (`mode: file`) are unaffected because the program is not in argv. Reach: the model writes the program; a literal `{template}` (string templating, a format string, documentation in a string) is silently replaced by a path, changing what runs and what it prints.

Severity: **medium** — a silent correctness bug in exactly the input the tool exists to run, but it needs the literal, so it is narrow. Fix: substitute `{template}` into the configured flags **before** `{code}` is inserted (the path is known at that point: `workDir/template<ext>`), i.e. move the template write ahead of `prepare` or pass the template path into it.

### P6BM-03 (medium, A) The detached finishers have no `recover` — r1-03, r2-02, r3-02 — **confirmed by reading; the count of recover sites is exact**

`grep -rn "recover()" internal/tools/native/*.go` (non-test) returns **two** lines: `subagent_manager.go:645` and `tool_smith_bridge.go:345` (the latter swallows the panic and logs nothing). Neither covers the two goroutines this entry names:

- `shell_async.go:167-185`: the goroutine that runs `run(runCtx)` — for exec that is `attachExecTemplate`, `stageToolSmithFunctions`, `copyToolSmithFunction`, `runShellChildDetailed`; for tool_smith the whole script run — then writes the result file and calls `Complete`.
- `process_manager.go:378`: `go manager.waitForProcess(command, entry)` — it runs `finish`, the async `End`, `prune` and `persistManifest`.

An unrecovered panic in any goroutine terminates the process, so the outcome is a dead server, and (as r2 notes) the handle never reaches a terminal state because `Complete` is not reached. D-1 fixed the *call* path (`tools/catalog.go:407-415`) and its own acceptance text asked for the other goroutines to be audited (`audit_audit.md:220`), so this is prior art with new sites, not a new class.

Severity: **medium** from three mediums. Nothing in the tree panics on these paths today (the reachable trigger is a future bug or a runtime panic in a huge allocation), but the blast radius is the whole server, and the fix is a three-line `defer`. Also log the bridge's swallowed panic (`tool_smith_bridge.go:345`) — the same diagnostics gap as P6AM-13.

### P6BM-04 (medium, A) An async run loses the run's context values — r2-03, r3-03 — **confirmed by reading**

`startShellAsync` builds `runCtx, cancel := context.WithCancel(context.Background())` and re-attaches only the output watch (`shell_async.go:162-164`). Detaching *cancellation* is intended (the call returns; the child must survive it). Dropping the *values* is not: `bash_child.go:163-165` registers the group with `childGroupsFrom(ctx)` only when `request.sweepAtRunEnd` is set, and that setting is `settings.KillChildrenOnRunEnd` (`bash.go:269`). On the sync path the tool's own context carries `ChildGroups` (`app/bound_runner.go:119`, `app/subagent.go:81`); on the async path the lookup finds nil, so a descendant left behind by an async exec/tool_smith is never swept at run end even with `kill_children_on_run_end: true`.

Severity: **medium** (raised from r2's low because r3's medium names the same cause and the consequence is the documented setting silently not applying). Fix: `context.WithCancel(context.WithoutCancel(ctx))`, one line; test: an async exec that leaves a group must appear in `ChildGroups` after the call returns.

### P6BM-06 (medium, B/A) A recycled pgid in `ChildGroups.sweep` — r1-05 — **confirmed structurally; not reproducible under this step's rules**

`add` is deferred in `bash_child.go:165`, i.e. it runs when the child exits or the call ends, and it stores only the pgid if a member is alive (`child_groups.go:33-40`). `sweep` runs once, at the **run's** end (`WithChildGroups` returns `groups.sweep`), which can be hours later; for each stored pgid it re-checks `processGroupAlive` and then sends SIGTERM, with SIGKILL after that call's grace (`child_groups.go:42-57`, `process_group_unix.go:54-56`). If the original group ended in between and the number was reused as a group id by an unrelated process, the stranger gets signalled. The neighbours are safe for a *different* reason and must not be cited against this: `killGroupAfter`/`endProcessGroup`/the orphan path poll liveness inside one grace window (`process_manager.go:612-631`, `process_orphan.go:131-157`), and a reaped leader's start time cannot be re-checked. r2's HY-11 ("recycled pgid disproved") is about that grace-timer path, not this one (C3).

Severity: **medium**: the damage is killing an unrelated process group, but it needs pid wrap between call end and run end (real on hosts with `pid_max` 32768 under fork-heavy load — the reviewers' judgement, not measured here). Fix needs a design choice (drop a group as soon as it is observed dead; or record the group leader's start time / a pidfd, Linux-only, as `classifyOrphan` already does).

### P6BM-13 (medium, C/B) 12.5 GiB of process logs at the defaults — r2-04 — **confirmed arithmetic from the shipped defaults**

`defaultNativeProcessLogCap = 64 * 1024 * 1024` and `defaultNativeProcessMaxCompleted = 100` (`sessionfs/native_process_settings.go:15,18`), and the schema publishes the same numbers (`sample/schemes/setting.schema.json:1564-1568`: `minimum 1, maximum 1073741824, default 67108864`). Each process opens **two** spill writers at that cap (`process_manager.go:273-274`), and `prune` keeps `max_completed` finished processes (`:1056-1117`). 64 MiB × 2 × 100 = 12,800 MiB = **12.5 GiB (13.4 GB) per tool-storage directory** — the figure r2 rounds to "12.8 GB", all written before retention removes anything, plus up to `max_running` (8) more live: another 1 GiB. Reach: the `process` tool is model-reachable, and a chatty child (`yes`, a build log) fills its 128 MiB without any mistake by the user.

Severity: **medium**. It is a settings-default question, not a code defect — the knob exists and is documented — so the fix is a config round: lower the default and/or add a per-directory total-bytes budget checked before opening the writers. Do **not** hardcode a cap (the repository's config rules), and note the TS-parity origin of 64 MiB before changing it.

## 4. Verification of the unique low findings, the rest, and the refuted one

### P6BM-05 (low, A) The aggregate budget cut drops the newest bytes and advances the cursor — r3-05, r4-02 — **reproduced (direction) + confirmed by reading (cursor)**

Probe `TestP6BMLatestBudgetCutDropsNewestAndAdvancesCursor` (same overlay, exit 0), with `latest_bytes` 100 / `latest_total_bytes` 150 and a fake driver whose text is 60×`A` then 40×`Z` (oldest → newest):

    first block len=100 second block len=50
    second block: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    second block keeps newest bytes: false

So the second handle's block is cut to its **oldest** 50 bytes and the newest 50 are dropped, while `latestBlocks` then calls `h.manager.SetProgress(id, next)` with `next` carrying the collector's **full** produced offsets (`async_latest.go:60-63`, `:136-137`): the dropped bytes are never offered again. The per-stream cut does the opposite and is honest (`outputLatest` uses `tailUTF8` and an explicit "…+N bytes earlier" marker), so the two cuts in one function disagree about which end matters.

**What is and is not a defect.** `latestBlocks` documents "A block that fits only in part is cut and its cursor moves" (`async_latest.go:36-38`), and `TestLatestTotalIsCappedAcrossHandles` pins the 50-byte second block, so the cursor movement is intended. Two things are not covered by that intent and are why the entry stays: the cut keeps the **oldest** bytes (against the "newest news only" contract in the same comment and against `outputLatest`'s own tail-keeping), and there is no truncation marker, so the model cannot tell it lost anything. r3 stated the first precisely; r4 stated the second.

Severity: **low** (r4 filed medium, r3 low). It is a summary line; the full result is still in the handle's result file and comes back on `wait`, the loss needs ≥2 live handles (defaults: 500 bytes per stream ×2 + head ≈ 1.1 KB per handle against 2000 total), and the behaviour is tested. Fix: keep the cursor when the text was cut (as the `budget <= 0` path already does), or cut with `tailUTF8` and append the existing earlier-bytes marker.

### P6BM-07 (low, A) `AsyncHost.Close` does not join the shell-run goroutines — r1-04 — **confirmed by reading**

`AsyncHost.Close` is `h.manager.Close()` (`async_host.go:84`). The manager stops each driver and then waits for `liveTotal + completing == 0` for at most `StopWait` (`asyncmgr/manager.go` `Close`; 2 s default). The shell driver's `Stop` only cancels the run context, which sends SIGTERM and then honours `kill_grace_ms` (default 2 s) before killing — so a child that ignores SIGTERM outlives both the grace and `StopWait`. The goroutine then writes `<async>/exec/<id>.txt` and calls `Complete` on a manager whose index is closed and whose `ioClosed` is set — the same shape as the known `.async: directory not empty` flake, and the run is not in `ChildGroups` either (P6BM-04) nor in an owner record (unlike `process`). Nothing observes it from the test suite.

Severity: **low** (r1 said low-medium): requires a SIGTERM-ignoring child at shutdown, and the residue is a stray result file. Fix: a `sync.WaitGroup` (or the `shellRuns` done channels) waited on by `Close` after `manager.Close`, bounded by the kill deadline; then `Complete` before returning. This is decision 3.

### P6BM-08 (low, A) A stale manifest can overwrite the terminal one — r1-06 — **confirmed by reading, not reproduced (r1's probe found 0 stale manifests in 400 sequential start/wait cycles, which does not exercise the race)**

`Stop` clones the manifest under `entry.mu`, releases the lock, then writes `manager.manifests[id]` and persists (`process_manager.go:495-510`); `finish` writes the same two places after taking the same two locks (`:637-661`). If `finish` completes inside that window, `Stop` reinstates `state: running`. `setLeftRunning` has the same shape and a wider window (`:716-724`). Consequences r1 lists are all readable in the code: the run-capacity count `Start` performs (`:233`) and `runningOwnedLocked` count a dead process, `prune` never removes a non-terminal manifest (`List` filters by it), and after a restart `Recover` marks the process lost.

Severity: **low**: a narrow race whose effect is a phantom "running" record until restart. Fix: write `manager.manifests` under `entry.mu` and only when `!processTerminal(entry.manifest.State)`; test with a loop that stops a `sleep 0` process at the moment it exits.

### P6BM-09 (low, A) `Close` returns while `prune` still deletes — r1-07 — **confirmed by reading; r4 cleared this ordering (C4)**

`finish` closes `entry.done` **before** calling `prune`, with a comment explaining why (`process_manager.go:667-676`: a `Wait` on this process must not wait for other processes' directories). `Close` waits for processes through `Stop` → `waitForManagedProcess(entry, killGrace)`, which returns when `done` closes (`:525-552`, `:737-746`). So `Close` can return with a `RemoveAll` of old process directories still in flight; a caller that removes the storage dir immediately after (a test with `t.TempDir()`, resolver shutdown) races it. r4 recorded the same ordering as "checked and sound" — the ordering is deliberate, but the consequence is real; both positions are recorded in C4 and the entry stays low.

### P6BM-10 (low, A) A `Stop` before `command.Start` does not stop the process — r1-08 — **confirmed by reading**

From `Async.Begin` (`process_manager.go:290`) to `command.Start` (`:348`) the entry is live but has no pid and no command, so `Stop` takes the `pid <= 0 || command == nil` branch, marks `LeftRunning` and returns (`:482-523`); `Start` then spawns anyway and nothing signals it. Reachable from a sub-agent run end (`CancelOwner`/`stopOwned`) and, if `Close`'s `starting.Wait()` were removed, from shutdown. The window is an owner-record write plus a manifest fsync, so it is milliseconds.

Fix: after `command.Start()`, check the entry's `StopReason` and signal the group at once (or mark the entry so `Start` aborts before spawning).

### P6BM-11 (low, B/C) Listing problems are uncapped — r1-09 — **confirmed by reading**

`scanExecHelpers` appends one stray line per top-level entry of the library that is not a runner name (`exec_functions.go:158-164`) and `executeExecList` renders **all** of `problems` (`:255-266`) while capping only `shown` at `list_max_results` (`:283-285`). The malformed-file half is bounded by `list_max_files` because it comes from `scanned`; the stray half is bounded only by the directory size. The library lives in the workspace, so the model (or a checked-out repository) chooses it, and the result text is not size-limited downstream (`tools.RenderToolResult` only wraps it, `tools/output.go:46-70`). `tool_smith_list.go:97-136` has the same shape.

Severity: **low**: it needs a deliberately junk-filled library, the cost is one large tool result (not persisted), and nothing is corrupted. Fix: cap `problems` at `list_max_results` and add "…and N more".

### P6BM-12 (low, A/D) The sweep misses the `/tmp` fallback — r1-10, r2-07 — **confirmed by reading**

`sweepStaleScratchOnce` calls `sweepStaleScratch(os.TempDir(), …)` once per process (`scratch_sweep.go:21-25`), while `makeToolSmithWorkDir` puts `tool-smith-*` under `toolSmithShortTempDir` (`/tmp`) whenever the socket path would exceed 100 bytes (`tool_smith.go:242-262`). With `TMPDIR` set elsewhere, those directories are never swept after a crash. `exec-` dirs always use `os.TempDir()` (`exec.go:219`), so only tool_smith is affected.

Fix: run the sweep over both directories inside the same `sync.Once`, deduplicated. The sweep itself is sound: `Lstat`, directories only, exactly 0700, prefix plus 7 h age, `RemoveAll` on the path only — both r1 and r2 checked it and neither found a symlink path.

### P6BM-14 (low, C) Every `tool_smith` run stages the whole library — r2-05 — **confirmed by reading; measurement is §9 PH2**

`stageToolSmithFunctions` does one `os.ReadDir`, then for each of up to `list_max_files` (200) `.ts` files a `readToolSmithListEntry` (EvalSymlinks + stat + open + 2 KiB header) and a `copyToolSmithFunction` (another EvalSymlinks + stat + open + full copy up to `function_max_bytes` = 64 KiB) into `workDir/fn` (`tool_smith_fn.go:27-77`). At the defaults that is up to 200 × 64 KiB ≈ 12.8 MiB copied per run even when the script calls nothing, which r2 sized at ~2 MiB / ~600 syscalls for a realistic 200 × 10 KiB library.

### P6BM-15 (low, C) Each async poll re-indexes every live transcript — r2-06 — **confirmed by reading, with r2's wording corrected (C6)**

`activityLatest` opens the transcript, and calls `indexTranscriptTurns(ctx, file, stat.Size(), cfg.LatestScanMaxBytes)` (`async_latest.go:238`), whose body is `scanTranscript(ctx, file, 0, size, …)` with a `json.Unmarshal` per line — i.e. **the whole file from offset 0**, up to `latest_scan_max_bytes` (8 MiB, `asyncmgr/config.go:72`). Only then does a second pass start at the cursor's turn (`:240-244`). r2 wrote "scanTranscript walks it again", which overstates the second pass: it walks from `turns[first].off`, i.e. the news. The per-call cost is therefore one full index pass per live agent handle per `wait`/`list`/`status` that attaches latest blocks (`async_tool.go:194,213,255` via `host.latestBlocks`), independent of how much is new.

### P6BM-16 (low, A/C) `Close` stops processes sequentially — r2-08 — **confirmed by reading; measurement is §9 PH3**

`Close` collects the running ids and calls `Stop` one at a time (`process_manager.go:525-552`); each `Stop` may wait `kill_grace_ms` for SIGTERM and again for SIGKILL (`:479-523`), i.e. up to 2 × 2 s per process at the defaults, and 8 × 4 s ≈ 32 s per storage directory at `max_running` 8 when the children ignore SIGTERM. `resolver.Close` runs managers in sequence, and the HTTP shutdown deadline does not cover this (`internal/app/shutdown.go:14-36`). The comment on `finish`'s early `done` (P6BM-09) is unrelated; the reference pattern is `stopOrphanGroups`, which signals every group first and then waits (`process_orphan.go:139-158`).

### P6BM-17 (low, D) Three copies of the containment-and-open sequence, two library listings — r1-11, r2-09 — **confirmed by reading**

`openExecFunction` (`exec_functions.go:60-83`), `copyToolSmithFunction` (`tool_smith_fn.go:76-107`) and `readToolSmithListEntry` (`tool_smith_list.go:222-240`) each do `EvalSymlinks` → `toolSmithInside(dir)` **and** `toolSmithInside(realRoot)` → `Stat` + `IsRegular` → the shared `openToolSmithFile`, with near-identical problem strings ("cannot be resolved", "symlink leaves the functions directory", "is not a regular file"). The security-critical part (`openToolSmithFile`: `O_NOFOLLOW` + `SameFile`) **is** shared, so this is duplication of the *checks*, not of the primitive — which is why it stays low. The listings (`executeExecList` `exec_functions.go:223-337` and `executeToolSmithList` `tool_smith_list.go:72-175`) repeat the filter parsing and the `truncated`/`shown`/`suggestion` block with the same wording and already diverge ("malformed entries" vs "malformed files"). Fix: one `openLibraryFile(dir, realRoot, file, maxBytes)` and one `renderLibraryListing(...)`; keep the two scanners.

### P6BM-18 (low, D) `process_manager.go` mixed duties, repeated failure block, duplicated env builder — r1-12, r3-06 — **confirmed by reading**

`wc -l` = **1,117** for one file owning lifecycle, disk format, log windows and env construction; `Start` spans `:218-385` and repeats `manager.finish(entry, ProcessFailed, ProcessManifest{…null, null})` at `:308-312`, `:332-337`, `:350-355`. `buildNativeProcessEnvironment` (`:754-787`) and `buildNativeBashEnvironment` (`bash.go:382-420`) produce the same allowlist → overrides → deny result by different means: bash filters `denied` during both merges, process merges then `delete`s. Equivalent today (r1 checked the same thing and called env inheritance sound); it is a "change it twice" hazard, not a bug. Fix, later and alone: split the file, add `failStart(entry, cause)`, and extract one `buildNativeEnv`.

### P6BM-19 (low, D) Dead code — r1-13 — **confirmed by grep**

`grep -rn "executeNativeExec\b" internal --include=*.go` (non-test) finds only the definition at `exec.go:182`; its callers are three `_test.go` files. `grep -rn "Directory()" internal --include=*.go` (non-test) finds only the definition at `process_manager.go:191`. Both are "test-only production code" rather than dead code (C10): the wrapper exercises the same path with `host == nil` that the registered closure uses, so deleting it means the tests move to `_test.go`, not that coverage is lost.

### P6BM-20 (low, A) Empty call id in one error — r1-14 — **confirmed by reading**

`prepare` has no call id and renders `tools.ExecutionError("", tools.StatusError, "spawn_failed", "The code file could not be written", …)` (`exec.go:366`); `RenderToolResult` writes that into `{"id": …}` (`tools/output.go:47-53`), so the result header carries `"id":""`. Every other error in the file passes `callID`; the outer wrapper re-renders nothing, so the model still matches the output, but transcripts and the bridge see a blank id.

Fix: return the system error from `prepare` and render it with `callID` at `exec.go:227-232`.

### P6BM-21 (low, B) Process start errors carry the storage path — r1-15 — **confirmed by reading**

`process.go:172` returns `process_start_failed` with `err.Error()`, and `Start`'s errors wrap `os` failures naming the storage directory (`process_manager.go:262` "create process storage: …", `:298-309`, `:883`). So the model can read e.g. `/home/…/tool-storage/processes/proc_…: permission denied`. It is the profile's tool storage, not the workspace, and the class (a server path in bounded model-visible text) was merged low before (P5M-03). Fix: map to a fixed message and log the detail, as `async_capacity_reached` already does.

### P6BM-22 (low, D) Test gaps and sleep-based synchronisation — r1-16, r2-10 — **confirmed by grep**

Verified counts at `385cc89`: `grep -rn "AgentDepth\|RootRun\|RemoteHops" internal/tools/native/tool_smith*_test.go` → **no output** (so nothing pins P6BM-01's fix); `grep -rn "{template}" exec*_test.go` finds the placeholder only in test argv definitions, never in a code argument (so nothing pins P6BM-02); `grep -c "time.Sleep"` gives 6 in `tool_smith_bridge_test.go`, 1 in `tool_smith_async_test.go`, 2 in `tool_smith_run_test.go`, and 6 in `child_groups_test.go` (five at 300 ms, one at 20 ms). `tool_smith_bridge_test.go:577-580` concludes "`Close` is blocked" after two 100 ms sleeps — the assertion can pass for the wrong reason on a loaded machine. Also missing: a stop/exit race (P6BM-08), a start/stop window (P6BM-10), a panic in a finisher (P6BM-03), a stubborn child at `Close` (P6BM-07).

### P6BM-23 (low, B, hardening) The forced orphan kill re-checks existence only — r1-17 — **confirmed by reading**

`classifyOrphan` proves identity once (owner record + dead server + pid start time, `process_orphan.go:104-129`); `stopOrphanGroups` then sends SIGTERM, waits one grace polling `processExists(pid)`, and forces the rest (`:131-157`). A pid reused inside that grace (default 2 s) would be shot. r1 rates it negligible on Linux's sequential allocator and asks for a start-time comparison before the forced signal; cheap, and it closes the last B-C3 residue direction. Keep at low and **unreachable in practice** as filed.

### P6BM-24 (low, A/C) Retention only runs on a finish — r1-18 — **confirmed by reading**

`prune` has exactly one call site, inside `finish` (`process_manager.go:673`); `Recover` (`:867-940`) never prunes, so a profile that stops starting processes keeps its old manifests and logs on disk indefinitely, and recovered "lost" manifests are counted in `manifests` until the next finish. Bounded by disk, not memory. Fix: call `prune` at the end of `Recover`.

### P6BM-25 (low, D) The async tool_smith hook nil-ing — r3-04 — **fact confirmed, refused as a defect**

`startToolSmithAsync` sets `execution.RequestInteraction = nil; execution.OnInteractionResolved = nil` before it runs the script (`tool_smith_async.go:22-26`), while the sync path builds the bridge with the outer call's hooks (`tool_smith.go:212-221`). The divergence is real. It is also **required**: the call has already returned, so a question raised by a nested tool afterwards has no one to answer and no place to be rendered — r2 reached the same conclusion and listed it under "checked and sound" (§6 of `p6b_r2.md`), which is the contradiction C9. What is missing is one comment saying so (and, if the description is silent, one sentence in the tool description). Keep as a documentation nit; do not remove the nil-ing.

## 5. Refuted filings, contradictions and corrections

### C1 (severity) r2 files the bridge `ExecutionContext` drop as **high**; r1, r3 and r4 file it medium — **medium, with the escalation named**

Confirmed as a fact (§3.1). r2's high rests on consequence 1 (a child reading transcripts against `allow_children: false`). I keep medium because: the affected nested tools are not in the default script allow-list (`read, grep, glob`), reaching the guard requires the local user to add `transcript`/`profile_message` to `tools.native.tool_smith.tools`, the settings that the drop defeats are themselves config-scoped switches, and nothing is reachable remotely or unauthenticated. The plan's own bar for high ("reachable bug or leak with real impact") is arguable, not clear. **Decision 1 puts the bar to the user**; if the user considers "a configured guard silently not applied" enough, this becomes high and should be fixed first.

### C2 (fact) r2's consequence 4 (`max_runs_per_root` bypass) and the `subagent_remote` half of consequence 5 are **unreachable through the bridge**

`nativeToolSmithExcludedTools` is `bash, exec, process, async, subagent, tool_smith, task` (`sessionfs/native_tool_smith_settings.go:41`), so a script cannot call `subagent` at all; `subagent_call.go`'s `rootRun: execution.RootRun` and `subagent_remote.go:127`'s hop arithmetic are therefore not reachable from a dropped-`RootRun`/`RemoteHops` context. r1 said so explicitly ("Not reachable: nested `subagent` is excluded") and r3 repeated it; r2's list is right about 1, 2, 3 and the `profile_message_remote` half of 5, wrong about 4 and the `subagent_remote` half. The entry is unchanged (the reachable consequences are enough) but the severity argument must not rest on the unreachable ones.

### C3 (contradiction) r2's HY-11 ("a recycled pgid can be signalled by the grace timer — disproved") versus r1-05 (medium) — **both right, different code**

r2 verified `process_manager.go`'s grace paths, which do poll liveness inside one grace window (`:612-631`) and whose owner records carry a start time (`process_orphan.go:104-129`). r1 files `child_groups.go`'s sweep, whose window is call-end → run-end (hours) and which has no identity proof at all. r3's H7 ("orphan recovery can signal a recycled pid — disproved") is about the same safe path as r2's. Resolution: P6BM-06 confirmed for `ChildGroups.sweep` only; the two clearances stand for their own paths and must not be quoted against it.

### C4 (contradiction) r4 clears the `done`/`prune` ordering that r1-07 files — **ordering deliberate, consequence real; the entry stays low**

r4: "final async notification is sent before `done` closes, while pruning follows `done`" (listed as checked and sound). r1: therefore `Close` returns while `prune` still removes directories. Both read the same two lines; the comment at `process_manager.go:667-669` shows the early `done` is deliberate (a `Wait` must not wait for other processes' cleanup). The deliberate part is the *ordering*; the unconsidered part is that `Close` is a caller who then owns a directory something else is still deleting. Kept as P6BM-09, low, and its fix must keep the early `done` (track the finisher with a `WaitGroup` instead).

### C5 (verdict) The `latestBlocks` budget cut: r3 and r4 file a bug, the code documents it, a test pins it — **fact confirmed, defect narrowed**

`async_latest.go:36-38` documents "A block that fits only in part is cut and its cursor moves", and `TestLatestTotalIsCappedAcrossHandles` asserts the 50-byte second block (that is why r3 and r4 both ran it and it passed). What the intent does **not** cover: the cut keeps the head, i.e. drops the newest bytes — the opposite of `tailUTF8`'s policy in the same function — and there is no marker. Severity therefore low, not r4's medium, and the fix is "keep the cursor when cut" plus a tail cut, not a redesign.

### C6 (correction) r2-06's "`scanTranscript` walks it again" overstates the second pass

`indexTranscriptTurns` scans from offset 0 (`transcript_scan.go:96-115`); the follow-up scan starts at `turns[first].off` (`async_latest.go:240-244`), i.e. at the cursor's turn, not at 0. The finding stands on the first pass alone (a full-file index per poll); §9 PH1 should measure the index, not "two full passes".

### C7 (correction) r1-03's "the bridge recover swallows the panic with no log" is true and is a **second** diagnostics gap

`tool_smith_bridge.go:345` is `if recover() != nil {` with nothing logged; `subagent_manager.go:645` is the other. So P6BM-03's fix has two halves: add the `defer recover` to both finishers *and* log at the two existing sites. Same class as P6AM-13 (the recovered panic is invisible), which the user has not yet decided.

### C8 (severity) r1's "low-medium" for P6BM-07 — **low**

"Low-medium" is not a scale the plan has (§5.1); against the definitions it is a leak of a stray file plus a `Complete` on a closed manager, requiring a SIGTERM-ignoring child at shutdown. Low.

### C9 (contradiction) r2 §6 lists the async hook nil-ing as sound; r3 files it as a silent divergence — **r2 right**

See P6BM-25. The nil-ing is necessary; the missing comment is the only residue. Recorded so the next phase does not "fix" the nil-ing.

### C10 (framing) r1-13 calls `executeNativeExec` and `Directory()` "production dead code" — they are **test-only**

Both are reachable from `_test.go` files that exercise the real path; the change is moving the wrapper into a test file (or deleting `Directory()` if its tests do not need it), not removing coverage. Same entry, different fix.

## 6. Reviewer-pair agreement

| pair | shared entries | notes |
|---|---|---|
| r1 + r2 | P6BM-01, 03, 04, 12, 17, 22 | five of the six are the phase's substance: the bridge drop, the recover gap, the lost context values, the scratch fallback, the library duplication. They split on severity twice (bridge: r2 high; async context: r2 low vs r3 medium) and on the hook nil-ing (C9). |
| r1 + r3 | P6BM-01, 03, 18 | r1 reached `process_manager.go`'s size and Start's repeated block, r3 the env builder in the same file; both found the recover gap independently. |
| r1 + r4 | P6BM-01 | r4's only overlap with the widest reviewer is the carried-in item. |
| r2 + r3 | P6BM-01, 03, 04, 05 | the closest pair on concurrency: both reached the detached-goroutine and lost-context items from different starting points. |
| r2 + r4 | P6BM-01 | — |
| r3 + r4 | P6BM-05 | identical mechanism, opposite severity. |

Three independent statements exist for P6BM-01 and P6BM-03; four for P6BM-01 (r4 included); two for P6BM-04, P6BM-05, P6BM-12, P6BM-17, P6BM-18, P6BM-22. **Seventeen entries are single-reviewer and twelve of those are r1's.** As recorded in earlier merges, the retrieval index serves `audit_findings/`, so overlap is weak corroboration; the entries this merge trusts most are the ones it executed (P6BM-02, P6BM-05) or read end-to-end at the cited lines (all of them — see §10).

**Reviewer reliability, as measured here:** r1 has the widest net and, after re-reading every location it cites, **no wrong location and no wrong mechanism** — its three weaknesses are a mis-typed severity scale ("low-medium"), a framing slip (C10), and skipped index navigation. r2 is the most precise on mechanisms and the only reviewer that carried an explicit correction of its own hypotheses (HY-2, HY-3, HY-11); its two faults are the high severity in C1 and two unreachable consequences in C2. r3 is short and accurate (6 filings, 5 entries, one refused defect) and its one severity (medium for the context values) is the one I adopted over r2's low. r4 is **thin**: 2 filings from the same phase, both already known, and its "H3: no additional finding" is contradicted by ten entries below that live in files it declared read (§11).

## 7. Prior art handling

| prior item | status there | p6b filings | handling here |
|---|---|---|---|
| **P6AM-22** — the bridge drops `AgentDepth`, `RootRun`, `RemoteHops` | merged p6a as out-of-phase, fact confirmed, fix shape proposed (`tools/catalog.go` `ForNested()` + a field-walking test) | r1-01, r2-01, r3-01, r4-01 | **P6BM-01**, now in its own phase. New evidence: every consumer line, the exact 11-of-17 count, the second hand copy and the out-of-band completion that makes it not a defect, and the two refuted consequences (C2). The fix stays `ForNested()` + `reflect` field walk. |
| **D-1 / `206ebd5`** — panic outside `Tool.Execute` kills the process; ACCEPT High, fixed for the call path, with "audit the other unrecovered goroutines" as its own recommendation (`audit_audit.md:220`) | fixed for `tools/catalog.go` only | r1-03, r2-02, r3-02 | **P6BM-03** is D-1's remainder: two goroutines created by the async rework afterwards. Not a repeat — a new site of the same class, medium not high because the process is no longer killed on the call path. |
| **B-C3** — recycled pgid | partly fixed for `killGroupAfter` (liveness polling) and the orphan path (start-time proof) | r1-05, r1-17, r2 HY-11, r3 H7 | **P6BM-06** is the open remainder (`ChildGroups.sweep`, which has neither proof), **P6BM-23** the hardening tail of the orphan path. Both keep the old id as prior art. |
| **A-2** — process env inheritance | merged, allowlist + denies accepted | r1 A8 (disproved), r2 §6, r3 §6 | **not re-filed.** Re-read here: both builders apply the allowlist then remove `JAD_API_TOKEN`, `apiKeyEnv` and `env_deny`; `PYTHONPATH`/`NODE_PATH` are not in the allowlist. P6BM-18's env-duplication half is the only residue and is maintainability. |
| `176fd7d` / `f442484` (Close waits; queued end signal) | fixed for the async manager and the process finish path | r1-04, r1-07, r2 HY-10 | **P6BM-07** and **P6BM-09** are that class in two places the earlier fix did not reach (the shell finisher goroutine; `prune` after `done`). r2's HY-10 correctly cleared the *waiting* half (it does wait for `done` per process). |
| **D-3** — shutdown has no deadline | ACCEPT, open; covers the HTTP server | r2-08, r1 H2 | **P6BM-16** is adjacent and not the same: D-3 is the missing deadline, this is one manager's sequential stops (up to ~32 s). If D-3 is built first, it bounds P6BM-16 instead. |
| `audit_plan.md` §2 "latest" benchmarks | the plan notes no benchmark exists for the latest builder | r2-06, r1 H1, r3 H1, r4 H1 | PH1 is a new benchmark, as three reviewers independently asked. |
| TS-parity defaults (`e12e430`/`74d2c11`, per r2) | — | r2-04 | **P6BM-13** changes a shipped default; it needs the parity table checked before the value moves (decision 5). |

Nothing in p6b contradicts an earlier audit record. The one place where a *decision* would have to change is P6BM-13 (a default value), which is a config round, not a defect.

## 8. Ranked list (severity, then evidence, then finders, then effort)

| rank | id | sev | evidence | finders | effort | why here |
|---|---|---|---|---|---|---|
| 1 | P6BM-01 | medium | read (every consumer) + r1's probe | 4 | S | a tier-1 guard silently zeroed on a config-reachable path; one `ForNested()` helper plus a field-walk test closes it |
| 2 | P6BM-04 | medium | read | 2 | S | `kill_children_on_run_end` silently does not apply to async runs; one-line fix |
| 3 | P6BM-03 | medium | read (recover-site count) | 3 | S | a panic in either finisher kills the server and strands the handle; three lines each |
| 4 | P6BM-02 | medium | **reproduced** | 1 | S | silent rewrite of the model's own program for node/ruby/ts |
| 5 | P6BM-06 | medium | read | 1 | M | an unrelated process group can be signalled; needs a design choice |
| 6 | P6BM-13 | medium | read (defaults + schema) | 1 | M | 12.5 GiB of disk reachable from a model call at shipped defaults; config round |
| 7 | P6BM-08 | low | read | 1 | S | phantom `running` manifest for the rest of the process's life |
| 8 | P6BM-07 | low | read | 1 | M | `Close` returns before the shell finisher; the 176fd7d symptom again |
| 9 | P6BM-09 | low | read | 1 | S | `Close` returns while `prune` deletes; bites tests and shutdown |
| 10 | P6BM-05 | low | **reproduced** (direction) | 2 | S | newest news dropped silently; cursor moves past it |
| 11 | P6BM-10 | low | read | 1 | S | a stop in a few-ms window is lost and the process runs on |
| 12 | P6BM-16 | low | read | 1 | S | up to ~32 s of serial shutdown per storage dir |
| 13 | P6BM-12 | low | read | 2 | S | crash residue in `/tmp` never swept |
| 14 | P6BM-11 | low | read | 1 | S | an uncapped list result from a model-writable directory |
| 15 | P6BM-20 | low | read | 1 | S | `"id":""` in one error path |
| 16 | P6BM-21 | low | read | 1 | S | a storage path in a model-visible error |
| 17 | P6BM-24 | low | read | 1 | S | disk grows when a profile stops starting processes |
| 18 | P6BM-15 | low | read | 1 | M | full transcript index per agent handle per poll |
| 19 | P6BM-14 | low | read | 1 | M | up to 12.8 MiB copied per tool_smith run |
| 20 | P6BM-17 | low | read | 2 | S | three copies of the containment check; two listing renderers |
| 21 | P6BM-22 | low | grep | 2 | S | the tests that would have caught P6BM-01, 02, 07, 08, 10 |
| 22 | P6BM-19 | low | grep | 1 | S | test-only production code |
| 23 | P6BM-18 | low | read | 2 | M | 1,117-line file, three-times-repeated failure block |
| 24 | P6BM-23 | low | read | 1 | S | hardening; a pid reused inside the 2 s grace |
| 25 | P6BM-25 | low | read (refused as a defect) | 1 | S | one comment; the code is right |

## 9. Performance hypotheses for the measurement step (grouped; **measured in §12-bis**)

20 raw hypotheses (r1 H1-H8, r2 H01-H06, r3 H1-H5, r4 H1) → **9 groups**. "existing" means the benchmark is in the tree at `385cc89` and was checked by name in this merge; "new" means nothing covers it. Every row is `[unmeasured]`.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| PH1 | Each async wait/list/status re-indexes the whole transcript (from offset 0, `json.Unmarshal` per line) of every live agent handle, up to `latest_scan_max_bytes` 8 MiB, before showing anything (P6BM-15) | `async_latest.go:213-249`, `transcript_scan.go:95-119`, caller `async_tool.go:194,213,255` | 2/10 live handles × 1 MiB and 8 MiB transcripts, polled 20 times with and without new bytes | new `BenchmarkActivityLatestPoll` (2 handles × 1 MiB and × 8 MiB; also N = 10); report bytes read, allocs and ns/op, and the ratio "new bytes : indexed bytes". **No benchmark exists for the latest builder** (`audit_plan.md` §2) | r1 H1, r2 H02, r3 H1, r4 H1 |
| PH2 | Every `tool_smith` run stages the whole library: `ReadDir` + header read + full copy per file (P6BM-14) | `tool_smith_fn.go:27-77` | 200 files × 10 KiB, then 200 × 64 KiB; 50 runs | new `BenchmarkToolSmithStageFunctions` (generated `.ts` files in `t.TempDir()`); compare with a lazy/vendored variant in `/tmp` | r1 H6, r2 H01, r3 H2 |
| PH3 | `ManagedProcessManager.Close` stops processes serially, 2 graces each (P6BM-16) | `process_manager.go:525-552,479-523` | `max_running` 8 children that ignore SIGTERM, `kill_grace_ms` 2000 | new `BenchmarkManagerCloseManyRunning` (8 `trap '' TERM; sleep 100` children); compare serial against signal-all-then-wait | r1 H2, r3 H4, finding r2-08 |
| PH4 | The process output writer holds `entry.mu` across the spill write, blocking `Stop`, `finish`, `snapshotEntry` and the latest block (read, not filed) | `process_manager.go:200-215`, `bash_spill.go` | one child at ~100 MB/s while status polls | **existing** `BenchmarkProcessWriteWithStatusPollers` (`process_output_contention_test.go:30`, from `5d09534`) — run it as the measurement; do not file a defect | r1 H3, r2 H06 |
| PH5 | `prune` sorts every terminal manifest on each `finish`; `List` clones and sorts all manifests per call | `process_manager.go:1056-1117`, `List` | `max_completed` 100 retained, 20 finishes/min, `async list` in a poll loop | **existing** `BenchmarkProcessListManyFinished`, `BenchmarkProcessListWhileStarting`; add `BenchmarkPrune100Completed` | r2 H05, r3 H3 |
| PH6 | `Start` holds `manager.mu` across `MkdirAll` and a full running-count scan, and persists with fsync twice per start | `process_manager.go:228-262,945-980` | 1,000 stored manifests, 8 starts/s; `strace -c -e fsync` for the count | new `BenchmarkProcessStart`; counts via `strace`, not wall time | r1 H7, r1 H8 |
| PH7 | Every exec call re-resolves the library (`EvalSymlinks` ×2 + `stat`) and writes the template file | `exec_functions.go:33-58`, `exec_template.go:36-66`, `exec.go:219-240` | 50/1,000 calls of `true` with a 200-file library | new `BenchmarkExecTemplate` (attach only) and `BenchmarkExecTrue`; syscall count vs `process` start-to-exit as the floor | r1 H4, r2 H03 |
| PH8 | `async`/`process` detail re-renders the full head+tail windows of every live process on every poll | `process_async.go:139-146`, `process_manager.go:680-701` | 8 running processes, 750 B head + 1,200 B tail per stream, poll loop | **existing** `BenchmarkProcessListManyFinished`; add `BenchmarkProcessDetailRunning` | r2 H04, r3 H5 |
| PH9 | `sweepStaleScratchOnce` does a synchronous `ReadDir` of the temp dir while the first catalog is built | `scratch_sweep.go:23-25` | TMPDIR with 100,000 entries | new test timing `sweepStaleScratch` on a synthetic dir; one-time cost | r1 H5 |

Order for the measurement step: **PH1 first** — three reviewers asked for it independently, it is the only group whose cost grows with the *run* rather than with a call, and it decides whether P6BM-15's fix is a cursor-based index or a cache. Then **PH3** (it quantifies a user-visible shutdown stall and P6BM-16's fix), then **PH2** (same question as PH1, per run: measured cost against library size), then **PH4** (an existing benchmark, cheap to run and the only one that can turn a read item into a finding), then **PH6**, **PH8**, **PH7**, **PH5**, **PH9** last. Nothing in the fix list depends on a measurement except P6BM-15's shape (PH1) and P6BM-13's default value (no benchmark; the parity table decides).

## 10. Verification log and repository state

Repo state: `git rev-parse --short HEAD` → `385cc89`; `git status --short` before and after: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`; `git diff --check` → exit 2 for the same **pre-existing** trailing-whitespace line at `EXECUTOR.md:21` that p6a's merge recorded. No code, config, `sample/` or git state changed; the only repository file written is this report. Scratch `/tmp/p6bmerge` (one probe test file, one overlay JSON) is removed at the end. No process was killed or restarted; no `-race`, no benchmark, no profile, no `go test ./...`.

| command (from `backend-go/` unless noted) | exit | observed |
|---|---|---|
| `git rev-parse --short HEAD` (repo root) | 0 | `385cc89` |
| `git status --short` (repo root) | 0 | the five entries above, before and after |
| `git diff --check` (repo root) | **2** | only `EXECUTOR.md:21: trailing whitespace` (pre-existing) |
| `go build ./internal/... ./cmd/...` | **0** | no output (r4 ran no build; the tree builds) |
| `go vet ./internal/tools/native/` | **0** | no output |
| `gofmt -l internal/tools/native` | **0** | no output |
| `go test ./internal/tools/native/ -count=1` | **0** | `ok jad/backend-go/internal/tools/native 97.300s` — the whole package, including the two tests the prompts call flaky |
| `go test -overlay /tmp/p6bmerge/overlay.json ./internal/tools/native/ -run 'TestP6BM' -count=1 -v` | 0 | `argv before: ["-r" "{template}" "-e" "console.log(\"{template}\")"]` / `argv after : ["-r" "/tmp/…/template.js" "-e" "console.log(\"/tmp/…/template.js\")"]` / `code element changed: true`; `first block len=100 second block len=50`, `second block: "AAAA…"`, `second block keeps newest bytes: false` |
| `grep -rn "recover()" internal/tools/native/*.go` (minus `_test.go`) | 0 | 2 lines: `subagent_manager.go:645`, `tool_smith_bridge.go:345` |
| `grep -rn "AgentDepth\|RootRun\|RemoteHops" internal/tools/native/tool_smith*_test.go` | 1 | **no output** (no test pins the bridge fields) |
| `grep -rn "{template}" internal/tools/native/exec*_test.go` | 0 | only in `Argv` definitions, never in a code argument |

Read at `385cc89` (targeted ranges, never a whole Go file, per the navigation rules): `tools/native/` — `tool_smith_bridge.go` (indexed snippet `startToolSmithBridge:200-256`), `tool_smith.go:173-238,230-262`, `tool_smith_async.go:1-45`, `tool_smith_fn.go:15-107`, `tool_smith_list.go:222-240`, `tool_smith_open.go` (via the three callers), `exec.go:181-250,330-400`, `exec_template.go:1-100`, `exec_functions.go:130-350`, `process_manager.go:185-235,265-300,330-400,470-560,590-680,700-760,867-900`, `process.go:185-205`, `process_orphan.go:100-160`, `shell_async.go:130-200`, `async_host.go:60-110`, `async_latest.go:30-270`, `async_tool.go:180-265`, `child_groups.go:1-58`, `scratch_sweep.go:1-56`, `bash.go:232-250,382-420`, `bash_child.go:155-175`, `skill.go:144-155`, `process_async.go:89`, `transcript.go:180-200`, `transcript_path.go:70-95`, `transcript_scan.go:95-119`, `profile_message.go:348-366`, `subagent_call.go:45-100`, `subagent_manager.go:555-585`. Outside the phase, for leads only: `tools/catalog.go:27-71` (indexed), `tools/output.go:40-70`, `sessionfs/native_exec_settings.go:110-125,300-355`, `sessionfs/native_process_settings.go:1-30,95-180`, `sessionfs/native_tool_smith_settings.go:30-62`, `asyncmgr/config.go:43-90,121`, `asyncmgr/manager.go` `Close`, `app/bound_runner.go:119`, `app/subagent.go:81`, `sample/schemes/setting.schema.json:1558-1575`, `audit_audit.md:12,50,220`, `audit_plan.md:123`.

**Not run, by design:** no `-race` (task), no benchmark (task), no `verify-go.sh` (no change set), no live interpreter beyond what the package suite already uses, no stdio child spawned by a probe, no Windows build or run, no frontend command, no `messages-golden-diff` (nothing that writes a message changed).

## 11. Coverage gaps, unverified items, and the r4 spot-check

### The r4 spot-check (the task asked for it explicitly)

r4 declared all 33 files read, ran only two targeted tests, no build/vet/gofmt, and filed 2 findings, both already filed by others. Its own hypothesis list says "H3: no additional finding after source review" for exec construction, managed-process env/cwd and lifecycle, tool_smith helper boundaries, scratch handling, process groups and orphan cleanup, and async ownership. **Ten of this merge's entries live in exactly those areas and are confirmed by source reading here:** P6BM-03 (process wait goroutine), P6BM-07 (`AsyncHost.Close`), P6BM-08 (stale manifest), P6BM-09 (`Close` vs `prune` — which r4 listed as *sound*), P6BM-11 (uncapped listing), P6BM-13 (the 12.8 GiB default), P6BM-16 (serial `Close`), P6BM-18, P6BM-19, P6BM-24. So r4's clearance is a coverage failure, not a judgement: it reported the ordering in P6BM-09 correctly as a fact and then drew the opposite conclusion. Its two filings are correct but redundant, and its severity (medium) held for one of them and was lowered for the other.

I spot-checked r4's own areas independently rather than trusting its list: `async_tool.go:180-265` (the list/wait/status rendering and the progress updates) — no additional defect beyond P6BM-05's cause; `process_manager.go:185-235` (the stdin writer goroutine and `processStreamWriter.Write`) — an untracked goroutine exists but only does one `io.WriteString`+`Close`, not worth a finding; `exec.go:330-400` (validation of code size, NUL, stdin-mode/input) — sound as r2 and r3 describe. I found nothing r4 missed that the other three did not already have.

### Gaps
- **No `-race` run.** Every concurrency claim here is read from mutex and goroutine structure (`entry.mu`/`manager.mu`/`persistMu` ordering, the two untracked goroutines, `ChildGroups.mu`). The `-race` pass on this package is a measurement-step item and matters most for P6BM-08.
- **No benchmark.** §9's nine groups are untouched; the only numbers quoted (r1's `Close` timing, r2's 2 MiB/600 syscalls) are the reviewers', labelled as such.
- **No stdio child was spawned by a probe.** P6BM-06 (pid wrap) and P6BM-07 (a stubborn child at `Close`) were not reproduced; the package suite does spawn children, so the paths compile and run, but neither scenario was constructed.
- **Windows:** `process_group_windows.go`, `process_orphan_windows.go`, `tool_smith_open_windows.go` were not read by this merge beyond what the reviewers quote; no Windows build. Any fix that adds `Setpgid` (P6BM-06) needs the Windows decision from P6AM-19's row.
- **`tool_smith_prelude.ts`** (reply buffering, a known deferral in `DECISIONS.md`) and `exec_templates/*.js|py|rb` were not reviewed as code here; r1 read them for env/path use only, r2 for boundary behaviour, r3 for entry-point rules.
- **`process_statecontext.go`** and `process_manifest_order.go` got no entry; r1 and r3 read them (order/limit logic, one documented key-order exception) and found nothing, and no finding above touches them.
- **The `process` stdin path** (`process_manager.go:330-375`, the writer goroutine) was read only around the `WaitDelay`/`command.Start` ordering it shares with P6BM-10.

### Unverified (stays in the list, flagged)
- **P6BM-06**: the recycled-pgid window is read from `child_groups.go`; forcing a pid wrap was neither possible nor attempted. The probability argument is the reviewers'.
- **P6BM-07**: `Close` returning before the finisher is read from `asyncmgr.Manager.Close`; the "child ignores SIGTERM so the goroutine outlives `StopWait`" consequence follows from `kill_grace_ms` and was not run.
- **P6BM-08** and **P6BM-10**: both are narrow races; the code paths are unambiguous but neither was provoked (r1's 400-cycle probe did not exercise the stop/exit interleaving).
- **P6BM-15**: the indexing cost per poll is read, not measured; PH1 decides its size.
- **P6BM-25**: whether a *description* warns that async runs cannot ask questions was not checked in the tool definition text; the code-side nil-ing is confirmed.

## 12. Cross-phase leads and relocations

- **`subagent_call.go:54-60`** — the second hand copy (P6BM-01's fix must cover it; the file is 6c). Not a defect on its own (§3.1).
- **`transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290`, `transcript_path.go:81`, `profile_message.go:356`, `profile_message_remote.go:80`** — the consumers that make P6BM-01 matter; their gate logic is 6c/7 and is not itself at fault.
- **`subagent_manager.go:491-521,569-575`** — root-run quota and the out-of-band field completion; 6c.
- **`bash.go:238`, `resolveExistingTarget(…, settings.Policy, …)`** — `runShellChildDetailed` resolves `cwd` with the tool's policy, not `scopedPolicy(policy, execution)`, while `edit.go:92`, `write.go:94`, `transcript_path.go:78` and `transcripts_list.go:133` do narrow it. A child with `paths` can therefore run exec/tool_smith/bash in a cwd outside its scope. r1 filed this as a lead and I confirm it at `385cc89` (`grep -n "scopedPolicy"` lists no `bash.go`); `bash.go` belongs to 6a/7, so it is **relocated, not filed here** — it needs a `DECISIONS.md` entry saying whether a shell's cwd can be path-scoped (a shell cannot be, which may be the answer).
- **`sessionfs/native_process_settings.go:15,18`** and `sample/schemes/setting.schema.json:1564-1568` — P6BM-13's defaults live in phase 2a's files; the fix is a settings change with its schema/sample/DECISIONS round.
- **`internal/app/shutdown.go:14-36`** — D-3's HTTP-only deadline; P6BM-16 would be bounded by it.
- **`asyncmgr/config.go:72`** — `latest_scan_max_bytes` 8 MiB sizes PH1/P6BM-15 (phase 4).
- **`app/bound_runner.go:119`, `app/subagent.go:81`, `bash_child.go:163-165`** — the `ChildGroups` plumbing that P6BM-04 bypasses and P6BM-06 abuses (phase 1/7).
- **`tools/catalog.go:407-415`, `tools/output.go:47-53`** — P6BM-03's reference recover and P6BM-20's renderer; both are p6a code, unchanged here.

## 12-bis. Measurement step: PH1-PH9 and the race pass (run at `385cc89`)

Method: the nine groups of §9, serialized, on the same machine as the merge (AMD Ryzen 5 3600, 12 threads, go1.27.3 linux/amd64, load average 0.4-1.4 during the runs — the higher figure is this step's own compiles; every group's five samples agreed to within 2-6% except two that GC touches — PH3's serial case (1.41-1.61 s) and PH8's single-process case (44-62 us) — so no run is discarded and both stable cases are quoted). Each group uses the benchmark §9 names when one exists; where none exists a harness was compiled into the package with `go test -overlay` from `/tmp/p6bmeas` (one `_test.go`, one probe test, one overlay JSON; removed at the end). **No repository file, no `sample/` file and no git state changed.** Only ratios and per-operation costs are reported; absolute ns/op is the reviewers' only where labelled.

**Verdicts in one line: 6 confirmed, 1 confirmed in half (PH2's mechanism corrected), 1 refuted (PH4), 1 refuted as material (PH7).** One severity change: **P6BM-15 low → medium**. No new finding.

| id | hypothesis | result | ratio | verdict |
|---|---|---|---|---|
| PH1 | each async poll re-indexes the whole transcript of every live handle (P6BM-15) | 1 MiB: **35.4 ms/poll**; 8 MiB (8128 KiB): **276.6 ms**; 10 handles x 1 MiB in one status call: **356.0 ms**; over-cap 9.1 MiB: **4.8 us** | 8 MiB vs 1 MiB = **7.8x** for 7.9x bytes; 10 handles vs 1 = **9.9x**; over-cap vs 1 MiB = **7,400x cheaper** | **confirmed**, and it is the largest cost measured in this phase |
| PH2 | every `tool_smith` run stages the whole library, cost grows with library bytes (P6BM-14) | 200 x 10 KiB: **12.44 ms/run**; 200 x 64 KiB: **13.63 ms/run**; empty library (floor): **27.1 us** | 64 KiB vs 10 KiB files = **1.10x for 6.4x bytes**; vs floor = **459x / 503x** | **confirmed as a per-run cost, mechanism corrected**: it is file-count and syscall bound (62-68 us per file, flat in file size), not byte bound |
| PH3 | `Close` stops processes serially, 2 graces each (P6BM-16) | 8 children that ignore SIGTERM, `kill_grace_ms` 200: serial **1.606 s**, signal-all-then-wait **202.6 ms** | **7.9x** (matches 8 x grace) | **confirmed**; at the shipped 2000 ms grace the same shape is ~16 s vs ~2 s |
| PH4 | the output writer holds `entry.mu` across the spill write and stalls pollers/the child (read, not filed) | existing `BenchmarkProcessWriteWithStatusPollers`, 32 KiB chunks: no poller **29.4 us/write**, 1 poller **30.1 us**, 4 pollers **29.9 us** | 1 poller **1.02x**, 4 pollers **1.02x** vs none | **refuted** — no measurable contention; do not file, as §9 said |
| PH5 | `prune` sorts every terminal manifest on each `finish`; `List` clones and sorts all manifests per call | `prune`: 100 manifests **26.7 us**, 1000 **338 us**; `List(1000)` **554 us** and **820 KB** per call; `List` while a start holds the section: p99 **130 us**; start-to-exit with 2000 prunable manifests **1.25 ms** | prune 10x manifests = **12.7x**; List is **O(n)** with a clone per manifest | **confirmed, immaterial at the shipped defaults** (`max_completed` 100 => 27 us per finish, ~55 us per list) |
| PH6 | `Start` holds `manager.mu` across `MkdirAll` and a full running-count scan, and fsyncs twice per start | start-to-exit with 0 stored manifests **673 us**, with 1000 stored **990 us**; **63 fsync per 20 start->exit cycles = 3.15 per cycle** (2 per start, 1 per finish) | 1000 stored vs 0 = **1.48x** (the scan is ~320 us) | **confirmed**, both halves; the fsync count is exactly r1's H8 (2 + 1). Counts only: under `strace` the same op takes 3.8 ms, so the strace wall time is discarded |
| PH7 | every exec call re-resolves the library (`EvalSymlinks` x2 + `stat`) and writes the template | resolve + template attach: no library **52.8 us**, 200-file library **69.8 us** (+43 allocs); whole exec of `true`: **2.20 ms** vs **2.18 ms** | attach step **1.32x**; whole call **1.00x** (the step is ~1% of a call) | **refuted as material** — the resolution does happen per call, but the library-attributable part is ~1% of a call (a spawn is ~30x the whole resolve step); no finding (r1 H4 / r2 H03) |
| PH8 | `async`/`process` detail re-renders the full head+tail windows of every live process on every poll | 1 running process **47.8 us**; 8 running **438.7 us** | 8 vs 1 = **9.2x** (~55 us per process, linear + GC) | **confirmed, immaterial** (<0.5 ms per status poll) |
| PH9 | `sweepStaleScratchOnce` reads the whole temp dir synchronously on the first catalog build | 1,000 entries **391 us**; 10,000 **5.44 ms**; 100,000 **59.9 ms** (17.3 MB allocated) | linear: **~0.6 us per entry**; 100k vs 1k = **153x** | **confirmed, one-time and usually invisible**: a realistic `/tmp` of 1-5k entries costs 0.4-2 ms once per process |

### PH1 in detail, because it decides a fix shape and a severity

`activityLatest` (`async_latest.go:208-271`, `indexTranscriptTurns` at `:238`) calls `indexTranscriptTurns(ctx, file, stat.Size(), cfg.LatestScanMaxBytes)` on **every** call, and that walks the file from offset 0 to build the turn table (`transcript_scan.go:95-119`). The scan that follows already starts at `turns[first].off`, so the tail-only part is fine — the cost is the index. Measured, per call:

- 1 MiB transcript: 35.4 ms, and **identical whether or not there are new bytes** (no-new 35.3-35.5 ms, +1 KiB new 35.6-35.9 ms). The hypothesis' "including calls with no newly appended data" is exactly right.
- 8,128 KiB (the `latest_scan_max_bytes` default is 8 MiB): 276.6 ms. Scaling from 1 MiB is 7.8x for 7.9x the bytes => **linear in file size**, ~30 MB/s of indexed JSONL.
- 10 live handles of 1 MiB each, which is what one `async list`/`status`/`wait` does through `latestBlocks` (`async_latest.go:40-77`): **356 ms**, i.e. 9.9x one handle => **linear in the handle count, no batching**. At the 8 MiB cap, ten handles would be ~2.8 s in one tool call, and `latest_total_bytes` (2000) only bounds the *output*, never the indexing.
- Allocation amplification: 11.1 MB allocated per 1 MiB poll and 83.6 MB per 8 MiB poll (~10-11x the file), one `json.Unmarshal` per line.
- The size guard is a real fast path: 9.1 MiB against the 8 MiB cap returns in **4.8 us** (7,400x cheaper than indexing the same file under the cap), so the "transcript too large to summarize" branch is not a cost.

**Effect: P6BM-15 goes from low to medium.** Justification in the phase's own terms: the entry was filed low because it was "a per-call cost of a polling path"; it is measured here at 35 ms per 1 MiB handle and 356 ms for ten, on the *status* path a caller polls in a loop, and it grows to seconds before the documented cap does its job. Its fix is cheap and its shape is now decided by the data: index from a turn boundary at or before the cursor (the measurement shows the index is the whole cost, and `scanTranscript` already knows how to start at an offset), optionally caching `(path, size, mtime)` turns. Nothing else in the list changes severity.

### The remaining groups, judged against what they were meant to decide

- **PH2** refutes the *bytes* framing it was given: 200 x 64 KiB (12.8 MB) costs 1.10x what 200 x 10 KiB (2 MB) costs. Per file it is one `ReadDir` entry, an `EvalSymlinks`, a `stat`, an open, a `headerBytes` read, a second open and a copy: ~62-68 us flat. So the fix's value is bounded by `list_max_files` (200), i.e. ~12.5 ms per `tool_smith` run that calls `fn`, and a lazy per-file copy would remove it for scripts that use one function. P6BM-14 stays **low**: it is per-run, not per-poll, and it is paid once per script by a tool that then spawns a process.
- **PH3** is the only other group with a user-visible number: 8 stubborn children cost 1.6 s at a 200 ms grace and would cost ~16 s at the shipped 2000 ms `kill_grace_ms`, while signaling all before waiting costs one grace. P6BM-16 stays **low** (a shutdown stall, not a correctness fault) but the fix is now demonstrably an 8x on session close.
- **PH5** confirms the shape and clears the magnitude: at `max_completed` 100 a `finish` pays 27 us and a list pays ~55 us. It becomes visible only far above the default (1000 retained => 338 us per finish, 554 us + 820 KB per list).
- **PH6** confirms both halves, and its fsync count is the one number r1 asked for. The ~320 us running-count scan at 1000 manifests only exists beyond `max_completed` 100 by an order of magnitude. **No finding**, matching the merge's read verdict.
- **PH9** is a one-time 0.4-2 ms on a realistic temp dir; only a `/tmp` with ~100k entries (a scratch-heavy CI box) reaches 60 ms. It does not add or re-rank a finding.
- **PH4** and **PH7** are refuted and stay out of the fix list. PH4 in particular is worth recording: the code comment in `snapshotEntry` warns that "the child's writer waits on `entry.mu` meanwhile", and the measurement says that wait is invisible against the spill write it is nested in at 32 KiB chunks.

### The `-race` pass (the item §11 called the biggest gap)

`bash .jad/exec/bash/verify-go.sh --race "./internal/tools/native/"` — gofmt **exit 0**, vet **exit 0**, full `go test -count=1 ./internal/... ./cmd/...` **exit 0** (native 97.2 s), `go test -race -count=3 ./internal/tools/native/` **exit 0** in 311.4 s with **0 `DATA RACE` reports**, `git diff --check` **exit 2** for the same pre-existing `EXECUTOR.md:21` trailing-whitespace line that p6a's merge and this merge's §10 both record. The script's own exit status is 1 because of that pre-existing line; the four Go checks are clean.

What the pass does and does not settle: it exercises the package's whole suite under the detector three times, so no race *the suite provokes* exists in it. It does **not** settle **P6BM-08** (stale manifest overwriting the terminal one) or **P6BM-10** (a `Stop` before `command.Start`), because no test constructs those interleavings — r1's 400-cycle sequential probe did neither. Those two stay "read-confirmed, not reproduced", as §11 said.

### What this step adds to the report

- severity changes: **1** — P6BM-15 low → **medium**; medium count 6 → 7 (P6BM-01, 02, 03, 04, 06, 13, 15); low 19 → 18.
- new findings: **0**. The only new *detail* is PH1's 10-11x allocation amplification, which is part of P6BM-15's fix, not a separate entry.
- hypotheses resolved: 20 raw -> 9 groups -> **6 confirmed** (PH1, PH3, PH5, PH6, PH8, PH9), **1 confirmed with its mechanism corrected** (PH2: count-bound, not byte-bound), **1 refuted** (PH4), **1 refuted as material** (PH7).
- fix-shape decisions the numbers now make: P6BM-15 wants a cursor-based index (not a cache of the whole transcript — the per-call index *is* the cost); P6BM-14's value is capped at `list_max_files` x ~65 us; P6BM-16 is worth 8x on shutdown. **No measurement decides P6BM-13's default value**; that remains the settings-parity question of §13 decision 5.

### Commands (from `backend-go/` unless noted) and exit codes

| command | exit | observed |
|---|---|---|
| `go vet -overlay /tmp/p6bmeas/overlay.json ./internal/tools/native/` | **0** | harness compiles, no vet report |
| `go test -overlay … -bench BenchmarkPH1ActivityLatestPoll -count=5` | **0** | 57.6 s; five samples per case, spread <= 2% |
| `go test -overlay … -bench BenchmarkPH3CloseManyRunning -benchtime=1x -count=5` | **0** | serial 1.41-1.61 s, parallel 202.3-202.7 ms |
| `go test -overlay … -bench 'BenchmarkPH2\|BenchmarkPH5Prune' -count=5` | **0** | 43.3 s |
| `go test -overlay … -bench 'BenchmarkPH6\|BenchmarkPH7\|BenchmarkPH8' -benchtime=10x -count=5` | **0** | 1.2 s |
| `go test -overlay … -bench BenchmarkProcessWriteWithStatusPollers -benchtime=50x -count=5` | **0** | PH4 |
| `go test -overlay … -bench 'BenchmarkProcessListManyFinished\|BenchmarkProcessListWhileStarting\|BenchmarkProcessEndWithPrunableHistory' -benchtime=50x -count=5` | **0** | 39.3 s, PH5 |
| `go test -overlay … -bench BenchmarkPH9SweepStaleScratch -benchtime=5x -count=5` | **0** | 13.4 s, three sizes |
| `go test -overlay … -c -o /tmp/p6bmeas/native.test ./internal/tools/native/` | **0** | test binary for strace |
| `strace -f -c -e trace=fsync,fdatasync -test.bench BenchmarkPH6ProcessStart/stored=0 -test.benchtime=20x` | **0** | 63 fsync / 20 cycles; strace wall time discarded |
| `bash .jad/exec/bash/verify-go.sh --race "./internal/tools/native/"` (repo root) | **1** (diffcheck only) | gofmt 0, vet 0, test 0, race 0 (311.4 s, 0 data races), diffcheck 2 (pre-existing `EXECUTOR.md:21`) |
| `TestPH3Probe` (overlay, `-run TestPH3Probe`) | **0** | per-child timeline: TERM at 0 ms, 7 of 8 dead within the first grace, all 8 collected at 200 ms |

### Unmeasured, and why

- **PH1 at the `AsyncHost` level.** I measured `activityLatest` (the driver call `latestBlocks` makes), not the `manager.LiveInfo`/`Driver`/`SetProgress` envelope around it (a few map lookups, microseconds against 35 ms). The 10-handle case loops the driver, so the manager's per-handle lookups are not in the 356 ms.
- **Exactly 8 MiB.** The guard is `stat.Size() > cfg.LatestScanMaxBytes`, so a file at the cap is indexed and one byte over is not; I measured 8,128 KiB (under) and 9,344 KiB (over), not the boundary byte.
- **PH2's lazy variant** was not implemented; only the floor (empty library) was measured, so the saving is bounded from below, not proven.
- **PH3 at the shipped grace.** I used `kill_grace_ms` 200 to keep the run short; the ratio does not depend on the grace, the absolute costs do (linear), and the 16 s figure for the default is arithmetic, not measured.
- **PH6 fsync counts under strace** are counts only (strace inflates wall time 5.7x); no per-syscall latency on the real storage, no `fdatasync` seen (count 0).
- **PH8** measures the collector render via `detailText`, not the full `async` response (JSON/text encoding of the block is not in it).
- **PH9** with 1,000 *stale* matching directories (the `RemoveAll` side) was not measured; the fresh case (all 1,000 skipped) is what is reported.
- **No profiles** (CPU/memory), no cold-vs-warm cache separation, no multi-machine comparison, **nothing on Windows**, and **no `messages.jsonl` golden diff** (no output path changed in this step).

### The 15 decisions of §13, re-issued with a recommendation after the measurements

| # | decision | recommendation after measurement | needs the user? |
|---|---|---|---|
| 1 | P6BM-01 bridge `ExecutionContext` drop: medium or high | **medium**, fix now (`ForNested()` + field-walk test; both call sites) | no — Agent 1 |
| 2 | diagnostics batch (P6BM-03, P6BM-25, C7) | **yes**, S | no |
| 3 | P6BM-07 + P6BM-09, quiet `Close` | **yes**, one commit, M | no |
| 4 | P6BM-02 substitution order | **yes**, S | no |
| 5 | P6BM-13 log default (12.5 GiB at defaults) | settings round: parity table first, then lower `log_cap_bytes` or add a per-directory budget. **No benchmark decides this** | **yes — changes a shipped default** |
| 6 | P6BM-04 context values for async runs | **yes**, one line + test | no |
| 7 | P6BM-06 recycled pgid | **(a) prune-on-death + (b) start-time re-check behind GOOS**, with P6BM-23 riding along | no — Agent 1, Windows fallback documented |
| 8 | P6BM-05 cut the other end | **tail cut + marker**, and fix the test fixture | **yes (mild)** — changes tool output shape |
| 9 | P6BM-08 / P6BM-10 state fixes | **yes**, S, each with the missing test | no |
| 10 | hygiene batch (12, 11, 20, 21, 24) | **yes**, one commit | no |
| 11 | P6BM-14 / P6BM-15 performance | **P6BM-15 raised to medium and fixed with a cursor index now**; P6BM-14 stays low (12.5 ms/run, count-bound) | no |
| 12 | maintainability batch (17, 18, 19, 22) | **yes**, behaviour-neutral | no |
| 13 | severity confirmations | **confirm, with one change: P6BM-15 low -> medium** (measured) | no |
| 14 | measurement go-ahead + `-race` | **done in this step**; race clean, 0 data races | no |
| 15 | fix split and order | the same four batches, with (i) the concurrency batch first, P6BM-15's index folded into (iv) | **yes — authorisation and order** |

Scratch: `/tmp/p6bmeas` (one benchmark file, one probe test, one overlay JSON, one test binary, five raw logs) is removed at the end of this step; nothing to remove from the tree.

## 13. Needs the user

### Decisions

1. **P6BM-01 — the bridge `ExecutionContext` drop: medium or high?** **Recommendation: medium, and fix it now** with `func (e ExecutionContext) ForNested() ExecutionContext` in `internal/tools/catalog.go` (copy everything, reset only `CallID`/`ToolName`/`RawArguments`), used by `tool_smith_bridge.go:224` **and** `subagent_call.go:54` (which then overrides `AgentName`, `PathScope`, `ProcessOwner`, and keeps its out-of-band fields), plus the `reflect` field-walking test (set every field, assert equal-or-explicitly-reset) and one bridge test that a nested tool sees `AgentDepth`/`RootRun`/`RemoteHops`. Effort S. **If you rate it high**, it goes to the front of the fix queue and the test must also cover the `transcript`/`allow_children` gate end to end.
2. **P6BM-03 / P6BM-25 / C7 — one small "diagnostics" batch:** `defer` a `recover` in `shell_async.go:167` and `process_manager.go:378` that logs and completes the handle as failed; log the panic the bridge already recovers (`tool_smith_bridge.go:345`); and add the missing comment at `tool_smith_async.go:22-26` explaining why async runs drop the interaction hooks. No behaviour change beyond the recover. Effort S.
3. **P6BM-07 + P6BM-09 — make `Close` quiet.** **Recommendation:** (a) `AsyncHost` keeps a `WaitGroup` over shell-run goroutines and `Close` waits for it after `manager.Close`, bounded by the kill deadline, so `Complete` and the result write cannot happen after `Close` returned; (b) `ManagedProcessManager` keeps a `WaitGroup` over `waitForProcess` and `Close` waits for it — keeping the early `close(entry.done)` that C4 shows is deliberate. Effort M together; the two are the same 176fd7d class and should ship as one commit.
4. **P6BM-02 — fix the substitution order.** **Recommendation:** compute the template path before `{code}` is inserted (or substitute `{template}` only in the configured flags, never in the element that now carries the code), and add the "code containing a literal `{template}`" test that `exec_template_test.go` lacks. Effort S.
5. **P6BM-13 — the log default.** **Recommendation:** treat it as a settings change, not a code fix: check the TS-parity table first (r2's `e12e430`/`74d2c11`), then either lower `log_cap_bytes` or add a per-directory total budget enforced before the writers open; update the schema default, the sample and `DECISIONS.md` in the same commit. Until decided, nothing is broken — the knob works. Effort M.
6. **P6BM-04 — one line.** `context.WithCancel(context.WithoutCancel(ctx))` in `shell_async.go:162`, with a test that an async exec's leftover group is in `ChildGroups` after the call returns. Effort S. (The report should say so in `DECISIONS.md`'s async section, since the comment there currently says "only a stop … ends it", which stays true.)
7. **P6BM-06 — the recycled-pgid design choice.** Options: (a) prune a group from the map when it is first observed dead (so the window shrinks to the lifetime of a live group), (b) record the leader's start time and re-verify before the hard kill (Linux-only, as `classifyOrphan` does), (c) both. **Recommendation: (a) plus (b) behind GOOS, with the Windows fallback documented** — (a) alone still has a window if the group dies between add and sweep. Effort M. Related, and cheaper: **P6BM-23** (compare `processStartTime` before the forced orphan kill), which is S and can ride along.
8. **P6BM-05 — cut the other end.** On a budget cut, cut with `tailUTF8` (keep the newest) and append the existing "…+N bytes earlier" marker; or, simpler and stricter, do not advance the cursor when the text was cut, matching the documented "does not move" path. **Recommendation: tail cut + marker, and update the comment at `async_latest.go:36-38` to say which end is kept**; the existing `TestLatestTotalIsCappedAcrossHandles` must gain a distinguishing fixture (all-`x` text cannot tell the ends apart). Effort S.
9. **P6BM-08 / P6BM-10 — two small state fixes.** Write `manager.manifests` under `entry.mu` and only when not terminal; and after `command.Start()` check `StopReason` and signal at once. Each with the test P6BM-22 says is missing (stop-at-exit loop; start/stop window). Effort S.
10. **P6BM-12 / P6BM-11 / P6BM-20 / P6BM-21 / P6BM-24 — the "hygiene" batch.** Sweep both temp dirs in the same `sync.Once`; cap `problems` at `list_max_results` with "…and N more"; return the code-file write error to be rendered with `callID`; map process start errors to a fixed message and log the detail; call `prune` at the end of `Recover`. Five small edits, one commit, each with its test. Effort S.
11. **P6BM-14 / P6BM-15 — performance, after measurement.** Both are real per-call costs; **PH1 decides** whether the transcript index should become cursor-based (the likely answer: index from the last turn at or before `cursor.off`) and **PH2** whether the library should be staged lazily. Do not change either before the numbers exist; if PH1 shows the index is a large share of a poll, P6BM-15 moves from low to medium on the measured value.
12. **P6BM-17 / P6BM-18 / P6BM-19 / P6BM-22 — maintainability, one consolidation commit, behaviour-neutral.** Extract `openLibraryFile` and `renderLibraryListing`; add `failStart(entry, cause)`; move `executeNativeExec` into a `_test.go` and delete `Directory()` if unused; replace the `time.Sleep` synchronisation with the channels the tests already have. Do **not** split `process_manager.go` in the same commit as any fix. Effort S/M.
13. **Severity confirmations.** Medium: P6BM-01, 02, 03, 04, 06, 13. Everything else low, with one filing raised (r2-03 → P6BM-04), two lowered (r2-01 → P6BM-01, r4-02 → P6BM-05) and one refused (P6BM-25). **Recommendation: confirm as merged.** The one that could reasonably go the other way is **P6BM-01 at high** (decision 1).
14. **Measurement go-ahead for §9's nine groups in the order PH1, PH3, PH2, PH4, PH6, PH8, PH7, PH5, PH9**, plus **one `-race` pass** on `internal/tools/native` — it has none today and P6BM-08 is the item that most needs it. No fix decision waits on a measurement except decision 11.
15. **What I did not do and would need your word for.** Nothing in this step wrote code. The natural fix split is: (i) the concurrency batch (decisions 2, 3, 6, 9 — recover, `Close`, the context values, the two state races), (ii) the boundary batch (decisions 1, 4, 5, 7), (iii) the hygiene and maintainability batches (decisions 10, 12), (iv) performance after measurement (decision 11). Batch (ii) needs the config round for P6BM-13 and the Windows decision for P6BM-06.

Scratch: `/tmp/p6bmerge` (one probe test file, one overlay JSON) — removed at the end of this step; nothing to remove from the tree.
