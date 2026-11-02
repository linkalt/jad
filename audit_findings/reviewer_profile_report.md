# Profile tool audit (reviewer)

Scope: 5ad5bea, def84d7, 80d3da9, f1d91cd, 397e633, 41a8bec on 89ee723. Read-only.

## Findings

### F1: reset lock key mixes raw and canonical reference (Medium, unverified at runtime)
- backend-go/internal/app/profile_reset.go:59 `r.gate.Lock(reference)` uses the raw argument.
- profile_reset.go:45 `Begin` and :51 `End` use `key` = `sessionKey(resolved.Reference, ...)`, canonical.
- Old HTTP path (git show 89ee723:internal/httpapi/mutation_handlers.go, handleSessionReset) locked via `h.lockSession(reference)`, which canonicalises through `CanonicalSessionReference` (httpapi/process_lock.go:56-62).
- The HTTP reset route passes the bare path id (handler.go:384). With two or more roots, `makeLocation` canonicalises a bare id to `root:id` (sessionfs/roots.go:322, Resolve).
- Effect: a reset by bare id takes the in-process session lock under a different key than a run start (`tryStartRun`, handler.go:497, canonical). The lock may not exclude a concurrent run start. The process lock and the run-active check still apply, so the gap is narrower than a full bypass. Not reproduced: my two-root probe returned before the lock (session not a catalog layout), so the runtime result is not established.
- Fix direction: canonicalise `reference` (or use `resolved.Reference`) before `gate.Lock`, as `httpapi.lockSession` does.

### F2: transcripts schema says "Omit it" for runs, but an empty path now returns profiles_moved (Low, stale doc)
- sample/tools/native/transcripts.json:15 and backend-go/internal/tools/native/transcripts.go:21 description: "Omit it, or use path profile:... for the sub-agent runs of one profile".
- transcripts.go parse: empty path -> kind "profiles" (transcripts_list.go:55) -> `profiles_moved`. Test transcripts_redirect_test.go:12 confirms `{}` redirects.
- The manifest sync test (sample_manifest_sync_test.go) passes because it compares text with the Go schema, not the wording. Omitting path is no longer a run listing; the text should say so.

### F3: the httpapi reset error message for a nil Session changed (Low, behaviour)
- Old: 500 with "session maintenance provider is not initialized" (git show 89ee723 mutation_handlers.go:70).
- New: profile_reset.go:41 `errors.New(...)`, so writeJSONError returns a generic 500 "internal server error" (handler.go:977). The status is kept, the message is lost. Minor, no test covers it.

### F4: ProfileActions type assertion discards ok (Low, unhandled path)
- backend-go/internal/tools/native/catalog.go (f1d91cd): `actions, _ := messenger.(ProfileActions)`. A messenger without the methods gives nil, and the tool answers profile_unavailable at execute (profile.go:152), not at build. Acceptable by design (profile.go:80-81 doc), but a config with the tool on and no actions loads silently. Confirm with the owner whether that is intended.

## Traced and clean
- Gate sharing: httpapi newSessionGate (mutation_gate.go:9) takes resolver's Gate; app service.go:49-50 wires SetProfileRuns and SetRunActive; RunActive uses the same runKey (handler.go:829) as sessionKey (resolver.go:147). Same key scheme.
- Gate order: Begin before lock, End before unlock (profile_reset.go:45-58). Matches design note; accepted gap not re-reported.
- Remote: GET /profile is info-only (remoteapi/server.go handleProfile); ActionProfile is an explicit token action (tokens.go:37, no default-all); remote Client.ProfileInfo sends no compact/reset (remote/run.go:182).
- Schema drift: profile.json vs Go schema (sample_manifest_sync_test); profile/transcripts pattern lists in profile.schema.json match the catalog names.
- Remote info path: remoteBackend.ProfileInfo (app/remote_api.go:302) checks MayProfile and profile membership before reading.

## Commands (cwd backend-go)
- go build ./... : exit 0
- go vet (app, httpapi, sessiongate, remoteapi, tools/native, sessionfs, remote) : exit 0
- gofmt -l (same packages) : no output, exit 0
- go test -count=1 ./internal/sessiongate/ ./internal/remoteapi/ ./internal/remote/ ./internal/sessionfs/ : exit 0
- go test -count=1 ./internal/app/ ./internal/tools/native/ ./internal/httpapi/ : exit 0 (41s, 102s, 17s)
- go test -run TestTranscriptsProfileListingRedirectsToProfile : exit 0
- Probe (go test -overlay, /tmp/reviewer-profile/probe): panicked then returned early; not conclusive for F1. Removed from tree; git status clean for backend-go.

## Not covered
- F1 runtime reproduction (needs a two-root catalog layout fixture).
- Golden messages.jsonl diff (not run; record says the same).
- audit_findings/phase_profile_review.md not opened (another reviewer's file).
