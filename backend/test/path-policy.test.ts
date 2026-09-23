import assert from "node:assert/strict";
import {lstat, mkdtemp, readFile, rm, symlink, writeFile} from "node:fs/promises";
import {homedir, tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {
  firstLine,
  makeReadToolSession,
  makeWriteToolSession,
  outputText,
  refreshReadSession,
  refreshWriteSession,
} from "./tool-session.js";

async function outsideDirectory(prefix: string): Promise<string> {
  return mkdtemp(join(tmpdir(), prefix));
}

async function runRead(
  sessionDir: string,
  path: string,
  callId: string,
): Promise<string> {
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const output = await catalog.executeFunctionCall(
    {
      type: "function_call",
      call_id: callId,
      name: "read",
      arguments: JSON.stringify({path}),
    },
    {workspaceRoot: sessionDir, sessionDir},
  );
  return outputText(output.output);
}

async function runWrite(
  sessionDir: string,
  path: string,
  callId: string,
): Promise<string> {
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const output = await catalog.executeFunctionCall(
    {
      type: "function_call",
      call_id: callId,
      name: "write",
      arguments: JSON.stringify({path, content: "written outside\n"}),
    },
    {workspaceRoot: sessionDir, sessionDir},
  );
  return outputText(output.output);
}

test("the paths object form with an empty allow list lifts the cwd restriction", async () => {
  const sessionDir = await makeReadToolSession({paths: {allow: [], deny: []}, max_bytes: 4096});
  const externalRoot = await outsideDirectory("agent-harness-policy-open-");
  try {
    const outsidePath = join(externalRoot, "outside.txt");
    await writeFile(outsidePath, "outside content\n", "utf8");

    const result = await runRead(sessionDir, outsidePath, "read-open-outside");
    assert.equal(firstLine(result).status, "ok");
    assert.match(result, /outside content/);

    const listing = await runRead(sessionDir, externalRoot, "read-open-listing");
    assert.equal(firstLine(listing).status, "ok");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("the rule array form keeps the workspace-only default", async () => {
  const sessionDir = await makeReadToolSession({paths: []});
  const externalRoot = await outsideDirectory("agent-harness-policy-default-");
  try {
    const result = await runRead(
      sessionDir,
      join(externalRoot, "outside.txt"),
      "read-default-outside",
    );
    assert.equal(firstLine(result).status, "forbidden");
    assert.match(result, /error_code: path_outside_workspace/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("deny rules win over an empty allow list and over an allowed root", async () => {
  const sessionDir = await makeReadToolSession();
  const externalRoot = await outsideDirectory("agent-harness-policy-deny-");
  const outsidePath = join(externalRoot, "secret.txt");
  try {
    await writeFile(outsidePath, "secret\n", "utf8");

    await refreshReadSession(sessionDir, {paths: {allow: [], deny: [externalRoot]}});
    const denied = await runRead(sessionDir, outsidePath, "read-deny-empty-allow");
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);
    assert.doesNotMatch(denied, new RegExp(externalRoot.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));

    await refreshReadSession(sessionDir, {paths: {allow: [externalRoot], deny: [outsidePath]}});
    const deniedFile = await runRead(sessionDir, outsidePath, "read-deny-allowed-root");
    assert.equal(firstLine(deniedFile).status, "forbidden");
    assert.match(deniedFile, /error_code: path_denied/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("a non-empty allow list restricts which outside paths are reachable", async () => {
  const sessionDir = await makeReadToolSession();
  const allowedRoot = await outsideDirectory("agent-harness-policy-allowed-");
  const otherRoot = await outsideDirectory("agent-harness-policy-other-");
  try {
    await writeFile(join(allowedRoot, "allowed.txt"), "allowed\n", "utf8");
    await writeFile(join(otherRoot, "other.txt"), "other\n", "utf8");
    await refreshReadSession(sessionDir, {paths: {allow: [allowedRoot]}});

    const allowed = await runRead(
      sessionDir,
      join(allowedRoot, "allowed.txt"),
      "read-nonempty-allowed",
    );
    assert.equal(firstLine(allowed).status, "ok");

    const other = await runRead(sessionDir, join(otherRoot, "other.txt"), "read-nonempty-other");
    assert.equal(firstLine(other).status, "forbidden");
    assert.match(other, /error_code: path_outside_workspace/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(allowedRoot, {recursive: true, force: true});
    await rm(otherRoot, {recursive: true, force: true});
  }
});

test("an unrestricted tool reaches through a symlinked directory", async () => {
  const sessionDir = await makeReadToolSession({paths: {allow: [], deny: []}, max_bytes: 4096});
  const targetRoot = await outsideDirectory("agent-harness-policy-symlink-target-");
  const linkRoot = await outsideDirectory("agent-harness-policy-symlink-link-");
  try {
    await writeFile(join(targetRoot, "linked.txt"), "through the link\n", "utf8");
    await symlink(targetRoot, join(linkRoot, "link"));

    const result = await runRead(
      sessionDir,
      join(linkRoot, "link", "linked.txt"),
      "read-through-symlink",
    );
    assert.equal(firstLine(result).status, "ok");
    assert.match(result, /through the link/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(targetRoot, {recursive: true, force: true});
    await rm(linkRoot, {recursive: true, force: true});
  }
});

test("write follows the same object form policy", async () => {
  const sessionDir = await makeWriteToolSession();
  const externalRoot = await outsideDirectory("agent-harness-policy-write-");
  const outsidePath = join(externalRoot, "nested", "outside.txt");
  try {
    await refreshWriteSession(sessionDir, {paths: []});
    const restricted = await runWrite(sessionDir, outsidePath, "write-policy-restricted");
    assert.equal(firstLine(restricted).status, "forbidden");
    assert.match(restricted, /error_code: path_outside_workspace/);
    assert.equal(await readFile(outsidePath, "utf8").catch(() => undefined), undefined);

    await refreshWriteSession(sessionDir, {paths: {allow: [], deny: []}});
    const unrestricted = await runWrite(sessionDir, outsidePath, "write-policy-unrestricted");
    assert.equal(firstLine(unrestricted).status, "ok");
    assert.equal(await readFile(outsidePath, "utf8"), "written outside\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("an allow rule reaches through a symlinked directory when it covers the resolved path", async () => {
  const sessionDir = await makeReadToolSession({paths: {allow: ["/"], deny: []}, max_bytes: 4096});
  const targetRoot = await outsideDirectory("agent-harness-rule-symlink-target-");
  const linkRoot = await outsideDirectory("agent-harness-rule-symlink-link-");
  try {
    await writeFile(join(targetRoot, "linked.txt"), "rule through the link\n", "utf8");
    await symlink(targetRoot, join(linkRoot, "link"));

    const result = await runRead(
      sessionDir,
      join(linkRoot, "link", "linked.txt"),
      "read-rule-through-symlink",
    );
    assert.equal(firstLine(result).status, "ok");
    assert.match(result, /rule through the link/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(targetRoot, {recursive: true, force: true});
    await rm(linkRoot, {recursive: true, force: true});
  }
});

test("an allow rule that covers only the symlink, not its target, stays forbidden", async () => {
  const sessionDir = await makeReadToolSession();
  const targetRoot = await outsideDirectory("agent-harness-rule-symlink-gap-target-");
  const linkRoot = await outsideDirectory("agent-harness-rule-symlink-gap-link-");
  try {
    await writeFile(join(targetRoot, "linked.txt"), "should not be reachable\n", "utf8");
    await symlink(targetRoot, join(linkRoot, "link"));
    await refreshReadSession(sessionDir, {paths: {allow: [linkRoot]}});

    const result = await runRead(
      sessionDir,
      join(linkRoot, "link", "linked.txt"),
      "read-rule-symlink-gap",
    );
    assert.equal(firstLine(result).status, "forbidden");
    assert.match(result, /error_code: symlink_outside_workspace/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(targetRoot, {recursive: true, force: true});
    await rm(linkRoot, {recursive: true, force: true});
  }
});

test("rejects unsupported keys inside the paths object", async () => {
  const sessionDir = await makeReadToolSession({
    paths: {allow: [], deny: [], except: ["/etc"]},
  });
  try {
    await assert.rejects(
      new SessionConfigLoader(sessionDir).load(),
      /tools\.native\.read\.paths\.except is not supported; use allow and deny/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects a paths value that is neither rules nor an allow/deny object", async () => {
  const sessionDir = await makeReadToolSession({paths: "everything"});
  try {
    await assert.rejects(
      new SessionConfigLoader(sessionDir).load(),
      /tools\.native\.read\.paths must be an array of \{action, path\} rules or an object with allow and deny path lists/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("write follows a symlinked target when the policy allows the resolved file", async () => {
  const sessionDir = await makeWriteToolSession({paths: {allow: [], deny: []}});
  const externalRoot = await outsideDirectory("agent-harness-symlink-write-");
  const realTarget = join(externalRoot, "real.txt");
  const link = join(sessionDir, "link.txt");
  try {
    await writeFile(realTarget, "before\n", "utf8");
    await symlink(realTarget, link);

    const result = await runWrite(sessionDir, link, "write-through-allowed-symlink");
    assert.equal(firstLine(result).status, "ok");
    assert.equal(await readFile(realTarget, "utf8"), "written outside\n");
    assert.equal((await lstat(link)).isSymbolicLink(), true);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("a workspace-only policy still refuses a symlinked target", async () => {
  const sessionDir = await makeWriteToolSession();
  const externalRoot = await outsideDirectory("agent-harness-symlink-denied-");
  const realTarget = join(externalRoot, "real.txt");
  const link = join(sessionDir, "link.txt");
  try {
    await writeFile(realTarget, "untouched\n", "utf8");
    await symlink(realTarget, link);

    const result = await runWrite(sessionDir, link, "write-through-denied-symlink");
    assert.equal(firstLine(result).status, "forbidden");
    assert.match(result, /error_code: symlink_forbidden/);
    assert.equal(await readFile(realTarget, "utf8"), "untouched\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("rejects a misspelled tool key with a suggestion", async () => {
  const sessionDir = await makeReadToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "read.json")],
        tools: {native: {read: {max_bytes: 8, path: [{action: "allow", path: "/"}]}}},
      }),
    );
    await assert.rejects(
      new SessionConfigLoader(sessionDir).load(),
      /tools\.native\.read\.path is not supported; did you mean paths\?/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects tools.native.mcp and points at tools.mcp", async () => {
  const sessionDir = await makeReadToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "read.json")],
        tools: {native: {read: {max_bytes: 8}, mcp: {}}},
      }),
    );
    await assert.rejects(
      new SessionConfigLoader(sessionDir).load(),
      /tools\.native\.mcp is not supported; supported keys are [^;]+; MCP servers are configured under tools\.mcp/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects an unknown node under tools", async () => {
  const sessionDir = await makeReadToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "read.json")],
        tools: {native: {read: {max_bytes: 8}}, python: {}},
      }),
    );
    await assert.rejects(
      new SessionConfigLoader(sessionDir).load(),
      /tools\.python is not supported; supported keys are mcp, native/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("expands a leading tilde in rule paths", async () => {
  const sessionDir = await makeReadToolSession();
  const home = homedir();
  try {
    await refreshReadSession(sessionDir, {paths: {allow: [], deny: ["~"]}});
    const rules = (await new SessionConfigLoader(sessionDir).load()).settings.tools.native.read!.paths;
    assert.equal(rules[0]!.action, "deny");
    assert.equal(rules[0]!.path, home);

    const denied = await runRead(sessionDir, home, "read-tilde-deny");
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);

    const allowed = await runRead(sessionDir, join(sessionDir, "read.json"), "read-tilde-workspace");
    assert.equal(firstLine(allowed).status, "ok");

    await refreshReadSession(sessionDir, {paths: {allow: ["~home-only"], deny: []}});
    const literal = (await new SessionConfigLoader(sessionDir).load()).settings.tools.native.read!.paths;
    assert.ok(literal[0]!.path.endsWith(`~home-only`), literal[0]!.path);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("expands a leading tilde in cwd, session paths, storage_dir, and backup_dir", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-tilde-config-"));
  const home = homedir();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: "~",
        paths: {messages: "~/harness-tilde/messages.jsonl"},
        tools: {
          native: {
            write: {backup_dir: "~/harness-tilde/backups"},
            task: {storage_dir: "~/harness-tilde/tasks"},
          },
        },
      }),
    );
    const settings = (await new SessionConfigLoader(sessionDir).load()).settings;
    assert.equal(settings.cwd, home);
    assert.equal(settings.paths.messages, join(home, "harness-tilde", "messages.jsonl"));
    assert.equal(settings.tools.native.write!.backup_dir, join(home, "harness-tilde", "backups"));
    assert.equal(settings.tools.native.task!.storage_dir, join(home, "harness-tilde", "tasks"));
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
