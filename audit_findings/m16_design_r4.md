# M16 Checkpoint Lock-Scope Design Proposal (r4)

Scope: backend-go checkpoint begin/Finish/Restore lock scope (audit row M16, HEAD 1b6a05e). Read-only design proposal.

Evidence base (HEAD 1b6a05e, read via codebase-memory get_code_snippet/search_graph/search_code and read.sh line ranges for exact line anchors; grep -n only to number already-read anchors).

## Proposal

**Design: split the one staging lock into a short state lock (`s.mu`) and a long `gitGate`; keep git serialized and the on-disk layout fixed.**

(a) Outside `s.mu`: every git subprocess — prepare/snapshot/count-objects/commit-tree/update-ref/diff-tree/checkout/gc (capture service.go:442-485, keep :487-496, restore :726-824, prune's deleteRef :503-509, reclaim :546-557) — under a new per-staging `gitGate` (next to `locks`/`repoSizes`, :179-183). Under `s.mu` only: readAll→writeAll read-modify-write (begin :601-613, Finish :632-656, storeRecord :827-840), the repoSize cache (:448-457, :478-482), `gcPending` (:519-522, :525-528), and prepare's init single-flight.

(b) Correctness: `gitGate.Lock` keeps begin, Finish and Restore's git/workspace phases mutually exclusive exactly as today, so Finish's after-capture cannot mix with Restore's checkout (:770-781) and two captures cannot share `snapshot.index` (git.go:63). Same-checkpoint ordering is program order: Begin returns only after its record write (:609-613), Finish is deferred on the run (run.go:85-89), and a restore during a run is refused upstream (DECISIONS.md:131). Record updates are keyed by id and applied atomically under `s.mu`, so concurrent ops on different checkpoints cannot lose updates.

(c) Exact lines: service.go:589-590 → lock only :601-613 plus the cache/flag sets; :605's prune keeps bookkeeping under `s.mu` but queues its deleteRefs (:503-509) to run after unlock; :628-629 → lock :632-638 (lookup) and :645-656 (apply result, writeAll); :680-681 → lock :682-700 (lookup/validate) only, then `gitGate` around the :715 `restore` call; storeRecord :827-840 stays (already pure RMW); reclaim :546-557 takes `gitGate`, not `s.mu`; git.go:231-235 comment restated ("callers hold the git gate").

(d) Crash recovery identical: prepare still sweeps `snapshot.index.lock`/`restore.index.lock` (git.go:231-236) because `gitGate` preserves the "no git of this service runs between commands" invariant; the pre_restore record is still stored before the first workspace write (service.go:757-761); Report still maps open records to interrupted at read time (:404-417).

(e) Deterministic fail-first test, minimal seam: an unexported hook invoked at `shadowGit.run` entry (or `run` as a func field set in NewService :186-203). The test blocks a Finish inside `git write-tree` via channels, then asserts `s.mu.TryLock()` succeeds and `List()` returns; today the mutex is held across git (:628-643) so it fails; after the split it passes. Unblock; assert the record is `complete`. No sleeps.

(f) No on-disk format change. The interchange contract is `checkpoints.json` record fields (identical to TypeScript, DECISIONS.md:128), refs `refs/checkpoints/<id>-{before,after}` (service.go:30) and the bare repo — all untouched. Fixed index names (git.go:63-64) stay; it is the naive per-capture-index fix that churns them (and lock_test.go:16, which writes those exact lock names). design_checkpoint.md:20 already calls the index "a temporary index file": names are private-repo internals, not format.

(g) Risks: git throughput unchanged — phases still serialize; this fixes scope, not parallelism (overlap needs per-op indices, a later layout-affecting option); gc still blocks captures, as today; lock-order bugs if a future path takes `s.mu` while holding `gitGate` (document the order: `gitGate` outermost). Rough size: ~80-110 lines in service.go, one comment in git.go, ~60-line test.

## Checked and sound
- begin/Finish/Restore/capture/keep/restore/storeRecord/plan/prune/reclaim and git.go run/prepare/snapshot/commit/setRef/checkout read in full at HEAD 1b6a05e.
- DECISIONS.md:124-157 (checkpoint decisions), design_checkpoint.md:20,26 read for the format question.
