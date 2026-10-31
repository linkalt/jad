# Phase p7a merged (coordinator)

- Phase: p7a, native file and search tools (`backend-go/internal/tools/native`, 27 non-test files).
- Inputs: `audit_findings/p7a_r1.md` to `p7a_r4.md`, read after all four were finished (audit_plan.md 6.2 and 7.2).
- HEAD `f425dee`. Date 2026-10-30 (UTC). `git status --short -- backend-go` was empty before and after the probes.
- No source change, no commit, no benchmark, no `-race`. Probes ran as `-overlay` files from `/tmp/p7a_merge`, removed at the end.
- Status: `confirmed (repro)` = run in a /tmp probe with an exit code; `confirmed (read)` = source read at the cited lines; `disproved`; `decision` = the fact is true, the disposition is a design call.

## 1. Reviewer summary

| reviewer | findings filed | severity | notes |
|---|---|---|---|
| r1 | 4 | medium 2, low 2 | all 4 confirmed; one wrong sound claim (dead code, P7A-12); P7A-16 filed as a decision, not a finding |
| r2 | 10 | medium 2, low 8 | all 10 confirmed as facts; X9 disproved the provider-key leak, which is wrong for ripgrep and glob (P7A-04); several line citations do not match HEAD (fetch_store.go is 342 lines, image_file.go 77, ripgrep.go 425, so `ripgrep.go:654` and `fetch_store.go:406-425` do not exist) |
| r3 | 6 | medium 1, low 5 | 5 confirmed; P7A-17 disproved; its H02 says every entry is Stat'ed, but `renderDirectory` only Infos up to the limit (read.go:367) |
| r4 | 2 | medium 1, low 1 | 2 confirmed; 2 sound claims disputed (P7A-02, P7A-07); 12 confirmed items not filed; P7A-16 not addressed |

## 2. Merged findings (normalised by cause plus file:line)

Severity count: critical 0, high 0, medium 3, low 13. One disproved item (P7A-17) is not counted. Lens: A correctness, B security, C performance, D maintainability.

| id | title | severity | lens | location | found by | status |
|---|---|---|---|---|---|---|
| P7A-01 | Directory read lists entries that a deny rule hides | medium | B | read.go:350 (`pathAllowed`), path.go:92-102 | r1, r2, r3; missed by r4 | confirmed (repro, exit 1) |
| P7A-02 | Fetch storage has no reclaim, so a full store fails every later fetch | medium | A | fetch_store.go:116-121 | r1, r2; disputed by r4 | confirmed (repro, exit 0) for the state; defect status is a user decision (section 4) |
| P7A-03 | `isPublicFetchAddress` accepts IANA special-purpose addresses that its exclusions miss (198.51.100.1, 203.0.113.1, 192.88.99.1, 2001:2::1, 2001::1, 2001:10::1, 2001:20::1) | medium | B | fetch.go:469-526 | r4; missed by r1, r2, r3 | confirmed (repro, exit 0): filter acceptance only; impact conditional on routing |
| P7A-04 | ripgrep and glob children get the provider API key; bash does not | low | B | ripgrep.go:101, ripgrep.go:353-362, glob.go:236 | r1; r2 wrongly disproved it; missed by r3, r4 | confirmed (repro, exit 0) |
| P7A-05 | fetch accepts any TCP port on a public address (no port policy) | low | B | fetch.go:388-444, fetch.go:537-545 | r2; missed by r1, r3, r4 | confirmed (repro, exit 0): validation only, nothing dialed |
| P7A-06 | read_image stored reference reads the whole object before the read_image size limit | low | C | image_file.go:25-29, imagestore/store.go:526 | r2, r4 | confirmed (read) |
| P7A-07 | Spill throttle map grows per call when a legacy `stdout_full` template puts `{call_id}` in the directory | low | A/C | bash_spill.go:267-290, bash_spill.go:97 and :108 | r2, r3; disputed by r4 | confirmed (repro, exit 0) |
| P7A-08 | edit commits over a same-inode in-place change made after its read | low | A | edit.go:124, edit.go:212, mutate.go:258-264, mutate.go:287-333 | r1; r3 and r4 say the commit is sound | confirmed (read), not reproduced |
| P7A-09 | fetch follows an https to http redirect with no signal | low | B | fetch.go:327-333, fetch.go:394 | r3; missed by r1, r2, r4 | confirmed (read), not reproduced (no network) |
| P7A-10 | image read path has no identity check after open (a FIFO swap can block) | low | A | image_file.go:38-52 | r3; missed by r1, r2, r4 | confirmed (read), not reproduced |
| P7A-11 | `PathLocks.Lock` on a nil receiver locks nothing shared | low | A | path_locks.go:22-26 | r3; missed by r1, r2, r4 | confirmed (read); production callers replace nil first (write.go:36, edit.go:37) |
| P7A-12 | Dead code: `bashStreamCollector` render and renderWithProducedBytes; `readFetchResponse` is test-only | low | D | bash_output.go:154-156, bash_output.go:190-192, fetch.go:611-614 | r2; r1 said none | confirmed (read) |
| P7A-13 | fetch page converts the whole stored document to UTF-16 three times per page | low | C | fetch_store.go:158, :171, :188, :265; ripgrep.go:415-417 | r2; missed by r1, r3, r4 | confirmed (read); perf unmeasured (M3) |
| P7A-14 | fetch `Create` stats every stored file under the store mutex | low | C | fetch_store.go:91, :116, :280-299 | r2; missed by r1, r3, r4 | confirmed (read); perf unmeasured (M2) |
| P7A-15 | grep re-resolves every matched file after the search | low | C | grep.go:122-131 (call at :124) | r2; missed by r1, r3, r4 | confirmed (read); perf unmeasured (M4) |
| P7A-16 | Per-agent `paths` narrows write, edit and transcript tools only; read, grep, glob, read_image do not consult it | low | B | path_scope.go:14; callers edit.go:92, write.go:94, transcript_path.go:78, transcripts_list.go:133 | r2 (finding); r1 (decision, not filed); r4 silent | decision: the fact is confirmed (read); DECISIONS.md:334 records write and edit only |
| P7A-17 | glob lists a symlink whose target is denied | (not counted) | B | glob.go:280 | r3; r2 and r4 say glob is sound | disproved (repro, exit 0) |

## 3. Consensus, unique and contradictions

- **Consensus** (2+ reviewers filed it): P7A-01 (r1, r2, r3), P7A-02 (r1, r2), P7A-06 (r2, r4), P7A-07 (r2, r3). Four items. P7A-02 and P7A-07 are also disputed by r4, so they are in section 4 as well.
- **Unique** (one reviewer filed it, no other reviewer disputes it): P7A-03 (r4), P7A-05 (r2), P7A-09 (r3), P7A-10 (r3), P7A-11 (r3), P7A-13 (r2), P7A-14 (r2), P7A-15 (r2). Eight items, all verified by reading or reproduction.
- **Contradictions** (one reviewer filed, another asserted sound or decision): P7A-04 (r1 vs r2), P7A-08 (r1 vs r3 and r4), P7A-12 (r2 vs r1), P7A-16 (r2 vs r1 decision), P7A-17 (r3 vs r2 and r4). Five items, each resolved in section 4.
- Totals: 4 + 8 + 5 = 17 merged items. 16 confirmed, 1 disproved (P7A-17), 0 unverified.
- Verification method: 6 confirmed by reproduction (P7A-01, 02, 03, 04, 05, 07), 10 confirmed by reading (P7A-06, 08, 09, 10, 11, 12, 13, 14, 15, 16). Every medium was reproduced; every consensus item was read at source.
- Repeats of prior art: none dropped. Re-confirmed open prior items: P7A-04 is the open recommendation at audit_design.md:74 ("children get the allowlist unless a setting says otherwise"), and the native-tree gap is still there. The IPv6 host-list gap is P2A-10 (open), not repeated here.

## 4. Evidence for each non-consensus decision

**P7A-04 (r1 files it; r2 says sound).** r2's X9 disproved the leak. The bash half is right: `buildNativeBashEnvironment` drops `apiKeyEnv` (bash.go:382-393). The ripgrep and glob half is wrong: `environmentWithoutServerToken` strips only `JAD_API_TOKEN` (ripgrep.go:353-362), and glob passes it at glob.go:236. Probe `TestP7AMergeProbeChildEnvironment` (`OPENAI_API_KEY` and `JAD_API_TOKEN` set in the test): rg/glob env `provider_key_present=true server_token_present=false`; bash env `provider_key_present=false`. Exposure needs a configured `grep.binary` or `glob.binary` that prints its environment. Severity low, as r1 rated it.

**P7A-02 (r1 and r2 file it; r4 calls the cap configured behaviour).** The state is confirmed by repro: the first 900-byte `Create` under a 2000-byte cap succeeds, and the next three return `fetch storage is full` (`TestP7AMergeProbeFetchStoreFull`, exit 0). Nothing removes a stored document: the only `os.Remove` calls are the rollback at fetch_store.go:128 and the temp file at :308, and `scratch_sweep.go` sweeps only `exec-` and `tool-smith-` prefixes. DECISIONS.md:103 records that the capacity check is race-free, so the cap itself is intended. It does not record a decision that a full store stays full, and the error gives no recovery hint. Disposition is the user's call (eviction, or a documented manual reset). Status: state confirmed by repro; defect status open.

**P7A-07 (r2 and r3 file it; r4 says no reachable per-call key).** Probe `TestP7AMergeProbeSpillKeyGrowth` (exit 0): 1000 calls with legacy `stdout_full` and `stderr_full` templates whose directory contains `{call_id}` grow the throttle map from 0 to 2000 keys. The same 1000 calls with the generated template add one key (`TestP7AMergeProbeSpillKeyGeneratedTemplate`, exit 0, 2000 to 2001). The legacy template is accepted by config: `normalizeBashSpillTemplate` (native_bash_settings.go:447-449) only requires `{call_id}` to appear somewhere; `planBashSpill` passes the raw call id into the path (bash_spill.go:97) and takes its directory as the key (bash_spill.go:108). The trigger is operator configuration, not the model. Severity low.

**P7A-08 (r1 files it; r3 and r4 call the commit sound).** `applyEdit` reads the content and checks identity (edit.go:124). `commitFile` checks identity only (mutate.go:258-264 and 287-333), so a same-inode in-place write between the read and the rename is replaced. Read only; not reproduced. The window is milliseconds and the actor is a same-user process. r3's and r4's sound verdicts were about symlink and TOCTOU escape, and those hold (`pinDirectory`, `os.SameFile`). Write replaces the whole file by design, so only edit is affected. Severity low.

**P7A-12 (r2 files it; r1 says no single-reference names).** `(*bashStreamCollector).render` (bash_output.go:154) and `renderWithProducedBytes` (bash_output.go:190) have no callers. Every production and test call goes to `bashOutputCollector` (bash_child.go:84, 279, 309; process_manager.go:462, 729; bash_test.go:212; exec_test.go:354). `readFetchResponse` (fetch.go:611) is called only from tests (fetch_test.go:267; fetch_transport_test.go:51, 233). r1's dead-code claim is wrong for these items. Confirmed by reading.

**P7A-16 (r2 files it; r1 says decision).** `scopedPolicy` has four callers: edit.go:92, write.go:94, transcript_path.go:78, transcripts_list.go:133. read, grep, glob and read_image do not call it. DECISIONS.md:334 records that the child's `paths` narrows "where the child's `write` and `edit` may change files". So write-only narrowing is recorded design, and r1 is right that it is not a defect. r2's point that transcript tools apply the scope to reads while read does not is a fact. Disposition: a decision challenge for the user, not a fix. Status: fact confirmed by reading; defect status is a decision.

**P7A-17 (r3 says glob lists a symlink to a denied target; r2 and r4 say glob is sound).** Probe with the argv of `globCommandArgs` (`rg --files --no-config --no-messages --sort path --null -- .` in a directory holding `plain.txt` and `link.txt -> outside/denied.txt`, run outside the Go test harness): output is `./plain.txt` only, exit 0. ripgrep does not follow symlinks without `-L`, and glob does not pass it. Disproved for the default configuration. r3's H03 name leak does not occur here.

**P7A-03 (r4 only; not contested).** `isPublicFetchAddress` (fetch.go:469-526) returns true for 198.51.100.1, 203.0.113.1, 192.88.99.1, 2001:2::1, 2001::1, 2001:10::1 and 2001:20::1 (`TestP7AMergeProbeSpecialUse`, exit 0). It returns false for 192.0.2.1 and 198.18.0.1 and for 2001:db8::1. So the list is inconsistent: 192.0.2.0/24 is blocked, 198.51.100.0/24 and 203.0.113.0/24 are not. The impact needs such a range to be routed on the deployment network, so medium is conditional. Confirmed by repro (filter acceptance only; no dial).

**P7A-05 (r2 only; not contested).** `validateFetchURL` never checks the port. `parsed.Port()` is used as given (fetch.go:439), and `Do` dials it (fetch.go:537-545). Probe `TestP7AMergeProbeFetchAnyPort` (exit 0): `http://93.184.216.34:22/`, `http://93.184.216.34:6379/` and `https://93.184.216.34/` all pass validation with `err=<nil>`. Nothing is dialed. r2's reachability-oracle impact is read only, not tested.

**P7A-09 (r3 only).** `validateFetchURL` accepts http and https (fetch.go:394), and each redirect target is passed through it (fetch.go:327-333). Nothing compares the redirect scheme with the original. Confirmed by reading; not reproduced (no network, by rule).

**P7A-10 and P7A-11 (r3 only).** P7A-10: `readImageFile` runs Lstat, Open and readWithin (image_file.go:38-52) with no `os.SameFile`, unlike `readVerifiedRegularFile` (path.go:126-154). Read only. P7A-11: `PathLocks.Lock` on a nil receiver builds a fresh map (path_locks.go:22-26). Both constructors replace nil before capture (write.go:36, edit.go:37), so production is safe. Read only.

**P7A-01 (r1, r2, r3 agree).** The root is `pathAllowed` (path.go:92-102): it tests workspace containment and allow rules and never deny rules. `renderDirectory` filters with it (read.go:350). Repro `TestP7AMergeProbeDirectoryReadLeak` (exit 1; the FAIL is the intended confirmation): the result contains `entry: {"name":"denied.txt","kind":"file","size_bytes":1}` while the policy denies that path. The existing test uses `DefaultLimit: 1` (read_test.go:90-114), so `denied.txt` falls past the limit and the assertion passes for the wrong reason, as r1 said.

## 5. r4 "checked and sound" claims, file by file

r4 filed two findings (P7A-R4-01 in fetch.go, P7A-R4-02 in image_file.go). Its other files are "no finding" entries. Verdicts: **holds** = the claim is right; **wrong** = a merged finding contradicts it; **missed** = a merged finding on the file that r4 did not file.

| file | r4 claim | verdict | evidence |
|---|---|---|---|
| bash.go | env allowlist, cwd, shared runner sound | holds | buildNativeBashEnvironment (bash.go:382-422) |
| bash_child.go | lifecycle sound | holds | r2 and r3 agree |
| bash_executable_unix.go, bash_exit_unix.go | sound | holds (not re-read by me) | r4 only |
| bash_executable_windows.go, bash_exit_windows.go | not executed | out of scope | Windows not run by any reviewer |
| bash_output.go | bounded collector sound | missed P7A-12 | dead `bashStreamCollector` render methods (bash_output.go:154-156, 190-192) |
| bash_spill.go | throttle bounded, no reachable per-call key | wrong (P7A-07) | legacy `stdout_full` with `{call_id}` in the directory: 2000 keys per 1000 calls (probe) |
| edit.go, mutate.go | commit path checked, no finding | missed P7A-08 | edit reads the content, the commit checks identity only (edit.go:124; mutate.go:258-264, 287-333) |
| edit_text.go | sound | holds | no contrary evidence |
| fetch.go | SSRF guard sound apart from P7A-03 | missed P7A-05, P7A-09 | any port passes validation (fetch.go:388-444); http allowed on redirect (fetch.go:327-333, 394) |
| fetch_store.go | caps honoured; eviction absent, treated as configured | disputed (P7A-02); missed P7A-13, P7A-14 | state confirmed by repro; the perf items are reads |
| fetch_transport.go | pool key sound | holds | fetch.go:551 key includes scheme, host, port, ip |
| file_change.go | sound | holds (not re-read by me) | r4 only |
| glob.go | filters and checks sound | holds (P7A-17 disproved) | rg lists no symlink without `-L` (probe) |
| grep.go | post-filter sound | missed P7A-15 (perf) | per-group resolveExistingTarget (grep.go:124) |
| image_file.go | P7A-06 only | missed P7A-10 | no SameFile after open (image_file.go:38-52); r4's "limit+1 bounded" is true for the file path |
| mutate.go | pinned handles, identity sound | missed P7A-08 | as edit.go above |
| mutate_path.go | symlink and policy checks sound | holds | r2 and r3 agree |
| path.go | policy checked before and after resolve, no finding | missed P7A-01 (the listing filter `pathAllowed`, path.go:92-102, ignores deny) | the resolve path is sound; the listing filter is not |
| path_locks.go | ref counting sound, no finding | missed P7A-11 (low) | nil receiver, path_locks.go:22-26 |
| path_scope.go | narrowing sound, no finding | holds; P7A-16 is a decision r4 did not address | scopedPolicy callers at four sites |
| read.go | listing checked, no finding; H01 perf | wrong (P7A-01); H01 holds | renderDirectory filters with pathAllowed (read.go:350); ReadDir reads every entry (read.go:343) |
| read_image.go | sound | holds | shared helper gap is P7A-06 |
| ripgrep.go | token removed, no finding | missed P7A-04 | provider key not stripped (ripgrep.go:353-362); P7A-13 uses the shared utf16Length (ripgrep.go:415-417) |
| write.go | commit and locking sound | holds for write; P7A-08 is edit-only | write replaces the whole file by design |
| html-to-markdown link | generated, out of scope | out of scope | not reviewed by any reviewer |

Summary of r4's sound claims: 3 wrong or contested (P7A-07, P7A-01 on read.go and path.go, P7A-02 on fetch_store.go). 12 confirmed items r4 did not file: P7A-01, 04, 05, 08, 09, 10, 11, 12, 13, 14, 15, 16. r4 is right on glob (P7A-17 disproved), on the bash environment, on the mutation symlink and pin checks, and on the fetch SSRF guard for hosts and redirect hops.

## 6. Per-reviewer agreement and disagreement

Which findings each reviewer filed (merged ids): r1 {01, 02, 04, 08}; r2 {01, 02, 05, 06, 07, 12, 13, 14, 15, 16}; r3 {01, 07, 09, 10, 11, 17}; r4 {03, 06}. P7A-16 is r1's decision (not filed as a finding), so r1's set does not include it.

Pair counts (from those sets). **Both** = filed by both. **Only A** and **only B** = filed by one of the pair and not the other.

| pair | both | only A | only B | total differing |
|---|---|---|---|---|
| r1 vs r2 | 2 (01, 02) | 2 (04, 08) | 8 (05, 06, 07, 12, 13, 14, 15, 16) | 10 |
| r1 vs r3 | 1 (01) | 3 (02, 04, 08) | 5 (07, 09, 10, 11, 17) | 8 |
| r1 vs r4 | 0 | 4 (01, 02, 04, 08) | 2 (03, 06) | 6 |
| r2 vs r3 | 2 (01, 07) | 8 (02, 05, 06, 12, 13, 14, 15, 16) | 4 (09, 10, 11, 17) | 12 |
| r2 vs r4 | 1 (06) | 9 (01, 02, 05, 07, 12, 13, 14, 15, 16) | 1 (03) | 10 |
| r3 vs r4 | 0 | 6 (01, 07, 09, 10, 11, 17) | 2 (03, 06) | 8 |

Per reviewer: unique findings (filed by no other reviewer) and items another reviewer disputes or calls sound.

| reviewer | findings | unique to it | disputed or contradicted by another reviewer |
|---|---|---|---|
| r1 | 4 | 08 (04 is also filed by r2's disproof, so it is contested, not unique) | 02 (r4 disputes); 04 (r2 disproves its ripgrep half); 08 (r3 and r4 call the commit sound); P7A-12 is a sound claim r1 made that r2 contradicts |
| r2 | 10 | 05, 12, 13, 14, 15, 16 (06 is also filed by r4, so it is shared; 16 is r1 decision and so counts as unique to r2 finding) | 02 (r4 disputes); 07 (r4 disputes); P7A-17 (r2 and r4 say glob is sound, r3 filed it, disproved) |
| r3 | 6 | 09, 10, 11, 17 (4 unique) | 07 (r4 disputes); 17 disproved (r2 and r4 sound) |
| r4 | 2 | 03 (1 unique) | none filed as a dispute, but r4's sound claims are contradicted by 01, 02, 07 |

Disagreement summary. r4 disputes 2 items (P7A-02 and P7A-07). r2 wrongly disproves one item (P7A-04 for ripgrep and glob). r1 wrongly calls one item sound (P7A-12, dead code). r3 files one item (P7A-17) that is disproved. r4 misses 12 confirmed items. Reliability: r1 has all four findings confirmed and one wrong sound claim; r4 has three sound claims contradicted for two findings.


## 7. Measurement list (performance hypotheses, deduplicated)

| id | hypothesis | source | file:line | benchmark | status |
|---|---|---|---|---|---|
| M1 | `read` of a large text file reads the whole file before slicing lines | r1 H01 | path.go:126-154, read.go:262-331 | BenchmarkNativeReadLargeFile (50-line range) | unmeasured; F-12 residual |
| M2 | fetch `Create` scans every stored file under s.mu (same cause as P7A-14) | r1 H02, r2 H02 | fetch_store.go:90-132, :280-299 | new BenchmarkFetchStoreCreate (100, 1000, 5000 documents) | unmeasured |
| M3 | fetch page converts the stored document to UTF-16 three times (P7A-13) | r2 H01 | fetch_store.go:158, :171, :188, :265 | new BenchmarkFetchStorePage (1 MB, 8 MB) | unmeasured |
| M4 | grep post-filter re-resolves each matched file (P7A-15) | r2 H03 | grep.go:122-131 | extend BenchmarkRunRipgrep or a new post-filter bench | unmeasured |
| M5 | write and edit read the whole target to decide unchanged | r2 H04 | mutate.go:266-285, write.go:183, edit.go:203 | BenchmarkNativeEditWithBackupOn128KiBFile plus a new unchanged-write bench | unmeasured |
| M6 | spill planning lists the spill directory per call once the interval passes | r2 H05, r3 H05 | bash_spill.go:272-312 | BenchmarkPlanBashSpillLargeDirectory at interval 3600 and 0 | unmeasured; F-11 check |
| M7 | ripgrep UTF-16 per match line (`truncateJSString`) | r3 H01 | ripgrep.go:309, :419-425 | BenchmarkRunRipgrep with long lines | unmeasured |
| M8 | directory listing reads every entry before the limit | r4 H01, r3 H02 | read.go:343-370 | new bench at 100, 10k, 100k entries | unmeasured |
| M9 | glob post-filter policy check per listed file | r3 H03 | glob.go:280 | BenchmarkExecuteNativeGlob | unmeasured |
| M10 | fetch HTML path copies the body three times | r3 H04 | fetch.go:354-358 | new fetch-html httptest bench at 100 KiB and 1 MiB | unmeasured |
| M11 | read_image stored reference reads the whole object before the limit (P7A-06) | r2 (P7A-10), r4 (P7A-06) | image_file.go:25, imagestore/store.go:526 | new bench (not in the plan list) | unmeasured |

Measurement list size: 11 items (M1 to M11). The serialized measurement step (plan 6.3) is not run here, so none of these is measured.

## 8. Cross-phase leads (merged)

- The fetch store is keyed by `storage_dir` and shared across sessions (resolver.go:812-823), so capacity is shared too. Relevant to P7A-02; belongs to phase 2b (app).
- `nativeFetchHostPattern` (native_fetch_settings.go:37) cannot name an IPv6 literal. Re-confirms P2A-10 (open, low; p2a_merged.md:46). Not a new finding.
- `sessionfs.allowedByNativeReadPolicy` (files.go:199-204) has the same deny-on-lexical-path class as P7A-01 for session file listing. Related to P2B2-06 (open, low). Its listing filter is separate from `pathAllowed`, so fixing P7A-01 does not fix it.
- `scratch_sweep.go` sweeps only `exec-` and `tool-smith-` prefixes; the fetch store is never swept. Relevant to P7A-02; the sweep belongs to the 6c/7b area.
- P7A-07 is a per-configuration-directory variant of the spill map growth noted in audit_performance_plan.md P-049 (resolver maps, low). It is not a repeat of P-049.

## 9. Commands run (exit codes)

| command | exit | note |
|---|---|---|
| `git status --short -- backend-go` (before and after the probes) | 0 | empty; the root dirty files are the reviewers' own |
| `go test -overlay /tmp/p7a_merge/overlay.json ./internal/tools/native/ -run '^TestP7AMergeProbeDirectoryReadLeak$' -count=1 -v` | 1 | PROBE CONFIRMED: `denied.txt` listed (P7A-01); the FAIL is the intended confirmation |
| `go test -overlay /tmp/p7a_merge/overlay.json ./internal/tools/native/ -run '^TestP7AMergeProbeFetchStoreFull$' -count=1 -v` | 0 | PROBE CONFIRMED: the store is full after one document (P7A-02) |
| `go test -overlay .../overlay2.json ... -run '^TestP7AMergeProbeSpecialUse$' -count=1 -v` | 0 | P7A-03 acceptance table |
| `go test -overlay .../overlay4.json ... -run '^TestP7AMergeProbe(ChildEnvironment|FetchAnyPort)$' -count=1 -v` | 0 | P7A-04 and P7A-05 |
| `go test -overlay .../overlay3.json ... -run '^TestP7AMergeProbeSpillKey' -count=1 -v` | 0 | P7A-07: 0 to 2000 keys (legacy), 2000 to 2001 (generated) |
| `rg --files --no-config --no-messages --sort path --null -- .` in a probe tree with a symlink to a denied file | 0 | lists `./plain.txt` only (P7A-17 disproved) |
| `git diff --check` | 0 | whitespace check, run before this file was written |

Not run: `go build`, `go vet`, `gofmt`, the full package test run, `-race`, benchmarks, network access. The probes compiled and ran inside the package test build, so they exercised the same package build, but no separate build or vet was run for this merge.

## 10. Cleanup

Probe overlays and scratch files were under `/tmp/p7a_merge`. They are removed at the end of this merge (see the final step). No file in the repository was created, changed or staged apart from this file.
