# Profile tool phase commit record

Split of the profile change, committed as A to F on top of 89ee723 (F is the docs commit that carries this record). Groups are stacked in order A to F; each row's build/test is the stacked result, run in a fresh `git worktree` at HEAD with each group's files copied on top of the previous ones. The ignored generated file `backend-go/internal/tools/native/html-to-markdown-rs_cgo_link.go` was copied into the worktree (a link needs it; it is not tracked). Excluded from all commits: EXECUTOR.md, MASTER.md, audit_findings/p6a_r4.md.

Golden contract (messages.jsonl): the sessionfs and tools/native fixture tests ran in the stacked builds; the messages-golden-diff scenario byte diff was not run.

| commit | purpose | finding IDs | impact flag | golden checked | build | vet | test |
|---|---|---|---|---|---|---|---|
| 5ad5bea | reset refactor: sessiongate gate and locks, httpapi gate move, app ResetProfile; mutation_gate_test coverage moved to app | none (design note: gate-order gap, accepted) | shared (mutation gate, process lock, Begin/End) | match: fixture tests pass (not a scenario byte diff; see note) | 0 | 0 | 0 |
| def84d7 | settings: tools.native.profile keys, normalizer, schema entries, drift test | none | CONFIG (setting.schema.json, profile.schema.json, layout-settings.cases.json, tool_shape) | match: fixture tests pass (not a scenario byte diff; see note) | 0 | 0 | 0 |
| 80d3da9 | remote: profile token action and server route (GET /profile), remoteapi tests | none | output (HTTP route), CONFIG (remote.schema.json, tokens) | not checked: does not touch transcript/session/tools/app/checkpoint/sessionfs or testdata | 0 | 0 | 0 |
| f1d91cd | native.profile tool, catalog branch, transcripts redirect and sample, remote client call, remote_transcripts test | none | CONFIG (profile.json, transcripts.json), output (transcripts profiles_moved text), shared (catalog build, remote.Client) | match: fixture tests pass (not a scenario byte diff; see note) | 0 | 0 | 0 |
| 397e633 | app port: profileMessenger ProfileInfo/Compact/Reset, remote info backend, app tests | none | shared (ProfileActions on resolver), output (remote info text) | match: fixture tests pass (not a scenario byte diff; see note) | 0 | 0 | 0 |
| this commit (docs) | docs: DECISIONS redirect and token action entries; design note | none | none (docs) | not checked: docs only | n/a | n/a | n/a |

Commands per row: `go build ./...`, `go vet <changed packages>`, `go test -count=1 <changed packages>` (A: sessiongate, httpapi, app; B: sessionfs; C: remoteapi; D: tools/native, remote, app; E: app). Exit codes shown; all 0.

Order notes:
- Dependency order is forced: E uses `native.ProfileInfoText` and the D `remote.Client.ProfileInfo`; D's redirect test needs the app listing path, which is in D.
- The first stack attempt failed. Group C (tool tests) referenced `remoteapi.ProfileRequest` before the remote route existed, so the route moved ahead of the tool. A redirect test moved from E into D so the transcripts redirect and its test land together.
- Not verified: the stacked test run is per-commit at the package level, not the full `verify-go.sh` gate on each commit.
