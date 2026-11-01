# Design: system_prompt_path for subagent and profile_message

Scope: `tools.native.subagent.system_prompt_path` and `tools.native.profile_message.system_prompt_path`
(one path string each, read at settings load, appended after the existing prompt, joined by a blank line).

## Subagent (no open question)

- `native_subagent_settings.go`: add `system_prompt_path` to `nativeSubagentKeys`. Read it with
  `optionalSubagentString` (non-empty string, null = absent), resolve it with `readPromptDocument`
  (relative to the session cwd, `~` expanded, `maxPromptDocumentBytes`). Errors name the key
  (`tools.native.subagent.system_prompt_path[0] could not be read: <abs path>`); the `[0]` suffix is accepted.
- Store the resolved path (`NativeSubagentSettings.SystemPromptPath`) and text (`SystemPrompt`).
- After `withoutDeniedSubagents`, append the text to every agent's `PromptText` with `\n\n`;
  an empty `PromptText` becomes the file text alone. Inline and folder agents get it alike.
- `app/subagent.go` `childConfig` already copies `request.Agent.PromptText`; no app change.
- `settings_inputs.go`: `collect` adds `subagent.SystemPromptPath` next to the prompt documents.
- Schema: `sample/schemes/setting.schema.json` (tools.native.subagent), settings tests.
- Tool description: not changed; the path is never sent to the model.

## profile_message (OPEN: needs a decision)

Finding: the child is not built from the caller's config. `profileMessenger.Prepare`
(`app/profile_message.go`) starts an ordinary run of the target profile through
`httpapi.ProfileRunRequest` (`httpapi/profile_run.go`). The run's settings come from
`Resolver.Resolve` -> `sessionfs.LoadSessionSettings(location, targetProfile)`, so the child's
prompt is the target profile's own `PromptText` (`resolver.go` ~456 and ~1105 are that target
config). The caller's `tools.native.profile_message` block never reaches it, so appending its
file text needs new plumbing.

Option A (recommended): carry the text with the message.
`NativeProfileMessageSettings.SystemPrompt` (loaded at settings load) -> `native.ProfileMessageRequest`
-> `profileMessagePlan` -> `httpapi.ProfileRunRequest.SystemPrompt` -> applied in
`boundRunner.Run` to a copy of the target config (`PromptText = target + "\n\n" + file`) before
`prepareRun`. The runner is fingerprinted on `PromptText`, so a different text gets its own
runner. Touches about 6 files. Only profile messages from a session with the key get the text.

Option B: read the key from the target profile's own `tools.native.profile_message`.
Smaller, but the caller's setting then has no effect on its own messages, which does not match
"the profile_message child config". Not recommended.

Also needed for A: `normalizeNativeProfileMessageSettings` has no `cwd` parameter today; it
would take one (5 callers).

Decision needed: A or B. Until it is made, the profile_message half is not implemented.

## Tests planned

- Subagent: settings test loads a valid file, checks the order (instructions, folder prompt.md,
  documents, then the file). Missing file: load error naming the key. Stamp test: edit changes the stamp.
- profile_message: waits on the decision above.

## profile_message (decided: option A)

Decision (user, this session): option A. The caller's file text travels with the message.

Remote rule (user, this session): when the target is a remote hop and `system_prompt_path` is
set, the call fails with a configuration error that names the key. The text is never dropped
silently.

Remote protocol check: `remoteapi.MessageRequest` carries Session, Profile, Message, Caller and
Hops only (`internal/remoteapi/server.go:57`). `remote_api.go` `SendMessage` builds the remote
run from those fields. No run-level prompt addition exists on the remote protocol, so the text
cannot be carried to a remote target. Hence the refusal.

Flow (local target):
1. `sessionfs.normalizeNativeProfileMessageSettings(setting, settingPath, cwd)` reads the path
   with `readPromptDocument` (maxPromptDocumentBytes), stores `SystemPromptPath` and
   `SystemPrompt` on `NativeProfileMessageSettings`. A missing file is a load error naming the key.
2. `tools/native` `profileMessageTool.execute` (local branch) copies `SystemPrompt` into
   `ProfileMessageRequest.SystemPrompt`.
3. `profileMessenger.Prepare` copies it into `profileMessagePlan.systemPrompt`.
4. `profileMessagePlan.request` puts it into `httpapi.ProfileRunRequest.SystemPrompt`.
5. `httpapi.profileRunStarter.StartProfileRun` stores it on the run, then passes it through
   `RunOptions.SystemPrompt` (`session` package) into `executeRun`.
6. `boundRunner.Run` applies it to a copy of `r.config` (`Model.PromptText` = target text, then
   "\n\n", then the file text; empty target text gives the file text alone) before `prepareRun`.
   The stored config is not changed, so the target's own settings never see the caller's path.

Stamp: the path is a settings input, like the subagent path (`settings_inputs.go`). For the
profile_message half it is collected from the caller's `NativeProfileMessage`.

Implemented:
- `sessionfs.NativeProfileMessageSettings.SystemPromptPath`/`SystemPrompt`, read by
  `normalizeNativeProfileMessageSettings(setting, settingPath, cwd)` (cwd is new).
- `sessionfs.JoinPromptText` (exported from the subagent half; the same join rule for both).
- Remote refusal: `tools/native/profile_message_remote.go` `executeRemote` returns the error code
  `system_prompt_path_remote` naming `tools.native.profile_message.system_prompt_path` before any
  message is sent.
- `session.RunOptions` is not changed for the text: `httpapi.executeRun` passes the run's
  `systemPrompt` into `RunOptions.SystemPrompt` (a field the runner ignores); `app/bound_runner.go`
  `Run` appends it to a local copy of `r.config` before `prepareRun`.

Tests:
- Local order: `app/profile_message_test.go` `TestProfileMessageAppendsTheCallersSystemPromptAfterTheTargetsPrompt`
  (tool call through the caller's settings; the child's provider request has the target's
  instructions, then the caller's text).
- Remote refusal names the key: `tools/native/profile_message_remote_test.go`
  `TestRemoteProfileMessageRefusalNamesTheKey` and a case in `TestRemoteProfileMessageRefusals`.
- Missing file names the key: `sessionfs/native_profile_message_settings_test.go`.
- Stamp: `sessionfs/settings_stamp_inputs_test.go` `TestSettingsStampSeesAProfileMessageSystemPromptFileEdit`.
- Schema: `sessionfs/native_tool_schema_test.go` blank path refused by both the schema and the loader.
- Target config unchanged: `TestProfileMessageSystemPromptDoesNotChangeTheTargetConfig`.

Known limit: the mutation that writes the appended text into `r.config` (value receiver) is not
caught by any test, because `r.config` is a copy and the next message re-resolves from settings,
so the write has no observable effect on the public path. It is not a live bug; the test
covers the observable contract.
