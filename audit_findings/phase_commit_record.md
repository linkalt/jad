# Phase 1-8 commit record

HEAD at write: a014011..2950fa4 on top of 276dd6d. Source: /tmp/inv/commits.tsv (151 commits, 7ba102c..276dd6d), flags re-checked with `git show --name-only`.
Golden contract (messages.jsonl): fixture tests checked per row (see the golden column and the note under the table); the messages-golden-diff scenario byte diff was not run.

| hash | purpose | finding IDs | impact flag | golden checked |
|---|---|---|---|---|
| 276dd6d | MarkRunning never overwrites a terminal record | M22 | PERSIST (asyncmgr store) | match: fixture tests pass (not a scenario byte diff; see note) |
| ee3d1fb | recovery adopts terminal record over live index row | M01 | PERSIST (asyncmgr recover) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 35c1091 | Close shuts folder to later writers | M04 | PERSIST (asyncmgr store) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 51f35cf | cap retention_days, unread_ttl_days | P2A-01, M23 | CONFIG (setting.schema.json), PERSIST (prune) | match: fixture tests pass (not a scenario byte diff; see note) |
| daa0c13 | confine agent model file to folder | P2A-02 | CONFIG (agent, setting schema) | match: fixture tests pass (not a scenario byte diff; see note) |
| 336cdf4 | bounded regular-file prompt and env reads | P2A-03 | shared read path (readRegularFile) | match: fixture tests pass (not a scenario byte diff; see note) |
| 4edef7c | statecontext bounded reads | P2B2-10 | CONFIG (setting schema) | match: fixture tests pass (not a scenario byte diff; see note) |
| 3a0509a | anthropic-adapter caller_token_env | P5M-02 | CONFIG (anthropic-adapter schema) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 62b0d17 | cap one upstream SSE event | P5M-04 | CONFIG (max_event_bytes) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 0e24513 | MCP tools/list paging bound | P6AM-04, P6AM-08 | CONFIG (mcp-server schema) | match: fixture tests pass (not a scenario byte diff; see note) |
| e21314d | decide_systemone max_input_bytes | P6AM-09 | CONFIG (setting schema) | match: fixture tests pass (not a scenario byte diff; see note) |
| 3c14280 | process log stream cap 8 MiB (default change) | P6BM-13 | CONFIG (default) | match: fixture tests pass (not a scenario byte diff; see note) |
| ccbde3f | hide other sessions reasoning by default | P7B-MRG-07 | CONFIG (setting schema), tool output | match: fixture tests pass (not a scenario byte diff; see note) |
| c500efb | per-agent paths limits writes only (text) | P7A-16 | CONFIG (schema text only) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 556fa05 | bound library asset reads | P8-M-03 | CONFIG (root-setting schema), shared skill read | match: fixture tests pass (not a scenario byte diff; see note) |
| 4e228b3 | memory JSON escapes written once | P7B-MRG-01 | PERSIST (memory file bytes), output bytes | match: fixture tests pass (not a scenario byte diff; see note) |
| 2a3c4e5 | sync parent dir after transcript publish | P8-M-01 | PERSIST (transcript files) | match: fixture tests pass (not a scenario byte diff; see note) |
| 286c0f7 | remove compaction cache before empty publish | P8-M-02 | PERSIST (transcript) | match: fixture tests pass (not a scenario byte diff; see note) |
| 099989f | list does not create message dirs | N18 | PERSIST (transcript jsonl) | match: fixture tests pass (not a scenario byte diff; see note) |
| a36a07a | no-op result has no backup path | P8-M-13 | output (result text), check messages.jsonl | match: fixture tests pass (not a scenario byte diff; see note) |
| 0f344bc | sync temp file before rename | M33 | PERSIST (asyncmgr store) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 3add47b | nested run session on run and async handle | P6CM-03 | PERSIST (subagent manifest) | match: fixture tests pass (not a scenario byte diff; see note) |
| 6cd618b | never record stopped over terminal manifest | P6BM-08 | PERSIST (process manifest) | match: fixture tests pass (not a scenario byte diff; see note) |
| 4af6285 | MCP one JSON form, text cut, envelope | P6AM-14, 15, 16 | output envelope, shared text helpers | match: fixture tests pass (not a scenario byte diff; see note) |
| 0a47610 | MCP structured content encoded once | P6AM-02, 03 | output encoding | match: fixture tests pass (not a scenario byte diff; see note) |
| dbd365e | shared whole-file hash loop, strconv.Itoa | P8-M-10, P5M-14 | shared function (hash) | match: fixture tests pass (not a scenario byte diff; see note) |
| 9bcd920 | sub-agent Close bounded 2 s, logs leftovers | P6CM-05 | shared lifecycle | match: fixture tests pass (not a scenario byte diff; see note) |
| ef22f19 | extract shared subagent test layout (test-only prep) | P6CM-19 | none (test helper) | match: fixture tests pass (not a scenario byte diff; see note) |
| a7ad56b | bounded layout parse, env_deny trim, listen_unix_mode refuses group/other write | P2A-21, P2B2-20, P2B1-09 | CONFIG (remote_api, native process settings), PERSIST (config file read) | match on clean re-run; first parallel run failed TestManyConcurrentSubscribersSeeAnUninterruptedStream (test race, no fixture) |
| 372ceee | anthropic-adapter error text, URL redaction, UTF-8 cut, tool-shape | P2B2-16, P5M-08, P5M-13 | output (adapter error text), shared (openaiauth token error) | match: fixture tests pass (not a scenario byte diff; see note) |
| f1966df | token file TOCTOU check, log file mode 0600, peer uid check on dial | M31, N28, N11 | CONFIG (log file mode), PERSIST (log file), shared (remote client dial) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 27f8f28 | refuse listen_unix path over the sun_path limit | N12 | CONFIG (listen_unix) | match: fixture tests pass (not a scenario byte diff; see note) |
| 2ebad42 | no-replace socket publication (os.Link), owner-checked unlink | N27 | CONFIG (listen_unix), PERSIST (socket file) | match on clean re-run; first parallel run timed out TestProfileMessageRefusesALoopThroughACallBlockedOnAQueuedMessage (load, no fixture) |
| f6940e4 | remote List matches Load for symlinked entries | M29 | PERSIST (remote entry listing) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| faee4c8 | async status captures ok under the lock | M25 | shared (asyncmgr Status), output (status ok flag) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| fc10205 | decode padded image input without copying | P8-M-12 | shared (imagedata.Decode), output (size judgement) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| d4d38f9 | remove SubagentManager.Closed, test through the resolver | P6CM-19 | shared (native subagent manager API) | match: fixture tests pass (not a scenario byte diff; see note) |
| cac77d1 | test N11 post-dial peer refusal with a foreign-uid socket (test only) | N11 | none (test only) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| a014011 | remove dead readUsageCache, stringField, TurnStore reference | N19 | none (dead code removed) | match: fixture tests pass (not a scenario byte diff; see note) |
| 2f051ab | remove unused MatchGlobs and ErrNotLive | M18 | shared (exported API removed) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| e1f4f3e | remove unused LayoutSettingKey | P2B1-11 | shared (exported API removed) | match: fixture tests pass (not a scenario byte diff; see note) |
| 2950fa4 | move Resolver doc comment onto Resolver | N21 | none (comment) | match: fixture tests pass (not a scenario byte diff; see note) |
| 54167eb | refuse a relative server.remote_api.listen_unix; resolve tokens_file only | P2B2-26 | CONFIG (remote_api settings) | match: fixture tests pass (not a scenario byte diff; see note) |
| 725fc88 | refuse a query or fragment in decide_systemone base_url | P2B2-21 | CONFIG (extension decide_systemone) | match: fixture tests pass (not a scenario byte diff; see note) |
| ad2eead | suggest the closest setting key for an unknown catalog-layout key | P2B1-13 | CONFIG (error text, settings key) | match: fixture tests pass (not a scenario byte diff; see note) |
| b32717c | refuse JAD_SESSIONS_DIR (deliberate tightening) | P2B1-10 | CONFIG (env var); library lookup in contentlibrary still reads it (see open) | match: fixture tests pass (not a scenario byte diff; see note) |
| 71c2417 | refuse exponent notation for tool limits (tightening) | P2B2-19 | CONFIG (max_tool_rounds, max_run_tokens, max_parallel_tool_calls) | match: fixture tests pass (not a scenario byte diff; see note) |
| be71753 | warn on unknown root-setting keys, do not refuse | P2B1-08 | CONFIG (root setting load, log warnings) | match: fixture tests pass (not a scenario byte diff; see note) |
| 07678f5 | validate input items before externalizing their images | P2B2-11 | PERSIST (image store), output (refusal order) | match: fixture tests pass (not a scenario byte diff; see note) |
| a496004 | tool batch shouldStop wired to the round context (behaviour-neutral) | P2B2-25 | shared (tool batch) | match: fixture tests pass (not a scenario byte diff; see note) |
| 0051fbd | broken token table answers 503, not 401 | M24 | output (HTTP status), shared (remoteapi Authenticate) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 6e794cc | process abort reason capped at the summary limit | P2B2-18 | output (process reason), shared (process abort) | match: fixture tests pass (not a scenario byte diff; see note) |
| 964672d | recovered tool-batch panic reports a generic error | P2B2-18 | output (model-visible text) | match: fixture tests pass (not a scenario byte diff; see note) |
| cae680e | a later CancelOwner keeps the strongest end of a handle | M12 | PERSIST (asyncmgr row end state) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 9363ede | owner entry goes once its last dropped count is read | M21 | shared (asyncmgr owners map) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 3b0efa2 | GET /profiles answers 404 when the token sees none of the session profiles | M27 | output (HTTP status), shared (remoteapi) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 2aa70cc | request log names catalog, image, checkpoint and run-image routes | P3-13 | output (log text only) | match on clean re-run; first parallel run failed TestManyConcurrentSubscribersSeeAnUninterruptedStream (test race, no fixture) |
| adf41ea | unreadable session file listed without an etag | P3-11 | output (catalog listing JSON), shared (frontend contract) | match: fixture tests pass (not a scenario byte diff; see note) |
| 3e70f78 | debug log redacts token and secret headers and transport errors | P3-10 | output (debug log text) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 8ec56fc | reset, front compact and state context clear the gate first | P3-12 | shared (mutation gate ordering) | match: fixture tests pass (not a scenario byte diff; see note) |
| 6fa0f2d | library error text no longer names the absolute directory | P3-09 | output (HTTP 422 text) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 8f28ef4 | checkpoint restore clears the gate and restoring mark before the process lock | P3-12 | shared (mutation gate ordering) | match on clean re-run; first parallel run failed TestManyConcurrentSubscribersSeeAnUninterruptedStream (test race, no fixture) |
| a6786ae | content cache fallback reads only the prefix its cap keeps | P3-16 | shared (run content cache) | match: fixture tests pass (not a scenario byte diff; see note) |
| 2fcd63f | a cancelled Wait on timeout takes no signals | M26 | shared (asyncmgr Wait) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| aef79b3 | a run ID that cannot be random is refused with 503 | M30 | output (HTTP 503 status) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |
| 65de512 | listen_unix_mode schema description names the group/other write-bit refusal (range unchanged) | step-2 impact on a7ad56b | CONFIG (schema text) | match: fixture tests pass (not a scenario byte diff; see note) |
| 5591abe | contentlibrary no longer reads JAD_SESSIONS_DIR; loader doc updated | step-2 impact on b32717c | CONFIG (env var), shared (library lookup) | match: fixture tests pass (not a scenario byte diff; see note) |
| 96264e3 | DECISIONS root-discovery line no longer cites a TypeScript precedence | step-2 impact on 5591abe | none (DECISIONS text) | not checked: does not touch transcript/session/tools/httpapi/app/checkpoint/sessionfs or testdata |

Range 7a1e43a..98a2330 (47 commits). Rows above cover 42 code commits; the other five carry no code change: 72e151f (decisions, one characterisation test), 0f101a7, 9c86443, 61046da, 98a2330 (DECISIONS.md only). Four rows were already in the table and are kept: 276dd6d, 099989f, a36a07a, 0f344bc. Open items from the impact pass are closed: 65de512 (listen_unix_mode description names the write-bit refusal; the range 384-438 was kept, and the claim that 0644 is refused was wrong: 0644 is accepted) and 5591abe (contentlibrary no longer reads JAD_SESSIONS_DIR).

Flag rule: CONFIG = edits sample/schemes or a config key; PERSIST = writes a file on disk; output = text or bytes a client sees; shared = a function with several callers. A flag is a path or subject match confirmed by git show; the impact loop was not run on these older commits.

Golden check (HEAD 96264e3, step 2): the messages-golden-diff scenario byte diff was NOT run, because it assumes cmd/agent-backend, which does not exist. The substitute is the fixture tests that read the frozen golden files under internal/**/testdata (transcript, session, tools/native, httpapi, app, go test -count=1), run in a baseline worktree and at each commit. Four of the 47 touched rows (a7ad56b, 2aa70cc, 8f28ef4, 2ebad42) failed once when three runs shared the machine and passed on a clean re-run alone (the failures were a subscriber-ordering test with no fixture and a 30 s timeout under load); those rows say so. The 21 "not checked" rows do not touch transcript, session, tools, httpapi, app, checkpoint, sessionfs or testdata, so the fixture tests do not exercise them. This record is untracked, so this column is not in git.
