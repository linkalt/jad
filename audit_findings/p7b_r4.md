# Phase 7b Native Tools Audit — Reviewer r4

**Phase:** p7b — native transcript and miscellaneous tools
**Reviewer:** r4
**HEAD:** `09139f5416974ef745399e0ae735d6058206e064`
**Date:** 2026-10-30
**Files read:** See the complete file-by-file list below; scope expanded to all non-test files matching the p7b phase-map globs at this HEAD.
**Commands:** See final command table; commands will be recorded only after execution.

## Understanding note

The p7b tools are registered in the native catalog and execute through `tools.Tool` implementations. Transcript access resolves a requested session/profile/run, then applies session/profile gates and a transcript-file policy before scanning/rendering or returning bounded detail; remote transcript sources use the corresponding remote resolver. Memory and task tools persist bounded structured data through their stores and serialization helpers. Hooks expose event/run integrations, while skill/catalog/request-draft provide discovery, native definitions, and user-decision requests. Cross-session transcript authorization and child-run gating are the primary trust boundaries.

**Scope note:** The section 3.2 p7b globs expand at this HEAD to 22 non-test files (7,871 lines by `wc -l`), not the prompt's stated 12 files/about 5,300 lines. I am covering every file matched by the phase map, including shared memory/task/hooks helper files, rather than silently excluding matches.

## Hypotheses

- H1 (assumed): Local transcript selectors cannot bypass session/profile gates by alternate path, symlink, remote source, or forged run identifier.
- H2 (assumed): Transcript list/detail/step/cut operations impose effective bounds on untrusted arguments and file sizes, and propagate cancellation.
- H3 (assumed): Memory/task persistence stays within configured bounds and is robust to malformed/partial files and concurrent calls.
- H4 (assumed): Hook and request-draft callbacks have bounded lifetimes, cancellation, and safe error behavior.
- H5 (assumed): Catalog/skill definitions and registration do not expose tools contrary to configuration or create schema/dispatch inconsistencies.
- H6 (assumed): Shared helpers do not add repeated full-store scans or unbounded allocations on common operations.

## Findings

### P7B-R4-01 Memory JSON writer double-escapes string delimiters and persists invalid JSON
- severity: high
- lens: A correctness
- location: backend-go/internal/tools/native/memory_json.go:373-388; backend-go/internal/tools/native/memory_store.go:176-199, 246-265
- evidence: `quoteMemoryString` writes raw literals `\\"`, `\\\\`, etc., adding two backslashes where JSON requires one. I ran a `/tmp` overlay probe (`go test -overlay /tmp/r4/overlay.json ./internal/tools/native -run '^TestR4MemoryJSONStringEscapes$' -count=1`): valid create/update input `{"text":"a\\\"b"}` serialized to malformed JSON and reparsing failed (`invalid character 'b' after object key:value pair`, exit 1). `CreateValue`/`UpdateValue` persist `memoryJSONPretty` output, so such records become unreadable on later `Get`/`List`. Existing memory JSON test exercises ordering but no escaped string (memory_json_test.go:5-17). [measured]
- suggested fix: replace manual escaping in `quoteMemoryString` with a correct JSON string encoder while preserving the intended JS-compatible escaping; add quote, backslash, and control-character round-trip tests in memory_json_test.go.
- effort: S (under 1 h)
- prior art: new

## Performance hypotheses (for measurement)

- H1: Transcript list mode indexes and scans the complete selected transcript and materializes every step before selecting at most `MaxListEntries`/`ResponseMaxBytes`; `transcript.go:304-305,362-370`, `transcript_steps.go:47-48,87,198-239`, `transcript.go:374-399`. The complete scan is bounded by configured `ScanMaxBytes` and timeout.
  workload: A 100 MiB transcript / hundreds of thousands of JSONL records, default whole-history listing, with 100-entry output page.
  benchmark: Proposed `BenchmarkNativeTranscriptListLarge` using temp-file fixture with (e.g.) 10k vs 100k realistic bounded transcript records; compare allocations and time per tool call.
  expected: CPU and heap scale with all selected records even when the first page output is capped; cursor paging re-scans/indexes the file each page.
- H2: Memory create/update quota accounting walks every record filename across all memory types under the store root lock on each write; `memory_store.go:163-180,230-265,611-654` (size cache avoids restatting previously seen names, but directory/name enumeration remains proportional to record count).
  workload: 10k records across allowed types, sequential creates/updates and several catalogs sharing one root.
  benchmark: Existing `BenchmarkMemoryStoreCreate`/`BenchmarkMemoryStoreCreateMany` if present in `memory_benchmark_test.go`; otherwise proposed `BenchmarkMemoryCreateWithExistingRecords` at 100, 1k, 10k seeded records.
  expected: Directory enumeration and lock-held work scale with total records per mutation.

## Cross-phase leads

- None confirmed outside the p7b scope. The direct `runBashCommand` helper is in the 7a/6b scope; it uses configured environment/process-group limits and bounded output.

## Checked and sound

- Transcript child reads are rejected at execute entry when `AgentDepth > 0` and `AllowChildren` is false (`transcript.go:188-191`; also `transcripts.go:168-177`); cross-session and cross-profile flags are applied in resolver/gate (`transcript_path.go:160-168,220-229`; `transcripts.go:238-247`).
- Local transcript path selectors run through workspace/policy/symlink resolution and transcript-shape checking (`transcript_path.go:91-105`; `path.go:23-75` indexed snippet). Session/profile transcript reads do not expose rejected paths through arbitrary selector grammar.
- Remote transcript selection validates name and profile allowlists, omits remote file-path selector support, forwards a fixed argument whitelist, and increments remote hop count (`transcript_remote.go:59-107,247-279`; `remote/run.go:181-196` indexed snippet).
- Transcript file scans are read-only, LF terminated, cancellable, per-line bounded; total transcript scan is capped and step detail output is paged (`transcript.go:295-305`; `transcript_scan.go:39-89`; `transcript_detail.go:77-93,101-145`).
- Task inputs are normalized then re-validated with action-specific allowed keys, state/priority/text/array limits and bounded list response (`task.go:102-221,224-449`). Store mutation serializes under mutex, validates stored document, and atomically renames temp output (`task_store.go:207-232,352-433`).
- Memory store enforces type/ref grammar, JSON depth/record/total quotas, cursor bounds/TTL and symlink-safe records, with a per-root PathLocks key (`memory_store.go:128-155,163-205,498-524,527-569,657-705,762-900`). Escaped memory JSON string serialization was disproved by the overlay probe and is P7B-R4-01.
- Hook callbacks pass through bounded/cancellable child execution; pre-tool blocking is explicit and event payloads are capped (`hooks.go:111-174`; `hooks_events.go:150-225`; indexed `runBashCommand`).
- Request drafts validate both byte and depth caps before dispatch and pass the same caps into `InteractionQuestion` (`request_draft.go:57-114`). Headless default waits on configured timer but stops promptly on context cancellation (`117-126`).
- Catalog requires a manifest definition whose source/name/schema matches each enabled tool's in-code contract (`catalog.go:243-479,529-550`).
- Targeted transcript/security, memory, task, request-draft and catalog tests passed (see commands below).

## Not covered

- No benchmark or profile was run (reviewer measurements are prohibited); H1 and H2 remain hypotheses.
- No `-race`, full test suite, external remote server, real provider, real filesystem race/TOCTOU adversary, or Windows-specific execution was run.
- No full end-to-end HTTP transcript-token holder path was exercised; authorization was traced from tool selector/gates and the app token-profile wrapper snippet.

## Commands run

| Command | Exit | Result |
|---|---:|---|
| `cd backend-go && go build ./internal/... ./cmd/...` | 0 | Passed |
| `cd backend-go && go vet ./internal/... ./cmd/...` | 0 | Passed |
| `cd backend-go && gofmt -l internal/tools/native` | 0 | No output (clean) |
| `cd backend-go && go test ./internal/tools/native -run 'TestTranscriptRefusesAChildWhenAllowChildrenIsOff|TestTranscriptDoesNotReadAnotherSessionsTranscriptWithoutPermission|TestTranscriptPathMustStayInsideWorkspace|TestMemory' -count=1` | 0 | Passed |
| `cd backend-go && go test ./internal/tools/native -run 'TestTranscript.*(Session|Profile|Child|Remote|Path)|TestTranscripts.*(Session|Profile|Run|Remote)|TestBuildCatalogLoadsConfiguredRequestDraft|TestRequestDraft|TestTask' -count=1` | 0 | Passed |
| `/tmp` overlay escaped-string repro: `go test -overlay /tmp/r4/overlay.json ./internal/tools/native -run '^TestR4MemoryJSONStringEscapes$' -count=1` | 1 | Correctly failed: serialized valid quoted string was not parseable JSON; demonstrates P7B-R4-01. |
| `git log --oneline -12 -- backend-go/internal/tools/native/{transcript.go,transcript_path.go,transcript_remote.go,transcripts.go,transcripts_list.go,transcript_steps.go,memory.go,memory_group.go,memory_json.go,memory_store.go,task.go,task_store.go,hooks.go,hooks_events.go,hooks_run_events.go,skill.go,catalog.go,request_draft.go}` | 0 | Recent scoped history reviewed. |
| `find`/`wc -l` scope enumeration for non-test files matching transcript*/memory*/task*/hooks*, skill.go, catalog.go and request_draft.go | 0 | 22 files, 7,871 lines at this HEAD. |

## File-by-file coverage

- `backend-go/internal/tools/native/transcript.go` — read full (lines 1–481); constructor, argument/cursor handling, size gate, scan/render, path-policy helpers reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_cut.go` — read full (lines 1–81); bounded truncation and step/range parsing reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_detail.go` — read full (lines 1–145); detail mode bounds, range selection, pagination, and cursor creation reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_path.go` — read full (lines 1–243); profile/session parsing, child-run owner and allow_children gates, cross-session/profile flags, path policy and manifest/live-run lookup reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_remote.go` — read full (lines 1–329); remote name/profile allowlists, cursor wrapping, forwarded argument filtering, hop count, timeout and remote error rendering reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_render.go` — read full (lines 1–51); transcript text extraction and image payload scrubbing/markers reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_scan.go` — read full (lines 1–141); LF-only bounded line scan, cancellation, turn indexing and cursor identity reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcripts.go` — read full (lines 1–248); list-level parsing, child gate, cursor data, and session/profile access gate reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcripts_list.go` — read full (lines 1–465); session/profile/run listing, path-policy filtering, owner gates, symlink containment, manifests/live-manager merge, scan caps and pagination reviewed. No finding confirmed at this point.
- `backend-go/internal/tools/native/transcript_steps.go` — read full (lines 1–344); record parsing, call/result association, list step materialization, search/context selection and argument truncation reviewed. No correctness/security defect confirmed; full-range list materialization is a performance hypothesis below.
- `backend-go/internal/tools/native/memory.go` — read full (lines 1–470); operation/schema validation, type/ref/limit handling, error rendering, and state-context collection reviewed. Finding P7B-R4-01 confirmed through its storage helper.
- `backend-go/internal/tools/native/memory_group.go` — read full (lines 1–94); bounded grouping, scan budget and deterministic ordering reviewed. No finding confirmed.
- `backend-go/internal/tools/native/memory_json.go` — read full (lines 1–447); ordered JSON parsing, JS-compatible writer, numbers and canonical fingerprints reviewed. Finding P7B-R4-01.
- `backend-go/internal/tools/native/memory_store.go` — read full (lines 1–1062); locking, CRUD persistence, caps, cursors, quota scans, path safety and atomic writes reviewed. Finding P7B-R4-01.
- `backend-go/internal/tools/native/task.go` — read full (lines 1–703); action inference/aliases, input validation, per-action exact-key checks, store calls and bounded output/state-context reviewed. No finding confirmed.
- `backend-go/internal/tools/native/task_store.go` — read full (lines 1–520); task JSON compatibility, mutex, validation, capacity, atomic persistence and clone behavior reviewed. No finding confirmed.
- `backend-go/internal/tools/native/hooks.go` — read full (lines 1–246); post-edit callback gating, cwd resolution, argv substitution, bounded output, timeout/kill configuration and file change notice reviewed. No finding confirmed.
- `backend-go/internal/tools/native/hooks_events.go` — read full (lines 1–258); pre/post callback ordering, blocking semantics, payload clipping, event output wrapping and process result handling reviewed. No finding confirmed.
- `backend-go/internal/tools/native/hooks_run_events.go` — read full (lines 1–78); run event dispatch and child callback reporting reviewed. No finding confirmed.
- `backend-go/internal/tools/native/skill.go` — read full (lines 1–242); live directory resolution, name normalization, context propagation, UTF-8 clipping and list/detail rendering reviewed. No finding confirmed.
- `backend-go/internal/tools/native/catalog.go` — read full (lines 1–632); native tool inclusion, transcript/dependent tool wiring, shared stores/locks, manifest schema checks, hook wrappers and remote registration reviewed. No finding confirmed.
- `backend-go/internal/tools/native/request_draft.go` — read full (lines 1–591); draft size/depth/title validation, headless timeout and cancellation, interaction answer, JSON serialization, changed-path calculation reviewed. No finding confirmed.
- Shared helper `runBashCommand` (`backend-go/internal/tools/native/bash_child.go:66-283`) — indexed full snippet; hook children inherit configured filtered environment, execute in isolated process groups, observe context/timeout, bound collection, and perform termination/draining. No p7b issue confirmed; helper belongs to 7a/6b scope.

All 22 files matched by section 3.2 p7b globs at this HEAD were read in full with exact ranges. The prompt stated 12 files/about 5,300 lines, but the phase map's `transcript* memory* task* hooks*` globs include these additional helper files; total actual coverage is 22 files, 7,871 lines.
| `transcript.go` | 481 | Full |
| `transcript_cut.go` | 81 | Full |
| `transcript_detail.go` | 145 | Full |
| `transcript_path.go` | 243 | Full |
| `transcript_remote.go` | 329 | Full |
| `transcript_render.go` | 51 | Full |
| `transcript_scan.go` | 141 | Full |
| `transcripts.go` | 248 | Full |
| `transcripts_list.go` | 465 | Full |
| `transcript_steps.go` | 344 | Full |
| `memory.go` | 470 | Full |
| `memory_group.go` | 94 | Full |
| `memory_json.go` | 447 | Full |
| `memory_store.go` | 1,062 | Full |
| `task.go` | 703 | Full |
| `task_store.go` | 520 | Full |
| `hooks.go` | 246 | Full |
| `hooks_events.go` | 258 | Full |
| `hooks_run_events.go` | 78 | Full |
| `skill.go` | 242 | Full |
| `catalog.go` | 632 | Full |
| `request_draft.go` | 591 | Full |

## Hypothesis status

- H1: verified as an algorithmic property by the call/data flow; severity only raised as an unmeasured performance hypothesis, not a defect.
- H2: verified as an algorithmic property by `totals` file enumeration and invoked under `withLock`; retain only as an unmeasured measurement hypothesis.
- H3: disproved as an unconditional claim: escaping strings break the store serialization path; see P7B-R4-01.
- H4: no defect found in reviewed hook/request-draft paths.
- H5: no defect found in reviewed catalog/skill dispatch and manifest binding.
- H6: memory quota scan does proportional directory/name work, as H2; otherwise no additional repeated-work defect confirmed.

**Finding-format check:** each finding includes all required 5.1 fields (severity, lens, location, evidence, suggested fix, effort, prior art).

## Conclusion

Full phase-map file coverage is complete: all 22 matching source files (7,871 lines) were read in full. One high-severity correctness finding is confirmed. The prompt's “12 files/about 5,300 lines” figure did not match the p7b glob expansion at this checkout; the report follows the map and includes every matching file.
