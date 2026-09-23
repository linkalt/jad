import assert from "node:assert/strict";
import {lstat, mkdir, mkdtemp, readFile, readdir, rename, rm, stat, symlink, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {commitFile} from "../src/tools/native/mutate.js";
import {
  firstLine,
  informationFields,
  makeWriteToolSession,
  outputText,
  refreshWriteSession,
  type NativeWriteSessionOptions,
} from "./tool-session.js";

type ToolCatalog = Awaited<ReturnType<typeof loadToolCatalog>>;

async function openWriteSession(
  options: NativeWriteSessionOptions = {},
): Promise<{sessionDir: string; catalog: ToolCatalog}> {
  const sessionDir = await makeWriteToolSession(options);
  const config = await new SessionConfigLoader(sessionDir).load();
  return {sessionDir, catalog: await loadToolCatalog(config)};
}

async function runWrite(
  catalog: ToolCatalog,
  sessionDir: string,
  callId: string,
  args: Record<string, unknown>,
  workspaceRoot: string = sessionDir,
): Promise<string> {
  const result = await catalog.executeFunctionCall(
    {
      type: "function_call",
      call_id: callId,
      name: "write",
      arguments: JSON.stringify(args),
    },
    {workspaceRoot, sessionDir},
  );
  return outputText(result.output);
}

function backupNames(entries: string[], fileName: string): string[] {
  return entries.filter((name) => name.startsWith(`${fileName}.bak-`));
}

const BACKUP_DIRECTORY = "backups";

function backupDirectoryPath(sessionDir: string, relativeDirectory = ""): string {
  return join(sessionDir, BACKUP_DIRECTORY, relativeDirectory);
}

/** Backup names of one file inside the backup directory; [] while it is absent. */
async function backupNamesIn(
  sessionDir: string,
  relativeDirectory: string,
  fileName: string,
): Promise<string[]> {
  try {
    return backupNames(
      await readdir(backupDirectoryPath(sessionDir, relativeDirectory)),
      fileName,
    );
  } catch {
    return [];
  }
}

function temporaryNames(entries: string[]): string[] {
  return entries.filter((name) => name.includes(".tmp-"));
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

async function pause(milliseconds: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, milliseconds));
}

test("refuses a target replaced by a symlink before commit", async () => {
  const {sessionDir} = await openWriteSession();
  const outside = join(sessionDir, "outside.txt");
  const targetPath = join(sessionDir, "target.txt");
  try {
    await writeFile(targetPath, "before\n", "utf8");
    await writeFile(outside, "outside\n", "utf8");
    const original = await lstat(targetPath);
    await rm(targetPath);
    await symlink(outside, targetPath);
    const settings = (await new SessionConfigLoader(sessionDir).load()).settings.tools.native.write!;
    const result = await commitFile({
      target: {
        workspaceRoot: sessionDir,
        realPath: targetPath,
        displayPath: "target.txt",
        errorPath: "target.txt",
        existed: true,
        mode: original.mode & 0o7777,
        size: original.size,
        identity: {device: original.dev, inode: original.ino},
      },
      settings,
      data: Buffer.from("replacement\n"),
      backup: false,
    });
    assert.equal(result.error, "target_changed");
    assert.equal(await readFile(outside, "utf8"), "outside\n");
    assert.equal((await lstat(targetPath)).isSymbolicLink(), true);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("refuses a parent directory replaced before atomic commit", async () => {
  const {sessionDir} = await openWriteSession();
  const parent = join(sessionDir, "parent");
  const movedParent = join(sessionDir, "moved-parent");
  const outside = await mkdtemp(join(tmpdir(), "agent-harness-write-parent-race-"));
  const targetPath = join(parent, "target.txt");
  try {
    await mkdir(parent);
    await writeFile(targetPath, "before\n", "utf8");
    await writeFile(join(outside, "target.txt"), "outside\n", "utf8");
    const original = await lstat(targetPath);
    await rename(parent, movedParent);
    await symlink(outside, parent);
    const settings = (await new SessionConfigLoader(sessionDir).load()).settings.tools.native.write!;
    const result = await commitFile({
      target: {
        workspaceRoot: sessionDir,
        realPath: targetPath,
        displayPath: "parent/target.txt",
        errorPath: "parent/target.txt",
        existed: true,
        mode: original.mode & 0o7777,
        size: original.size,
        identity: {device: original.dev, inode: original.ino},
      },
      settings,
      data: Buffer.from("replacement\n"),
      backup: false,
    });
    assert.equal(result.error, "target_changed");
    assert.equal(await readFile(join(outside, "target.txt"), "utf8"), "outside\n");
    assert.equal(await readFile(join(movedParent, "target.txt"), "utf8"), "before\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(outside, {recursive: true, force: true});
  }
});

test("refuses a configured backup directory replaced by a symlink", async () => {
  const {sessionDir} = await openWriteSession();
  const targetPath = join(sessionDir, "target.txt");
  const backupRoot = join(sessionDir, "configured-backups");
  const outside = await mkdtemp(join(tmpdir(), "agent-harness-write-backup-race-"));
  try {
    await writeFile(targetPath, "before\n", "utf8");
    await symlink(outside, backupRoot);
    const original = await lstat(targetPath);
    const settings = {
      ...(await new SessionConfigLoader(sessionDir).load()).settings.tools.native.write!,
      backup_dir: backupRoot,
    };
    const result = await commitFile({
      target: {
        workspaceRoot: sessionDir,
        realPath: targetPath,
        displayPath: "target.txt",
        errorPath: "target.txt",
        existed: true,
        mode: original.mode & 0o7777,
        size: original.size,
        identity: {device: original.dev, inode: original.ino},
      },
      settings,
      data: Buffer.from("replacement\n"),
      backup: true,
    });
    assert.equal(result.error, "backup_failed");
    assert.equal(await readFile(targetPath, "utf8"), "before\n");
    assert.deepEqual(await readdir(outside), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(outside, {recursive: true, force: true});
  }
});

test("writes files, creates parent directories, and reports compact fields", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  try {
    const created = await runWrite(catalog, sessionDir, "write-create", {
      path: "nested/deeper/file.txt",
      content: "created content\n",
    });
    assert.equal(firstLine(created).status, "ok");
    assert.match(created, /"summary":"Created nested\/deeper\/file\.txt"/);
    assert.deepEqual(informationFields(created), {
      path: "nested/deeper/file.txt",
      action: "created",
      bytes: "16",
    });
    assert.equal(
      await readFile(join(sessionDir, "nested/deeper/file.txt"), "utf8"),
      "created content\n",
    );

    const replaced = await runWrite(catalog, sessionDir, "write-replace", {
      path: "nested/deeper/file.txt",
      content: "replacement content\n",
    });
    const replacedFields = informationFields(replaced);
    assert.equal(firstLine(replaced).status, "ok");
    assert.match(replaced, /"summary":"Updated nested\/deeper\/file\.txt"/);
    assert.equal(replacedFields.action, "updated");
    assert.equal(replacedFields.bytes, "20");
    assert.ok(
      replacedFields.backup_path?.startsWith("backups/nested/deeper/file.txt.bak-"),
    );
    assert.equal(
      await readFile(join(sessionDir, replacedFields.backup_path!), "utf8"),
      "created content\n",
    );
    // The workspace itself stays clean: the backup lives in the backup directory.
    assert.deepEqual(backupNames(await readdir(join(sessionDir, "nested/deeper")), "file.txt"), []);
    assert.equal(
      await readFile(join(sessionDir, "nested/deeper/file.txt"), "utf8"),
      "replacement content\n",
    );

    const updatedAgain = await runWrite(catalog, sessionDir, "write-update-again", {
      path: "nested/deeper/file.txt",
      content: "updated again\n",
    });
    assert.equal(firstLine(updatedAgain).status, "ok");
    assert.equal(informationFields(updatedAgain).action, "updated");
    assert.ok(informationFields(updatedAgain).backup_path);
    assert.equal(
      (await backupNamesIn(sessionDir, "nested/deeper", "file.txt")).length,
      2,
    );

    const binary = await runWrite(catalog, sessionDir, "write-binary", {
      path: "binary.dat",
      content: "AP8=",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(binary).status, "ok");
    assert.equal(informationFields(binary).bytes, "2");
    assert.deepEqual(await readFile(join(sessionDir, "binary.dat")), Buffer.from([0, 255]));

    const schemaFailure = await runWrite(catalog, sessionDir, "write-schema-error", {
      path: "missing-content.txt",
    });
    const schemaOutput = firstLine(schemaFailure);
    assert.equal(schemaOutput.status, "invalid_input");
    assert.match(schemaFailure, /missingProperty/);
    assert.match(
      schemaFailure,
      /example_operation: \{"path":"example.txt","content":"\.\.\."\}/,
    );
    assert.equal(
      (await readdir(sessionDir)).some((name) => name === "missing-content.txt"),
      false,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("applies native write allow and deny path policies", async () => {
  const sessionDir = await makeWriteToolSession();
  const externalRoot = await mkdtemp(join(tmpdir(), "agent-harness-write-policy-"));
  const outsidePath = join(externalRoot, "nested", "outside.txt");
  try {
    const execute = async (paths: unknown[], callId: string) => {
      await refreshWriteSession(sessionDir, {paths});
      const catalog = await loadToolCatalog(
        await new SessionConfigLoader(sessionDir).load(),
      );
      return runWrite(catalog, sessionDir, callId, {
        path: outsidePath,
        content: "outside content\n",
      });
    };

    const defaultDenied = await execute([], "write-policy-default-denied");
    assert.equal(firstLine(defaultDenied).status, "forbidden");
    assert.match(defaultDenied, /error_code: path_outside_workspace/);
    assert.match(defaultDenied, /path: \.\.\/outside\.txt/);
    assert.doesNotMatch(defaultDenied, new RegExp(escapeRegExp(externalRoot)));
    assert.equal(await readFile(outsidePath, "utf8").catch(() => undefined), undefined);

    const rootAllowed = await execute([{action: "allow", path: "/"}], "write-policy-allowed");
    assert.equal(firstLine(rootAllowed).status, "ok");
    assert.equal(await readFile(outsidePath, "utf8"), "outside content\n");

    const denyWins = await execute([
      {action: "allow", path: "/"},
      {action: "deny", path: externalRoot},
    ], "write-policy-denied");
    assert.equal(firstLine(denyWins).status, "forbidden");
    assert.match(denyWins, /error_code: path_denied/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("rejects workspace escapes, symlinks, and directory targets", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  const workspace = join(sessionDir, "workspace");
  const outsideTarget = join(sessionDir, "outside-target.txt");
  try {
    await mkdir(workspace);
    await writeFile(outsideTarget, "outside\n", "utf8");

    const escape = await runWrite(
      catalog,
      sessionDir,
      "write-escape",
      {path: "../escape.txt", content: "escape\n"},
      workspace,
    );
    assert.equal(firstLine(escape).status, "forbidden");
    assert.match(escape, /error_code: path_outside_workspace/);
    assert.match(escape, /path: \.\.\/escape\.txt/);
    assert.equal(
      await readFile(join(sessionDir, "escape.txt"), "utf8").catch(() => undefined),
      undefined,
    );

    const nestedEscape = await runWrite(
      catalog,
      sessionDir,
      "write-nested-escape",
      {path: "nested/../../escape2.txt", content: "escape\n"},
      workspace,
    );
    assert.equal(firstLine(nestedEscape).status, "forbidden");
    assert.match(nestedEscape, /error_code: path_outside_workspace/);
    assert.equal(
      await readFile(join(sessionDir, "escape2.txt"), "utf8").catch(() => undefined),
      undefined,
    );

    const absolute = await runWrite(
      catalog,
      sessionDir,
      "write-absolute-outside",
      {path: outsideTarget, content: "escape\n"},
      workspace,
    );
    assert.equal(firstLine(absolute).status, "forbidden");
    assert.match(absolute, /error_code: path_outside_workspace/);
    assert.doesNotMatch(absolute, new RegExp(escapeRegExp(sessionDir)));
    assert.equal(await readFile(outsideTarget, "utf8"), "outside\n");

    const link = join(workspace, "link.txt");
    await symlink(outsideTarget, link);
    const symlinkWrite = await runWrite(
      catalog,
      sessionDir,
      "write-symlink",
      {path: "link.txt", content: "escape\n"},
      workspace,
    );
    assert.equal(firstLine(symlinkWrite).status, "forbidden");
    assert.match(symlinkWrite, /error_code: symlink_forbidden/);
    assert.equal(await readFile(outsideTarget, "utf8"), "outside\n");
    assert.equal((await lstat(link)).isSymbolicLink(), true);

    const externalDir = join(sessionDir, "external-dir");
    await mkdir(externalDir);
    await symlink(externalDir, join(workspace, "linkdir"));
    const symlinkedParent = await runWrite(
      catalog,
      sessionDir,
      "write-symlink-dir",
      {path: "linkdir/child.txt", content: "escape\n"},
      workspace,
    );
    assert.equal(firstLine(symlinkedParent).status, "forbidden");
    assert.match(symlinkedParent, /error_code: symlink_forbidden/);
    assert.deepEqual(await readdir(externalDir), []);

    await mkdir(join(workspace, "a-directory"));
    const directory = await runWrite(
      catalog,
      sessionDir,
      "write-directory",
      {path: "a-directory", content: "x"},
      workspace,
    );
    assert.equal(firstLine(directory).status, "error");
    assert.match(directory, /error_code: target_is_directory/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates content before creating directories, backups, or files", async () => {
  const {sessionDir, catalog} = await openWriteSession({max_bytes: 8});
  try {
    const existingPath = join(sessionDir, "existing.txt");
    await writeFile(existingPath, "original\n", "utf8");
    const before = await stat(existingPath);

    const invalidNested = await runWrite(catalog, sessionDir, "write-invalid-nested", {
      path: "missing/dir/file.txt",
      content: "!!!",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(invalidNested).status, "error");
    assert.match(invalidNested, /error_code: invalid_base64/);
    assert.match(
      invalidNested,
      /suggestion: base64 content must use the standard alphabet/,
    );
    assert.match(invalidNested, /path: missing\/dir\/file\.txt/);
    assert.equal((await readdir(sessionDir)).includes("missing"), false);

    const invalidExisting = await runWrite(catalog, sessionDir, "write-invalid-existing", {
      path: "existing.txt",
      content: "not base64!",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(invalidExisting).status, "error");
    assert.match(invalidExisting, /error_code: invalid_base64/);
    assert.equal(await readFile(existingPath, "utf8"), "original\n");
    assert.equal((await stat(existingPath)).ino, before.ino);
    assert.deepEqual(await backupNamesIn(sessionDir, "", "existing.txt"), []);
    assert.equal((await readdir(sessionDir)).includes(BACKUP_DIRECTORY), false);

    const oversizedNested = await runWrite(catalog, sessionDir, "write-oversized-nested", {
      path: "too-big/dir/file.txt",
      content: "0123456789",
    });
    assert.equal(firstLine(oversizedNested).status, "blocked");
    assert.match(oversizedNested, /error_code: size_limit/);
    assert.match(oversizedNested, /bytes: 10/);
    assert.match(oversizedNested, /max_bytes: 8/);
    assert.equal((await readdir(sessionDir)).includes("too-big"), false);

    const unpadded = await runWrite(catalog, sessionDir, "write-unpadded-base64", {
      path: "unpadded.bin",
      content: " \nAP8= \n",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(unpadded).status, "ok");
    assert.deepEqual(await readFile(join(sessionDir, "unpadded.bin")), Buffer.from([0, 255]));
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports identical content as unchanged without touching the file", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  try {
    const created = await runWrite(catalog, sessionDir, "noop-create", {
      path: "same.txt",
      content: "same content\n",
    });
    assert.equal(informationFields(created).action, "created");
    const before = await stat(join(sessionDir, "same.txt"));

    const unchanged = await runWrite(catalog, sessionDir, "noop-same", {
      path: "same.txt",
      content: "same content\n",
    });
    assert.equal(firstLine(unchanged).status, "ok");
    assert.match(unchanged, /"summary":"Unchanged same\.txt"/);
    assert.deepEqual(informationFields(unchanged), {
      path: "same.txt",
      action: "unchanged",
      bytes: "13",
    });
    const after = await stat(join(sessionDir, "same.txt"));
    assert.equal(after.ino, before.ino);
    assert.equal(after.mtimeMs, before.mtimeMs);
    const entries = await readdir(sessionDir);
    assert.deepEqual(temporaryNames(entries), []);
    assert.deepEqual(await backupNamesIn(sessionDir, "", "same.txt"), []);

    const encoded = await runWrite(catalog, sessionDir, "noop-base64", {
      path: "same.txt",
      content: "c2FtZSBjb250ZW50Cg==",
      contentEncoding: "base64",
    });
    assert.equal(informationFields(encoded).action, "unchanged");

    const unchangedAgain = await runWrite(catalog, sessionDir, "noop-again", {
      path: "same.txt",
      content: "same content\n",
    });
    assert.equal(informationFields(unchangedAgain).action, "unchanged");

    const updated = await runWrite(catalog, sessionDir, "noop-different", {
      path: "same.txt",
      content: "different\n",
    });
    const updatedFields = informationFields(updated);
    assert.equal(updatedFields.action, "updated");
    assert.ok(updatedFields.backup_path);
    assert.equal(
      await readFile(join(sessionDir, updatedFields.backup_path!), "utf8"),
      "same content\n",
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test(
  "preserves the original when the backup cannot be created",
  async () => {
    const {sessionDir, catalog} = await openWriteSession();
    const targetPath = join(sessionDir, "protected.txt");
    try {
      await writeFile(targetPath, "original\n", "utf8");
      // A regular file where the backup directory belongs makes the backup fail.
      await writeFile(backupDirectoryPath(sessionDir), "not a directory\n", "utf8");
      const output = await runWrite(catalog, sessionDir, "write-backup-failure", {
        path: "protected.txt",
        content: "replacement\n",
      });
      assert.equal(firstLine(output).status, "error");
      assert.match(output, /error_code: backup_failed/);
      assert.equal(await readFile(targetPath, "utf8"), "original\n");
      assert.deepEqual(temporaryNames(await readdir(sessionDir)), []);
    } finally {
      await rm(sessionDir, {recursive: true, force: true});
    }
  },
);

test("serializes concurrent writes to one path", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  try {
    const outputs = await Promise.all(
      [1, 2, 3, 4, 5].map((index) =>
        runWrite(catalog, sessionDir, `write-race-${index}`, {
          path: "race.txt",
          content: `value ${index}\n`,
        }),
      ),
    );
    for (const output of outputs) {
      assert.equal(firstLine(output).status, "ok");
    }
    const actions = outputs.map((output) => informationFields(output).action);
    assert.equal(actions.filter((action) => action === "created").length, 1);
    assert.equal(actions.filter((action) => action === "updated").length, 4);
    assert.equal(actions.includes("unchanged"), false);

    const finalContent = await readFile(join(sessionDir, "race.txt"), "utf8");
    assert.match(finalContent, /^value [1-5]\n$/);
    const entries = await readdir(sessionDir);
    assert.deepEqual(temporaryNames(entries), []);
    assert.equal((await backupNamesIn(sessionDir, "", "race.txt")).length, 4);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("enforces the backup count and size quotas", async () => {
  const countSession = await openWriteSession({backup_max_count: 2});
  try {
    const {sessionDir, catalog} = countSession;
    for (let index = 1; index <= 5; index += 1) {
      const output = await runWrite(catalog, sessionDir, `write-count-${index}`, {
        path: "count.txt",
        content: `v${index}\n`,
      });
      assert.equal(firstLine(output).status, "ok");
      await pause(3);
    }
    const backups = await backupNamesIn(sessionDir, "", "count.txt");
    assert.equal(backups.length, 2);
    const newest = backups.sort().reverse()[0]!;
    assert.equal(
      await readFile(join(backupDirectoryPath(sessionDir), newest), "utf8"),
      "v4\n",
    );
  } finally {
    await rm(countSession.sessionDir, {recursive: true, force: true});
  }

  const sizeSession = await openWriteSession({backup_max_total_bytes: 12});
  try {
    const {sessionDir, catalog} = sizeSession;
    for (let index = 1; index <= 3; index += 1) {
      await runWrite(catalog, sessionDir, `write-size-${index}`, {
        path: "size.txt",
        content: `v${index}`.padEnd(10, "."),
      });
      await pause(3);
    }
    assert.equal((await backupNamesIn(sessionDir, "", "size.txt")).length, 1);
  } finally {
    await rm(sizeSession.sessionDir, {recursive: true, force: true});
  }

  const foreignSession = await openWriteSession({backup_max_count: 1});
  try {
    const {sessionDir, catalog} = foreignSession;
    await writeFile(join(sessionDir, "keep.txt"), "keep\n", "utf8");
    await mkdir(backupDirectoryPath(sessionDir), {recursive: true});
    await writeFile(join(backupDirectoryPath(sessionDir), "keep.txt.bak-manual"), "manual\n", "utf8");
    await mkdir(
      join(backupDirectoryPath(sessionDir), "keep.txt.bak-1700000000000-0123456789abcdef"),
    );
    for (let index = 1; index <= 3; index += 1) {
      await runWrite(catalog, sessionDir, `write-foreign-${index}`, {
        path: "keep.txt",
        content: `v${index}\n`,
      });
      await pause(3);
    }
    const entries = await readdir(backupDirectoryPath(sessionDir));
    const generated = entries.filter(
      (name) =>
        /^keep\.txt\.bak-\d{13}-[0-9a-f]{16}$/.test(name) &&
        name !== "keep.txt.bak-1700000000000-0123456789abcdef",
    );
    assert.equal(generated.length, 1);
    assert.equal(entries.includes("keep.txt.bak-manual"), true);
    assert.equal(
      entries.includes("keep.txt.bak-1700000000000-0123456789abcdef"),
      true,
    );
    assert.equal(await readFile(join(sessionDir, "keep.txt"), "utf8"), "v3\n");
  } finally {
    await rm(foreignSession.sessionDir, {recursive: true, force: true});
  }
});

test("writes binary, NUL, CRLF, Unicode, and empty content byte-exactly", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  try {
    const nul = await runWrite(catalog, sessionDir, "content-nul", {
      path: "nul.bin",
      content: "a\u0000b",
    });
    assert.equal(informationFields(nul).bytes, "3");
    assert.deepEqual(await readFile(join(sessionDir, "nul.bin")), Buffer.from([0x61, 0x00, 0x62]));

    const crlf = "line one\r\nline two\r\n";
    await runWrite(catalog, sessionDir, "content-crlf", {path: "crlf.txt", content: crlf});
    assert.deepEqual(await readFile(join(sessionDir, "crlf.txt")), Buffer.from(crlf, "utf8"));

    const unicode = "héllo — 漢字 🎉 e\u0301";
    const unicodeResult = await runWrite(catalog, sessionDir, "content-unicode", {
      path: "unicode.txt",
      content: unicode,
    });
    assert.equal(informationFields(unicodeResult).bytes, String(Buffer.byteLength(unicode, "utf8")));
    assert.deepEqual(await readFile(join(sessionDir, "unicode.txt")), Buffer.from(unicode, "utf8"));

    const binary = Buffer.from([0x00, 0x01, 0x80, 0xff, 0x0a, 0x0d]);
    await runWrite(catalog, sessionDir, "content-binary", {
      path: "blob.bin",
      content: binary.toString("base64"),
      contentEncoding: "base64",
    });
    assert.deepEqual(await readFile(join(sessionDir, "blob.bin")), binary);

    const empty = await runWrite(catalog, sessionDir, "content-empty", {
      path: "empty.txt",
      content: "",
    });
    assert.equal(informationFields(empty).action, "created");
    assert.equal(informationFields(empty).bytes, "0");
    assert.equal((await stat(join(sessionDir, "empty.txt"))).size, 0);

    const emptyAgain = await runWrite(catalog, sessionDir, "content-empty-again", {
      path: "empty.txt",
      content: "",
    });
    assert.equal(informationFields(emptyAgain).action, "unchanged");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports one canonical path for successes and errors", async () => {
  const {sessionDir, catalog} = await openWriteSession({max_bytes: 8});
  try {
    await mkdir(join(sessionDir, "a-directory"));
    const relative = await runWrite(catalog, sessionDir, "path-relative", {
      path: "canonical.txt",
      content: "content\n",
    });
    assert.equal(informationFields(relative).path, "canonical.txt");

    const absolute = await runWrite(catalog, sessionDir, "path-absolute", {
      path: join(sessionDir, "canonical.txt"),
      content: "content\n",
    });
    assert.equal(informationFields(absolute).path, "canonical.txt");
    assert.equal(informationFields(absolute).action, "unchanged");

    const absoluteError = await runWrite(catalog, sessionDir, "path-absolute-error", {
      path: join(sessionDir, "canonical.txt"),
      content: "0123456789",
    });
    assert.equal(firstLine(absoluteError).status, "blocked");
    assert.match(absoluteError, /path: canonical\.txt/);
    assert.doesNotMatch(absoluteError, new RegExp(escapeRegExp(sessionDir)));

    const directoryError = await runWrite(catalog, sessionDir, "path-directory-error", {
      path: join(sessionDir, "a-directory"),
      content: "x",
    });
    assert.equal(firstLine(directoryError).status, "error");
    assert.match(directoryError, /error_code: target_is_directory/);
    assert.match(directoryError, /path: a-directory/);
    assert.doesNotMatch(directoryError, new RegExp(escapeRegExp(sessionDir)));

    const invalidAbsolute = await runWrite(catalog, sessionDir, "path-invalid-absolute", {
      path: join(sessionDir, "nested", "value.txt"),
      content: "!",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(invalidAbsolute).status, "error");
    assert.match(invalidAbsolute, /error_code: invalid_base64/);
    assert.match(invalidAbsolute, /path: nested\/value\.txt/);
    assert.doesNotMatch(invalidAbsolute, new RegExp(escapeRegExp(sessionDir)));
    assert.equal((await readdir(sessionDir)).includes("nested"), false);

    const relativeEscape = await runWrite(catalog, sessionDir, "path-relative-escape", {
      path: "../sibling.txt",
      content: "x",
    });
    assert.equal(firstLine(relativeEscape).status, "forbidden");
    assert.match(relativeEscape, /path: \.\.\/sibling\.txt/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects trailing separators and non-directory parents", async () => {
  const {sessionDir, catalog} = await openWriteSession();
  try {
    const trailing = await runWrite(catalog, sessionDir, "path-trailing-separator", {
      path: "new-directory/",
      content: "x",
    });
    assert.equal(firstLine(trailing).status, "error");
    assert.match(trailing, /error_code: invalid_path/);
    assert.equal((await readdir(sessionDir)).includes("new-directory"), false);

    await mkdir(join(sessionDir, "existing-directory"));
    const trailingExisting = await runWrite(catalog, sessionDir, "path-trailing-existing", {
      path: "existing-directory/",
      content: "x",
    });
    assert.equal(firstLine(trailingExisting).status, "error");
    assert.match(trailingExisting, /error_code: target_is_directory/);

    await writeFile(join(sessionDir, "plain-file.txt"), "plain\n", "utf8");
    const nonDirectoryParent = await runWrite(catalog, sessionDir, "path-file-parent", {
      path: "plain-file.txt/child.txt",
      content: "x",
    });
    assert.equal(firstLine(nonDirectoryParent).status, "error");
    assert.match(nonDirectoryParent, /error_code: parent_not_directory/);
    assert.equal(await readFile(join(sessionDir, "plain-file.txt"), "utf8"), "plain\n");

    const rootTarget = await runWrite(catalog, sessionDir, "path-root", {
      path: ".",
      content: "x",
    });
    assert.equal(firstLine(rootTarget).status, "error");
    assert.match(rootTarget, /error_code: target_is_directory/);

    // A backslash is an ordinary character in a POSIX file name.
    const backslash = await runWrite(catalog, sessionDir, "path-backslash", {
      path: "sub\\dir.txt",
      content: "literal\n",
    });
    assert.equal(firstLine(backslash).status, "ok");
    assert.equal(informationFields(backslash).path, "sub\\dir.txt");
    assert.equal(await readFile(join(sessionDir, "sub\\dir.txt"), "utf8"), "literal\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("applies the fsync policy and validates write settings", async () => {
  const unsynced = await openWriteSession({fsync: false});
  try {
    const created = await runWrite(unsynced.catalog, unsynced.sessionDir, "fsync-create", {
      path: "unsynced.txt",
      content: "content\n",
    });
    assert.equal(informationFields(created).action, "created");
    const updated = await runWrite(unsynced.catalog, unsynced.sessionDir, "fsync-update", {
      path: "unsynced.txt",
      content: "content two\n",
    });
    assert.equal(informationFields(updated).action, "updated");
  } finally {
    await rm(unsynced.sessionDir, {recursive: true, force: true});
  }

  const synced = await openWriteSession({fsync: true});
  try {
    const created = await runWrite(synced.catalog, synced.sessionDir, "fsync-default", {
      path: "synced.txt",
      content: "content\n",
    });
    assert.equal(firstLine(created).status, "ok");
  } finally {
    await rm(synced.sessionDir, {recursive: true, force: true});
  }

  const invalid = await makeWriteToolSession();
  try {
    for (const settings of [
      {backup_max_count: 0},
      {backup_max_count: 1.5},
      {backup_max_total_bytes: -1},
      {backup_dir: 7},
      {backup_dir: ""},
      {backup_dir: []},
      {fsync: "yes"},
    ]) {
      await refreshWriteSession(invalid, settings as NativeWriteSessionOptions);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.write\./,
      );
    }
  } finally {
    await rm(invalid, {recursive: true, force: true});
  }
});

test("collects backups in the backup directory without touching the workspace", async () => {
  const {sessionDir, catalog} = await openWriteSession({backup_max_count: 2});
  try {
    await runWrite(catalog, sessionDir, "collect-create", {
      path: "src/app.ts",
      content: "v1\n",
    });
    await runWrite(catalog, sessionDir, "collect-update", {
      path: "src/app.ts",
      content: "v2\n",
    });
    await pause(3);
    const output = await runWrite(catalog, sessionDir, "collect-third", {
      path: "src/app.ts",
      content: "v3\n",
    });

    // The workspace holds only the file itself; the backup sits in the directory.
    assert.deepEqual(await readdir(join(sessionDir, "src")), ["app.ts"]);
    const fields = informationFields(output);
    assert.match(
      fields.backup_path!,
      /^backups\/src\/app\.ts\.bak-\d{13}-[0-9a-f]{16}$/,
    );
    const backups = await backupNamesIn(sessionDir, "src", "app.ts");
    assert.equal(backups.length, 2);
    const newest = backups.sort().reverse()[0]!;
    assert.equal(
      await readFile(join(backupDirectoryPath(sessionDir, "src"), newest), "utf8"),
      "v2\n",
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("resolves a relative backup_dir against cwd and accepts an absolute one", async () => {
  const workspace = await mkdtemp(join(tmpdir(), "agent-harness-workspace-"));
  const absolute = await mkdtemp(join(tmpdir(), "agent-harness-absolute-"));
  try {
    const relativeSession = await openWriteSession({cwd: workspace, backup_dir: "bk"});
    try {
      const {sessionDir, catalog} = relativeSession;
      await runWrite(catalog, sessionDir, "relative-create", {path: "note.md", content: "v1\n"}, workspace);
      const output = await runWrite(
        catalog,
        sessionDir,
        "relative-update",
        {path: "note.md", content: "v2\n"},
        workspace,
      );
      const backup = informationFields(output).backup_path!;
      assert.match(backup, /^bk\/note\.md\.bak-\d{13}-[0-9a-f]{16}$/);
      assert.equal(await readFile(join(workspace, backup), "utf8"), "v1\n");
      assert.deepEqual((await readdir(workspace)).sort(), ["bk", "note.md"]);
    } finally {
      await rm(relativeSession.sessionDir, {recursive: true, force: true});
    }

    const absoluteWorkspace = await mkdtemp(join(tmpdir(), "agent-harness-workspace-"));
    const absoluteSession = await openWriteSession({
      cwd: absoluteWorkspace,
      backup_dir: absolute,
    });
    try {
      const {sessionDir, catalog} = absoluteSession;
      await runWrite(
        catalog,
        sessionDir,
        "absolute-create",
        {path: "note.md", content: "v1\n"},
        absoluteWorkspace,
      );
      const output = await runWrite(
        catalog,
        sessionDir,
        "absolute-update",
        {path: "note.md", content: "v2\n"},
        absoluteWorkspace,
      );
      const backup = informationFields(output).backup_path!;
      assert.equal(backup.startsWith(`${absolute}/`), true);
      assert.equal(await readFile(backup, "utf8"), "v1\n");
      assert.deepEqual(await readdir(absoluteWorkspace), ["note.md"]);
    } finally {
      await rm(absoluteSession.sessionDir, {recursive: true, force: true});
      await rm(absoluteWorkspace, {recursive: true, force: true});
    }
  } finally {
    await rm(workspace, {recursive: true, force: true});
    await rm(absolute, {recursive: true, force: true});
  }
});

test("keeps backups beside the file when backup_dir is null", async () => {
  const {sessionDir, catalog} = await openWriteSession({backup_dir: null});
  try {
    await runWrite(catalog, sessionDir, "sidecar-create", {path: "src/raw.txt", content: "v1\n"});
    const output = await runWrite(catalog, sessionDir, "sidecar-update", {
      path: "src/raw.txt",
      content: "v2\n",
    });
    assert.match(
      informationFields(output).backup_path!,
      /^src\/raw\.txt\.bak-\d{13}-[0-9a-f]{16}$/,
    );
    assert.equal((await readdir(sessionDir)).includes(BACKUP_DIRECTORY), false);
    assert.equal((await readdir(join(sessionDir, "src"))).length, 2);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps one backup history per file inside a shared backup directory", async () => {
  const {sessionDir, catalog} = await openWriteSession({backup_max_count: 1});
  try {
    for (const name of ["a.txt", "b.txt"]) {
      await runWrite(catalog, sessionDir, `${name}-create`, {
        path: `shared/${name}`,
        content: `${name} v1\n`,
      });
      await pause(3);
      await runWrite(catalog, sessionDir, `${name}-update`, {
        path: `shared/${name}`,
        content: `${name} v2\n`,
      });
      await pause(3);
    }
    const names = ["a.txt", "b.txt"];
    for (const name of names) {
      const backups = await backupNamesIn(sessionDir, "shared", name);
      assert.equal(backups.length, 1);
      assert.equal(
        await readFile(join(backupDirectoryPath(sessionDir, "shared"), backups[0]!), "utf8"),
        `${name} v1\n`,
      );
    }
    assert.equal(
      (await readdir(backupDirectoryPath(sessionDir, "shared"))).length,
      names.length,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("mirrors targets outside the workspace under @external", async () => {
  const outside = await mkdtemp(join(tmpdir(), "agent-harness-outside-"));
  const {sessionDir, catalog} = await openWriteSession({
    paths: [{action: "allow", path: outside}],
  });
  try {
    const target = join(outside, "external.txt");
    await runWrite(catalog, sessionDir, "external-create", {path: target, content: "v1\n"});
    const output = await runWrite(catalog, sessionDir, "external-update", {
      path: target,
      content: "v2\n",
    });
    const backup = informationFields(output).backup_path!;
    assert.match(
      backup,
      /^backups\/@external\/[0-9a-f]{16}\/external\.txt\.bak-\d{13}-[0-9a-f]{16}$/,
    );
    assert.equal(await readFile(join(sessionDir, backup), "utf8"), "v1\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(outside, {recursive: true, force: true});
  }
});
