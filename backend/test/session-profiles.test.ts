import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { AgentSession, buildResponseRequest, type AgentSessionSharedStores } from "../src/session.js";
import type {LoadedSessionConfig} from "../src/config.js";
import { MemoryStore } from "../src/tools/native/memory-store.js";
import { TaskStore } from "../src/tools/native/task-store.js";

/**
 * Two profiles of one session directory, each with its own transcript and model.
 */
async function writeSession(root: string): Promise<string> {
  const sessionDir = join(root, "session1");
  await mkdir(join(sessionDir, "primary"), {recursive: true});
  await mkdir(join(sessionDir, "alternate"), {recursive: true});
  for (const profile of ["primary", "alternate"]) {
    await writeFile(
      join(sessionDir, profile, "model.json"),
      JSON.stringify({
        schema_version: 1,
        provider: "fake",
        api_key_env: "FAKE_API_KEY",
        model: `${profile}-model`,
      }),
    );
  }
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "profiles",
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {
            messages: "profiles/primary/messages.jsonl",
            staging: "profiles/primary/staging",
            model_default: "primary/model.json",
            models: ["primary/model.json"],
          },
          alternate: {
            messages: "profiles/alternate/messages.jsonl",
            staging: "profiles/alternate/staging",
            model_default: "alternate/model.json",
            models: ["alternate/model.json"],
          },
        },
      },
    }),
  );
  return sessionDir;
}

test("opens two profiles of one session on separate transcripts", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-session-profiles-"));
  const sessionDir = await writeSession(root);
  let primary: AgentSession | undefined;
  let alternate: AgentSession | undefined;
  try {
    primary = await AgentSession.open(sessionDir, {profile: "primary"});
    alternate = await AgentSession.open(sessionDir, {profile: "alternate"});

    assert.equal(primary.store.messagesPath, join(sessionDir, "profiles/primary/messages.jsonl"));
    assert.equal(alternate.store.messagesPath, join(sessionDir, "profiles/alternate/messages.jsonl"));
    assert.notEqual(primary.store.messagesPath, alternate.store.messagesPath);
    assert.notEqual(primary.store.stagingDir, alternate.store.stagingDir);

    // A profile the session does not configure cannot be opened at all.
    await assert.rejects(
      () => AgentSession.open(sessionDir, {profile: "missing"}),
      /path profile 'missing' is not defined/,
    );
  } finally {
    await primary?.close();
    await alternate?.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("restores accepted request context from the matching profile after compaction", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-session-profile-request-context-"));
  const sessionDir = await writeSession(root);
  let primary: AgentSession | undefined;
  let alternate: AgentSession | undefined;
  const config = {
    model: {model: "test-model", request: {}},
    settings: {prompt_text: "", prompt_role: "system"},
  } as unknown as LoadedSessionConfig;

  const requestContext = async (session: AgentSession): Promise<string> => {
    const history = [];
    for await (const item of session.store.readInputItems({persistCache: false})) history.push(item);
    const request = buildResponseRequest(config, history, [], {
      role: "developer",
      acceptedRequest: await session.store.readAcceptedRequest(),
    });
    const input = request.input as unknown as Array<{
      content?: Array<{text?: string}>;
    }>;
    return input.flatMap((item) => item.content?.map((part) => part.text ?? "") ?? []).join("\n");
  };

  try {
    primary = await AgentSession.open(sessionDir, {profile: "primary"});
    alternate = await AgentSession.open(sessionDir, {profile: "alternate"});
    assert.notEqual(primary.store.acceptedRequestPath, alternate.store.acceptedRequestPath);

    for (const [session, profile] of [[primary, "primary"], [alternate, "alternate"]] as const) {
      const callId = `call-${profile}`;
      const draft = {goal: `approved for ${profile}`};
      await session.store.writeAcceptedRequest(draft, "user", "accept", callId);
      await session.store.appendInputItems([
        {type: "message", role: "user", content: [{type: "input_text", text: `Start ${profile}`}]},
        {type: "function_call", call_id: callId, name: "request_draft", arguments: JSON.stringify({draft})},
        {type: "function_call_output", call_id: callId, output: JSON.stringify({decision: "accept", draft})},
      ]);
      const result = await session.store.compactHistory({
        turnType: "assistant_final",
        keepTurns: 1,
        keepToolPairs: 0,
        backupMode: "replace",
      });
      assert.equal(result.compacted, true);
    }

    const primaryContext = await requestContext(primary);
    const alternateContext = await requestContext(alternate);
    assert.match(primaryContext, /approved for primary/);
    assert.doesNotMatch(primaryContext, /approved for alternate/);
    assert.match(alternateContext, /approved for alternate/);
    assert.doesNotMatch(alternateContext, /approved for primary/);
  } finally {
    await primary?.close();
    await alternate?.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("shares cwd-derived stores without recovering or closing them per profile", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-session-shared-"));
  const sessionDir = await writeSession(root);
  // The owner recovers the shared manager once; the profiles must neither
  // recover it again — that would mark the other profile's live processes lost —
  // nor close it while another profile still uses it.
  let recoveries = 0;
  let closes = 0;
  const processManager = {
    recover: async (): Promise<void> => {
      recoveries += 1;
    },
    close: async (): Promise<void> => {
      closes += 1;
    },
  };
  const shared = {
    processManager,
    taskStore: new TaskStore(join(sessionDir, "tasks")),
    memoryStore: new MemoryStore(join(sessionDir, "memory")),
  } as unknown as AgentSessionSharedStores;

  let primary: AgentSession | undefined;
  let alternate: AgentSession | undefined;
  try {
    primary = await AgentSession.open(sessionDir, {profile: "primary", shared});
    alternate = await AgentSession.open(sessionDir, {profile: "alternate", shared});
    assert.equal(recoveries, 0);

    await primary.close();
    primary = undefined;
    assert.equal(closes, 0);

    await alternate.close();
    alternate = undefined;
    assert.equal(closes, 0);
  } finally {
    await primary?.close();
    await alternate?.close();
    await rm(root, {recursive: true, force: true});
  }
});
