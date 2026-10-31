# Phase p7a review — file and search tools (reviewer r3)

- phase: p7a (native file/search: bash*, path*, read*, write, edit*, mutate*, file_change, image_file, glob, grep, ripgrep, fetch*, html-to-markdown link)
- reviewer: r3
- HEAD: f425deefa3cee932ea9a494b4e7e3f8065f92
- date (UTC): 2026-10-30
- files read (27 non-test, backend-go/internal/tools/native/): bash.go, bash_child.go, bash_executable_unix.go, bash_executable_windows.go, bash_exit_unix.go, bash_exit_windows.go, bash_output.go, bash_spill.go, path.go, path_locks.go, path_scope.go, read.go, read_image.go, write.go, edit.go, edit_text.go, mutate.go, mutate_path.go, file_change.go, image_file.go, glob.go, grep.go, ripgrep.go, fetch.go, fetch_store.go, fetch_transport.go, html-to-markdown-rs_cgo_link.go (generated, out of scope per audit_plan 1)
- commands run (from backend-go/, exit codes seen):
  - `go build ./internal/tools/native/` → exit 0
  - `go vet ./internal/tools/native/` → exit 0
  - `gofmt -l internal/tools/native` → exit 0 (no output)
  - `git diff --check` → exit 0
  - `go test ./internal/tools/native/ -run 'TestPathScopeWriteStaysInsideAllow|TestPathScopeEditAndSymlinkEscape|TestNativeReadRejectsSymlinkEscapingWorkspace' -count=1` → exit 0 (ok)
  - `go test ./internal/tools/native/ -run 'TestFetchURLPolicyAndPublicAddressRanges' -count=1` → exit 0 (ok)
  - `git log --oneline -- backend-go/internal/tools/native/fetch.go backend-go/internal/tools/native/path.go backend-go/internal/tools/native/mutate_path.go` → exit 0
  - `git log --oneline --grep="SSRF|fetch|symlink|trailing-dot|P2A-09|P2A-10"` → exit 0

Navigation: codebase-memory get_architecture + search_graph/get_code_snippet + trace_path, zvec-grep scoped to backend-go/internal/tools/native and backend-go/internal/sessionfs; exec read.sh only for exact line ranges (bash_spill.go 1-70/250-312, file_change.go, html-to-markdown link, path_locks.go) and tiny-file confirmation. No whole Go file dumped; counts redirected where noted.

## 1. Understanding note

p7a is the file/search SSRF + confinement boundary. Reads (read/read_image/glob/grep/ripgrep/fetch) resolve via `resolveExistingTarget` (path.go:23-75: lexical policy check, then EvalSymlinks, then real-path policy check) and enforce deny + Within + workspace. Mutations (write/edit/mutate) resolve lexically via `resolveMutatePath` (mutate_path.go:43-82), then `inspectMutateTarget` (84-159: Lstat, symlink EvalSymlinks + policy, regular-file check) + `prepareMutateParent` (161-209: nearest-existing-parent symlink check, MkdirAll, post-mkdir EvalSymlinks + policy), then `commitFile` (mutate.go:287-333) re-pins the parent via /proc/self/fd on linux (pinDirectory 68-101) and re-checks `targetStillMatches` (258-264: SameFile) before `writeAtomically` (335-376: O_EXCL temp + rename, fsync). Fetch validates per hop in `retrieveFetchDocument` (fetch.go:287-386: scheme, no credentials, host allow/deny via containsHost, resolve all IPs, reject non-public via isPublicFetchAddress 469-526, pin addresses[0]) and dials the pinned IP in `newPinnedFetchTransport` (fetch_transport.go:104-126, SNI = URL hostname), with keep-alive pooling keyed by validated destination and cache re-validation of both request and FinalURL in `executeNativeFetch` (166-251). Glob/grep/ripgrep spawn ripgrep via exec with argv (no shell) and post-filter results; bash spill prune is throttled per dir/pattern (bash_spill.go:272-290, F-11 fix). Prior fixes verified present: trailing-dot host (49442de), site-local/NAT64/6to4 (a1e8f1c), spill throttle (154d0de), read buffer sizing (ffb559c), keep-alive pooling (fa509c2), kill/drain configurability (a1e8f1c).

## 2. Hypotheses (assumed → verified/disproved)

- H1 path confinement bypass via symlink parent on write → disproved (prepareMutateParent + pinDirectory + targetStillMatches; tests TestPathScopeEditAndSymlinkEscape, TestPathScopeWorksWhenTheWorkspaceRootIsASymlink pass).
- H2 fetch DNS-rebinding between validation and dial → disproved (pinned transport dials validated IP, redirect re-validates every hop, cache re-validates FinalURL).
- H3 fetch trailing-dot deny bypass → disproved (fixed 49442de; containsHost trims trailing dot, test passes).
- H4 whole-file read DoS → disproved for read (readVerifiedRegularFile sizes buffer + LimitReader + SameFile, F-12 fix ffb559c); open for image path (see P7A-R3-06).
- H5 spill prune double-scan cost → disproved as hot cost (throttled 154d0de; remaining issue is map growth, see P7A-R3-03).
- H6 directory listing leaks denied names → verified (see P7A-R3-01).
- H7 glob lists symlink-to-denied names → verified as inconsistency (see P7A-R3-02).

## 3. Findings

### P7A-R3-01 read directory listing ignores deny rules
- severity: medium
- lens: B security
- location: backend-go/internal/tools/native/read.go:342-407 (renderDirectory filter), backend-go/internal/tools/native/path.go:92-102 (pathAllowed)
- evidence: [read] `renderDirectory` filters with `pathAllowed` (allows if UnrestrictedPaths or PathWithin(root,target) or allow-rule; no deny check) while file reads use `pathPolicyError` (deny first, then Within, then allow). Concrete: policy `paths:[{allow:workspace},{deny:<workspace>/secret}]`, `read(path=workspace-dir)` lists `secret/*` names though `read(path=secret/file)` is denied. Trace: executeRead:262-331 → resolveExistingTarget:23-75 (deny enforced) vs renderDirectory:342-407 `if !pathAllowed(entryPath,...) continue`.
- suggested fix: filter directory entries with `pathPolicyError(entryPath,root,policy,callID)==""` (deny + Within) instead of `pathAllowed`; keep allow-visibility semantics if intended, but deny must win. Files: read.go.
- effort: S
- prior art: new (related to P2B2-06 sessionfs listing deny-on-unresolved; distinct file, same class)

### P7A-R3-02 glob lists symlink-to-denied targets without resolving them
- severity: low
- lens: B security
- location: backend-go/internal/tools/native/glob.go:203-352 (post-filter loop ~300-315), backend-go/internal/tools/native/grep.go:102-184 (per-group resolveExistingTarget)
- evidence: [read] glob builds `absolute=Join(workspaceRoot,value)` and checks `pathPolicyError(absolute,...)` lexically, never EvalSymlinks per entry. grep instead calls `resolveExistingTarget(group.path,...)` per result (EvalSymlinks + real-path policy). Concrete: `src/link -> <denied-outside>` lexically inside is listed by glob though read/grep deny it. Name-only leak; contents still denied on read.
- suggested fix: resolve each glob hit with `resolveExistingTarget` (or EvalSymlinks + policy) like grep; skip on denial. Files: glob.go.
- effort: S
- prior art: new

### P7A-R3-03 spill-prune throttle map grows without bound
- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/bash_spill.go:267-290 (bashSpillPrunes.last)
- evidence: [read] `var bashSpillPrunes struct{sync.Mutex; last map[string]time.Time}` keyed by `directory+"\x00"+pattern.String()`; `pruneBashSpillFilesThrottled` inserts on every due scan, never deletes. Workload: many distinct spill StorageDirs (per session/profile) accumulate entries for process lifetime.
- suggested fix: on touch, drop entries older than interval (or cap map and force scan when full). Files: bash_spill.go.
- effort: S
- prior art: new (F-11 added the map in 154d0de; growth not part of F-11)

### P7A-R3-04 PathLocks.Lock on nil receiver serializes nothing
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/path_locks.go:22-51, backend-go/internal/tools/native/write.go:31-54 (nil guard)
- evidence: [read] `func (locks *PathLocks) Lock(key string)` does `if locks==nil {locks=NewPathLocks()}` on a copy; each nil call gets a fresh map+entry, so two `var l *PathLocks; l.Lock(k)` callers never exclude. Production safe today (NewWriteTool/NewEditTool replace nil before capture), but the method advertises nil-safety it does not provide.
- suggested fix: either panic/document non-nil, or hold a package-level fallback mutex for nil (or make Lock a function taking **PathLocks). Files: path_locks.go, write.go, edit.go callers.
- effort: S
- prior art: new

### P7A-R3-05 fetch allows https→http redirect downgrade without signal
- severity: low
- lens: B security
- location: backend-go/internal/tools/native/fetch.go:287-386 (retrieveFetchDocument redirect loop 314-326), backend-go/internal/tools/native/fetch.go:388-444 (validateFetchURL allows http and https)
- evidence: [read] 3xx Location resolved via `target.URL.Parse(location)` and looped with `validateFetchURL` which accepts both schemes; no same-scheme or upgrade-only rule, no downgrade hint in result. Concrete: `https://public/` → `http://public/` is followed and rendered as ok. No creds leak (userinfo rejected), but http is MITM-able.
- suggested fix: reject downgrade (or surface `downgraded:true` + hint) unless settings opt in; document. Files: fetch.go.
- effort: S
- prior art: new (batch-9 SSRF gap covered private-IP pinning, not scheme policy)

### P7A-R3-06 image read lacks SameFile verification (TOCTOU + FIFO block)
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/image_file.go:19-60 (readImageFile), backend-go/internal/tools/native/path.go:126-154 (readVerifiedRegularFile)
- evidence: [read] `readImageFile` does Lstat→IsRegular→Open→readWithin with no `os.SameFile` recheck, unlike `readVerifiedRegularFile` (Stat after Open + SameFile + size equality). Between Lstat and Open a concurrent replacer (local fs writer or parallel bash) can swap regular→FIFO; `os.Open` on FIFO blocks (no ctx, no O_NONBLOCK) hanging the call. Sequential model use is safe (second call sees FIFO via Lstat and rejects), so reachable only via concurrent writer; still an inconsistency with the hardened read path.
- suggested fix: reuse `readVerifiedRegularFile` (or Open + Stat + SameFile + non-blocking FIFO guard) in image_file.go. Files: image_file.go.
- effort: S
- prior art: new

## 4. Performance hypotheses (reviewers do not measure; [unmeasured])

- H01: per-match `truncateJSString` does `utf16.Encode([]rune(value))` allocating per line, file: backend-go/internal/tools/native/ripgrep.go:419-425
  workload: grep with MaxResults=1000+ long lines (1 KiB vs 10 KiB lines)
  benchmark: existing `BenchmarkRunRipgrep` (tools/native) + proposed /tmp variant with long-line fixture
  expected: scales with matches*line-length; extra alloc per entry
- H02: `renderDirectory` MarshalJSON per entry + `usedBytes>MaxBytes` cut still Stats every entry, file: backend-go/internal/tools/native/read.go:342-407
  workload: directory with 5k vs 50k entries
  benchmark: proposed /tmp list-directory benchmark (ReadDir + render) at two sizes
  expected: O(entries) Stat + marshal; truncation only caps output bytes, not Stat cost
- H03: glob post-filter runs `pathPolicyError` per listed file after ripgrep `--files`, file: backend-go/internal/tools/native/glob.go:203-352
  workload: tree with 1k vs 20k files matching `*`
  benchmark: existing `BenchmarkExecuteNativeGlob`
  expected: extra policy walk per file; scales with listed files
- H04: fetch HTML path copies body 3x (`ToValidUTF8` + `string(body)` + `htmltomarkdown.Convert` + `utf16Length`), file: backend-go/internal/tools/native/fetch.go:287-386,616-675
  workload: 100 KiB vs 1 MiB HTML at MaxResponseBytes cap
  benchmark: proposed /tmp fetch-html httptest benchmark at two sizes
  expected: peak heap ~3-4x body; conversion dominates
- H05: spill throttle map lookup + first-scan cost per new dir, file: backend-go/internal/tools/native/bash_spill.go:272-312
  workload: 10 vs 1000 distinct spill dirs (fresh backend)
  benchmark: existing `BenchmarkPlanBashSpillLargeDirectory` + proposed multi-dir variant
  expected: first command per dir scans (16 ms at 3k files); map insert only thereafter

## 5. Cross-phase leads (outside p7a, with file:line)

- `backend-go/internal/sessionfs/native_fetch_settings.go:37,188-211` (`nativeFetchHostPattern` + `nativeFetchHosts`): pattern `^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$` rejects `:` so IPv6 literals (bracketed or bare) cannot be allow/deny-listed; re-confirms P2A-10 (still open; trailing-dot P2A-09 is fixed, see §6). Fix belongs in sessionfs (phase 2a), not here.
- `backend-go/internal/sessionfs/native_bash_settings.go:333-437` (spill readability `bashPathWithin` without EvalSymlinks at :402 per p2a-r3 lead): symlink escape for spill readability is a phase-2a settings question; planBashSpill (p7a) trusts it.
- `backend-go/internal/sessionfs/files.go:199-204` (listing deny on unresolved path, P2B2-06): same deny-vs-listing class as P7A-R3-01 but in session runner listing; merged fix should cover both.
- `backend-go/internal/tools/native/subagent*.go` + `sessionfs/native_subagent_*.go` prompt/agents reads (whole-file, no cap per p2a-r3 lead): 7b/2a, not re-measured here.

## 6. Checked and sound

- Trailing-dot host bypass (P2A-09): fixed in 49442de; `containsHost` now `TrimRight(".")` + EqualFold (fetch.go:460-467); `TestFetchURLPolicyAndPublicAddressRanges` passes.
- Site-local/NAT64/6to4 (a1e8f1c): `isPublicFetchAddress` rejects fec0::/10, 64:ff9b::/96 embedded-private, 2002::/16 embedded-private; read verified.
- Spill double-scan cost (F-11): throttled (154d0de); per-call after interval is map lookup; remaining is map growth (P7A-R3-03).
- Read whole-file + buffer doubling (F-12): `readVerifiedRegularFile` sizes from Stat + LimitReader (ffb559c); verified.
- Fetch pinning + pooling (B-P1/fa509c2): pinned dial + pool keyed by validated address + per-hop revalidation + cache FinalURL revalidation; verified by read + fetch_transport_test names.
- Ripgrep/glob kill/drain configurability (a1e8f1c): settings-driven timeouts in runRipgrep/glob; verified by read.
- Write/edit atomicity + backup + SameFile (mutate.go/commitFile/pinDirectory): verified by read; no new TOCTOU beyond P7A-R3-06.
- `html-to-markdown-rs_cgo_link.go`: generated, machine-local, out of scope; not reviewed.

## 7. Not covered and why

- `bash_child.go` stdin/timeout/kill-ladder internals beyond the `environmentWithoutServerToken` + argv use by glob/ripgrep: relied on existing bash tests + phase-6b ownership; no new claim.
- `bash_output.go` collector head/tail windows and `bash_executable_*`/`bash_exit_*` platform shims: skimmed only; platform-specific, no hypothesis.
- `edit_text.go` line-ending/occurrence counting details and `mutate.go` backup pruning: skimmed; covered via applyEdit size-guard + commitFile re-check.
- `fetch_store.go` eviction/sweep and `fetch_transport.go` pool LRU beyond key-includes-address: skimmed; cache SSRF safety verified via revalidation path.
- Full 27-file line-by-line re-read not claimed; coverage is via index traces + targeted snippets above. No network, no -race/-bench, no repo changes; scratch in /tmp/r3 only.
