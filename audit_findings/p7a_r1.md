# Phase p7a review, reviewer r1

Phase: p7a, native file and search tools (`backend-go/internal/tools/native`).
HEAD: `f425dee`. Date: 2026-10-30. Read-only review; no source, config or git state changed.

## Header

- Files (27, from the p7a phase map): bash.go, bash_child.go, bash_executable_unix.go, bash_executable_windows.go, bash_exit_unix.go, bash_exit_windows.go, bash_output.go, bash_spill.go, path.go, path_locks.go, path_scope.go, read.go, read_image.go, write.go, edit.go, edit_text.go, mutate.go, mutate_path.go, file_change.go, image_file.go, glob.go, grep.go, ripgrep.go, fetch.go, fetch_store.go, fetch_transport.go, html-to-markdown-rs_cgo_link.go. Coverage depth is in section 7.
- Shared helpers read: `sessionfs.PathWithin`, `imagestore.Store.Open`, `tools.ExecutionError`, `bash_output` collector (index only), `sessionfs` path and fetch settings (`native_fetch_settings.go`, `file_settings.go`), `app/resolver.go` fetch and mutation store wiring, `tools/catalog.go` `PathScope`, `subagent_call.go` scope wiring.
- Commands and exit codes: section 8.

## 1. Understanding note

- Path confinement: every native file tool resolves its argument through `resolveExistingTarget` (read, grep, glob, bash cwd, image) or `resolveMutatePath` plus `inspectMutateTarget` and `prepareMutateParent` (write, edit). Both canonicalise the workspace root with `EvalSymlinks`, check `pathPolicyError` (deny rules, `Within`, then the workspace or allow rules), and for mutations pin the parent directory by handle (`pinDirectory`, `/proc/self/fd` on Linux) and compare file identity (`os.SameFile`) before the atomic rename (`commitFile`, `writeAtomically`).
- Per-call scope: `scopedPolicy` (path_scope.go:14) applies `execution.PathScope` to edit, write and transcript tools. Child agents get `narrowScope` from subagent_call.go:48-52.
- SSRF: `validateFetchURL` (fetch.go:388) checks scheme, credentials, host policy, resolves every address, rejects any non-public address (`isPublicFetchAddress`, fetch.go:469), and picks one. `pinnedFetchRequester.Do` (fetch.go:537) and `newPinnedFetchTransport` (fetch_transport.go:104) dial only that address, keep proxies off, and keep automatic redirects off. `retrieveFetchDocument` (fetch.go:287) re-runs validation on every redirect hop, up to `MaxRedirects`.
- Child processes: bash, exec and hooks use `buildNativeBashEnvironment` (bash.go:382, allowlist plus inline env minus deny). ripgrep and glob use `environmentWithoutServerToken` (ripgrep.go:353).
- Mutable state: `PathLocks` (path_locks.go) serialises mutations per lexical path. The fetch store (fetch_store.go) has one mutex in `Create`, a process-wide URL cache, and a directory with no eviction.

## 2. Hypotheses

| id | statement | status |
|---|---|---|
| H-1 | A model-issued directory `read` lists entries that a session `paths` deny rule hides | verified (probe, F-01) |
| H-2 | Fetch storage has no eviction, so once full every fetch fails | verified (probe, F-02) |
| H-3 | Edit commit checks file identity but not content, so an in-place external write between read and commit is overwritten | verified by reading; not reproduced (F-03) |
| H-4 | ripgrep and glob children get the full server environment minus `JAD_API_TOKEN`, while bash gets an allowlist | verified by reading (F-04) |
| H-5 | Argument injection through model-supplied `include`/`exclude` to ripgrep | disproved (argv probe; section 6) |
| H-6 | Symlink or TOCTOU escape from write/edit parent or target | disproved by reading and existing tests (section 6) |
| H-7 | SSRF via redirect, DNS rebinding, or dial to a different address than validated | disproved by reading and existing tests (section 6) |
| H-8 | Fetch host policy bypass via trailing-dot host | disproved, fixed in `49442de` (section 6) |
| H-9 | Spill file path escape via a model-controlled call id | disproved (`encodeBashSpillCallID` percent-encodes; section 6) |
| H-10 | `PathLocks` keyed on the lexical path lets a symlink alias race an edit | disproved as a corruption path (section 6) |
| H-11 | ripgrep or glob flag injection through the `--glob` value position | disproved (argv probe; section 6) |
| H-12 | Child agents' `paths` not applied to read-only tools | decision, not a defect (section 6) |

## 3. Findings

### P7A-R1-01  Directory `read` lists entries that a deny rule hides
- severity: medium
- lens: B security (also D test quality)
- location: backend-go/internal/tools/native/read.go:350 (`pathAllowed` ignores deny rules); backend-go/internal/tools/native/path.go:92-102 (`pathAllowed`); backend-go/internal/tools/native/path.go:77-90 (`pathPolicyError`, which does check deny); backend-go/internal/tools/native/read_test.go:90-111 (the existing test passes for the wrong reason)
- evidence: Probe `TestProbeDirectoryListingLeaksDeniedEntry` (overlay in /tmp/r1/probe, tree untouched). A workspace holds `a.txt`, `b.txt`, `denied.txt`. The policy denies `denied.txt`. `read` with `{"path":"."}` and `DefaultLimit: 100` returned `entry_count: 3` and listed `{"name":"denied.txt","kind":"file","size_bytes":1}`. Opening the file is blocked (`resolveExistingTarget` applies `pathPolicyError`), so the leak is the entry name and kind. Command: `go test -overlay /tmp/r1/probe/overlay.json ./internal/tools/native/ -run TestProbeDirectoryListingLeaksDeniedEntry -count=1 -v`, exit 1 (the probe reports `PROBE CONFIRMED`). [measured]
- Who can reach it: the model, through a session whose `paths` deny rule names a file. The file name is disclosed, not its content.
- Why the existing test misses it: `TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit` sets `DefaultLimit: 1`. The denied entry sorts after `allowed.txt`, so the limit cuts it off before the leak. Its assertion passes for the wrong reason. Control run: `go test ./internal/tools/native/ -run TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit -count=1`, exit 0.
- suggested fix: in `renderDirectory`, replace the `pathAllowed` check with `pathPolicyError(entryPath, target.workspaceRoot, settings.Policy, callID) == ""`, so deny rules and `Within` both apply. Add a test whose limit exceeds the number of entries and asserts the denied name is absent. Files: read.go, read_test.go.
- effort: S
- prior art: new. Related class to P2B2-06 (lexical deny matching, `sessionfs` session listing, a different file); not a repeat.

### P7A-R1-02  Fetch storage fills permanently and never frees space
- severity: medium
- lens: A correctness (bounds) and D (error text gives no recovery path)
- location: backend-go/internal/tools/native/fetch_store.go:113-120 (capacity check in `Create`); fetch_store.go:280-296 (`fetchDirectoryBytes`, a full directory scan per store); fetch_store.go:128 and :308 (the only `os.Remove` calls: failed write and temp file); backend-go/internal/app/resolver.go:812-823 (one store per `storage_dir`, shared by sessions); backend-go/internal/sessionfs/native_fetch_settings.go:27 (default `max_storage_bytes` 64 MiB)
- evidence: Probe `TestProbeFetchStoreFillsPermanently` with `MaxStorageBytes: 2000` and a 900-byte document. The first `Create` succeeded. Each of the next three returned `fetch storage is full`, and nothing in the store removes a stored document. Exit 0, PASS (the probe asserts the full-store behaviour). Command: `go test -overlay /tmp/r1/storeprobe/overlay.json ./internal/tools/native/ -run TestProbeFetchStoreFillsPermanently -count=1 -v`. [measured] The in-memory URL cache expires (`cache_ttl_ms`), but the files on disk do not. A first attempt used a 16-byte cap, which the metadata JSON alone exceeds; that run failed for a probe reason, not a code reason, and is not counted.
- Who can reach it: the model, by fetching many distinct documents in one storage directory until the cap is reached. Every later fetch from that directory fails until a person deletes files by hand. The error text does not say so.
- Note: DECISIONS.md:103 records the capacity check as a deliberate, race-free design, so the cap is intended. The defect is the absence of any reclaim (no eviction, no TTL on files, no clear) and an error with no recovery hint.
- suggested fix: evict the oldest documents by `FetchedAt` when `Create` would exceed the cap, or delete files past `cache_ttl_ms`. Add a hint to the error ("remove old entries under storage_dir or raise max_storage_bytes"). Files: fetch_store.go and a new test in fetch_test.go (no fetch_store_test.go exists). Decision needed from the user: eviction or a documented manual reset.
- effort: M
- prior art: new. Not in audit_backend.md or audit_performance_plan.md.

### P7A-R1-03  Edit can overwrite an in-place external change made between its read and its commit
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/edit.go:124-125 (verified read), edit.go:212 (`commitFile`); backend-go/internal/tools/native/mutate.go:258-264 (`targetStillMatches`, identity only); backend-go/internal/tools/native/mutate.go:287-333 (`commitFile`, two identity checks, no content check)
- evidence: [read]. The edit reads the content, computes the replacement, and commits by atomic rename. `targetStillMatches` compares `os.SameFile` only. A write from another process that changes the file in place (same inode, no rename) inside the read-to-rename window is replaced without detection. jad's own writes go through rename, so they change the inode and are caught. Not reproduced. The window is milliseconds, and the actor is a local process writing the same file.
- suggested fix: before the rename, re-read the target and compare it with the bytes the edit was computed from (as `hasSameContent` does for writes), and return `changed_during_edit` on mismatch. Files: mutate.go, edit.go. Or document the single-writer assumption.
- effort: S
- prior art: new.

### P7A-R1-04  ripgrep and glob children inherit the full server environment; bash does not
- severity: low
- lens: B security (secrets reach a child process)
- location: backend-go/internal/tools/native/ripgrep.go:101 (`command.Env = environmentWithoutServerToken()`); ripgrep.go:353-362 (strips only `JAD_API_TOKEN=`); backend-go/internal/tools/native/glob.go:236 (same); contrast backend-go/internal/tools/native/bash.go:382-422 (allowlist, plus `apiKeyEnv` and `env_deny`) and process_manager.go:878-880 (deletes `apiKeyEnv`)
- evidence: [read]. ripgrep and glob receive every variable except `JAD_API_TOKEN`, including the provider key variable (`apiKeyEnv`) and `env_deny` entries. `runRipgrep` takes no `apiKeyEnv` (grep.go:116), so the provider key is not removed. Exposure is conditional: the stock `rg` binary does not print its environment, but `tools.native.grep.binary` and `tools.native.glob.binary` accept any executable. audit_design.md:74 (root file) recommended "children get the allowlist unless a setting says otherwise", and this tree does not follow it for ripgrep and glob. Current status of that recommendation: not verified.
- suggested fix: build the environment for ripgrep and glob with `buildNativeBashEnvironment(os.Environ(), settings-with-policy, apiKeyEnv)` or a shared helper, and pass `apiKeyEnv` into `runRipgrep` and `executeNativeGlob`. Files: ripgrep.go, grep.go, glob.go, bash.go (helper). A test that asserts the provider key is absent from the child environment.
- effort: S
- prior art: audit_design.md:74 (recommendation, not an ID; status unverified).

## 4. Performance hypotheses (for the measurement step, section 5.3)

- H01: `read` on a large text file reads the whole file before it slices lines, so a 50-line range costs the file size. file: backend-go/internal/tools/native/path.go:126-154 (`readVerifiedRegularFile`, reads up to `MaxFileBytes`) called from read.go:262-331 (`executeRead`). workload: 1 MB, 16 MB, 64 MB text files, `max_file_bytes` at its 64 MB default. benchmark: existing `BenchmarkNativeReadLargeFile` (read_benchmark_test.go:16-49), with a 50-line range. expected: time and bytes scale with file size, not with the requested range. Prior audit: F-12 cut the span memory (`ffb559c`), but the whole-file read remains, so this is a residual, not a repeat. [unmeasured] by this review; the prior F-12 numbers (64 MB: 110 ms / 67 MB after the fix) are a reference, not a measurement here.
- H02: every fetch that stores a document scans the whole fetch directory (ReadDir plus `Info` per file) under the store mutex. file: backend-go/internal/tools/native/fetch_store.go:90-132 (`Create`, mutex held), fetch_store.go:280-296 (`fetchDirectoryBytes`). workload: N stored documents in one `storage_dir` (shared across sessions, resolver.go:812); the cost scales with N per fetch. benchmark: new `BenchmarkFetchStoreCreate` in `/tmp`, fixture: 100, 1000 and 5000 pre-written documents of about 4 KB, one `Create` per run. expected: linear in N per call, inside the lock, so concurrent fetches queue behind it. [unmeasured]

## 5. Cross-phase leads

- backend-go/internal/app/resolver.go:812-823: the fetch store is keyed by `storage_dir`, so every session that shares one directory shares one capacity. Relevant to P7A-R1-02; belongs to phase 2b/app.
- backend-go/internal/sessionfs/file_settings.go:160-213: deny rules are normalised lexically (`resolveNativeRulePath`). Possibly the same class as P2B2-06 (see 6). Outside 7a; not examined.

## 6. Checked and sound

- H-5 argument injection, ripgrep: `grepSearchFlags` puts model `include` and `exclude` after `--glob`. A probe with `rg --json --no-config --glob '--version' -- needle dir` (exit 1, no output, the value is read as a glob) and `--glob '!--version'` (exit 0) showed no flag injection. `--` precedes the pattern and paths. Tool: `/opt/my/bin/rg`, ripgrep 15.2.0.
- H-6 symlink and TOCTOU escape from write and edit: `resolveMutatePath` (mutate_path.go:43) resolves the root and the candidate. `inspectMutateTarget` (mutate_path.go:84) resolves symlinks and re-checks policy on the real path. `prepareMutateParent` (mutate_path.go:161) refuses a symlinked directory in the chain and re-checks the real parent. `pinDirectory` (mutate.go:68) pins the parent by handle and checks its `/proc/self/fd` path. Tests pass: `TestMutationsRejectEscapesAndSymlinks`, `TestPathScopeEditAndSymlinkEscape`, `TestPathScopeWorksWhenTheWorkspaceRootIsASymlink`, `TestNativeReadRejectsSymlinkEscapingWorkspace`.
- H-7 SSRF: `validateFetchURL` (fetch.go:388) checks every resolved address with `isPublicFetchAddress` (fetch.go:469: loopback, private, link-local, CGNAT, NAT64 and 6to4 embedded IPv4 re-checked). `newPinnedFetchTransport` (fetch_transport.go:104) dials only the validated IP, with `Proxy: nil` and TLS `ServerName` set to the hostname. `Do` (fetch.go:537) sets `CheckRedirect` to `ErrUseLastResponse`, so redirects are handled only by `retrieveFetchDocument`, which validates each hop (fetch.go:299). The pool key includes the IP (fetch_transport.go:13-20). Tests pass: `TestFetchToolRedirectsAndRejectsPrivateDNS`, `TestFetchToolPinsEveryCallAndRedirectHopToItsOwnValidatedAddress`, `TestPinnedFetchTransportVerifiesTLSAgainstTheURLHostnameNotTheIP`.
- Response bounds: `readFetchResponseTruncating` (fetch.go:616) applies the size limit after gzip, deflate and brotli decoding, so a decompression bomb is capped at `MaxResponseBytes`.
- H-8 trailing-dot host bypass: `containsHost` (fetch.go:460) trims the dot. Fixed by `49442de` (P2A-09). Checked, not re-reported.
- H-9 spill path escape: `encodeBashSpillCallID` (bash_spill.go:228) percent-encodes the call id before it reaches a filename template, and `bashGeneratedSpillFilename` (bash_spill.go:16) adds a fixed prefix and suffix. The spill directory is `storage_dir` or `{cwd}/.bash-output`.
- H-10 `PathLocks` symlink alias: `PathLocks` is keyed on the lexical candidate (path_locks.go:22). A symlink alias to the same file takes a different lock. The outcome is `changed_during_write` from the identity check, not corruption, because `commitFile` re-checks identity. Acceptable.
- H-11 ripgrep flag injection through the bash-style argument list: covered by H-5.
- `bash`: `buildNativeBashEnvironment` (bash.go:382) applies the allowlist and removes `JAD_API_TOKEN`, `apiKeyEnv` and `env_deny` from the base and from inline `env`. `runShellChildDetailed` resolves the cwd through `resolveExistingTarget` (bash.go:224). Argument `file` is passed as argv, not via shell.
- `read_image`: a `stored-image://` reference goes through `imagestore.Store.Open` (store.go:499), which validates the name (`ValidName`), checks `Lstat` against the opened file with `SameFile`, and enforces the size limit. Paths go through `resolveExistingTarget`.
- `write`: base64 is decoded with canonical checks (write.go:141-159); size is limited before any write (write.go:103).
- `fetch_store` cursors: `parseFetchCursor` (fetch_store.go:246) accepts only the UUID-plus-offset regex (fetch_store.go:61), so a cursor cannot name a path.
- Temp files: `writeAtomically` (mutate.go:335) uses `O_EXCL` and random suffixes; `atomicFetchWrite` (fetch_store.go:305) uses `CreateTemp` and `chmod 0600`.
- Glob pattern: `globCommandArgs` (glob.go:370) puts `--` before the path. Results are filtered through `pathPolicyError` (glob.go:280).
- Grep results: each group path is re-resolved through `resolveExistingTarget` (grep.go:124-133), so denied files drop out of the output.
- Spill pruning: F-11 (prune on every call) is fixed; `pruneBashSpillFilesThrottled` (bash_spill.go:272) rate-limits it. F-12 span memory is fixed by `ffb559c` (see H01 for the residual).
- Child scope, decision: `PathScope` is applied to write, edit and transcript tools (path_scope.go:14), and agents' `paths` narrow writes only, as commit `6fa038a` states. `read`, `grep` and `glob` do not consult the scope. This matches the recorded design, so it is not a finding. A reader who disagrees should raise it as a decision challenge.
- Dead code: a scan of the 205 function names in the non-test files of this phase found none with a single reference across the package.
- Build: `go build ./...` exit 0; `go vet ./internal/tools/native/` exit 0; `gofmt -l internal/tools/native` exit 0 with no output.

## 7. Not covered, and why

- Read in full (see method disclosure above for the whole-file reads): path.go, path_scope.go, path_locks.go, mutate_path.go, fetch.go (validation, `Do`, retrieval, body reader), fetch_transport.go, bash_executable_unix.go, bash_exit_unix.go, file_change.go, html-to-markdown-rs_cgo_link.go, the key paths of write.go, edit.go, mutate.go, read.go (`executeRead`, `renderDirectory`), grep.go, glob.go, ripgrep.go (`runRipgrep`, `readRipgrepEvents`), bash.go (`runShellChildDetailed`, env), bash_child.go (`runBashCommand`), fetch_store.go (`Create`, cursor parsing, atomic write).
- Method disclosure: several Go files were read whole with `cat`/`sed` in `exec` rather than by symbol: path_scope.go, path_locks.go, bash_executable_unix.go, bash_exit_unix.go, fetch_transport.go, file_change.go and the cgo link file. Their findings and clean verdicts rest on those reads. This breaks the index-first rule in REVIEWER.md section "Mandatory Code Navigation" (rule 4: never read a whole Go file). Stated here, not hidden.
- Read by symbol only, not line by line: bash_output.go (collector internals), bash_spill.go (planning and pruning), edit_text.go (diagnostics), read_image.go, image_file.go (`readImageFile`), read.go `renderFileRange` and `scanLines`, write.go base64 decode bodies, fetch_store.go `Page`, `CreatePage` and the URL cache, glob.go matcher (`compileGlob`).
- Not read: bash_executable_windows.go, bash_exit_windows.go (Windows build; this review ran on linux/amd64 only).
- Tests were used as evidence and for the test-quality note (P7A-R1-01), not reviewed in full.
- Fallbacks: I used `codebase-memory` `get_code_snippet` and `search_graph`, `zvec-grep` for orientation, and `read`-only shell (`sed`, `grep`, `cat -n`) for exact ranges and multi-symbol files. Some `search_graph` results surfaced snippet text from `audit_findings/*` files (for example p2a_*, p2b2_*, p6b_*). I did not open those files, and no finding relies on them.
- Not run: `-race`, benchmarks, profiles, `go test ./...`, and any test outside the targeted list.

## 8. Commands run (exit codes)

| command | exit | note |
|---|---|---|
| `go build ./...` (in backend-go) | 0 | |
| `go vet ./internal/tools/native/` | 0 | |
| `gofmt -l internal/tools/native` | 0 | no files listed |
| `go test -overlay /tmp/r1/probe/overlay.json ./internal/tools/native/ -run TestProbeDirectoryListingLeaksDeniedEntry -count=1 -v` | 1 | probe; FAIL is the intended confirmation (P7A-R1-01) |
| `go test ./internal/tools/native/ -run TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit -count=1` | 0 | control |
| `go test ./internal/tools/native/ -run 'TestNativeReadRejectsSymlinkEscapingWorkspace\|TestMutationsRejectEscapesAndSymlinks\|TestPathScopeEditAndSymlinkEscape\|TestFetchToolRedirectsAndRejectsPrivateDNS\|TestPinnedFetchTransportVerifiesTLSAgainstTheURLHostnameNotTheIP\|TestFetchToolPinsEveryCallAndRedirectHopToItsOwnValidatedAddress\|TestNativeReadListsDirectoriesWithPathPolicyAndConfiguredLimit' -count=1 -v` | 0 | 7 tests PASS |
| `go test -overlay /tmp/r1/storeprobe/overlay.json ./internal/tools/native/ -run TestProbeFetchStoreFillsPermanently -count=1 -v` | 0 | probe (P7A-R1-02); an earlier run with a 16-byte cap exited 1 for a probe-setup reason, not counted |
| `rg --json --no-config --glob '--version' -- needle dir` (probe dir in /tmp) | 1 | H-5 control, no flag injection |
| `rg --json --no-config --glob '!--version' -- needle dir` | 0 | H-5 |
| `rg --json --no-config --glob '*.txt' -- needle dir` | 0 | H-5 control |
| `git status --short -- internal/tools/native` | 0 | empty; tree unchanged |
| `git log`, `git blame`, `git show -s` (read-only) | 0 | |

Go runs used `GOCACHE=/tmp/r1/gocache` so the shared Go build cache was not touched. Scratch files are in `/tmp/r1/`, removed at the end.

## 9. Summary counts

- Findings: 4 (critical 0, high 0, medium 2, low 2). Lenses: B 2 (P7A-R1-01, P7A-R1-04), A 2 (P7A-R1-02, P7A-R1-03).
- Performance hypotheses: 2 (H01 and H02, both unmeasured).
- Hypotheses checked: 12 (H-1 to H-12): 4 became findings (H-1, H-2, H-3, H-4), 6 disproved (H-5 to H-11, except H-8 which is fixed), 1 decision (H-12), 1 fixed upstream (H-8).
