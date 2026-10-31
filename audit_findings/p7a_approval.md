# Phase p7a approval list (plan 6.4)

Inputs: audit_findings/p7a_merged.md, p7a_measure.md, p7a_r1.md to p7a_r4.md. HEAD f425dee. No source change, no commit, no test run in this step. All items are open except P7A-17 (closed: disproved).

Ranking: severity (medium first), then evidence tier, then consensus (reviewers who filed the item), then effort (S before M). Tier 1 is run evidence (a probe with an exit code, or a number in p7a_measure.md). Tier 2 is a source read, possibly with an allocation or profile number.

## 1. Ranked list

| rank | id | title | status | evidence | consensus | fix proposal | effort | recommendation |
|---|---|---|---|---|---|---|---|---|
| 1 | P7A-01 | Directory read lists entries a deny rule hides | open | tier 1, reproduced (exit 1) | 3 (r1, r2, r3) | filter renderDirectory entries (read.go:350) with pathPolicyError, not pathAllowed; test with a limit above the entry count | S | fix |
| 2 | P7A-02 | Fetch storage has no reclaim; a full store fails every later fetch | open | tier 1, state reproduced (exit 0); defect status is a decision | 2 (r1, r2) | see section 3 | M | decision |
| 3 | P7A-03 | isPublicFetchAddress accepts IANA special-purpose ranges its exclusions miss | open | tier 1, filter reproduced (exit 0) | 1 (r4) | add the ranges chosen in section 3 to fetch.go:469-526; cases in fetch_test.go | S | fix (scope per section 3) |
| 4 | P7A-07 | Spill throttle map grows per call with a legacy stdout_full template | open | tier 1, reproduced (exit 0; 0 to 2000 keys in 1000 calls) | 2 (r2, r3) | in bashSpillPrunes (bash_spill.go:267-290) drop entries older than the interval when the map is touched | S | fix |
| 5 | P7A-04 | ripgrep and glob children get the provider API key | open | tier 1, reproduced (exit 0) | 1 (r1; r2 disproved it, wrongly) | build the rg and glob child env without the provider key (share the bash env rule or strip apiKeyEnv); canary test | S | fix |
| 6 | P7A-05 | fetch accepts any TCP port on a public address | open | tier 1, validation reproduced (exit 0; nothing dialed) | 1 (r2) | default allow 80 and 443; a settings list for other ports | S | defer as low (behaviour change for non-standard ports) |
| 7 | P7A-13 | fetch page converts the whole stored document to UTF-16 on every page | open | tier 1, scaling measured (M3: 1 MB 10.3 ms, 8 MB 73.5 ms per page) | 1 (r2) | keep the UTF-16 length in the stored metadata and slice without converting the whole document; fetch paths only | S | fix, after the M10 check (section 4) |
| 8 | P7A-15 | grep re-resolves every matched file after the search | open | tier 1, scaling measured (M4: about 5.4 us per matched file) | 1 (r2) | filter groups with the cheap PathWithin check; resolve only where needed | S | defer as low |
| 9 | P7A-14 | fetch Create stats every stored file under the store mutex | open | tier 1, scaling measured (M2: 100 docs 0.21 ms, 5000 docs 8.9 ms) | 1 (r2) | cache per-file sizes in the store (the F-13 pattern) | M | defer as low |
| 10 | P7A-06 | read_image stored reference reads the whole object before the limit | open | tier 2 (read); allocation measured (M11: B/op equals image size; time noisy) | 2 (r2, r4) | stat the stored object, or use a limited open, and reject above maxBytes before reading | S | fix |
| 11 | P7A-08 | edit commits over a same-inode in-place change made after its read | open | tier 2, read only | 1 (r1) | re-read the target and compare with the bytes the edit used, before the rename | S | defer as low (or document the single-writer assumption) |
| 12 | P7A-09 | fetch follows https to http redirects with no signal | open | tier 2, read only | 1 (r3) | refuse the downgrade, or flag it in the result (policy choice) | S | defer as low |
| 13 | P7A-10 | image read has no identity check after open (a FIFO swap can block) | open | tier 2, read only | 1 (r3) | in readImageFile (image_file.go:38-52) use readVerifiedRegularFile (path.go:126-154) | S | fix |
| 14 | P7A-11 | PathLocks.Lock on a nil receiver locks nothing shared | open | tier 2, read only (no production caller reaches nil) | 1 (r3) | remove the nil branch or document the non-nil rule | S | defer as low |
| 15 | P7A-12 | dead code in bash_output.go and fetch.go | open | tier 2, read and grep | 1 (r2) | delete the unused render methods and readFetchResponse; point their tests at the used helpers | S | fix |
| 16 | P7A-16 | per-agent paths narrows write, edit and transcript tools only | open | tier 2, read; design recorded at backend-go/DECISIONS.md:334 | 1 (r2; r1 called it a decision) | none until the decision is made | S | decision challenge |
| - | P7A-17 | glob lists a symlink whose target is denied | closed (disproved) | tier 1, rg probe (exit 0; symlink not listed) | 1 (r3) | none | - | reject |


## 2. Performance items with no merged finding ID

| id | hypothesis | measured result | recommendation |
|---|---|---|---|
| M1 | read of a large file reads the whole file (r1 H01, F-12 residual) | 50-line range: 16 MB 32.5 ms, 64 MB 128.6 ms | defer as low |
| M5 | write and edit read the target to decide unchanged (r2 H04) | noisy: 9.4 and 10.6 percent spread at 1 MiB | defer; re-measure in a quiet window |
| M6 | spill planning lists the directory per call (r2 H05, r3 H05) | 16.8 ms per call at interval 0; 23 us at the default 3600 s | reject for the default; interval 0 is a configuration choice |
| M8 | directory listing reads every entry before the limit (r3 H02, r4 H01) | limit 100: 0.47 ms at 100 entries, 12.6 ms at 10,000 | defer as low; P7A-01 edits the same loop, so do it first and re-check |
| M9 | glob post-filter policy check per listed file (r3 H03) | about 2.0 us per listed file | reject; cost is small |

## 3. Decisions needed from the user

### P7A-02: fetch storage full

Option A, eviction (drop the oldest documents by FetchedAt when Create would pass the cap):
- automatic, so the tool keeps working;
- deletes documents a model may still page; their cursors then fail with fetch cursor was not found;
- the store is shared by every session with the same storage_dir (resolver.go:812-823), so one session can remove another session documents;
- changes a recorded capacity behaviour (backend-go/DECISIONS.md:103 records the cap as intended).

Option B, documented manual reset (keep the cap; record the recovery; improve the error):
- the code removes no data;
- the tool stays unusable until someone deletes files, which is the case now;
- needs a DECISIONS entry and an error that names storage_dir and max_storage_bytes.

Recommendation: option B now (S, no behaviour risk). Revisit eviction only if you want automatic recovery, and answer the shared-storage question first.

### P7A-16: per-agent paths

Facts: scopedPolicy (path_scope.go:14) has four callers: edit.go:92, write.go:94, transcript_path.go:78, transcripts_list.go:133. read, grep, glob and read_image do not call it. backend-go/DECISIONS.md:334 says the per-agent paths narrows where the child write and edit may change files.

Verdict: a decision challenge, not a defect. Write-only narrowing is recorded design. The field name paths and the transcript tools (which do scope reads) suggest a wider restriction, and a child can read whatever the session can read.

Recommendation: keep write-only and say so in the agent and tool descriptions (wording only). If you want read restriction too, that is four scopedPolicy calls plus tests (S), but it changes behaviour for existing agents, so decide it first.

### P7A-03: special-use address ranges

Already blocked by isPublicFetchAddress (checked at fetch.go:469-526): 192.0.0.0/16 (includes 192.0.2.0/24), 198.18.0.0/15, first octet 224 and above, 2001:db8::/32, and the IPv6 site-local, NAT64 and 6to4 cases.

Not blocked today, with my proposed disposition (the IANA registry status is from memory; confirm it against the registry text before the change):
- 198.51.100.0/24 (TEST-NET-2, documentation): block.
- 203.0.113.0/24 (TEST-NET-3, documentation): block.
- 192.88.99.0/24 (deprecated 6to4 relay anycast): block.
- 2001:2::/48 (benchmarking): block.
- 2001:10::/28 (deprecated ORCHID): block.
- 2001:20::/28 (ORCHIDv2): decide after checking the registry status.
- 2001::/32 (Teredo; 2001::1 is inside it): leave open unless you want the strict reading. Tunnel addresses are used on real networks.

What a fix changes for users: a fetch whose host resolves into a blocked range gets the non-public refusal. Public websites do not sit in these ranges, so ordinary fetches do not change. A network that uses TEST-NET space for real internal hosts (common in labs) loses fetch to those hosts. The change is limited to isPublicFetchAddress and needs cases in fetch_test.go.

## 4. What the measurement changed

- P7A-14 is cheaper than r2 estimated. r2 said tens of microseconds at the default cap; the measured cost is 0.21 ms at 100 documents (M2). Real but small: defer.
- P7A-13 narrowed to the fetch path. M7 puts the ripgrep UTF-16 cost at about 5 percent at 20,000 characters, so the ripgrep.go:415-417 part of the merged item is not worth a change.
- P7A-15 deferred. M4 gives about 5.4 us per matched file, so 200 matches (default max_results) cost about 1 ms, against a 1.6 ms ripgrep spawn. The post-filter share was not isolated.
- P7A-06 allocation measured: 58 KB and 3.15 MB for 64 KiB and 4 MiB stored images. Per r4 (not re-checked here), the store default object limit is 20 MiB, so one call can allocate that much before the 5 MiB check. The fix is still S.
- M10 lead (new, not yet a fix target). In the fetch HTML path the memory profile puts utf16Length at 41 percent cumulative and the UTF-16 encode at 20 percent flat, which is more than the three body copies in r3 H04. The UTF-16 conversions are fetch.go:373 (length of the converted markdown), fetch_store.go:158, :171 and :188 (length), and :161 and :186 (sliceFetchCharacters, which converts the whole document). The cost is conversion work, not copies. Before any P7A-13 change, benchmark a baseline and a prototype at the same size.
- M6 rejected for the default. The 16.8 ms spill cost only occurs at prune_interval_seconds 0. At the default it is 23 us, so the F-11 concern is not a default defect.
- M1 and M8 are linear in file size and directory size. Both are defer items.

## 5. Counts

- Items: 17. Severity: medium 3, low 13, disproved 1 (P7A-17, not ranked).
- Recommendations for the 17: fix 8 (P7A-01, 03, 04, 06, 07, 10, 12, 13); defer as low 6 (P7A-05, 08, 09, 11, 14, 15); decision 2 (P7A-02, 16); reject 1 (P7A-17).
- Performance-only items (section 2): defer 3 (M1, M5, M8); reject 2 (M6, M9).

Decisions needed: P7A-02 (eviction or manual reset), P7A-16 (write-only by design, or read restriction), P7A-03 (the ranges in section 3), and whether to go ahead with the P7A-13 change after the M10 check.
