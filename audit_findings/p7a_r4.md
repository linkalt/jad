# Phase p7a — Reviewer r4
- HEAD: f425dee
- Date: 2026-10-30
- Files read: 27 non-test files in `audit_plan.md` §3.2 p7a map, including the generated converter link shim on the requested file list.
- Commands run: listed in §9, with exit codes.

## 1. Understanding note
The native file/search tools are registered through the native tool catalog and consume per-session settings and execution context. Reads, writes, edits, image reads, glob and grep route path inputs through shared native path resolution/policy helpers; file mutation also uses backup/atomic-write helpers and path locks. Bash has a shared shell child lifecycle/output collector with spill management; grep/glob invoke ripgrep with bounded lifecycle. Fetch validates and pins resolved public addresses, uses transport controls, persists document pages/cursors in a fetch store, and converts HTML through the linked converter. Main review hypotheses are path-root/symlink and TOCTOU safety, resource caps and cancellation, and URL/redirect/DNS-pinning correctness.

## 2. Hypotheses
- H01: Existing-target and creation-target resolution consistently enforce policy under symlinks and rooted workspaces — in review.
- H02: Mutating tools do not introduce a path race between authorization and filesystem effects — in review.
- H03: Fetch validation and transport keep redirects, DNS rebinding, proxy and special IP ranges inside the SSRF policy — in review.
- H04: Bash/ripgrep/glob child processes, timeouts and outputs are bounded and cleaned up — in review.
- H05: Read/image/fetch storage paths and cursor/output handling impose safe size limits — in review.
- H06: Shared helpers, settings and error handling are maintainable and consistent with prior fixes — in review.

## 3. Findings
### P7A-R4-01 Unlisted IANA special-purpose addresses pass fetch's public-address filter
- severity: medium
- lens: B security
- location: backend-go/internal/tools/native/fetch.go:469-526 (URL validation at :389-425; pinned connection at :537-574 and fetch_transport.go:104-116)
- evidence: `validateFetchURL` accepts an IP literal without DNS, rejects it only when `isPublicFetchAddress` returns false, and the filter allows special-purpose blocks that are not listed in its exclusions: IPv4 `198.51.100.1`, `203.0.113.1`, `192.88.99.1`, and IPv6 `2001:2::1`, `2001::1`, `2001:10::1`, `2001:20::1` each logged `public=true` in a temporary overlay test against the actual package function (command and exit code below). TEST-NET and IPv6 protocol/benchmarking/documentation ranges are not ordinary globally routable service addresses; the pinned requester dials the supplied address directly after this check. Impact requires such a non-global range to be routed/reused on the deployment network; no live network request was made. [measured + read]
- suggested fix: reject the relevant IANA IPv4 and IPv6 special-purpose ranges that are not globally reachable (including TEST-NET and IPv6 protocol-assignment/benchmarking/documentation ranges); drive the range policy from a reviewed table and add cases alongside the existing NAT64/6to4 tests.
- effort: S
- prior art: new; distinct from the batch-9 fix for site-local IPv6, NAT64, local-use NAT64 and 6to4 private embedding (audit_audit.md, fixed in a1e8f1c)

### P7A-R4-02 Stored-image read_image references allocate before applying the tool's limit
- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/image_file.go:24-30; backend-go/internal/imagestore/store.go:499-535; backend-go/internal/sessionfs/native_read_image_settings.go:8-16; backend-go/internal/sessionfs/image_store_settings.go:13-18
- evidence: For `stored-image://` references, `readImageFile` calls `store.Open` and only checks `len(data) > maxBytes` after it returns. `Store.Open` allocates `make([]byte, info.Size())` and reads the whole object, bounded by the image-store `MaxFileBytes` setting (default 20 MiB, maximum 100 MiB), whereas read_image defaults to 5 MiB (maximum 10 MiB). Thus a stored object larger than the read_image limit is fully allocated and read before being rejected; ordinary filesystem paths use `readWithin(limit+1)` instead. A model can reach the reference path when a stored image URI is in its context. [read]
- suggested fix: add a size-limited image-store open (or stat/validate the file safely before reading) and reject against `maxBytes` before allocating; keep the existing opened-file identity check to avoid a symlink race.
- effort: S
- prior art: new

## 4. Performance hypotheses
- H01: `renderDirectory` calls `os.ReadDir` and retains/filter-scans all directory entries before applying the requested entry and output-byte limits (`read.go:343-370`). A directory with hundreds of thousands of entries makes a small `read` listing allocate and process in proportion to the entire directory. Workload: 100k+ entries in one directory. Benchmark: extend/add a `BenchmarkNativeReadDirectoryLimit` fixture with 100, 10k and 100k entries, comparing `limit: 10`; expected: current cost grows with total entries despite fixed output cap. [unmeasured]

## 5. Cross-phase leads
- None confirmed. Shared settings and `imagestore.Open` were read only to validate the in-scope `read_image` path; no independent audit claims about those modules.

## 6. Checked and sound
- Path policy is checked both before and after resolving existing targets; mutation rechecks symlink/path policy and uses pinned parent directory handles plus SameFile identity checks at commit (`path.go:23-75`, `mutate_path.go:84-209`, `mutate.go:68-112,258-333`). Existing tests cover ordinary escaping symlinks and agent path scopes (`read_test.go:171-192`, `mutation_test.go:232-268`, `path_scope_test.go:85-124`); no race reproduction was attempted.
- Path mutations are serialized by shared per-catalog `PathLocks`; write/edit constructors allocate a lock when none is passed (`write.go:31-38`, `edit.go:32-39`; `path_locks.go:22-50`). Atomic replacement preserves backup/error behavior (`mutate.go:287-376`).
- `readVerifiedRegularFile` validates opened-file identity and size before/after reading, sizes buffers from stat and limits growth; `read` rejects nonregular files and enforces both file and content budgets (`path.go:126-154`, `read.go:262-331`).
- Shell subprocesses use configured environment allowlists/denials, remove the server token, and resolve the executable against the filtered PATH (`bash.go:382-422`, `bash_executable_unix.go:22-59`). Child process timeout/context cancellation, TERM-to-KILL escalation, output drain deadlines, spill caps and cleanup are explicit (`bash_child.go:66-283`; `ripgrep.go:79-252`). Prior P-041 stale kill timers were corrected in `77b7213`; current unix kill timer checks group liveness (`process_group_unix.go:30-44`).
- `grep` and `glob` place untrusted patterns/paths after `--`, bound results/bytes/per-file size, filter denied paths, and check returned paths; ripgrep output and stderr are streamed/bounded (`grep.go:102-221`, `glob.go:203-352`, `ripgrep.go:79-161,254-351`). Glob brace expansion caps alternatives at 64 (`glob.go:24,58-86`).
- Fetch rejects non-http(s), credentials, denied hosts, all non-public resolved addresses, validates every redirect hop, disables automatic redirects/proxy, pins dial IP, and keys pooled transports by scheme/host/port/address (`fetch.go:287-444,469-526,537-587`; `fetch_transport.go:104-126`). The prior batch-9 IPv6 special-use gaps are fixed (`audit_audit.md`, `a1e8f1c`); the new IPv4 special-use gap is P7A-R4-01.
- Fetch storage caps writes at configured `max_storage_bytes` and cursor ids use a fixed pattern plus bounded integer offset (`fetch_store.go:90-175,246-256`). Storage eviction is not present; given the explicit cap and retained cursor files, I treat full-capacity failure as configured behavior rather than a defect.
- A candidate unbounded `bashSpillPrunes.last` map is process-global (`bash_spill.go:267-270`) and gets a key per directory/pattern when throttling is enabled (`:272-290`), but session settings resolve storage directories and the directory/pattern is shared for calls using one storage location. I found no reachable per-call unbounded key source in scoped code and did not file it.

## 7. File-by-file coverage
All 27 p7a non-test files from `audit_plan.md` §3.2 were read via indexed symbols or exact-range fallback. One plan-excluded generated converter link shim was inspected as requested.
- `bash.go` — shell args/env/cwd validation and settings flow checked; shared `runShellChildDetailed`; no finding.
- `bash_child.go` — pipe/goroutine lifecycle, cancellation, kill/drain timers, spill close checked; no finding.
- `bash_executable_unix.go` — filtered-PATH resolution and argv[0] behavior checked; no finding.
- `bash_executable_windows.go` — Windows PATH/PATHEXT lookup and argv[0] helper exact-range fallback; no finding; Windows not executed.
- `bash_exit_unix.go` — Unix wait status/signal mapping checked; no finding.
- `bash_exit_windows.go` — Windows exit code mapping exact-range fallback; no finding; Windows not executed.
- `bash_output.go` — bounded head/tail output collection, UTF-8/ANSI handling checked; no finding.
- `bash_spill.go` — bounded spill writer, capped prune throttle, regex/file-type checks and retention removal checked; no finding.
- `edit.go` — edit arg validation, path lock, identity-verified read, output-size and atomic commit path checked; no finding.
- `edit_text.go` — line-ending detection, replacement, diagnostic truncation helpers checked; no finding.
- `fetch.go` — URL validation/redirect response handling, content/body limits, HTML conversion and pinned requester checked; P7A-R4-01.
- `fetch_store.go` — cursor validation, per-document metadata/body writes, storage limit and URL cache checked; no finding.
- `fetch_transport.go` — validated-destination pool key, host LRU and transport cleanup checked; no finding.
- `file_change.go` — optional callback invocation checked; no finding.
- `glob.go` — bounded brace expansion, NUL-delimited ripgrep result parsing, result/path filters checked; no finding.
- `grep.go` — argument/flag construction, denied traversal globs, post-filtered matches checked; no finding.
- `html-to-markdown-rs_cgo_link.go` — generated, machine-local cgo shim inspected by exact lines; excluded from phase scope and not built.
- `image_file.go` — file reads are limit+1 bounded; stored-reference branch reads before enforcing read_image cap (P7A-R4-02).
- `mutate.go` — pinned directory handles, backup source/directory, target identity and atomic write helpers checked; no finding.
- `mutate_path.go` — parent/target symlink resolution and policy checks checked; no finding.
- `path.go` — existing target policy, redacted paths, verified regular-file read checked; no finding.
- `path_locks.go` — ref-counted same-key lock acquisition/release checked; no finding.
- `path_scope.go` — nested allow/deny scope rooted/canonicalized and intersects policy checked; no finding.
- `read.go` — read schema/limits, directory listing and bounded file range code checked; P7A-R4-H01 performance hypothesis only.
- `read_image.go` — read_image tool path, image sniffing and result conversion checked; no finding beyond shared helper P7A-R4-02.
- `ripgrep.go` — stdin-independent subprocess, JSON event parsing, result and stderr budgets, process group lifecycle checked; no finding.
- `write.go` — content decoding/limit, path locking and atomic commit checked; no finding.

## 8. Not covered
- No Windows execution; platform-specific files were read only.
- No network request, live DNS test, SSRF exploit, filesystem race stress, benchmark or full suite. Path race conclusions are source-based only.
- No build/vet/full `gofmt` checks. The temporary overlay test only exercises the IPv4 filter's acceptance outcome; it did not contact those addresses.

## 9. Commands run
- `python3 ... audit_plan.md phasemap ...` — exit 0; enumerated 27 non-test source paths.
- `git rev-parse --short HEAD` — exit 0; `f425dee`.
- `git log -8 --format='%h %s' -- <p7a source paths>` — exit 0; reviewed prior fixes including batch 9 `a1e8f1c`, spill throttle `154d0de`, and keepalive pool `fa50932`.
- `git show 49442de^:backend-go/internal/tools/native/fetch.go ...` — exit 0; confirmed fetch IPv4 filtering originates in initial native port; `git log -S ...` exit 0.
- `go test -overlay /tmp/r4/overlay.json ./internal/tools/native -run '^TestAuditR4IPv6SpecialUse$' -count=1 -v` (from `backend-go/`) — exit 0. Output showed `198.51.100.1`, `203.0.113.1`, `192.88.99.1`, `2001:2::1`, `2001::1`, `2001:10::1`, and `2001:20::1` accepted by filter; test source and overlay only in `/tmp/r4/`.
- `go test ./internal/tools/native -run '^(TestFetchURLPolicyAndPublicAddressRanges|TestReadImageOpensAStoredReference|TestReadImageHonoursTheSizeLimit|TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit)$' -count=1` — exit 0 (`ok jad/backend-go/internal/tools/native 0.010s`).
- `git diff --check -- audit_findings/p7a_r4.md` — exit 0 (report whitespace check).
- No build, vet, race or benchmark commands run.
