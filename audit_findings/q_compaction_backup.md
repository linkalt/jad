# Compaction backup question: is the compacted `messages.jsonl` kept in the backup?

- Asked: "If compactions happen, is the messages.jsonl that was compacted available in the messages.jsonl backup?"
- HEAD: `cf8c014c` (2026-11-06), go1.27.3 linux/amd64. Reviewer slot: answering Agent 1's question, not a phase review.
- Scope read (all read-only): `internal/transcript/{maintenance.go, front_compact.go, jsonl.go, input_history.go, turn.go, compaction_memo.go}`, `internal/sessionfs/{session.go, storage_paths.go}`, `internal/session/runner.go`, `internal/app/{maintenance.go, resolver.go, subagent.go, history.go, compaction_usage.go, bound_runner.go}`, `internal/httpapi/handler_ports.go`, `internal/tools/native/{transcript.go, transcript_path.go}`, `backend-go/DECISIONS.md` (lines 13, 239-246, 743, 991), existing tests listed in section 6.
- Navigation: `gonav navigate Compact` (returned 4 candidates), `read.sh <file> <start> <end>` for line ranges, `grep` for symbol/string sweeps, `git log -S`. No whole-file Go reads.
- Commands run and exit codes:
  - `go test -overlay /tmp/rvcomp/overlay.json -run TestProbeCompactionBackupModes -count=1 -v ./internal/transcript/` → **PASS, exit 0** (my probe, kept outside the tree, see section 3).
  - `go test -count=1 -run 'TestBackupTranscriptMinTurns|TestCompactHistoryKeepsFullerReplaceBackup|TestCompactHistoryPersistsCompactionOverlayAndResetInvalidatesFiles|TestCompactHistoryNoOpWhenRecentTurnDoesNotReduceHistory|TestFrontCompactRecoverableArchivesWholeInputBatchAndInvalidatesCaches|TestFrontCompactPermanentDoesNotCreateArchiveAndRejectsInvalidSelection|TestCompactHistoryRemovesTheCacheWhenTheTranscriptReplaceFails' ./internal/transcript/` → **ok, exit 0**.
  - No build/vet run was needed; no file in the tree was modified.

## 1. Short answer

**Yes for the context-compaction path, and in the default configuration it is a full, byte-exact archive — but not in three cases: `backup_mode: "replace"`, `reset`, and `front_compaction`, and never for sub-agent transcripts.**

1. Every context compaction (manual `POST .../compact` and automatic/overflow compaction inside a run) calls `transcript.TurnStore.CompactHistory`. Before the live file is rewritten it copies `messages.jsonl` verbatim to the backup file, default `<messages dir>/messages.backup.jsonl` (`internal/transcript/maintenance.go:158-161`, called before `s.journal.ReplaceRecords` at `:200`).
2. If the copy fails the compaction **aborts**; `messages.jsonl` is not touched (`maintenance.go:159-161`). So a compaction cannot silently skip its backup.
3. Default `compaction.backup_mode` is **`"append"`** (`internal/sessionfs/session.go:833`, asserted by `compaction_settings_test.go:14`): each compaction appends the whole pre-compaction transcript to the backup, so the backup becomes a growing, in-order archive of everything ever written to `messages.jsonl`. Nothing is pruned.
4. With `backup_mode: "replace"` the backup is overwritten each time, so only the *last* snapshot survives, and older turns that lived only in the compaction cache are then gone from the backup too (proved in section 3).
5. The backup is written **before** the rewrite with temp file + `Rename` (+ `fsync` of file and directory unless `persistence.durability: "process"`), so a crash never leaves a torn backup (`maintenance.go:635-695`).

## 2. Where it lives and how to read it

- Path: `paths.messages_backup`, default `filepath.Dir(paths.messages)/messages.backup.jsonl` = sibling of `messages.jsonl` (`internal/sessionfs/storage_paths.go:47`). The configured value may not be empty (`session.go:1262-1270`), so the `if s.options.MessagesBackupPath != ""` guard at `maintenance.go:158` is always true for a session store. Duplicate-path validation refuses `messages_backup` equal to `messages` (`storage_paths.go:88-118`).
- Wired in from config at `internal/app/resolver.go:954`; manual compaction passes `Compaction.BackupMode`/`BackupMinTurns` at `internal/app/maintenance.go:120-127`; automatic compaction passes the same at `internal/session/runner.go:988-1000` (overflow compaction at `runner.go:879-881` uses the same options).
- To read it: the `transcript` tool with `"source": "backup"` (`internal/tools/native/transcript.go:28,282`); the UI history block reads the last `compaction.keepbackup.turns` turns (default 20) of it (`internal/app/history.go:174-188`, `readBackupTail` at `:309`).
- There is **no restore path**: grep for backup-consuming code finds only reads (history block, catalog stamping `app/catalog.go:628,753`, fork copy `app/session_fork.go:250`, transcript tool). Recovery is manual (grep/copy the lines you want).
- Tone down one expectation: after a compaction the live `messages.jsonl` is replaced with only the *active tail* records, often **empty**; the retained turns then live in `staging/compaction-cache.json` (+ `.backup`), `maintenance.go:820-836`. My probe saw `messages.jsonl` = 0 bytes with 2 retained items. So if a user opens `messages.jsonl` after a compaction and sees an empty/short file, nothing is lost: the pre-compaction file is in the backup and the kept turns are in the compaction cache.

## 3. Probe evidence (outside the tree, `-overlay`)

Fixture: two turns written, `CompactHistory{TurnType:"assistant_final", KeepTurns:1}`, then one more turn and a second compaction.

```
mode=append  after1: backupEqualsPreCompactionMessages=true  liveAfter="" compacted=true compactedItems=4 retained=2
mode=append  after2: backup == snap1+snap2 byte for byte; backup contains TEXT-ONE=true TEXT-TWO=true TEXT-THREE=true
mode=replace after1: backupEqualsPreCompactionMessages=true
mode=replace after2: backup == snap2 only; TEXT-ONE=false TEXT-TWO=false TEXT-THREE=true
```

Consequences proved:
- The backup content is exactly the pre-compaction `messages.jsonl` bytes (both modes).
- `append` keeps every snapshot (full history); `replace` keeps only the newest, and the turns that were held in the compaction cache at that moment (TEXT-TWO) are in **no** file afterwards.

## 4. Findings

### QCB-01  `reset` deletes the compaction backup, so the compacted transcript becomes unrecoverable
- severity: high (data loss on an explicit user action)
- lens: A correctness
- location: `internal/transcript/maintenance.go:703-731` (`Reset`), `:739-756` (`removeDerivedFiles(reset=true)` adds `MessagesBackupPath`), also removes `<messages>.deleted-history`
- evidence: read; existing test `TestCompactHistoryPersistsCompactionOverlayAndResetInvalidatesFiles:73-77` asserts the backup is gone after `Reset()`. Recorded behaviour in `DECISIONS.md:13` ("reset clears the transcript, backup, and known per-profile caches"), so a decision challenge rather than a defect: I disagree that a *reset* should destroy the only copy of the previous history, because the destructive action is not reversible and the backup is not user-visible in the UI as something that will be deleted.
- suggested fix: smallest change is to keep the backup on reset (drop `s.options.MessagesBackupPath` from the reset list in `removeDerivedFiles`) and let the next compaction overwrite it; larger is a `.deleted-history`-style rotation. Requires a DECISIONS entry.
- effort: S
- prior art: new (decision recorded, outcome not audited before)

### QCB-02  `replace` mode makes context dropped by a second compaction unreachable in the backup
- severity: medium
- lens: A correctness
- location: `internal/transcript/maintenance.go:635-645` (replace overwrite), `:620-632` (`keepsFullerBackup` guard)
- evidence: probe in section 3 (`TEXT-TWO` present in the live transcript, retained at compaction 1, absent from the backup after compaction 2). `messages.jsonl` only holds records appended since the previous compaction (`maintenance.go:200` replaces it with the active tail), so a replace snapshot is not the session's history.
- suggested fix: either refuse `backup_mode: "replace"` when a compaction cache overlay exists for the transcript, or make `replace` write the *effective history* (`cache.Items` + transcript, expanded to plain messages) instead of the raw live file. Cheapest: document it and keep `append` the default (already the case).
- effort: M
- prior art: `DECISIONS.md:239-246` documents `min_turns` for this class of loss, not the loss itself.

### QCB-03  Front compaction never writes the backup, and the default mode leaves no archive at all
- severity: medium
- lens: A correctness / D maintainability
- location: `internal/transcript/front_compact.go:130-148` (`FrontCompactFromMessage` → `ReplaceWithPrefix`), `internal/sessionfs/session.go:921` (`normalizeFrontCompaction`) default `DeleteMode: "permanent"`
- evidence: read; the archive is `os.MkdirAll(<messages.jsonl>.deleted-history)` + one file per call (`front_compact.go:130-142`, `newDeletedHistoryArchiveName` at `:174-182`), only when `deleteMode == "recoverable"`. `messages.backup.jsonl` is not read or written by this path; `removeDerivedFiles(false, true)` (`maintenance.go:735-756`) does not touch it either. So after a front compaction the backup still describes the pre-front-compaction file only by accident (it is stale), and in `permanent` mode the removed lines exist nowhere.
- suggested fix: make `FrontCompactFromMessage` call `backupTranscript` before `ReplaceWithPrefix` (it already holds `s.mu`), or default `front_compaction.delete_mode` to `recoverable`.
- effort: S
- prior art: new (`DECISIONS.md:13` records "permanent mode does not archive"; the interaction with the compaction backup is not recorded)

### QCB-04  Sub-agent compaction keeps no backup: a child's pre-compaction transcript is unrecoverable
- severity: medium
- lens: A correctness
- location: `internal/app/subagent.go:278-284` (`NewTurnStore` with no `MessagesBackupPath`)
- evidence: read; the released guard `maintenance.go:158` fails on the empty path, so `CompactHistory` rewrites the child transcript with no copy and reports `BackupPath: ""`. `internal/tools/native/transcript_path.go:111-112` states it ("a sub-agent transcript has no backup"), and `DECISIONS.md:743` mentions the child's `compaction-cache.json` but not a backup.
- suggested fix: pass `filepath.Join(directory, "messages.backup.jsonl")` and `BackupMode` in `app/subagent.go:278`; the folder is per-run and small.
- effort: S
- prior art: new

### QCB-05  `append` mode rewrites the whole accumulated backup on every compaction (quadratic I/O, unbounded growth, no prune setting)
- severity: medium
- lens: C performance
- location: `internal/transcript/maintenance.go:645-695` (copy of the existing destination into a temp file, then `Rename`), `:158-161` (called per compaction); growth is unbounded — `compaction.keepbackup.turns` only limits *reads* (`internal/app/history.go:175-188`)
- evidence: read + probe (append backup was `snap1+snap2` after two compactions). With the default `auto_enabled: true`, `trigger_tokens: 200000` (`session.go:829-831`) a long session compacts repeatedly; compaction *k* reads and writes the sum of snapshots 1..k-1.
- workload: a session whose transcript reaches ~200 MB total: at the 10th compaction the backup write alone copies ~180 MB under `s.mu`.
- benchmark: `BenchmarkCompactHistory1000Turns` (transcript) extended with a prepopulated backup, or the existing `BenchmarkFrontCompactRecoverable1000Turns` shape.
- suggested fix: append to the backup in place (`O_APPEND` open + single write + `Sync`) since the file is append-only in this mode; keep temp+rename for `replace`. Add a `keepbackup.max_bytes`-style cap if the archive is meant to be readable.
- effort: S (in-place append) / M (cap)
- prior art: new

### QCB-06  Stale/misleading statements about the backup (default mode, tool description, dropped `backup_path`)
- severity: low
- lens: D maintainability
- location: `backend-go/DECISIONS.md:245` ("Not chosen … making `append` the default (unbounded growth)") vs `internal/sessionfs/session.go:833` default `"append"`; `internal/tools/native/transcript.go:28` ("backup is messages.backup.jsonl, the history before the last compaction") vs append accumulation; `internal/transcript/maintenance.go:68` `CompactionResult.BackupPath` is discarded by `internal/app/maintenance.go:121-160` and absent from `httpapi.CompactionResponse` (`internal/httpapi/handler_ports.go:196-201`)
- evidence: read; `compaction_settings_test.go:14` asserts the `"append"` default, so the decision text is the wrong side.
- suggested fix: correct the DECISIONS line and the tool description ("the raw transcript as it was before each compaction; append keeps every snapshot"), and return `backup_path` (or `backup_written`) in `CompactionResponse` so a client knows where to look.
- effort: S
- prior art: new

## 5. Checked and sound

- Compaction aborts when the backup write fails (`maintenance.go:158-161`), and the backup is taken before the cache write and before `ReplaceRecords`, so no ordering hazard.
- A compaction that would not reduce the history returns early (`maintenance.go:151-153`) before touching the backup, so no spurious snapshots; `TestCompactHistoryNoOpWhenRecentTurnDoesNotReduceHistory:191-193` asserts no backup file is created.
- `min_turns` guard is `replace`-only and never blocks the compaction itself; `append` ignores it (`backup_min_turns_test.go:34-77`).
- The compaction cache (including the retained history after a compaction) is written twice, primary and `.backup`, atomically (`maintenance.go:820-836`).
- Session fork copies the backup to the new session (`app/session_fork.go:250-252`), so a fork is not left with a stale/missing history block.
- Backup durability follows `persistence.durability`; at `process` it is not fsynced, which matches the journal's own rule (`maintenance.go:665-672,686-690`).

## 6. Not covered

- No real session on this machine has a `messages.backup.jsonl` (searched to depth 6), so no real-file measurement of size/growth; all growth numbers are read-level.
- Did not review `internal/session/overflow_cut.go`, the summary path's interaction with backups, or session deletion with `?state=delete`.
- Did not run the `app`/`httpapi` integration tests for compaction (outside the question's scope), and did not benchmark.
- Probe file was written to `/tmp/rvcomp/` only; nothing under `backend-go/` was created or modified.
