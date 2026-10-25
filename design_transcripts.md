# Design: `transcripts` (list what `transcript` can read)

Status: design. Facts below were read in code (file:symbol); nothing here is a guess unless marked **[unverified]**.

## 0. Goal
A read-only native tool that LISTS profiles of a session, sessions, and sub-agent runs, and for each item prints (a) the exact `path` string to give the `transcript` tool and (b) the full `messages.jsonl` file path. `transcript` has no `list` on purpose (its not-found error suggests names); this is a separate tool. No message counts: only size and mtime (counting needs a scan of every file).

## 1. What exists and is reused (verified)
- Path grammar and canonical strings: `transcript_path.go` `resolve`. Canonical forms it returns: `profile:<session>/<category>/<name>`, `subagent:<id>` (own profile), `subagent:<session>/<category>/<name>/<id>` (other). The listing prints exactly these, so a listed string always parses.
- Profiles of a session and a profile's files: `TranscriptProfiles.Candidates` (profile IDs of a readable layout session) and `.Profile` (-> `TranscriptTarget{Session, Profile, Files{Messages,Backup}, SubagentDir, Runs}`), implemented by `app/transcript_profiles.go` without creating files. Sessions: `sessionfs.ListSessionFiles(roots)`.
- Runs: manifests `<SubagentDir>/.runs/<id>.json` (`subagentStore.load`, read-only: ReadDir/ReadFile, drops a `dir` outside the storage dir) and the live `SubagentManager` (`RunTranscript`, `ownedRuns`; a persistent manager already loads manifests). Manifest fields used: id, owner, agent, task (80-byte preview), state, started_at, ended_at, dir. The parent tool call is the run folder name minus its `-<8 hex>` suffix (`app/subagent.go openStore`: `<call-id>-<hex of 4 bytes>`).
- Filters: `toolSmithFilter` / `toolSmithMatchesFilters` (substring, or `*`/`?` wildcard, case-insensitive, all filters must match name or description).
- Access: the same `allow_other_sessions`, `allow_other_profiles`, `allow_children` rules as `transcript_path.go` (`profileRefusal`, owner check for runs), the read path rules `transcriptDenied`/`deniedFile`, and `scopedPolicy` for a sub-agent's narrowed scope.
- Settings pattern: `sessionfs/native_transcript_settings.go`; registration points: `tool_shape.go nativeToolNames`, `session.go`, `native/catalog.go` (two places + `nativeToolConfigured`), `sample/tools/native/*.json`, `profile.schema.json` pattern, `setting.schema.json`, schema agreement test.

## 2. Decisions
1. **Own switches, same names and meaning as `transcript`** (`allow_other_sessions`, `allow_other_profiles`, `allow_children`, all default true) under `tools.native.transcripts`. Reason: the two tools can be enabled separately, so listing must not need `transcript` configured. Consequence, documented: a listed path may still be refused by `transcript` if its switches are stricter. No `allow_path` (the `path:` kind is not listed).
2. **Level by `path`** (none, or `kind:rest`; nothing else accepted):

| `path` | lists |
|---|---|
| (none) | profiles of the caller's session |
| `sessions` | sessions (needs `allow_other_sessions`; if off, only the caller's session) |
| `session:<id>` | profiles of that session |
| `profile:<category>/<name>` or `profile:<session>/<category>/<name>` | that profile's sub-agent runs |

Default is the current session's profiles, not sessions: it is what a profile asks for most, and `sessions` is one more call. Segment validation reuses `splitTranscriptName`.
3. **Switches off** (`allow_other_profiles` false): the profiles level shows only the caller's profile; runs level only for the caller's profile. `allow_other_sessions` false: `session:<other>` and `profile:<other>/..` are refused with the same codes as `transcript`; `sessions` shows only the caller's session. `allow_children` false and caller is a sub-agent: refused. A run owned by another session key is listed only under the owner rules `transcript` applies to reading it (not listed otherwise; the header counts how many were hidden).
4. **Input** (flat, `additionalProperties:false`, the validator names an unknown key): `path`, `filters` (array of strings), `status` (runs level only: queued, running, done, failed, timed_out, stopped, lost; an error elsewhere), `limit` (1..max_entries), `cursor` (not with `path`, `limit` may change). Old/odd keys are not specially rejected beyond the schema.
5. **Order and cursor**: profiles and runs newest first by last activity (mtime of `messages.jsonl`; for a run its ended/started time when the file is missing), ties by name. Sessions alphabetical (no cheap activity). Cursor = base64 JSON `{level path, last activity ns, last key}`; keyset paging (items strictly after it), so a profile appearing or a file changing between pages never repeats or loses an unrelated item. The cursor carries the level string and is rechecked against the current switches like `transcript`'s.
6. **Output**: plain text; header line, legend line (only when there are items, as `tool_smith`), then one line per item, fields separated by ` | `:
   - profile: `profile dev/coder | active <RFC3339 UTC> | 1.2 MB | backup yes | path profile:demo/dev/coder | file /abs/.../messages.jsonl` (`no transcript yet` replaces active/size when the file does not exist)
   - run: `run sub_ab12cd34 | done | agent researcher | started <t> | ended <t> | call call_0x_1 | 340 KB | path subagent:sub_ab12cd34 | file /abs/.../messages.jsonl`
   - session: `session demo | path session:demo` (what to give this tool; sessions have no transcript of their own).
   Header: `profiles of session demo: 3 of 3` / `sub-agent runs of profile:demo/dev/coder: 2 of 2` plus a cursor in INFORMATION when more. When `persist_runs` is off for the profile the header adds: persist_runs is off, so finished runs have no manifest and only runs the running server still holds are listed. A profile with no sub-agent storage says so. Missing directories list as empty, never an error. No `transcript` INFORMATION key: many files, each line has its path.
7. **Safety**: read-only (`os.Stat`/`Lstat`/`ReadDir`/`ReadFile` of manifests; never opens a transcript, never the Journal reader). A `messages.jsonl` that is a symlink is shown only if its resolved target stays inside the profile state dir / sub-agent storage dir, else the item is listed as `unreadable (outside the state root)` with no size. Items the read path rules deny (`transcriptDenied`, same cases as `transcript`) are not listed and are counted as hidden. Per call: `timeout_ms` (cap 6 h), ctx checked in every loop.
8. **Caps** (`tools.native.transcripts`, unknown key rejected naming it): `max_entries` (default 50, cap 500) page size; `scan_max_entries` (default 2000, cap 50000) most candidate items examined after filters; more -> error `too_many` telling to add filters; `timeout_ms` (default 30000, cap 21600000); the three `allow_*`. Read policy comes from `tools.native.read.paths` like `transcript`.
9. **Port**: `TranscriptProfiles` is reused for profiles; one new small optional interface `TranscriptSessions { Sessions(ctx) []string }` implemented by `profileMessenger`. `TranscriptTarget` gains `PersistRuns bool`. No other tool changes.
10. Not covered: non-catalog (non-layout) sessions (`Profile` already refuses them: the tool answers `not_a_catalog_session`); a profile that is not in the session's profile list; sub-agent runs of a profile without `tools.native.subagent`.

## 3. Phases (one commit each, tests with each)
1. This document.
2. Settings + registration + schema + sample + tool skeleton (input validation, path grammar, access checks, cursor codec). Tests: settings keys/caps, schema agreement, 6 h cap, catalog load from the sample, unknown key rejected, bad path forms.
3. Profiles and sessions levels: ordering, filters, limit/cursor, missing dirs, switches, symlink escape, path-rule denial.
4. Runs level: manifests + live manager merge, status filter, parent call, persist_runs note, owner rules, app wiring (`PersistRuns`, `Sessions`).
5. Concurrency (appends/renames during listing, `-race`), goroutine leak, never-writes check, DECISIONS.md, full suite.
Revert checks for: keyset cursor, symlink-escape check, switches, missing-dir-is-empty, no-write.
