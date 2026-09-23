import assert from "node:assert/strict";
import {lstat, mkdir, mkdtemp, readFile, readdir, rm, stat, symlink, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {DEFAULT_NATIVE_EDIT_MAX_BYTES, SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {
  DEFAULT_TEST_EDIT_MAX_BYTES,
  firstLine,
  informationEntries,
  informationFields,
  makeEditToolSession,
  makeWriteEditToolSession,
  outputText,
  refreshEditSession,
  type NativeEditSessionOptions,
} from "./tool-session.js";

type ToolCatalog = Awaited<ReturnType<typeof loadToolCatalog>>;

type ToolRunner = (
  name: string,
  callId: string,
  args: Record<string, unknown>,
  workspaceRoot?: string,
) => Promise<string>;

async function openCatalog(sessionDir: string): Promise<ToolCatalog> {
  return loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
}

function toolRunner(catalog: ToolCatalog, defaultWorkspace: string): ToolRunner {
  return async (name, callId, args, workspaceRoot = defaultWorkspace) => {
    const result = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: callId,
        name,
        arguments: JSON.stringify(args),
      },
      {workspaceRoot, sessionDir: defaultWorkspace},
    );
    return outputText(result.output);
  };
}

async function openEditSession(options: NativeEditSessionOptions = {}) {
  const sessionDir = await makeEditToolSession(options);
  return {sessionDir, run: toolRunner(await openCatalog(sessionDir), sessionDir)};
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

test("replaces one exact occurrence and reports the changed lines", async () => {
  const {sessionDir, run} = await openEditSession();
  const original = "line one\nreturn 1;\nline three\n";
  const expected = "line one\nreturn 42;\nline three\n";
  try {
    await mkdir(join(sessionDir, "src"));
    await writeFile(join(sessionDir, "src/app.ts"), original, "utf8");

    const output = await run("edit", "edit-minimal", {
      path: "src/app.ts",
      oldText: "return 1;",
      newText: "return 42;",
    });
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /^.*"summary":"Edited src\/app\.ts"/m);
    const fields = informationFields(output);
    assert.equal(fields.action, "edited");
    assert.equal(fields.replacements, "1");
    assert.equal(fields.bytes, String(Buffer.byteLength(expected, "utf8")));
    assert.equal(fields.line_ending, "lf");
    assert.equal(fields.start_line, "2");
    assert.equal(fields.end_line, "2");
    assert.ok(fields.backup_path?.startsWith("backups/src/app.ts.bak-"));
    assert.equal(await readFile(join(sessionDir, fields.backup_path!), "utf8"), original);
    assert.equal(await readFile(join(sessionDir, "src/app.ts"), "utf8"), expected);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("expresses deletion, insertion, and multi-line replacement", async () => {
  const {sessionDir, run} = await openEditSession();
  const path = "block.txt";
  try {
    const file = join(sessionDir, path);
    await writeFile(file, "header\nkeep\nold block\ntail\n", "utf8");

    const deleted = await run("edit", "edit-delete", {
      path,
      oldText: "keep\n",
      newText: "",
    });
    assert.equal(informationFields(deleted).action, "edited");
    assert.equal(informationFields(deleted).replacements, "1");
    assert.equal(await readFile(file, "utf8"), "header\nold block\ntail\n");

    const inserted = await run("edit", "edit-insert", {
      path,
      oldText: "header\n",
      newText: "header\ninserted\n",
    });
    assert.equal(informationFields(inserted).start_line, "1");
    assert.equal(informationFields(inserted).end_line, "2");
    assert.equal(await readFile(file, "utf8"), "header\ninserted\nold block\ntail\n");

    const block = await run("edit", "edit-multiline", {
      path,
      oldText: "old block\ntail\n",
      newText: "new block\n    indented\n",
    });
    assert.equal(informationFields(block).start_line, "3");
    assert.equal(informationFields(block).end_line, "4");
    assert.equal(await readFile(file, "utf8"), "header\ninserted\nnew block\n    indented\n");

    const prepended = await run("edit", "edit-prepend", {
      path,
      oldText: "header\n",
      newText: "first\nheader\n",
    });
    assert.equal(informationFields(prepended).start_line, "1");
    assert.equal(informationFields(prepended).end_line, "2");
    assert.equal(
      await readFile(file, "utf8"),
      "first\nheader\ninserted\nnew block\n    indented\n",
    );

    const appended = await run("edit", "edit-append", {
      path,
      oldText: "    indented\n",
      newText: "    indented\nlast\n",
    });
    assert.equal(informationFields(appended).start_line, "5");
    assert.equal(informationFields(appended).end_line, "6");
    assert.equal(
      await readFile(file, "utf8"),
      "first\nheader\ninserted\nnew block\n    indented\nlast\n",
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("normalizes server-side line endings before matching", async () => {
  const {sessionDir, run} = await openEditSession();
  try {
    const crlfPath = join(sessionDir, "crlf.txt");
    await writeFile(crlfPath, "one\r\ntwo\r\n", "utf8");
    const crlfEdit = await run("edit", "edit-crlf-normalized", {
      path: "crlf.txt",
      oldText: "two\n",
      newText: "two\nthree\n",
    });
    assert.equal(firstLine(crlfEdit).status, "ok");
    assert.equal(informationFields(crlfEdit).line_ending, "crlf");
    assert.equal(await readFile(crlfPath, "utf8"), "one\r\ntwo\r\nthree\r\n");

    const lfPath = join(sessionDir, "lf.txt");
    await writeFile(lfPath, "one\ntwo\n", "utf8");
    const lfEdit = await run("edit", "edit-lf-normalized", {
      path: "lf.txt",
      oldText: "one\r\n",
      newText: "ONE\r\n",
    });
    assert.equal(firstLine(lfEdit).status, "ok");
    assert.equal(informationFields(lfEdit).line_ending, "lf");
    assert.equal(await readFile(lfPath, "utf8"), "ONE\ntwo\n");

    // A mixed file has no single style, so only an exact match is accepted.
    const mixedPath = join(sessionDir, "mixed.txt");
    await writeFile(mixedPath, "a\r\nb\n", "utf8");
    const normalizedAttempt = await run("edit", "edit-mixed-normalized", {
      path: "mixed.txt",
      oldText: "a\nb\n",
      newText: "x\n",
    });
    assert.equal(firstLine(normalizedAttempt).status, "not_found");
    assert.match(normalizedAttempt, /error_code: no_match/);
    assert.equal(await readFile(mixedPath, "utf8"), "a\r\nb\n");

    const exact = await run("edit", "edit-mixed-exact", {
      path: "mixed.txt",
      oldText: "a\r\nb",
      newText: "x",
    });
    assert.equal(firstLine(exact).status, "ok");
    assert.equal(informationFields(exact).line_ending, "mixed");
    assert.equal(await readFile(mixedPath, "utf8"), "x\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("refuses to guess between repeated occurrences", async () => {
  const {sessionDir, run} = await openEditSession();
  const path = "repeat.txt";
  try {
    const file = join(sessionDir, path);
    const original = "value = 1;\nvalue = 2;\nvalue = 1;\n";
    await writeFile(file, original, "utf8");

    const ambiguous = await run("edit", "edit-ambiguous", {
      path,
      oldText: "value = 1;",
      newText: "value = 9;",
    });
    assert.equal(firstLine(ambiguous).status, "error");
    assert.match(ambiguous, /error_code: ambiguous_match/);
    const fields = informationFields(ambiguous);
    assert.equal(fields.occurrences, "2");
    assert.equal(fields.suggestion, "add surrounding context to oldText or set replaceAll: true");
    assert.deepEqual(
      informationEntries(ambiguous)
        .filter(([key]) => key === "line")
        .map(([, value]) => value),
      ["1", "3"],
    );
    assert.equal(await readFile(file, "utf8"), original);
    assert.deepEqual(await backupNamesIn(sessionDir, "", path), []);

    const replaced = await run("edit", "edit-replace-all", {
      path,
      oldText: "value = 1;",
      newText: "value = 9;",
      replaceAll: true,
    });
    assert.equal(firstLine(replaced).status, "ok");
    assert.match(replaced, /"summary":"Edited repeat\.txt \(2 replacements\)"/);
    const replacedFields = informationFields(replaced);
    assert.equal(replacedFields.action, "edited");
    assert.equal(replacedFields.replacements, "2");
    assert.equal(replacedFields.start_line, undefined);
    assert.equal(await readFile(file, "utf8"), "value = 9;\nvalue = 2;\nvalue = 9;\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("counts occurrences that span multiple lines", async () => {
  const {sessionDir, run} = await openEditSession();
  const original = "a\nb\nX\na\nb\nX\n";
  try {
    const file = join(sessionDir, "blocks.txt");
    await writeFile(file, original, "utf8");
    const ambiguous = await run("edit", "edit-multiline-ambiguous", {
      path: "blocks.txt",
      oldText: "a\nb\nX",
      newText: "done",
    });
    assert.equal(firstLine(ambiguous).status, "error");
    assert.match(ambiguous, /error_code: ambiguous_match/);
    assert.deepEqual(
      informationEntries(ambiguous)
        .filter(([key]) => key === "line")
        .map(([, value]) => value),
      ["1", "4"],
    );
    assert.equal(await readFile(file, "utf8"), original);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports a bounded diagnostic when the text does not match", async () => {
  const {sessionDir, run} = await openEditSession();
  const path = "code.ts";
  try {
    const file = join(sessionDir, path);
    const original = "function main() {\n    return 1;\n}\n";
    await writeFile(file, original, "utf8");

    const whitespace = await run("edit", "edit-no-match-whitespace", {
      path,
      oldText: "function main() {\n  return 1;\n}",
      newText: "x",
    });
    assert.equal(firstLine(whitespace).status, "not_found");
    assert.match(whitespace, /error_code: no_match/);
    const whitespaceFields = informationFields(whitespace);
    assert.equal(whitespaceFields.nearest_line, "1");
    assert.equal(whitespaceFields.matched_lines, "1");
    assert.equal(whitespaceFields.expected, "  return 1;");
    assert.equal(whitespaceFields.actual, "    return 1;");
    assert.equal(whitespaceFields.difference, "whitespace");
    assert.equal(
      whitespaceFields.suggestion,
      "re-read the file around nearest_line and use its exact text",
    );

    const content = await run("edit", "edit-no-match-content", {
      path,
      oldText: "function main() {\n    return 2;\n}",
      newText: "x",
    });
    const contentFields = informationFields(content);
    assert.equal(contentFields.nearest_line, "1");
    assert.equal(contentFields.matched_lines, "1");
    assert.equal(contentFields.expected, "    return 2;");
    assert.equal(contentFields.actual, "    return 1;");
    assert.equal(contentFields.difference, undefined);

    const unrelated = await run("edit", "edit-no-match-unrelated", {
      path,
      oldText: "class Other {",
      newText: "x",
    });
    assert.equal(firstLine(unrelated).status, "not_found");
    assert.equal(informationFields(unrelated).nearest_line, undefined);
    assert.equal(
      informationFields(unrelated).suggestion,
      "re-read the file and use its exact text",
    );

    assert.equal(await readFile(file, "utf8"), original);
    assert.deepEqual(await backupNamesIn(sessionDir, "", path), []);
    assert.deepEqual(temporaryNames(await readdir(sessionDir)), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("leaves the file alone when the replacement is identical", async () => {
  const {sessionDir, run} = await openEditSession();
  const path = "same.txt";
  try {
    const file = join(sessionDir, path);
    await writeFile(file, "same content\n", "utf8");
    const before = await stat(file);

    const unchanged = await run("edit", "edit-noop", {
      path,
      oldText: "same content\n",
      newText: "same content\n",
    });
    assert.equal(firstLine(unchanged).status, "ok");
    assert.match(unchanged, /^.*"summary":"Unchanged same\.txt"/m);
    const fields = informationFields(unchanged);
    assert.equal(fields.action, "unchanged");
    assert.equal(fields.replacements, "1");
    assert.equal(fields.start_line, undefined);
    assert.equal(fields.backup_path, undefined);
    const after = await stat(file);
    assert.equal(after.ino, before.ino);
    assert.equal(after.mtimeMs, before.mtimeMs);
    assert.deepEqual(await backupNamesIn(sessionDir, "", path), []);
    assert.deepEqual(temporaryNames(await readdir(sessionDir)), []);

    // A line-ending change that normalizes back to the current bytes is a no-op.
    const crlfPath = join(sessionDir, "crlf.txt");
    await writeFile(crlfPath, "x\r\n", "utf8");
    const normalized = await run("edit", "edit-noop-crlf", {
      path: "crlf.txt",
      oldText: "x\r\n",
      newText: "x\n",
    });
    assert.equal(informationFields(normalized).action, "unchanged");
    assert.equal(await readFile(crlfPath, "utf8"), "x\r\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects missing files, directories, symlinks, and escapes", async () => {
  const {sessionDir, run} = await openEditSession();
  const workspace = join(sessionDir, "workspace");
  const outside = join(sessionDir, "outside.txt");
  const externalRoot = await mkdtemp(join(tmpdir(), "agent-harness-edit-symlink-"));
  const externalFile = join(externalRoot, "outside.txt");
  const insideTarget = join(sessionDir, "inside.txt");
  try {
    await mkdir(workspace);
    await writeFile(outside, "outside\n", "utf8");
    await writeFile(externalFile, "outside\n", "utf8");
    await writeFile(insideTarget, "inside\n", "utf8");

    const missing = await run("edit", "edit-missing", {
      path: "workspace/missing.txt",
      oldText: "a",
      newText: "b",
    });
    assert.equal(firstLine(missing).status, "not_found");
    assert.match(missing, /error_code: not_found/);
    assert.match(missing, /suggestion: use write to create the file/);
    assert.equal(
      await readFile(join(workspace, "missing.txt"), "utf8").catch(() => undefined),
      undefined,
    );

    await mkdir(join(workspace, "a-directory"));
    const directory = await run("edit", "edit-directory", {
      path: "workspace/a-directory",
      oldText: "a",
      newText: "b",
    });
    assert.equal(firstLine(directory).status, "error");
    assert.match(directory, /error_code: target_is_directory/);

    // A link resolving outside the policy stays refused.
    await symlink(externalFile, join(workspace, "link.txt"));
    const symlinked = await run("edit", "edit-symlink", {
      path: "workspace/link.txt",
      oldText: "outside",
      newText: "changed",
    });
    assert.equal(firstLine(symlinked).status, "forbidden");
    assert.match(symlinked, /error_code: symlink_forbidden/);
    assert.equal(await readFile(externalFile, "utf8"), "outside\n");
    assert.equal((await lstat(join(workspace, "link.txt"))).isSymbolicLink(), true);

    // A link resolving inside the policy is edited through to its real file.
    await symlink(insideTarget, join(workspace, "inside-link.txt"));
    const insideLink = await run("edit", "edit-symlink-inside", {
      path: "workspace/inside-link.txt",
      oldText: "inside",
      newText: "edited",
    });
    assert.equal(firstLine(insideLink).status, "ok");
    assert.equal(await readFile(insideTarget, "utf8"), "edited\n");
    assert.equal((await lstat(join(workspace, "inside-link.txt"))).isSymbolicLink(), true);

    const escape = await run(
      "edit",
      "edit-escape",
      {path: "../outside.txt", oldText: "outside", newText: "changed"},
      workspace,
    );
    assert.equal(firstLine(escape).status, "forbidden");
    assert.match(escape, /error_code: path_outside_workspace/);
    assert.match(escape, /path: \.\.\/outside\.txt/);
    assert.equal(await readFile(outside, "utf8"), "outside\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
  }
});

test("inherits the write path policy and backup history", async () => {
  const deniedRoot = await mkdtemp(join(tmpdir(), "agent-harness-edit-deny-"));
  const allowedRoot = await mkdtemp(join(tmpdir(), "agent-harness-edit-allow-"));
  const deniedFile = join(deniedRoot, "blocked.txt");
  const allowedFile = join(allowedRoot, "allowed.txt");
  const sessionDir = await makeWriteEditToolSession({
    write: {
      backup_max_count: 2,
      paths: [
        {action: "allow", path: "/"},
        {action: "deny", path: deniedRoot},
      ],
    },
  });
  try {
    await writeFile(deniedFile, "blocked\n", "utf8");
    await writeFile(allowedFile, "allowed\n", "utf8");
    const run = toolRunner(await openCatalog(sessionDir), sessionDir);

    const denied = await run("edit", "edit-policy-denied", {
      path: deniedFile,
      oldText: "blocked\n",
      newText: "changed\n",
    });
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);
    assert.equal(await readFile(deniedFile, "utf8"), "blocked\n");

    const allowed = await run("edit", "edit-policy-allowed", {
      path: allowedFile,
      oldText: "allowed\n",
      newText: "changed\n",
    });
    assert.equal(firstLine(allowed).status, "ok");
    assert.match(allowed, /backup_path: .*allowed\.txt\.bak-/);
    assert.equal(await readFile(allowedFile, "utf8"), "changed\n");
    // A target outside the workspace is mirrored under @external/<hash>, and its
    // own directory stays clean.
    const externalRoot = join(backupDirectoryPath(sessionDir), "@external");
    const hashDirectories = await readdir(externalRoot);
    assert.equal(hashDirectories.length, 1);
    assert.match(hashDirectories[0]!, /^[0-9a-f]{16}$/);
    assert.equal(
      backupNames(await readdir(join(externalRoot, hashDirectories[0]!)), "allowed.txt").length,
      1,
    );
    assert.deepEqual(backupNames(await readdir(allowedRoot), "allowed.txt"), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(deniedRoot, {recursive: true, force: true});
    await rm(allowedRoot, {recursive: true, force: true});
  }
});

test("uses write defaults when only edit is configured", async () => {
  const sessionDir = await makeEditToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "edit.json")],
        tools: {native: {edit: {}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    const edit = config.settings.tools.native.edit!;
    assert.equal(edit.max_bytes, DEFAULT_NATIVE_EDIT_MAX_BYTES);
    assert.equal(edit.backup_max_count, 5);
    assert.equal(edit.backup_max_total_bytes, 10 * 1024 * 1024);
    assert.equal(edit.backup_dir, join(sessionDir, BACKUP_DIRECTORY));
    assert.equal(edit.fsync, true);
    assert.deepEqual(edit.paths, []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }

  const inherited = await makeWriteEditToolSession({
    write: {backup_max_count: 7, backup_max_total_bytes: 0, fsync: false},
  });
  try {
    const config = await new SessionConfigLoader(inherited).load();
    const edit = config.settings.tools.native.edit!;
    assert.equal(edit.backup_max_count, 7);
    assert.equal(edit.backup_max_total_bytes, 0);
    assert.equal(edit.backup_dir, join(inherited, BACKUP_DIRECTORY));
    assert.equal(edit.fsync, false);
  } finally {
    await rm(inherited, {recursive: true, force: true});
  }
});

test("refuses to edit non-UTF-8 content", async () => {
  const {sessionDir, run} = await openEditSession();
  try {
    const nulPath = join(sessionDir, "nul.bin");
    const nulData = Buffer.from([0x61, 0x00, 0x62]);
    await writeFile(nulPath, nulData);
    const nul = await run("edit", "edit-binary-nul", {
      path: "nul.bin",
      oldText: "a",
      newText: "b",
    });
    assert.equal(firstLine(nul).status, "unsupported_encoding");
    assert.match(nul, /error_code: binary_file/);
    assert.match(nul, /suggestion: use write with contentEncoding "base64"/);
    assert.deepEqual(await readFile(nulPath), nulData);

    const invalidPath = join(sessionDir, "invalid.bin");
    const invalidData = Buffer.from([0xff, 0xfe, 0x41]);
    await writeFile(invalidPath, invalidData);
    const invalid = await run("edit", "edit-binary-invalid", {
      path: "invalid.bin",
      oldText: "A",
      newText: "B",
    });
    assert.equal(firstLine(invalid).status, "unsupported_encoding");
    assert.deepEqual(await readFile(invalidPath), invalidData);
    assert.deepEqual(backupNames(await readdir(sessionDir), "invalid.bin"), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("enforces the edit byte limit before touching the file", async () => {
  const {sessionDir, run} = await openEditSession({max_bytes: 16});
  try {
    const bigPath = join(sessionDir, "big.txt");
    const bigContent = "0123456789\nabcdefghij\n";
    await writeFile(bigPath, bigContent, "utf8");
    const before = await stat(bigPath);

    const oversized = await run("edit", "edit-too-large", {
      path: "big.txt",
      oldText: "0123456789",
      newText: "x",
    });
    assert.equal(firstLine(oversized).status, "blocked");
    assert.match(oversized, /error_code: size_limit/);
    const fields = informationFields(oversized);
    assert.equal(fields.bytes, String(Buffer.byteLength(bigContent, "utf8")));
    assert.equal(fields.max_bytes, "16");
    assert.equal(await readFile(bigPath, "utf8"), bigContent);
    assert.deepEqual(backupNames(await readdir(sessionDir), "big.txt"), []);

    const growthPath = join(sessionDir, "growth.txt");
    await writeFile(growthPath, "0123456789", "utf8");
    const growth = await run("edit", "edit-too-large-result", {
      path: "growth.txt",
      oldText: "0123456789",
      newText: "0123456789abcdefghij",
    });
    assert.equal(firstLine(growth).status, "blocked");
    assert.match(growth, /error_code: size_limit/);
    assert.equal(informationFields(growth).bytes, "20");
    assert.equal(informationFields(growth).max_bytes, "16");
    assert.equal(await readFile(growthPath, "utf8"), "0123456789");
    assert.equal((await stat(bigPath)).ino, before.ino);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test(
  "preserves the file when the backup cannot be created",
  async () => {
    const {sessionDir, run} = await openEditSession();
    const path = "protected.txt";
    try {
      const file = join(sessionDir, path);
      await writeFile(file, "original\n", "utf8");
      // A regular file where the backup directory belongs makes the backup fail.
      await writeFile(backupDirectoryPath(sessionDir), "not a directory\n", "utf8");
      const output = await run("edit", "edit-backup-failure", {
        path,
        oldText: "original\n",
        newText: "replacement\n",
      });
      assert.equal(firstLine(output).status, "error");
      assert.match(output, /error_code: backup_failed/);
      assert.equal(await readFile(file, "utf8"), "original\n");
      assert.deepEqual(temporaryNames(await readdir(sessionDir)), []);
    } finally {
      await rm(sessionDir, {recursive: true, force: true});
    }
  },
);

test("honours edit's own backup settings over the write settings", async () => {
  const sessionDir = await makeWriteEditToolSession({
    write: {backup_max_count: 4},
    edit: {backup_max_count: 1, backup_dir: null},
  });
  try {
    const config = await new SessionConfigLoader(sessionDir).load();
    const native = config.settings.tools.native;
    assert.equal(native.write!.backup_max_count, 4);
    assert.equal(native.write!.backup_dir, join(sessionDir, BACKUP_DIRECTORY));
    assert.equal(native.edit!.backup_max_count, 1);
    assert.equal(native.edit!.backup_dir, null);

    const run = toolRunner(await openCatalog(sessionDir), sessionDir);
    await writeFile(join(sessionDir, "kept.txt"), "v0\n", "utf8");
    const written = await run("write", "own-write", {path: "kept.txt", content: "v1\n"});
    assert.equal(firstLine(written).status, "ok");
    await pause(3);
    const edited = await run("edit", "own-edit", {
      path: "kept.txt",
      oldText: "v1\n",
      newText: "v2\n",
    });
    assert.equal(firstLine(edited).status, "ok");
    await pause(3);
    const editedAgain = await run("edit", "own-edit-again", {
      path: "kept.txt",
      oldText: "v2\n",
      newText: "v3\n",
    });
    assert.equal(firstLine(editedAgain).status, "ok");

    // The write used the shared backup directory; both edits kept one sidecar
    // each run, which is edit's own count and directory rather than write's.
    const directoryBackups = await backupNamesIn(sessionDir, "", "kept.txt");
    assert.equal(directoryBackups.length, 1);
    assert.equal(
      await readFile(join(backupDirectoryPath(sessionDir), directoryBackups[0]!), "utf8"),
      "v0\n",
    );
    const sidecars = backupNames(await readdir(sessionDir), "kept.txt");
    assert.equal(sidecars.length, 1);
    assert.equal(await readFile(join(sessionDir, sidecars[0]!), "utf8"), "v2\n");
    assert.equal(await readFile(join(sessionDir, "kept.txt"), "utf8"), "v3\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("shares one lock and one backup history with write", async () => {
  const sessionDir = await makeWriteEditToolSession({
    write: {backup_max_count: 2},
  });
  try {
    const run = toolRunner(await openCatalog(sessionDir), sessionDir);
    const path = "shared.txt";
    const file = join(sessionDir, path);

    const created = await run("write", "shared-create", {path, content: "v1\n"});
    assert.equal(informationFields(created).action, "created");
    await pause(3);
    const edited = await run("edit", "shared-edit", {
      path,
      oldText: "v1\n",
      newText: "v2\n",
    });
    assert.equal(firstLine(edited).status, "ok");
    await pause(3);
    const rewritten = await run("write", "shared-write", {path, content: "v3\n"});
    assert.equal(informationFields(rewritten).action, "updated");
    await pause(3);
    const editedAgain = await run("edit", "shared-edit-again", {
      path,
      oldText: "v3\n",
      newText: "v4\n",
    });
    assert.equal(firstLine(editedAgain).status, "ok");

    const backups = await backupNamesIn(sessionDir, "", path);
    assert.equal(backups.length, 2);
    const contents = await Promise.all(
      backups.map((name) => readFile(join(backupDirectoryPath(sessionDir), name), "utf8")),
    );
    assert.deepEqual(contents.sort(), ["v2\n", "v3\n"]);
    assert.equal(await readFile(file, "utf8"), "v4\n");

    // The two tools also share one lock: concurrent calls cannot interleave.
    const [writeOutput, editOutput] = await Promise.all([
      run("write", "shared-parallel-write", {path, content: "aaa\nbbb\n"}),
      run("edit", "shared-parallel-edit", {
        path,
        oldText: "aaa\n",
        newText: "AAA\n",
      }),
    ]);
    assert.equal(firstLine(writeOutput).status, "ok");
    assert.equal(firstLine(editOutput).status, "ok");
    const finalContent = await readFile(file, "utf8");
    assert.equal(
      finalContent === "aaa\nbbb\n" || finalContent === "AAA\nbbb\n",
      true,
      `the file must be one complete serial outcome, not an interleaving: ${JSON.stringify(finalContent)}`,
    );
    assert.equal((await backupNamesIn(sessionDir, "", path)).length, 2);
    assert.deepEqual(temporaryNames(await readdir(sessionDir)), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("edits a large file without resending it", async () => {
  const {sessionDir, run} = await openEditSession();
  const path = "large.txt";
  try {
    const lines = Array.from({length: 500}, (_value, index) => `line ${index + 1}`);
    await writeFile(join(sessionDir, path), `${lines.join("\n")}\n`, "utf8");
    const output = await run("edit", "edit-large", {
      path,
      oldText: "line 250",
      newText: "LINE 250",
    });
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).start_line, "250");
    const content = await readFile(join(sessionDir, path), "utf8");
    assert.match(content, /line 249\nLINE 250\nline 251/);
    assert.equal((await backupNamesIn(sessionDir, "", path)).length, 1);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates the edit arguments and settings", async () => {
  const {sessionDir, run} = await openEditSession();
  try {
    await writeFile(join(sessionDir, "schema.txt"), "content\n", "utf8");
    const schema = await run("edit", "edit-schema", {
      path: "schema.txt",
      oldText: "content",
    });
    assert.equal(firstLine(schema).status, "invalid_input");
    assert.match(schema, /missingProperty/);
    const example = JSON.stringify({
      path: "example.txt",
      oldText: "before",
      newText: "after",
    });
    assert.ok(schema.includes(`example_operation: ${example}`));
    assert.equal(await readFile(join(sessionDir, "schema.txt"), "utf8"), "content\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }

  const invalid = await makeEditToolSession();
  try {
    for (const settings of [{max_bytes: 0}, {max_bytes: 1.5}, {max_bytes: 10 * 1024 * 1024 + 1}]) {
      await refreshEditSession(invalid, settings);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.edit\.max_bytes/,
      );
    }
    await refreshEditSession(invalid);
    const config = await new SessionConfigLoader(invalid).load();
    assert.equal(config.settings.tools.native.edit?.max_bytes, DEFAULT_TEST_EDIT_MAX_BYTES);

    for (const settings of [
      {backup_max_count: 0},
      {backup_max_total_bytes: -1},
      {backup_dir: 5},
      {backup_dir: ""},
      {fsync: "yes"},
    ]) {
      await refreshEditSession(invalid, settings as NativeEditSessionOptions);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.edit\./,
      );
    }
  } finally {
    await rm(invalid, {recursive: true, force: true});
  }
});

test("requires the edit definition when edit is configured", async () => {
  const sessionDir = await makeEditToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [],
        tools: {native: {edit: {}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    await assert.rejects(
      loadToolCatalog(config),
      /tools\.native\.edit is configured but native\.edit is missing from tool_definition_paths/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
