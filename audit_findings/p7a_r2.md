# Phase p7a review — reviewer r2

- Phase: p7a (native file and search tools: bash, path, read, write, edit, mutate, file_change, image_file, glob, grep, ripgrep, fetch, html-to-markdown link)
- Reviewer: r2
- HEAD: `f425dee` (worktree dirty only in `audit_plan.md`, `MASTER.md`, `EXECUTOR.md`, `REVIEWER*.md` and `audit_findings/`; no source file modified while reviewing)
- Date: 2026-10-27 (session date)
- Files read (27/27 non-test files of the sub-pass, plus the shared helpers they call): `bash.go`, `bash_child.go`, `bash_output.go`, `bash_spill.go`, `bash_executable_unix.go`, `bash_executable_windows.go`, `bash_exit_unix.go`, `bash_exit_windows.go`, `path.go`, `path_scope.go`, `path_locks.go`, `read.go`, `read_image.go`, `write.go`, `edit.go`, `edit_text.go`, `mutate.go`, `mutate_path.go`, `file_change.go`, `image_file.go`, `glob.go`, `grep.go`, `ripgrep.go`, `fetch.go`, `fetch_store.go`, `fetch_transport.go`, `html-to-markdown-rs_cgo_link.go`.
  Shared helpers read outside the 27 for evidence: `sessionfs/file_settings.go` (policy loading, `resolveNativeRulePath`), `sessionfs/files.go` (`PathWithin`), `sessionfs/native_fetch_settings.go`, `sessionfs/native_grep_settings.go` defaults, `imagestore/store.go:499-535` (`Store.Open`, `ValidName`), `tools/native/scratch_sweep.go` (checked whether stale fetch storage is swept: it is not).
- Tests read for evidence and test quality: `read_test.go`, `read_benchmark_test.go`, `grep_budget_test.go`, `glob_test.go`, `fetch_test.go` (SSRF cases), `path_scope_test.go`, `path_scope_narrow_test.go`, `bash_test.go` (spill), `exec_test.go:354`.
- Navigation: `codebase-memory get_architecture` (scoped to `backend-go/internal/tools/native`), `zvec-grep` (path confinement / fetch SSRF / bash spill), then `search_graph`/`get_code_snippet` were used for cross-checks; whole-file reads of the 27 target files used `read.sh`/`cat -n` (these files *are* the review target and were read in full, not sampled).

## 0. Commands run (with exit codes)

```
go build ./internal/... ./cmd/...                                            exit 0
go vet ./internal/tools/native/                                              exit 0
gofmt -l internal/tools/native                                               exit 0 (no output)
go test ./internal/tools/native/ -run 'TestNativeReadValidatesAndReadsTextWithRanges|TestNativeGlob' -count=1
                                                                             exit 0 (ok, 0.016s)
go test ./internal/tools/native/ -run TestR2ProbeReadListingHidesDeniedDir -count=1 -overlay /tmp/r2/overlay.json
                                                                             exit 1 (probe, expected FAIL: demonstrates P7A-R2-01; log /tmp/r2/probe.log)
```
The probe is a `/tmp/r2/deny_list_test.go` file added to the package with `-overlay`; nothing was written into the tree. Scratch removed at the end. No benchmark, `-race`, `-count>1`, network call or process kill was run.

## 1. Understanding note

- **Tool surface.** `catalog.go` (phase 6b/7b) builds these `tools.Tool` values; each `Execute` gets a `tools.ExecutionContext` (CallID, WorkspaceRoot, SessionDir, PathScope, OnFileChange). Every filesystem tool funnels through three helpers in this sub-pass: `resolveExistingTarget` (read side, `path.go:23`), `resolveMutatePath` + `inspectMutateTarget` + `commitFile` (write side, `mutate_path.go:44`, `mutate_path.go:84`, `mutate.go:287`) and `pathPolicyError`/`pathAllowed` (`path.go:77`, `path.go:92`).
- **Path confinement model.** The workspace root is symlink-resolved once per call; the requested path is made absolute, checked against the policy *unresolved*, resolved with `EvalSymlinks`, then checked again resolved (`path.go:42,63,66`). Mutations reject any symlinked path component (`mutate_path.go:167`) and pin the parent directory through `/proc/self/fd/N` so the write, the rename and the backup all operate on the directory that was checked (`mutate.go:68-112`). Identity checks (`os.SameFile`) guard read, backup and overwrite. `PathLocks` serialises native mutations of the same canonical path inside the process (`path_locks.go`).
- **Policy shape.** `NativePathPolicy` = ordered `Paths` rules (allow/deny), `UnrestrictedPaths`, and `Within` (the per-agent `PathScope` narrowing). `scopedPolicy` (`path_scope.go:14`) is applied by exactly four tools: `write`, `edit`, `transcript_path`, `transcripts_list`.
- **bash/exec share one runner.** `runShellChildDetailed` (`bash.go:217`) resolves+cwd-checks the directory, builds an allowlisted environment (`bash.go:382`, `JAD_API_TOKEN` and the model's API-key variable denied), runs the child through `runBashCommand` (`bash_child.go:66`) with pipe readers, a bounded collector (`bash_output.go`) and optional spill sinks (`bash_spill.go`), then renders. No shell path policy on arguments: `bash` is by design an unconfined command runner.
- **Search tools.** `glob` and `grep` spawn ripgrep (`glob.go:232`, `ripgrep.go:380`) with `--no-config`, an allowlisted-free env (`environmentWithoutServerToken`), ordered kill/drain deadlines, bounded stdout (collector head window / JSON event reader with match, line and byte budgets) and a post-filter of every returned path against the policy.
- **fetch.** `validateFetchURL` (`fetch.go:388`) parses the URL, rejects credentials, per-host allow/deny, resolves the host and requires *every* resolved address to be public (`isPublicFetchAddress`), then pins the dial to the first validated address inside a per-destination transport (`fetch_transport.go:104`); redirects are followed manually one hop at a time with revalidation (`fetch.go:317-333`). Documents are stored on disk under a quota and paged by an opaque `<uuid>.<utf16offset>` cursor (`fetch_store.go`).
- **State/lifecycle.** All state here is per call except `PathLocks` (per resolver), `FetchStore` (shared per storage dir: transport pool + URL cache + on-disk documents) and the package-level spill-prune throttle map.
- **Prior art honoured.** F-11 (spill prune), F-12 (`read` whole-file), F-16 (exec result file), P-020 (ripgrep spawn), P-041 (kill-grace timers), A-7/B-P1 (fetch connection reuse) are fixed and were re-checked; no repeat is filed.

## 2. Hypotheses (drafted before findings)

| id | statement | status |
|---|---|---|
| X1 | A deny rule hides a path from a direct `read` but not from a directory listing | **verified** (probe, finding 01) |
| X2 | `fetch` storage can become permanently unusable at `max_storage_bytes` | **verified by reading** the store; no prune path exists anywhere (finding 02) |
| X3 | `fetch` paging costs O(document) with 2-3x allocation per page | **verified by reading** (finding 03, needs a benchmark) |
| X4 | `FetchStore.Create` scans the whole directory per fetch under the store mutex | **verified by reading** (finding 04) |
| X5 | `grep` post-filter does one `EvalSymlinks`+`stat` per returned file | **verified by reading** (finding 05) |
| X6 | SSRF: a redirect, a decimal/hex IPv4 literal, an IPv4-in-IPv6 address or a DNS rebind can reach a private address | **disproved** (see "checked and sound": address-based validation on every hop + pinned dial; `fetch_test.go:158` covers redirect-to-private) |
| X7 | Symlink swap between check and use (TOCTOU) can escape the workspace on write/edit | **disproved** for the workspace boundary: parent is pinned by fd, target identity compared before rename (`mutate.go:288-326`); a hostile *same-user* process is out of the threat model |
| X8 | `PathScope` narrowing is applied consistently to every native tool | **disproved** (finding 06) |
| X9 | bash environment leaks the server token or the model API key to children | **disproved** (`bash.go:382-422`, `ripgrep.go:654`; `env_builder_parity_test.go` covers it) |
| X10 | `read` can be driven past its byte budgets to a panic (span window vs requested range) | **disproved** by tracing `scanLines`/`renderFileRange` (the window and the loop use the same cumulative byte criterion, so the span index never leaves the window) |
| X11 | ripgrep/glob subprocesses or their three goroutines can be left behind or block on an unread channel | **disproved** by trace (`ripgrep.go:448-552`; all channels buffered or closed once; readers closed on forced finish) |

## 3. Findings

### P7A-R2-01  A `read` of a directory lists entries that the path policy denies
- severity: medium
- lens: B security
- location: `backend-go/internal/tools/native/read.go:350` (filter), `backend-go/internal/tools/native/path.go:92-102` (`pathAllowed`, no deny loop); contrast `backend-go/internal/tools/native/path.go:77-90` (`pathPolicyError`, deny-aware) used by `glob.go:280`, `grep.go:124`, `write.go:95`, `edit.go:93`
- evidence: [measured] `/tmp/r2/deny_list_test.go` run with `-overlay` (command and exit 1 in section 0): workspace with `visible/` and `secret/`, policy `{Paths: [{action: deny, path: <ws>/secret}], UnrestrictedPaths: true}`; `read {"path":"."}` returns `entry_count: 2` and `entry: {"name":"secret","kind":"directory"}`, while `read {"path":"secret/file.txt"}` correctly returns `path_denied`. `renderDirectory` filters with `pathAllowed`, which only tests `UnrestrictedPaths`, workspace containment and `allow` rules — it never consults deny rules nor `policy.Within`. Reachable by: the model (one tool call with the directory as `path`), i.e. any run whose session/profile sets a deny rule to keep a subtree out of the model's reach; the model learns names (and file sizes) of exactly the entries the rule was meant to hide.
- suggested fix: filter the listing with `pathPolicyError(entryPath, target.workspaceRoot, settings.Policy, callID) == ""` (or add the deny/`Within` loop to a listing-specific helper) in `read.go`; add the missing test: a deny rule plus a directory read asserting the denied name is absent, and a `PathScope` variant. Note the same listing keeps working for the *unrestricted* default (no behaviour change without a deny rule).
- effort: S
- prior art: new (the p7a risk column names "path confinement" as the area; the existing deny test `read_test.go:102` only covers a direct file read, not a listing)

### P7A-R2-02  fetch document storage has no eviction, so reaching `max_storage_bytes` disables `fetch` permanently
- severity: medium
- lens: A correctness
- location: `backend-go/internal/tools/native/fetch_store.go:235-248` (quota checks), `fetch_store.go:406-425` (`fetchDirectoryBytes`), `fetch_store.go:216-258` (`Create` — the only writer), `fetch.go:239-246`
- evidence: [read] `Create` rejects a document when `usedBytes+storedBytes > MaxStorageBytes` with `FetchStoreError{"fetch storage is full"}`. Nothing in the package removes a stored document: the only `os.Remove` calls are the rollback of a half-written pair (`fetch_store.go:254`) and the temp file in `atomicFetchWrite` (`fetch_store.go:434`). No TTL, no LRU, no per-run cleanup, and `scratch_sweep.go` sweeps only `exec-`/`tool-smith-` temp dirs (`scratch_sweep.go:15-19`), not `.fetch`. Default `max_storage_bytes` is 64 MiB (`sessionfs/native_fetch_settings.go:33`), i.e. about 32 default-limit-sized pages of 2 MB responses; after that every later `fetch` in that toolset returns `fetch_storage_error` until someone deletes files by hand (or runs `bash rm` if that tool is enabled). `cache_ttl_ms` does not help: it adds an index, not a prune.
- suggested fix: smallest change that keeps the tool usable: when `Create` finds the directory over the quota, delete the oldest `<id>.md`/`<id>.json` pairs (mtime, skipping the document being created) inside `fetch_store.go` under the existing `s.mu`, and add a `fetch_store_test.go` case (`TestFetchStorePrunesOldestWhenFull`); alternatively add `retention_days`/`max_documents` keys, which needs schema+config work. Either way the "full" error must stop being terminal.
- effort: M
- prior art: new (audit_backend F-17 checked fetch as *bounded per call* only; the URL cache's "no eviction" is documented in `DECISIONS.md:844`, the document store's is not)

### P7A-R2-03  Every `fetch` page re-encodes the whole stored document to UTF-16 (two to four times per call)
- severity: low
- lens: C performance
- location: `backend-go/internal/tools/native/fetch_store.go:284` (`utf16Length(markdown)`), `:287` → `sliceFetchCharacters` `:391` (`utf16.Encode([]rune(value))`), `:297` (`utf16Length` again); `fetch_store.go:312,314` on create; `fetch.go:373` (`utf16Length(markdown)`) before storing
- evidence: [read] A cursor page of a stored document costs three full `utf16.Encode` conversions of the whole markdown (validation, slice, next-cursor test) plus one more at store time; each conversion allocates 2 bytes per unit, i.e. ~2x the document, on top of the `os.ReadFile` of the same document (`fetch_store.go:275`). `max_document_chars` defaults to 1,000,000 and may be raised to the tool byte ceiling, so a single page of a large document allocates tens of MB. `BenchmarkNativeFetchHTMLConversion` (`fetch_test.go:686`) measures the HTML conversion, not paging.
- suggested fix: keep the page offset in *rune* or byte units, or convert once and reuse it for the three uses (compute `units := utf16.Encode(...)` once in `Page`, pass it to `sliceFetchCharacters`, and derive the next condition from `offset+consumed < int64(len(units))`); same in `CreatePage`.
- effort: S
- prior art: new (F-17 bounded fetch *per call* and did not look at paging)

### P7A-R2-04  `FetchStore.Create` stats every stored file on every fetch, under the store mutex
- severity: low
- lens: C performance
- location: `backend-go/internal/tools/native/fetch_store.go:217-218` (`s.mu.Lock` … `defer Unlock`) with `:242` `fetchDirectoryBytes(s.directory)`; `fetch_store.go:406-425`
- evidence: [read] Each stored document is two files, so the aggregate-quota check is `os.ReadDir` + one `entry.Info()` (lstat) per file, per fetch, while holding `s.mu` that also serialises `Create` for every session sharing the toolset store. Cost grows linearly with the document count (64 MiB default / ~1.2 MB average ≈ 100 documents at the cap, so tens of µs now, but the same shape as F-13, where 5000 records cost 17 ms and the fix was to remember sizes).
- suggested fix: keep the same trick that fixed F-13: cache per-file sizes in the store (name → size, updated on create/remove, refreshed only for names it has not seen) in `fetch_store.go`.
- effort: M
- prior art: new for fetch; same class as F-13 (fixed 5d09534, memory store)

### P7A-R2-05  `grep` re-resolves every matched file and builds a JSON error it throws away
- severity: low
- lens: C performance
- location: `backend-go/internal/tools/native/grep.go:121-131`
- evidence: [read] For each result group (up to `max_results`, default 200, configurable higher, and one group per matched file) the post-filter calls `resolveExistingTarget`, which does `EvalSymlinks` of the workspace root, `Abs`/`Clean`, `EvalSymlinks` of the file and two `PathWithin` passes per rule — and, for a denied file, `pathPolicyError` renders a complete JSON `ExecutionError` string (`path.go:80,84,89`) that the caller immediately discards. So one grep over 200 matching files costs ~400 extra syscalls plus up to 200 JSON renders, all after the search itself, on top of the ~1.6 ms ripgrep spawn measured in P-020. The existing benchmark (`BenchmarkRunRipgrep`, `grep_budget_test.go:45`) covers `runRipgrep` only, so this cost has never been measured.
- suggested fix: filter groups with the cheap rule check the traversal flags already use (`deniedGrepTraversalFlags`-style `PathWithin` on the reported relative path) and keep one `resolveExistingTarget` only for groups whose resolved form differs, or add a `pathPolicyDenied(target, policy) bool` that returns a bool without rendering.
- effort: S
- prior art: new (P-020 measured the spawn only)

### P7A-R2-06  The per-agent `paths` scope narrows write/edit/transcript but not the read-side tools
- severity: low
- lens: B security
- location: `backend-go/internal/tools/native/path_scope.go:14`; applied at `write.go:94`, `edit.go:92`, `transcript_path.go:78`, `transcripts_list.go:133`; **not** applied at `read.go:267`, `grep.go:109`, `glob.go:214`, `image_file.go:41` (`readImageFile` → `resolveExistingTarget`)
- evidence: [read] `grep -rn scopedPolicy internal | grep -v _test` returns exactly the four call sites above. A child agent whose `paths: {allow: ["src"]}` narrows where it can *write*, but it can still `read`, `grep`, `glob` or `read_image` any path the session's own policy reaches (for example a `.env` or another profile's files under a session-level allow). This is close to `DECISIONS.md:334` ("narrows where the child's write and edit may change files"), so it is a decision-adjacent observation rather than a defect; the inconsistency is that `transcript`/`transcripts` do apply the scope to reads (design_transcript_reader.md §"Path scope" requires the same check "a read of that file would" — which `read` itself does not perform), and finding 01 shows the listing ignoring `Within` as well.
- suggested fix: decide the intent explicitly: either (a) record in `DECISIONS.md` that `paths` is write-only and say so in the sub-agent tool description, or (b) apply `scopedPolicy` in `executeRead`, `executeNativeGrep`, `executeNativeGlob` and `readImageFile` (four one-line changes) and add a test mirroring `path_scope_test.go:74` for `read`. Do not change behaviour without that decision.
- effort: S
- prior art: new (decision challenge, not a repeat)

### P7A-R2-07  Dead code: two `bashStreamCollector` render methods and a test-only fetch helper
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/bash_output.go:154-156` (`(*bashStreamCollector).render`), `bash_output.go:190-192` (`(*bashStreamCollector).renderWithProducedBytes`), `backend-go/internal/tools/native/fetch.go:611-614` (`readFetchResponse`)
- evidence: [read] `grep -rn --include=*.go 'bashStreamCollector) render'` and `'bashStreamCollector) renderWithProducedBytes'` return only the definitions; the `render`/`renderWithProducedBytes` calls in `bash_child.go:84,279,309`, `process_manager.go:462,729` and the tests all go to `(*bashOutputCollector)`, which freezes first. `readFetchResponse` has no production caller (only `fetch_test.go:267` and `fetch_transport_test.go:51,233`). Note `(*bashStreamCollector).render` also bypasses the new `mu` when it reads `producedBytes` (`bash_output.go:155`), so it is a trap for a future caller.
- suggested fix: delete the three functions and switch those tests to `collector.freeze().render…` (or `readFetchResponseTruncating`); `go vet` does not report unused methods, so this needs the manual grep.
- effort: S
- prior art: new

### P7A-R2-08  The spill-prune throttle map is never trimmed
- severity: low
- lens: A correctness
- location: `backend-go/internal/tools/native/bash_spill.go:267-290`
- evidence: [read] `bashSpillPrunes.last` is a package-level `map[string]time.Time` keyed by `directory + "\x00" + pattern.String()`; entries are added for every new spill directory/pattern pair and never removed. The key contains the spill path and the pattern (which embed the workspace root and the session directory through `pruneTemplate`), so a long-lived server that runs sessions in many workspaces keeps one entry per workspace for the life of the process. Each entry is ~100 bytes, so this is a slow, unbounded growth (same class as B-C1/A-8 "unbounded map", not the same site).
- suggested fix: drop entries older than the interval when the map is touched, or cap the map and scan when it is full, in `bash_spill.go`.
- effort: S
- prior art: new (F-11 added this map; its growth was not part of F-11)

### P7A-R2-09  `fetch` may connect to any port on a public host
- severity: low
- lens: B security
- location: `backend-go/internal/tools/native/fetch.go:388-444` (no port check), `fetch.go:537-545` (port used as given), `fetch_transport.go:117`
- evidence: [read] `validateFetchURL` validates scheme, host and every resolved address, but accepts `http://<public-host>:22/`, `:25`, `:6379`, … and dials that port; the result distinguishes `request_failed` (connection refused/timeout) from `http_error`/success, so the model gets a reachability oracle for arbitrary ports on *public* addresses. Private and loopback addresses stay blocked (`isPublicFetchAddress`), so this cannot reach the host's own services; the reachable impact is probing third parties from the server's egress IP.
- suggested fix: restrict to 80/443 unless the settings add an explicit list (small change in `validateFetchURL`: default allow `{80, 443}` when the scheme matches), and add a case to `fetch_test.go`.
- effort: S
- prior art: new (batch-9 work and `DECISIONS.md:1005` covered private-address SSRF, not ports)

### P7A-R2-10  `read_image` from the image store reads the whole file before enforcing its own limit
- severity: low
- lens: C performance
- location: `backend-go/internal/tools/native/image_file.go:352-361`
- evidence: [read] The stored-reference branch calls `store.Open(name)` (which allocates and reads the entire file, `imagestore/store.go:526-529`) and only afterwards compares `len(data) > maxBytes` and reports `image_too_large`. The file-path branch deliberately reads `maxBytes+1` bytes (`image_file.go:380`, `readWithin`), so the two branches have different memory behaviour; the exposure is capped by the store's own `MaxFileBytes` (`imagestore/store.go:523`) and by the image quota, hence low.
- suggested fix: pass the limit into the store (an `OpenLimited(name, max)` or a `Stat`+`LimitReader` variant) or check the size before reading in `image_file.go`.
- effort: S
- prior art: new

## 4. Performance hypotheses for the measurement step (5.3)

- H01: `fetch` page slicing converts the whole stored document to UTF-16 three times per cursor page (`fetch_store.go:284,287,297`).
  workload: 1 MB and 8 MB stored documents (`max_document_chars` up to the tool byte ceiling), 1 page 1 + 2 cursor pages.
  benchmark: new `BenchmarkFetchStorePage` (fixture: `FetchStore` over `b.TempDir()` with a synthetic `<uuid>.md` of N bytes; call `Page(cursor, 12000)`), next to the existing `BenchmarkNativeFetchHTMLConversion`.
  expected: scales with document size, ~2-3x document bytes allocated per page; a prototype that encodes once should be ~3x less.
- H02: `FetchStore.Create` lists and stats the whole fetch directory under the store mutex per fetch (`fetch_store.go:217-247`).
  workload: 100 and 1000 stored documents (200 and 2000 files), 1 create.
  benchmark: new `BenchmarkFetchStoreCreateWithExistingDocuments` (seed the dir with N pairs, one `Create`).
  expected: linear in file count; ~0.5-2 ms at 2000 files, all under the shared mutex.
- H03: `grep` post-filter costs one `EvalSymlinks`+`stat` per matched file plus a discarded JSON render per denied file (`grep.go:121-131`).
  workload: 200 and 1000 matching files (`max_results` at default and raised).
  benchmark: new `BenchmarkGrepPostFilterManyFiles`, or extend `BenchmarkRunRipgrep` with an `executeNativeGrep` variant over a 1000-file corpus.
  expected: adds a per-result-file constant (~2 syscalls) after the bounded search; small against the 1.6 ms spawn, relevant only at a raised `max_results`.
- H04: `write`/`edit` read the entire target file to decide "unchanged" (`mutate.go:266-285` `hasSameContent`, reached from `write.go:183` and `edit.go:203`).
  workload: 128 KiB and 4 MiB files, repeated identical write.
  benchmark: `BenchmarkNativeEditWithBackupOn128KiBFile` (`mutation_test.go:390`) as the base; add a `BenchmarkWriteUnchangedLargeFile`.
  expected: one full read + compare per no-op write, i.e. ~2x the bytes of the write itself; a size+mtime pre-check would remove it at the cost of a weaker guarantee.
- H05: `bash` spill planning still lists the spill directory once per call when the throttle interval elapses (`bash_spill.go:272-312`, `retention_days` default 14, `prune_interval_seconds` default 3600).
  workload: 3000 files (the F-11 fixture).
  benchmark: `BenchmarkPlanBashSpillLargeDirectory` (`bash_spill_test.go:88`), at `prune_interval_seconds` 3600 and 0.
  expected: F-11 measured 21.6 µs at the interval and 16.7 ms at 0; the point of re-running is to confirm the throttle still short-circuits after the 3e33c97 changes to `bash.go`/`bash_output.go`.

## 5. Cross-phase leads (not this sub-pass)

- `backend-go/internal/sessionfs/native_fetch_settings.go:39` `nativeFetchHostPattern` has no `:` alternative, so `allowed_hosts`/`denied_hosts` cannot name an IPv6 literal; the address guard still blocks private v6, so this is a configurability gap in phase 2a's area.
- `backend-go/internal/tools/native/async_latest.go:147-150` (phase 6b) reads `bashStreamCollector.since`, which returns only the tail window when the stream was truncated (`bash_output.go:450-464`) and reports the head bytes as "lost" even when they are newer than the cursor. Worth a look in 6b's reading of the "latest" block; no correctness claim from me.
- `backend-go/internal/tools/native/process_manager.go:462,729,774` uses the same collectors; if finding 01's fix changes `pathAllowed`, nothing there is affected, but the 6b reviewer should know the listing filter is the only deny-unaware caller of `pathAllowed`.
- `backend-go/internal/imagestore/store.go:499-535` validates stored-image names and identity (good); `read_image`'s trust in it (finding 10) is phase 8's contract.

## 6. Checked and sound (one line each, no finding)

- SSRF guard (`fetch.go:388-526`, `fetch_transport.go:104-126`): scheme/host/credential checks, deny-then-allow host policy with trailing-dot normalisation (`containsHost`), *all* resolved addresses must be public, IPv4-mapped/NAT64/6to4/site-local/documentation prefixes unwrapped or rejected, no proxy, redirects revalidated per hop with the pin rebuilt, dial pinned to the validated IP with TLS checked against the URL hostname, decompressed body limited by `io.LimitReader`. `fetch_test.go:158` covers redirect-to-private and `fetch_transport_test.go` covers the pin.
- `read` file path (`read.go:262-331`, `path.go:126-154`): symlink resolution plus policy on both forms, size pre-check, identity check before and after the read, bounded buffer, byte and line budgets with correct span indexing.
- Mutations (`mutate_path.go`, `mutate.go`): symlinked component rejected, parent directory pinned through `/proc/self/fd`, `os.SameFile` identity before backup and before rename, `O_EXCL` temp file + rename, mode preserved, backup pruned to count/bytes, directory fsync best-effort and commented.
- `edit` byte-budget arithmetic (`edit.go:163-180`) cannot overflow (guarded multiply), and `replaceAll` counts occurrences before allocating.
- `PathLocks`: refcounted entry removal under the map mutex, idempotent release via `sync.Once`, nil receiver tolerated (`path_locks.go:289-318`).
- `scopedPolicy`/`narrowScope`/`scopeRoot` widen nothing: rules are added, `Allow` is intersected, an empty intersection denies everything (`path_scope.go:59-63`, `narrowScope:226-267`; covered by `path_scope_narrow_test.go`).
- bash child lifecycle (`bash_child.go:66-283`): own pipes so a descendant cannot block `Wait`, stdin written by a goroutine that the deferred close releases, timeout → SIGTERM → grace → SIGKILL → left_running, drain deadline, every timer stopped, `readers` closed before the collector is rendered; no goroutine outlives the call (P-041's timer fix present at `bash_child.go:215,270`).
- ripgrep (`ripgrep.go:380-553`): buffered/closed-once channels, stderr bounded to 8 KB and drained, match/line/byte budgets stop the search by killing the group, exit-code classification ignores a timeout, ctx nil-safe, env scrubbed of `JAD_API_TOKEN`.
- glob (`glob.go:203-352`): `--no-config`, `--null`, sequential parsing, trailing partial entry dropped when truncated, every returned path re-checked against the policy (deny-aware), hidden/`.git` handling documented in the hints.
- bash environment (`bash.go:382-422`): allowlist, `EnvDeny`, `JAD_API_TOKEN` and `apiKeyEnv` always denied, sorted output; `prependEnvironmentList` appends so the last value wins.
- spill (`bash_spill.go`): file mode 0600, dir 0755, `CapBytes` enforced and reported, escaped/len-limited filenames, prune restricted to regular files matching the template and older than retention.
- image store references (`image_file.go:352-360` → `imagestore.ValidName`/`Open`): names are `<64 hex>.<ext>`, Lstat+open identity checked, content type verified, so `stored-image://…` cannot escape the store.
- `read_image` output: `imagedata.Sniff` rejects anything not png/jpeg/webp/gif before a data URL is built.
- gofmt/vet/build are clean for the package (section 0).

## 7. Not covered and why

- No benchmark, profile or `-race` run (the plan reserves those for the measurement step); every performance item above is `[read]` and needs H01-H05 to become `measured`.
- `bash`'s actual shell behaviour with a hostile command line (resource limits, cgroups, rlimits) is out of scope: the tool is documented as an unconfined runner (`DECISIONS.md`, `design_bash.md`).
- Windows-only code paths (`bash_executable_windows.go`, `bash_exit_windows.go`) were read for divergence only; not executed and not reviewed for correctness on that platform.
- `html-to-markdown-rs_cgo_link.go` is a generated machine-local shim (out of scope per plan §1); only confirmed that it is `//go:build`-free, `cgo`-guarded and excluded from `./internal/...` builds by its `import "C"` usage — the fetch HTML path was reviewed through the library call site only, not the library.
- `fetch_store.go` concurrency against a *second* process sharing the same storage dir (two servers, or a user deleting files) was not probed; `Create` serialises only in-process.
- Test quality was sampled (read/grep/glob/fetch/bash/path_scope suites); the `fetch_test.go` (720 lines) and `mutation_test.go` (416 lines) failure paths were skimmed, not audited line by line.
- `catalog.go` wiring (which settings each tool is built with) belongs to 7b/6b; assumed correct as given.
