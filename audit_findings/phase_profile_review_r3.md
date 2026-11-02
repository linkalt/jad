# Profile tool audit — reviewer findings (6 commits 5ad5bea..41a8bec)

1. **HIGH — native.profile `compact` bypasses the mutation gate, session/process locks and the run check.**
   `app/profile_actions.go:38-43` calls `compactTarget` (`app/profile_message.go:181-193`, comment "The caller holds the target's transcript" is false for the tool caller) with no `gate.Begin`, no `lockSessionProcess`, no `runActive` check. The HTTP compact route does all three (`httpapi/mutation_handlers.go:24-46`); `ResetProfile` does too (`app/profile_reset.go:35-80`). Only in-process `runMu` serialises; a cross-process run is not excluded. Also doc drift: `tools/native/profile.go:26` ("refused while a run of the profile is active") and DECISIONS.md claim a compact refusal that does not exist — it blocks on `runMu`. Evidence: git trace + snippets.

2. **MEDIUM — `allow_children:false` not enforced (missing refusal).**
   `setting.schema.json:1533` ("false refuses the tool to a sub-agent") and `sessionfs/native_profile_settings.go:29-31` document it; `tools/native/profile.go` never reads `AllowChildren` (only unconditional AgentDepth compact/reset check :146). Probe (`go test -overlay`, /tmp, removed; exit 1 = gap): sub-agent `list` served under `allow_children:false`. Parity: `transcripts.go:169` refuses. No test.

3. **MEDIUM — `allow_other_sessions` / `allow_other_profiles` unenforced for info/compact/reset and cross-session list.**
   Schema: "false lists and acts on only the calling session/profile". `profile.go` reads only `AllowOtherProfiles` (list filter :271); `AllowOtherSessions` is never read. Probe: `reset profile:other-session/dev/reviewer` reached the port under `allow_other_sessions:false`; `compact profile:dev/reviewer` under `allow_other_profiles:false`. Only `TestProfileListHonoursAllowOtherProfiles` exists.

4. **LOW — stale schema/docs.** `sample/schemes/root-setting.schema.json:232` token `actions` list omits `profile` (added `remoteapi/tokens.go:33-37`); no test pins it. `setting.schema.json` profile blurb ("act on the caller's session only") contradicts its own `allow_other_sessions` text; DECISIONS.md compact-refusal claim stale (see 1).

5. **LOW — lost coverage / lock-key regression.** (a) `TestTranscriptsLimitIsCappedByTheConfiguration` deleted (f1d91cd), no counterpart — `profile.go:127-130` cap untested. (b) `TestResetRejectsAnActiveRunForResolvedProfile` deleted (5ad5bea); replacements run and reset with one spelling, so the resolved-key `RunActive` lookup lost coverage. Related: `app/profile_reset.go:59` locks the raw `reference` while the old handler canonicalised (`httpapi/process_lock.go:56-60`); HTTP reset via a non-canonical spelling can take a different session lock than other paths.

Accepted gate-order gap: **confirmed OK** — `profile_reset.go:45-52` deferred `r.gate.End(key)` runs before `processUnlock()`; not re-reported.

Traced: each commit's callers/dependents (sessiongate move incl. `SetRunActive` wiring, settings normalizer/schema/drift test, remoteapi route/action/tokens/remote schema, profile tool incl. catalog, transcripts `profiles_moved` redirect, `remote.Client.ProfileInfo`, app `ProfileInfo/Compact/Reset` + remote backend, DECISIONS). Tests (all exit 0): sessiongate, sessionfs, tools/native (101s), remote, remoteapi, httpapi (17s), app (41s); `go build ./...` ok. Not covered: frontend TS, messages.jsonl golden byte diff (record didn't run it either).
