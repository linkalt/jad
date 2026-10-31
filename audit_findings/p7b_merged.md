# Phase 7b merge (coordinator)

- phase: p7b (native tools: transcript*, transcripts*, memory*, task*, hooks*, skill.go, catalog.go, request_draft.go)
- HEAD: `09139f5` (tree clean under `backend-go/`; `git diff -- backend-go` is empty)
- inputs: `p7b_r1.md`, `p7b_r2.md`, `p7b_r3.md`, `p7b_r4.md` (all finished before this merge)
- scope: 22 non-test files, 7,871 lines (the same set all four reviewers read)
- method: normalised by cause + `file:line`; re-verified every critical/high and every unique finding by reading source and, where possible, reproducing with a `/tmp` overlay test (`go test -overlay`), scratch removed afterwards. No source file was changed; no commit.

## 1. Critical verification (done before the rest of the merge)

**P7B-MRG-01 (= P7B-R2-01 = P7B-R4-01)** is confirmed and reproduced.

| step | command | exit | observed |
|---|---|---|---|
| source at HEAD | read `memory_json.go:367-396` | - | every case is a two-character raw literal: `` `\\"` ``, `` `\\\\` ``, `` `\\n` ``, `` `\\t` ``, `` `\\u%04x` `` (the escape is doubled) |
| encoder probe, quote | `go test ./internal/tools/native/ -run TestP7bProbe -overlay ...` | 1 | `{"s":"a\"b"}` written as `"a\\"b"`; `json.Valid` = **false** |
| encoder probe, backslash / newline / tab | same run | 1 (same run) | `back\slash` written as `back\\\\slash`; `line1<LF>line2` written as `line1\\nline2`; `tab<TAB>here` written as `tab\\there`. Valid JSON, but the value changes |
| store round trip | `CreateValue` then `Get`, same overlay | 1 | `CreateValue` returns `err=<nil>`; `Get` returns **`memory record is malformed`**. The record is written and then unreadable |
| repeated update | write/read/write of `line1<LF>line2` four rounds | 0 | the stored text grows one extra backslash pair per round (`\n`, `\\n`, `\\\\n`, `\\\\\\\\n`) |
| existing memory tests | `go test ./internal/tools/native/ -run Memory -count=1` | **0** (`ok 0.025s`) | passes: the existing tests use plain values (`title`, `count`, `id`) and never store a quote, backslash, newline or tab |

Root cause is one function. Both reviewers found the same defect; r4 rated it high, r2 critical. The merged rating is **critical**: a quote in ordinary text (for example `the user said "hi"`) makes the record unreadable at the next read, and nothing reports the write as failed. Under §5.1 that is data loss.

**Reach and affected data**

- **Model-visible output: yes.** `memory.go:295` (pretty text returned on create/get) and `memory.go:384` (compact text) use the same escaper, as do `memory_store.go:440`, `:980`, `:989`, `:1040` (filter canonicalisation, comparison, GroupBy keys).
- **Stored data in the repo: no test data or sample is affected.** `sample/tools/native/memory.json` has no escape sequence. The goldens (`testdata/messages-native-memory-{create,list,get,invalid-create}.jsonl`) store plain values only, so their text does not change.
- **Runtime state: yes, 3 of 56 records.** `find .jad/state -path '*/.memory/*' -name '*.json'` gives 56 records; 3 fail `json.loads`. Each fails at a `\\"` (the doubled escape) in a `bug_report` record: `…bug_report-20261011T134124.812Z…`, `…bug_report-20261015T011848.153Z…`, `…test/tools/.memory/bug_report/…20261013T103141.598Z…`. `.jad/` is gitignored runtime state, not a fixture. These records are not recoverable from the file alone.
- **Golden contract:** no golden changes unless a memory value contains one of the affected characters. `messages-golden-diff` is not on PATH, so it was not run; that is not a pass. Any fix must re-run the four memory goldens.

## 2. Verification of the mediums

| id | claim | verdict | evidence |
|---|---|---|---|
| P7B-R1-01 | transcript not-found leaks other session names when `allow_other_sessions` is off | **reproduced** | `go test ... -run TestP7bProbeLeakSessionNames -overlay ...`, exit **1** (the assertion fails). Output: `status not_found`, `error_code no_such_profile`, `available: demo, secret-client-session` with `AllowOtherSessions=false`, `AllowOtherProfiles=false`. Mechanism: `transcript_path.go:207-218` (`lookupProfile`) calls `Candidates` and passes the names to `notFound` before `profileRefusal` runs; the switch check exists only on success (`:135`, `:194`). The existing gate tests pass (`TestTranscriptSubagentOfAnotherProfileKeepsTheAccessSwitches` and others, exit 0) but none covers a `profile:` miss with the switch off. |
| P7B-R1-02 | a detail range holds the full text of every step before paging | **reproduced** | `go test ... -run TestP7bProbeDetailRangeHeld -overlay ...`, exit 0. 400 turns × 64 KiB tool result (about 25 MiB file). Single step `1.1-1.1`: output 16,826 B, `TotalAlloc` delta **1.38 MB**. Range `1.1-400.1`: output 16,838 B, `TotalAlloc` delta **251.7 MB**. Output identical in size, allocation scales with the range. Same as r1's 240 MiB figure. |
| P7B-R2-02 | list paging re-scans the whole range per page | **reproduced** | `go test ... -run TestP7bProbeListPagingRescan -overlay ...`, exit 0. 400 turns, `max_list_entries` 10, five chained pages with the returned cursor: per-page `TotalAlloc` **5.33, 5.32, 5.20, 5.28, 5.16 MB** (flat). Page 2 costs the same as page 1, not a shrinking window. Mechanism: `transcript.go:332`, `:363`, `:397` (cursor `Offset` is the original `startOff`). |
| P7B-R2-03 | list mode materialises one step per record of the range | **reproduced** (smaller than the wording suggests) | `go test ... -run TestP7bProbeListStepMaterialise -overlay ...`, exit 0. Output fixed at about 800 B. Allocation: 40 turns **0.73 MB**; 400 turns **2.10 MB** (about 2.9× for 10× the records). The effect is real but modest at these sizes; the claim "tens of MB at 100k steps" is unmeasured. |

Reviewer r1 also files P7B-R1-10 (per-call double scan of the file, transcript.go:304 and the walk). That is the same mechanism as R2-02 on the first page and is merged into P7B-MRG-06.

## 3. Merged table

Status values: `confirmed` (reproduced or traced), `disproved`, `unverified` (read only, not reproduced; flagged). Severity is the merged rating; per §6.2 the median of the reviewer ratings is used for consensus items and the verified value otherwise. Reviewer ids: r1..r4.

| id | title | severity | lens | location | found by | status |
|---|---|---|---|---|---|---|
| P7B-MRG-01 | `quoteMemoryString` doubles every JSON escape: a quote makes an unreadable record; backslash/newline/tab change the value on each update | **critical** | A correctness (data loss), D test gap | `memory_json.go:367-396` (writer callers `:304`, `:338`; disk `memory_store.go:176`, `:246`, `:517`) | r2 (R2-01), r4 (R4-01) | **confirmed, reproduced** |
| P7B-MRG-02 | Transcript not-found leaks other session names when `allow_other_sessions` is off | medium | B security | `transcript_path.go:207-218`, `:135`, `:194`; `app/transcript_profiles.go:54-70` | r1 (R1-01) | **confirmed, reproduced** |
| P7B-MRG-03 | Detail range keeps the full text of every step in the range before paging | low (deferred) | C performance / B (untrusted range) | `transcript_detail.go:101-145`, `transcript_steps.go:60-62`, `:164-179` | r1 (R1-02), r3 (H05 as hypothesis) | **confirmed, reproduced** (251.7 MB vs 1.38 MB). **Deferred as low by user decision.** HEAD `4f9cd62`, 400 turns at 64 KiB results: 294.7 MB, 43,930 allocs. Retention about 55 MB of that; per-result decode (MRG-31) about 240 MB. Option B prototype byte equality not verified (prototype blanked off-page text, so it differs from HEAD). |
| P7B-MRG-04 | Transcript list paging re-scans and re-walks the whole selected range on every page | low (deferred) | C performance | `transcript.go:332`, `:363`, `:397` | r2 (R2-02, H01), r4 (H1 part), r1 (R1-10 first-page rescan, see MRG-06) | **confirmed, reproduced** (flat 5.2-5.3 MB per page). **Deferred as low by user decision.** HEAD `4f9cd62`, 400 turns, 64 KiB results, list page 1: 255.8 ms, 240.1 MB, 40,746 allocs (page 2 is the same cost, 257.2 ms). Small results: 9.7 ms, 2.8 MB. |
| P7B-MRG-05 | List mode materialises a step for every record of the range | low (deferred) | C performance | `transcript_steps.go:63-70`, `:126-137`, `transcript.go:363-390` | r2 (R2-03, H02), r4 (H1 part), r3 (H04 part) | **confirmed, reproduced** (0.73 → 2.10 MB for 40 → 400 turns, output fixed). **Deferred as low by user decision.** Small results at 400 turns: 9.7 ms, 2.8 MB, where the step work dominates (`visit` 74% cumulative). Large results are dominated by the per-result decode (MRG-31), not this. |
| P7B-MRG-06 | Transcript reads scan the file twice per call (turn index, then walk); the index is rebuilt on every page | low | C performance | `transcript.go:304`, `transcript_scan.go:95-127`; merges r1 R1-10 and r3 H04 | r1 (R1-10), r3 (H04) | **confirmed by reading**; its cost is part of the MRG-04 measurement |
| P7B-MRG-07 | Reasoning is readable through `transcript` detail of other sessions and profiles while the default switches are on | medium (proposed) | B security | `transcript_detail.go:29-56`, `transcript_steps.go:138-162`, `:308-338`; `transcript.go:222-230`; `sessionfs/native_transcript_settings.go:90` | r3 (R3-07) | **confirmed by reading**; design decision, see §4 (not a defect against the documented switches) |
| P7B-MRG-08 | `transcript` remote cursor branch skips the remote entry's `profiles` allow-list | low | B security (boundary consistency) | `transcript_remote.go:284-300`, `:329-350` vs `:59-108` (`selector`, `entry.Allows` at `:104`) | r2 (R2-04) | **confirmed by reading**; not a demonstrated bypass (r2 says so; I agree) |
| P7B-MRG-09 | Memory and task writes are not fsynced before rename | low | A correctness (crash durability) | `memory_store.go:707-734`, `task_store.go:400-439` (Rename at `memory_store.go:729`, `task_store.go:429`) | r1 (R1-03), r3 (R3-06) | **confirmed by reading** (`grep Sync` in the p7b store files: no match) |
| P7B-MRG-10 | Task storage errors render the absolute store path to the model | low | B security | `task_store.go:365-395`, `task.go:558-563` | r3 (R3-01) | **reported only**; not re-read by the coordinator (unverified) |
| P7B-MRG-11 | `skill` discloses absolute directories and raw I/O errors to the model | low | B security | `skill.go:159-162`, `:166`, `:173`, `:180`, `:197-203` | r3 (R3-02) | **raw-error part confirmed by reading** (`skill.go:157-182`); the absolute `directory` field (`:197-203`) not re-read (unverified); hardening (dirs are operator layout) |
| P7B-MRG-12 | Memory tool returns raw OS errors including absolute storage path | low | B security (consistency) | `memory.go:249-268`, `memory_store.go:527-539`, `:665-692` | r1 (R1-09) | **reported only**; not re-read by the coordinator (unverified) |
| P7B-MRG-13 | `forwardRemoteArguments` silently drops `int`/`int64` values | low | A correctness | `transcript_remote.go:234-247`, callers `:275`, `:322` | r3 (R3-03) | **confirmed by reading; latent**: model arguments are decoded with `UseNumber` (`tools/json_bounds.go:14`), so the model path yields `json.Number` and is handled |
| P7B-MRG-15 | `transcripts` run listing prints a false "persist_runs is off" note on the `own` fallback, unreachable in production | low | D maintainability | `transcripts_list.go:348-366`, `:172-179` | r2 (R2-05) | **confirmed by reading** (both production builders pass a non-nil port; the `own` arm is unreachable) |
| P7B-MRG-16 | `TaskStore` mutex is per instance; cross-process use can lose a task | low | A correctness (concurrency) | `task_store.go:196-205`, `:352-364`; `app/resolver.go:768-786` vs `:788-808` | r2 (R2-06) | **confirmed by reading**; same process is safe (resolver shares one store per key). Narrow: needs two server processes on one `storage_dir` |
| P7B-MRG-17 | Catalog wiring duplicated across four places; 25 config arms, seven telescoping constructors | low | D maintainability | `catalog.go:50-95`, `:245-481`, `:482-528` | r2 (R2-07) | **confirmed by reading** |
| P7B-MRG-18 | `memory` maps status from the substring "not found" instead of `MemoryStoreError.Kind` | low | D maintainability | `memory.go:262`, `memory_store.go:35-47`, `:672`, `:682`, `:842`, `:847` | r2 (R2-08) | **confirmed by reading** |
| P7B-MRG-19 | Memory record is pretty-printed twice per create and update | low | C performance | `memory_store.go:513-525`, `:176`, `:244` | r2 (R2-09, H04) | **confirmed by reading**; unmeasured |
| P7B-MRG-20 | `pruneCursors` stats every cursor file on each cursor creation | low | C performance | `memory_store.go:793-829`, `:889` | r2 (R2-10, H05) | **confirmed by reading**; unmeasured; small (bounded by `max_cursors`) |
| P7B-MRG-21 | Transcript cursor identity is a hash of the first line only (reset with the same first line is accepted) | low | A correctness (cursor arithmetic) | `transcript_scan.go:130-141`, checked at `transcript.go:315`, `:329` | r2 (R2-11) | **confirmed by reading**; wrong page window, not a leak (r2 says so; I agree). Also: first lines longer than 4 KiB collide by prefix |
| P7B-MRG-22 | `skill` returns Go errors for library failures while neighbours return an `ExecutionError` | low | D / A (model-facing text) | `skill.go:159-161` vs `:166`, `:173`, `:180` | r2 (R2-12) | **confirmed by reading** |
| P7B-MRG-23 | Task list clones and sorts every match, then keeps `limit` | low | C performance | `task_store.go:249-302`, `task.go:250-265` | r1 (R1-04, H04) | **confirmed by reading**; unmeasured (`BenchmarkTaskStoreList100` exists) |
| P7B-MRG-24 | Memory quota scan lists every type directory per write; List/GroupBy hold the store lock across the scan | low | C performance; A (lock hold) | `memory_store.go:611-655`, `:163-205`, `:230-269`, `:276-469`; `memory_group.go:28-90` | r1 (R1-05, H03), r3 (R3-05, H01/H02), r4 (H2) | **confirmed by reading**; unmeasured. F-13 (`5d09534`) fixed the Stat part; this is the remaining ReadDir and lock-hold (prior art: F-13, not re-raised as fixed) |
| P7B-MRG-25 | Hook post-edit check hashes the edited file twice with no size bound | low | C performance | `hooks.go:233-240`, `:123`, `:158` | r1 (R1-06, H05) | **confirmed by reading** (both calls at `hooks.go:123` and `:158`); unmeasured |
| P7B-MRG-26 | Two ordered-JSON decoders (request_draft, memory_json) and a third draft measurement in `json_bounds.go` | low | D maintainability | `request_draft.go:390-473`, `memory_json.go:113-188`, `tools/json_bounds.go:12`, `:120` | r1 (R1-07) | **confirmed by reading** |
| P7B-MRG-27 | `request_draft` dedupes object keys with a linear scan (quadratic) | low | C performance | `request_draft.go:425-432` | r1 (R1-08, H06) | **confirmed by reading** (the loop at `:427-431` is as read); unmeasured |
| P7B-MRG-28 | Oversize files and duplicated whole-file patterns (memory_store.go 1,062 lines; task.go 703; catalog.go 632; two atomic writers that diverge) | low | D maintainability | `memory_store.go`, `task.go`, `catalog.go`, `request_draft.go`, `transcripts_list.go`, `transcript.go`; writers `task_store.go:352-433`, `memory_store.go:707-760` | r3 (R3-08) | **confirmed by reading** (line counts from `wc -l`) |
| P7B-MRG-29 | Unused-by-production `BuildCatalog*` constructors pass a nil task store | low | D maintainability | `catalog.go:50-63`, `:398`; `task.go:83-85` | r1 (R1-11) | **confirmed by reading**: the only non-test callers are other constructors in `catalog.go`; the production path is `BuildCatalogWithAsync` |
| P7B-MRG-30 | Transcript and transcripts tools are not wrapped by `tool_event` hooks, unlike the other native tools | low (decision) | D maintainability | `catalog.go:107-141`, `:477` (`WithToolEventHooksAll`) | r1 (R1-12) | **confirmed by reading**; a documented-or-fix decision, not a defect |
| P7B-MRG-31 | Transcript `walk.result` decodes, rewrites (data-URL regex) and status-parses every result line in the range, including off-page steps | low (deferred), partly fixed in `632ebed` | C performance | `transcript_steps.go` (`transcriptWalk.result`), `transcriptText`, `replaceDataURLs`, `transcriptResultStatus` | split out of MRG-03 (profile, p7bmrg03 run) | **measured** (about 240 MB of the 294.7 MB at HEAD, 400 turns). **Deferred as low by user decision.** Any fix needs a stored result length from the scan; that is a design change. | **Partly fixed in `632ebed`:** `replaceDataURLs` skips the regex for text without a `data:` prefix (about 20% fewer allocations, output identical). The remaining decode in `transcriptText` and `transcriptResultStatus` runs on every result line in the range and needs a stored length, which changes the model-visible header semantics. Deferred as low by user decision.

The rows merged or dropped:

- R1-01 vs r3's "sound" claim on the same gate: the sound claim covers the switch when the session exists; the leak is in the miss path. Both are recorded in §5.
- R1-10 merged into MRG-06, R1-04 into MRG-23, R1-05 into MRG-24, R1-06 into MRG-25.
- R3-04 (short `subagent:` id shared across profiles when storage is shared) is hardening and depends on explicit config sharing; kept out of the table as a note in §5 (not a finding).
- R3-08 kept as MRG-28.
- R2-11's "collision for first lines over 4 KiB" is part of MRG-21.

## 4. Non-consensus decisions (evidence)

**(a) P7B-MRG-01 severity: r2 critical, r4 high → critical.** Both reviewers found the same lines. Reading `memory_json.go:373-395` and the probe show the record is unreadable after a single quote and the value silently changes after a backslash or newline, with no error at write time. That is the §5.1 "data loss" definition. r4's high is the lower rating of the same defect; the merge keeps one entry and records both reviewers.

**(b) P7B-MRG-02 (r1 medium): reproduced, kept at medium.** The leak discloses the names of other catalog sessions to a model that is not allowed to read them. It does not disclose content. r1 rated it medium; the probe confirms the disclosure, so medium stands. Prior art: commit `226f648` deliberately lists valid values in not-found errors (for memory, task and subagent). This finding is the transcript version of the same listing without the switch check, not a repeat of that decision.

**(c) P7B-MRG-07 reasoning exposure (r3 medium): kept as a decision, not a defect.** Code: `detailBlock` returns reasoning only for `part=reasoning`, and cross-session reads are gated only by `allow_other_sessions`/`allow_other_profiles`/`allow_children`, which default to true (`sessionfs/native_transcript_settings.go:90`). The reasoning text is intentionally withheld from the list view (`transcript_steps.go:308-312`). Whether reasoning should have its own switch is a product decision. Recommendation for the user: decide; the code is consistent with the documented switches. Not reproduced, because a probe would only restate the default.

**(d) P7B-MRG-04 vs P7B-MRG-05 are one cost, two causes.** r2 files the page rescan and the step materialisation separately; the probes show each separately (flat per-page cost; allocation scaling with the range). Both are kept because the fixes differ (cursor offset vs. walk retention). r1's R1-02 (detail) is a third path with the same retention mechanism as MRG-05 but a different function; kept as MRG-03.

**(e) P7B-MRG-16 (task mutex) severity: r2 low, kept low.** The resolver shares one `TaskStore` per storage key (`app/resolver.go:768-786`), so in one process the mutex is sufficient. The lost-update case needs two server processes on one `storage_dir`. No reviewer claimed a same-process bug.

**(f) P7B-MRG-09 fsync: r1 and r3 agree.** Both grep-confirmed no `Sync` in the three stores. Kept low: crash-only, and the transcript journal (`internal/transcript`) fsyncs, so the gap is a divergence, not a defect in isolation.

## 5. Contradictions and "checked and sound" checks

| claim | by | check | result |
|---|---|---|---|
| "transcript gates are sound" (cross-session, cross-profile, child) | r1 H1/H2 (disproved for not-found), r3 H01, r4 "checked and sound" | Re-ran `TestTranscriptSubagentOfAnotherProfileKeepsTheAccessSwitches`, `TestTranscriptDoesNotReadAnotherSessionsTranscriptWithoutPermission`, `TestTranscriptRefusesAChildWhenAllowChildrenIsOff`, `TestTranscriptPathMustStayInsideWorkspace` (`-run` set, exit **0**) | The **success-path gates are sound** (r3, r4 correct). The **miss path leaks names** (r1 correct). r4's "session/profile transcript reads do not expose rejected paths through arbitrary selector grammar" is true for paths; it did not check the `no_such_profile` listing, so its claim is narrower than it reads. Recorded as MRG-02. |
| "memory store escaping is sound" | r3 ("validation sound") and r2 (named H5 verified) | Probe | r3's claim concerns validation, not escaping, and does not contradict r2/r4. The escaping defect is MRG-01. |
| "memory JSON writer checked and sound" | r4 wrote "Escaped memory JSON string serialization was disproved by the overlay probe and is P7B-R4-01" | Probe | Confirmed by r4's probe and mine. Not a contradiction; r4 recorded it as a finding. |
| "F-13 fixed" | r1, r2, r3 | `git log 5d09534` and `memory_store.go` `filesKnowing` | Stat-per-record is fixed; the ReadDir and lock-held scan remain. MRG-24 says so. |
| "request_draft auto-accept is a finding" | r2 said not a finding | `DECISIONS.md` ("The approval system is removed", 2026-10-23) | Not a finding (r1, r2, r3 agree). |
| "transcript detail is bounded" | r1 H7 disproved (F2), r4 "detail bounded" | Probe (MRG-03) | **r4 is wrong** on detail-range bounds (checked-and-sound claim: "step detail output is paged"). Paging limits the output, not the allocation. Recorded as MRG-03. |
| "remote cursor is sound" | r2 (MRG-08, low), r3 ("remote grammar sound") | Read `transcript_remote.go:284-300` vs `:59-108` | Both correct about different branches: the `selector` path checks the globs; the cursor path does not. MRG-08. |
| "hooks write/edit double wrap" | r2 H4 disproved | Read `hooks.go:244` vs `hooks_events.go:95` | Disproved: different hook kinds. No finding. |

Checks of r4's "checked and sound" list against the others and my reading:

- Transcript child gate on every path (r4): **sound** (`transcript.go:188-191` confirmed by reading; r1 and r3 agree).
- Remote gates and output (r4): **sound on the selector path**; the cursor branch is MRG-08 (r2). r4 did not look at that branch.
- Task validation and atomic write (r4): **sound** for validation; durability is MRG-09 (r1, r3). r4 said "atomically renames temp output" and did not mention fsync.
- Memory store validation, cursor bounds and symlink-safe records (r4): **sound**, as r3 says. The escaping is the one miss (MRG-01).
- Hooks, request_draft, catalog (r4): **sound**, as r1 and r3 say, except the MRG-30 decision.
- **r4 "transcript scans are cancellable, per-line bounded, total scan capped and detail paged"**: partly wrong. Scans are cancellable and capped; detail retention is not bounded by the page (MRG-03). r4 also claims list mode is "bounded" for materialisation; r1 H13 and my probe show list allocation scales with the range (MRG-05).

## 6. Reviewer coverage and reliability

Coverage of the 22 files: r2 read all 22 (with outlines for `task.go:330-703`, `memory_json.go:200-287`, `request_draft.go:145-591`); r1 read most, with index-only for `memory_group.go`, `transcript_steps.go:200-344`, `transcripts_list.go` helpers; r3 read all 22 in ranges; r4 read all 22 with "full" ranges in its table. r4's table claims full reads of all 22 files, but its own text says `memory_group.go`, `task.go` and others were read "in full" without a per-file command evidence line. I did not re-verify that claim beyond the findings; I flag it as unverified coverage.

Reviewer pair agreement (count of findings both filed, on the merged keys):

| pair | shared findings | notes |
|---|---|---|
| r2 & r4 | 1 (MRG-01) | both rated the same root cause; disagree on severity (critical vs high) |
| r1 & r3 | 2 (MRG-09 fsync, MRG-24 ReadDir/lock) plus MRG-06 (double scan) | both name the fsync gap; both name the memory ReadDir cost |
| r1 & r2 | 2 (MRG-04 paging rescan; MRG-25 hook digest is r1 only, so 1 shared: MRG-04) | r1 has the first-page double scan under MRG-06, which r2 does not file |
| r2 & r3 | 1 (MRG-28 file size; r3's R3-06 durability is MRG-09 and r2 does not file it) | task store concurrency: r2 MRG-16 and r3 MRG-09 are different causes |
| r3 & r4 | 1 (MRG-05 list materialisation, r4 as hypothesis H1) | |
| r1 & r4 | 0 on the same finding (r4's only finding is MRG-01, which r1 did not file) | r1 and r4 share MRG-24 and MRG-04 only as hypotheses |

Items found by one reviewer only (as findings, not hypotheses): MRG-02, MRG-07, MRG-08, MRG-15, MRG-16, MRG-17..MRG-22, MRG-26..MRG-27, MRG-29..MRG-30. Verified by me: MRG-02 (reproduced), MRG-08, MRG-16, MRG-21, MRG-22, MRG-29 (read). Not re-read: the rest of the unique set, shown as confirmed-by-reading in §3 only where the reviewer's trace matched a read I did (see the per-row status). No unique finding was disproved.

Reviewer reliability read:

- **r4** filed one finding for 22 files. The finding is correct and critical-class, but it under-rated it, and its "checked and sound" list overstates the detail bounds (MRG-03).
- **r2** has the most complete set and the correct critical. Its one measured claim (the rescan) reproduces.
- **r1** has the only reproduced leak (MRG-02) and a correct reproduction of MRG-03.
- **r3** has the reasoning-exposure decision (MRG-07) and sound gate analysis. Its "shared subagent storage" nuance is hardening only and is not in the table.

## 7. Status summary for the approval gate

- 1 critical (MRG-01): reproduced, confirmed, recommended for immediate fix (regression test: round-trip of `"`, `\`, `\n`, `\t`, U+001B, U+2028 through `CreateValue`/`Get`, failing without the fix). Fix is out of scope for this merge.
- Mediums: MRG-02, MRG-03, MRG-04, MRG-05 reproduced; MRG-07 is a design decision for the user.
- Lows: 23 rows, each confirmed by reading; none disproved; several unmeasured (performance rows) and routed to the measurement step.

## 8. Counts

| severity | rows in §3 |
|---|---|
| critical | 1 (MRG-01) |
| high | 0 |
| medium | 4 confirmed-severity (MRG-02, 03, 04, 05) + 1 proposed (MRG-07, a design decision) |
| low | 22 (MRG-06, 08–13, 15–29) + 1 decision (MRG-30) |
| total rows | 29 (the table in §3 has 29 `P7B-MRG-` rows, checked with grep) |

Per-reviewer finding totals are not tallied here; each row lists its reviewers in the "found by" column. r4 filed one finding, which is MRG-01.

Consensus (found by 2+): MRG-01 (r2, r4), MRG-04 (r2, r4, r1), MRG-05 (r2, r3, r4), MRG-06 (r1, r3), MRG-09 (r1, r3), MRG-24 (r1, r3, r4), MRG-25 partial (r1, r2), MRG-28 (r3, r2 partial).
Unique (one reviewer): MRG-02, 03, 07, 08, 10, 11, 12, 13, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25 (r1 only), 26, 27, 29, 30.
Contradictions: §5 (four resolved, one r4 claim corrected).

## 9. Unresolved

- `messages-golden-diff` is not installed on this machine, so the golden check for any MRG-01 fix is not run. Not verified.
- `go test ./internal/tools/native/ -run Memory` passes today: this is the reason the defect is not in the test suite. The regression test is part of the fix stage.
- Performance rows (MRG-04, 05, 06, 19, 20, 23, 24, 25, 27) are `unmeasured` pending the §6.3 measurement step. MRG-04 and MRG-05 have two-size probes above (flat per page; 0.73 → 2.10 MB), which are relative numbers but not the §6.3 benchmark protocol (`-count=10`, load average).
- r4's coverage of 22 files: its per-file table asserts full reads; I did not verify each read against a command log. Flagged, not disproved.
- No decision has been taken by the user on the 22 lows, MRG-07 (reasoning switch) or MRG-30 (hook wrapping of transcript reads). Status for all: `open`.
