# Profile tool phase — gap review (read-only)

Phase: profile tool change `89ee723..41a8bec` (HEAD `41a8bec`, 6 commits). Reviewer: autonomous reviewer, read-only.
Inputs read: `audit_findings/design_profile_tool.md`, `audit_findings/phase_profile_record.md`, `audit_plan.md` §1.1/4/5, `backend-go/DECISIONS.md:1251-1255`.
Accepted, not re-reported: gate-order mutation not caught (design_profile_tool.md).

Commands (all exit 0): `go build ./...`; `go vet ./internal/{app,httpapi,sessiongate,sessionfs,tools/native,remoteapi,remote}/`;
`go test -count=1 ./internal/sessiongate/`; `./internal/app/ -run 'TestReset|TestHTTPResetIsRefused'`;
`./internal/tools/native/ -run TestProfile`; `./internal/remoteapi/ -run TestProfile`; `./internal/sessionfs/ -run TestProfile`;
`./internal/httpapi/ -run 'TestMaintenanceClearsItsMutationGateBeforeReleasingTheProcessLock|TestCompactAndResetRoutes'`.
Probe (fails on purpose, `/tmp/rp/`, `go test -overlay`): P-01.

## Findings

### P-01 (high, B/D) `tools.native.profile.allow_children` and `allow_other_sessions` are loaded, documented and never enforced
- location: `backend-go/internal/tools/native/profile.go:116-176` (execute), `:253-300` (list); settings `internal/sessionfs/native_profile_settings.go:27-31,71`; schema `sample/schemes/setting.schema.json:1529-1534`; doc `DECISIONS.md:1254`
- evidence: [reproduced] `grep -n 'AllowOtherSessions\|AllowChildren' internal/tools/native/profile.go` → nothing; the only switch read is `AllowOtherProfiles` (`profile.go:271`). Siblings do enforce both: `transcripts.go:169`, `transcript.go:191`, `transcript_remote.go:290` (AllowChildren), `transcripts_list.go:265`, `transcript_path.go:229` (AllowOtherSessions). Probe with `AllowOtherSessions:false` → `{"action":"info","path":"profile:other/dev/reviewer"}` answers `profile dev/reviewer | context tokens: 9` and `{"action":"reset",…}` answers `reset profile dev/reviewer` (the port is reached); with `AllowChildren:false` a sub-agent (`AgentDepth:1`) info answers `context tokens: 9`. Both probe tests FAIL.
- consequence: an operator's `allow_other_sessions:false` still lets a profile read *and reset* another session's profile transcript; `allow_children:false` still lets a sub-agent list and read. Also lost coverage: the deleted `transcripts_list_test.go` case `TestTranscriptsNotFoundHidesOtherSessionsWhenTheyAreOff` has no counterpart.
- fix: refuse in `execute` like `transcripts.go:169` (`AgentDepth>0 && !AllowChildren` → StatusForbidden) and when `session != callerSession` (info/compact/reset) with `!AllowOtherSessions`; hide other sessions in `list` and in the `no_such_session` information. Files: `internal/tools/native/profile.go`.
- effort S. prior art: new.

### P-02 (medium, A) `ResetProfile` takes the in-process session lock on the uncanonicalised reference (regression)
- location: `backend-go/internal/app/profile_reset.go:59` (`r.gate.Lock(reference)`), vs `internal/httpapi/process_lock.go:53-62` (`lockSession` canonicalises) and the removed `mutations_handlers.go` line `unlock = h.lockSession(reference)`
- evidence: [read] trace. `sessiongate.keyedLocker.lock` (`sessiongate/gate.go:100-123`) keys a map by the raw string. Every other per-session path goes through `h.lockSession` (`catalog_sessions_handler.go:81,115`, `settings_handlers.go:28`, `start_turn.go:118`, `handler.go:497`, `front_compact_handler.go:44`, `state_context_handler.go:57`), which resolves `CanonicalSessionReference`. `sessionfs.Resolve` maps `local:demo`→`demo` with one root and `demo`→`root:demo` with two (`roots.go:280-322`, `internal/app/canonical_reference_test.go:9-19`). The cross-process lock is still canonical (`app/resolver.go:296-304`), so only in-process exclusion is lost: `POST /api/sessions/demo/reset` no longer excludes a concurrent `DELETE /api/sessions/demo` (or a settings save) for the same session.
- fix: pass the resolved reference — `r.gate.Lock(resolved.Reference)` (or canonicalise through the same helper). File: `internal/app/profile_reset.go:59`.
- effort S. prior art: new (introduced by 5ad5bea).

### P-03 (medium, A/D) the tool's `compact` action takes none of the maintenance locks the HTTP route takes
- location: `backend-go/internal/app/profile_actions.go:38-44` → `internal/app/profile_message.go:179-191` (`compactTarget`: resolve, `Session.Compact`), vs `internal/httpapi/mutation_handlers.go:23-42` (gate + `lockSessionAndTranscript`)
- evidence: [read] trace. `handleSessionCompact` takes the mutation gate and `lockSessionAndTranscript` (`start_turn.go:112-140`: turn queue + session lock + `sessionlock` transcript lock, waiting/refusing on `ErrSessionBusy`) before `resolved.Session.Compact`. `profileMessenger.CompactProfile` takes none of them; `boundMutation.Compact` only takes the in-process `shared.runMu` (`maintenance.go:78`), which a run also holds (`bound_runner.go:31-35`), so in-process a compact call *blocks* until the run ends instead of being refused, and no cross-process transcript lock is taken at all — the hazard the HTTP route exists to avoid. Contradicts `profile.go:26` input-schema text ("refused while a run of the profile is active"), `DECISIONS.md:1254` and `setting.schema.json:1518` ("through the same maintenance the HTTP routes use").
- fix: give the action the route's locking (extract a `CompactProfile` on `Resolver` that mirrors `handleSessionCompact`: gate, `lockSession`, transcript lock, active-run/`ErrGateHeld` refusals). Files: `internal/app/profile_actions.go`, `internal/app/profile_message.go`.
- effort M. prior art: new.

### P-04 (low, D) schema/doc drift for the new settings and the token action
- `sample/schemes/setting.schema.json:1549` — the new `tools.native.profile.allow_remote` description is the transcripts text verbatim ("lets the tool list the transcripts … remote:<name>/subagent:…, remote:<name>/session:…", "the remote's own token must also allow transcripts there"). The profile tool takes info only on `remote:<name>/profile:<session>/<cat>/<name>` (`profile.go:181-205`) and the remote token needs the `profile` action (`remoteapi/server.go:688`).
- `sample/schemes/setting.schema.json:1473` — the `tools.native.transcripts` description still says it lists "the profiles of a session"; that path now answers `profiles_moved` (`transcripts_list.go:55`). def84d7 added the profile block without updating it.
- `sample/schemes/root-setting.schema.json:232` — the token-table example still lists `"actions":["message","subagent","status","stop","transcripts"]`; `profile` was added to `remoteapi/tokens.go:37` and to `DECISIONS.md:1044` but not here.
- evidence: [read] grep. effort S.

### P-05 (low, D) dead duplicate `keyedLocker` left in httpapi
- location: `backend-go/internal/httpapi/settings_handlers.go:85-124`; the identical code now lives in `internal/sessiongate/gate.go:90-123`.
- evidence: [read] `grep -rn keyedLocker --include=*.go .` → only these two declarations, no use of the httpapi one (the `Handler.sessionLocks` field was removed in 5ad5bea). `go vet` does not report unused types. effort S.

### P-06 (low, D) dead branch in the profile path check
- location: `backend-go/internal/tools/native/profile.go:117` — `path != "sessions"` lets `list` with `path:"sessions"` past the specific message, then `parseProfilePath("sessions")` fails into the generic `invalid_path`. No `sessions` form exists for this tool (the tool's schema offers none). evidence: [read]. effort S.

## Checked and sound (one line each)
- Gate key format is identical on both sides: `httpapi.runKey` `handler.go:829-831` and `app.sessionKey` `resolver.go:147-149` (both `ref\x00profile`).
- `WhileFree` holds `gate.mu` across `runs.beginFrom`; no path takes `runs.mu` then a gate method, so no lock-order inversion (`registry.go:326-359`, `mutation_gate.go:50-63`).
- Reset refusal coverage moved, did not vanish: `app/profile_reset_test.go:112-155` (409 at a live run, no wait), `profile_reset_order_test.go` (gate held while waiting, cleared after), `httpapi` fakes re-wired (`mutation_handlers_test.go:20-35`, `handler_test.go:739-742`).
- Reset gate order is correct in the code (`profile_reset.go:50-58`: `gate.End` before `processUnlock`/`unlock`).
- Reset route statuses preserved (`writeJSONError` maps `*StatusError`); only the uninitialised-provider text became the generic 500 (`profile_reset.go:41`).
- `native.profile` manifest and Go schema agree (`sample_manifest_sync_test.go`), profile defaults/bounds match transcripts (`native_profile_settings_test.go:22-45`), schema keys equal loader keys (`native_profile_drift_test.go`).
- Remote path: token action gate, key whitelist, GET-only, globs (`remoteapi/server.go:685-712`, `profile_route_test.go`); client retries reads only, one remote action (`remote/run.go:179-192`); remote can never compact/reset (`profile.go:182-184`, `app/remote_api.go:302-320`).
- Sample `profile.json` inputSchema == in-code schema; profile added to `tool_shape.go:13` and both `profile.schema.json` allow/deny/lazy patterns.

## Not covered
Frontend callers; `sample/` sessions beyond schema/tool files; the full `verify-go.sh` gate and `messages-golden-diff` (the record already states this); no run of the app-level probe for P-03 (would need a second process).
