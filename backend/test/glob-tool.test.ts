import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {access, chmod, mkdir, mkdtemp, rm, symlink, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {
  compileGlob,
  expandBraceAlternatives,
  parseListedFiles,
} from "../src/tools/native/glob.js";
import {
  firstLine,
  informationFields,
  makeGlobToolSession,
  outputText,
  refreshGlobSession,
  type NativeGlobSessionOptions,
} from "./tool-session.js";

const ripgrepSkip = ((): false | string => {
  try {
    execFileSync("rg", ["--version"], {stdio: "ignore"});
    return false;
  } catch {
    return "ripgrep is not installed";
  }
})();

function gitSkip(): false | string {
  try {
    execFileSync("git", ["--version"], {stdio: "ignore"});
    return false;
  } catch {
    return "git is not installed";
  }
}

type GlobRunner = (callId: string, args: Record<string, unknown>) => Promise<string>;

async function exists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function openGlobSession(options: NativeGlobSessionOptions = {}, repository = false) {
  const sessionDir = await makeGlobToolSession(options);
  if (repository) {
    execFileSync("git", ["init", "-q", "."], {cwd: sessionDir, stdio: "ignore"});
  }
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: GlobRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "glob",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  return {sessionDir, run};
}

async function writeTree(sessionDir: string): Promise<void> {
  await mkdir(join(sessionDir, "src/sub"), {recursive: true});
  await mkdir(join(sessionDir, ".github/workflows"), {recursive: true});
  await mkdir(join(sessionDir, ".hidden"), {recursive: true});
  await writeFile(join(sessionDir, "src/a.test.ts"), "x\n", "utf8");
  await writeFile(join(sessionDir, "src/sub/b.test.ts"), "x\n", "utf8");
  await writeFile(join(sessionDir, "src/c.tsx"), "x\n", "utf8");
  await writeFile(join(sessionDir, "src/notes.md"), "x\n", "utf8");
  await writeFile(join(sessionDir, "package.json"), "{}\n", "utf8");
  await writeFile(join(sessionDir, ".env"), "SECRET=1\n", "utf8");
  await writeFile(join(sessionDir, ".github/workflows/ci.yml"), "on: push\n", "utf8");
  await writeFile(join(sessionDir, ".hidden/d.ts"), "x\n", "utf8");
}

const patternCases: Array<[string, string, boolean]> = [
  ["*.ts", "src/a.test.ts", false],
  ["*.ts", "src/sub/b.test.ts", false],
  ["*.ts", "src/c.tsx", true],
  ["*.ts", "package.json", true],
  ["**/*.ts", "src/a.test.ts", false],
  ["**/*.ts", "a.ts", false],
  ["src/**/*.ts", "src/sub/b.test.ts", false],
  ["src/**/*.ts", "src/a.ts", false],
  ["src/*/*.ts", "src/sub/b.test.ts", false],
  ["src/*.ts", "src/sub/b.test.ts", true],
  ["*.{ts,tsx}", "src/c.tsx", false],
  ["*.{ts,tsx}", "src/notes.md", true],
  ["a?test.ts", "a.test.ts", false],
  ["*.{ts", "*.{ts", false],
];

test("matches documented pattern forms", () => {
  for (const [pattern, path, expected] of patternCases) {
    const compiled = compileGlob(pattern);
    const name = path.slice(path.lastIndexOf("/") + 1);
    assert.equal(
      compiled?.test(name, path) ?? true,
      !expected,
      `${pattern} against ${path} should ${expected ? "not " : ""}match`,
    );
  }
  assert.equal(compileGlob("   "), undefined);
  assert.deepEqual(expandBraceAlternatives("*.{ts,tsx}").sort(), ["*.ts", "*.tsx"]);
  assert.deepEqual(expandBraceAlternatives("*.{ts"), ["*.{ts"]);
  assert.deepEqual(expandBraceAlternatives("*.{ts}").length, 1);
});

test("splits NUL-delimited ripgrep output without changing valid filenames", () => {
  assert.deepEqual(parseListedFiles("a.ts\u0000b/c.ts\u0000", false), ["a.ts", "b/c.ts"]);
  assert.deepEqual(
    parseListedFiles("  spaced name  \u0000rg: legitimate-file\u0000first\nsecond\u0000", false),
    ["  spaced name  ", "rg: legitimate-file", "first\nsecond"],
  );
  // Truncated output may end mid-path, so the final fragment is dropped.
  assert.deepEqual(parseListedFiles("a.ts\u0000b/c", true), ["a.ts"]);
});

test("reaps a glob wrapper's TERM-ignoring descendant after normal exit", async () => {
  const sessionDir = await makeGlobToolSession({binary: join(tmpdir(), "unavailable")});
  try {
    const binary = join(sessionDir, "fake-rg");
    const marker = join(sessionDir, "orphan-marker");
    await writeFile(
      binary,
      "#!/bin/sh\n(trap '' TERM; touch cleanup-ready; sleep 3; touch orphan-marker) </dev/null >/dev/null 2>&1 &\nwhile [ ! -f cleanup-ready ]; do :; done\nexit 0\n",
    );
    await chmod(binary, 0o700);
    await refreshGlobSession(sessionDir, {binary});
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const output = outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: "glob-cleanup",
            name: "glob",
            arguments: JSON.stringify({}),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
    assert.equal(firstLine(output).status, "ok");
    await delay(2500);
    assert.equal(await exists(marker), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("lists files matching a pattern at any depth", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession();
  try {
    await writeTree(sessionDir);
    const output = await run("glob-pattern", {pattern: "*.test.ts"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"2 files matching \*\.test\.ts"/);
    const fields = informationFields(output);
    assert.equal(fields.path, ".");
    assert.equal(fields.pattern, "*.test.ts");
    assert.equal(fields.files, "2");
    assert.equal(fields.truncated, "false");
    assert.equal(fields.hint, undefined);
    assert.match(output, /--- CONTENT token=[\w-]+ encoding=utf8 ---/);
    assert.match(output, /src\/a\.test\.ts\nsrc\/sub\/b\.test\.ts/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("truncates rendered paths with max_line_chars without corrupting the NUL list", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession({max_line_chars: 24, max_bytes: 4096});
  const filename = `${"a".repeat(23)}😀${"tail".repeat(8)}.txt`;
  try {
    await writeFile(join(sessionDir, filename), "x\n", "utf8");
    const output = await run("glob-line-cap", {pattern: "*.txt"});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).files, "1");
    assert.ok(output.includes(`${"a".repeat(23)}😀 [... truncated line]`));
    assert.ok(!output.includes(filename), "the full long path should not be rendered");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("lists every visible file without a pattern", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession();
  try {
    await writeTree(sessionDir);
    const output = await run("glob-all", {});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"\d+ files"/);
    const fields = informationFields(output);
    assert.equal(fields.pattern, undefined);
    assert.ok(Number(fields.files) >= 8, `expected at least 8 files, got ${fields.files}`);
    for (const path of [
      ".env",
      ".github/workflows/ci.yml",
      ".hidden/d.ts",
      "package.json",
      "src/a.test.ts",
      "src/c.tsx",
      "src/notes.md",
      "src/sub/b.test.ts",
    ]) {
      assert.ok(output.includes(path), `${path} should be listed`);
    }
    assert.doesNotMatch(output, /\.git\//);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps gitignored files out of a repository but not out of a plain tree", {skip: ripgrepSkip || gitSkip()}, async () => {
  const repository = await openGlobSession({}, true);
  try {
    await writeFile(join(repository.sessionDir, "kept.ts"), "x\n", "utf8");
    await writeFile(join(repository.sessionDir, "ignored.ts"), "x\n", "utf8");
    await writeFile(join(repository.sessionDir, ".gitignore"), "ignored.ts\n", "utf8");
    const output = await repository.run("glob-repo", {pattern: "*.ts"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /kept\.ts/);
    assert.doesNotMatch(output, /ignored\.ts/);
  } finally {
    await rm(repository.sessionDir, {recursive: true, force: true});
  }

  const plain = await openGlobSession();
  try {
    await writeFile(join(plain.sessionDir, "kept.ts"), "x\n", "utf8");
    await writeFile(join(plain.sessionDir, "unignored.ts"), "x\n", "utf8");
    await writeFile(join(plain.sessionDir, ".gitignore"), "unignored.ts\n", "utf8");
    const output = await plain.run("glob-plain", {pattern: "*.ts"});
    const fields = informationFields(output);
    assert.equal(fields.files, "2");
    assert.match(output, /unignored\.ts/);
  } finally {
    await rm(plain.sessionDir, {recursive: true, force: true});
  }
});

test("lists hidden files by default and skips them when disabled", {skip: ripgrepSkip}, async () => {
  const session = await openGlobSession();
  try {
    await writeTree(session.sessionDir);
    const output = await session.run("glob-hidden-on", {pattern: "*.{ts,yml,env}"});
    assert.match(output, /\.env/);
    assert.match(output, /\.github\/workflows\/ci\.yml/);
    assert.match(output, /\.hidden\/d\.ts/);
  } finally {
    await rm(session.sessionDir, {recursive: true, force: true});
  }

  const parity = await openGlobSession({include_hidden: false});
  try {
    await writeTree(parity.sessionDir);
    const output = await parity.run("glob-hidden-off", {pattern: "*"});
    assert.doesNotMatch(output, /\.env/);
    assert.doesNotMatch(output, /\.github\//);
    assert.match(output, /package\.json/);
  } finally {
    await rm(parity.sessionDir, {recursive: true, force: true});
  }
});

test("scopes the listing to a path and skips symlinks", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession();
  try {
    await writeTree(sessionDir);
    const scoped = await run("glob-scoped", {pattern: "*.ts", path: "src/sub"});
    assert.equal(firstLine(scoped).status, "ok");
    assert.equal(informationFields(scoped).path, "src/sub");
    assert.equal(informationFields(scoped).files, "1");
    assert.match(scoped, /src\/sub\/b\.test\.ts/);

    await writeFile(join(sessionDir, "real.ts"), "x\n", "utf8");
    await symlink(join(sessionDir, "real.ts"), join(sessionDir, "linked.ts"));
    await symlink(join(sessionDir, "src"), join(sessionDir, "linked-dir"));
    const links = await run("glob-links", {pattern: "*.ts"});
    assert.match(links, /real\.ts/);
    assert.doesNotMatch(links, /linked\.ts|linked-dir/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("returns an empty listing as a success with a hint", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession();
  try {
    await writeTree(sessionDir);
    const output = await run("glob-none", {pattern: "*.rs"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"0 files matching \*\.rs"/);
    const fields = informationFields(output);
    assert.equal(fields.files, "0");
    assert.equal(fields.truncated, "false");
    assert.match(fields.hint ?? "", /ignored by git/);
    assert.match(fields.hint ?? "", /not a git repository/);
    assert.doesNotMatch(output, /--- CONTENT token=/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("stops at the result cap and says so", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession({max_results: 3});
  try {
    await writeTree(sessionDir);
    const output = await run("glob-capped", {pattern: "*"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"3 files matching \*"/);
    const fields = informationFields(output);
    assert.equal(fields.files, "3");
    assert.equal(fields.truncated, "true");
    assert.equal(fields.truncation_reason, "max_results");
    assert.match(fields.hint ?? "", /narrow the pattern or path/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("stops at the byte budget and says so", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession({max_bytes: 48});
  try {
    await writeTree(sessionDir);
    const output = await run("glob-bytes", {});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.truncation_reason, "max_bytes");
    assert.match(fields.hint ?? "", /narrow the pattern or path/);
    assert.match(output, /narrow the pattern or path/);
    // Every returned line is a complete path, never a cut fragment.
    const content = output.split("--- CONTENT token=")[1] ?? "";
    for (const line of content.split("\n").slice(1)) {
      const value = line.trim();
      if (value.length === 0 || value.startsWith("---")) {
        continue;
      }
      assert.match(value, /^[\w.@/-]+$/);
    }
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports a timeout as a truncated success", {skip: process.platform === "win32" ? "uses a POSIX executable fixture" : false}, async () => {
  const binaryDir = await mkdtemp(join(tmpdir(), "agent-glob-timeout-bin-"));
  let sessionDir: string | undefined;
  try {
    const binary = join(binaryDir, "rg-timeout");
    await writeFile(binary, "#!/usr/bin/env node\nsetInterval(() => {}, 1000);\n", "utf8");
    await chmod(binary, 0o755);
    const session = await openGlobSession({binary, timeout_ms: 100});
    sessionDir = session.sessionDir;
    const output = await session.run("glob-timeout", {pattern: "*"});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).truncation_reason, "timeout");
  } finally {
    if (sessionDir) await rm(sessionDir, {recursive: true, force: true});
    await rm(binaryDir, {recursive: true, force: true});
  }
});

test("validates the searched directory against the path policy", {skip: ripgrepSkip}, async () => {
  const sessionDir = await makeGlobToolSession();
  await refreshGlobSession(sessionDir, {
    paths: [{action: "deny", path: join(sessionDir, "secret")}],
  });
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: GlobRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "glob",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  try {
    await writeFile(join(sessionDir, "plain.ts"), "x\n", "utf8");
    await mkdir(join(sessionDir, "secret"), {recursive: true});
    await writeFile(join(sessionDir, "secret/hidden.ts"), "x\n", "utf8");

    const denied = await run("glob-denied", {path: "secret"});
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);

    // A denied subtree must not leak through a broader listing either.
    const traversal = await run("glob-traversal", {pattern: "*.ts"});
    assert.equal(firstLine(traversal).status, "ok");
    assert.match(traversal, /plain\.ts/);
    assert.doesNotMatch(traversal, /hidden\.ts|secret/);

    const missing = await run("glob-missing", {path: "nope"});
    assert.equal(firstLine(missing).status, "not_found");

    const file = await run("glob-file", {path: "plain.ts"});
    assert.equal(firstLine(file).status, "invalid_input");
    assert.match(file, /error_code: not_a_directory/);

    const outside = await run("glob-outside", {path: "../outside"});
    assert.equal(firstLine(outside).status, "forbidden");
    assert.match(outside, /error_code: path_outside_workspace/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports an unusable ripgrep binary", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession({binary: "/nonexistent/rg-for-tests"});
  try {
    const output = await run("glob-no-binary", {pattern: "*"});
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: ripgrep_unavailable/);
    assert.match(output, /suggestion: install ripgrep or point tools\.native\.glob\.binary/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("normalizes near-miss argument spellings", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGlobSession();
  try {
    await writeTree(sessionDir);
    const output = await run("glob-aliases", {glob: "  *.ts  ", dir: "src"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.pattern, "*.ts");
    assert.equal(fields.path, "src");
    assert.equal(fields.files, "2");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates glob settings and inherits read path rules", async () => {
  const sessionDir = await makeGlobToolSession();
  try {
    const readRules = [{action: "deny", path: "secret"}];
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "glob.json")],
        tools: {native: {read: {paths: readRules}, glob: {max_results: 1000}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    const glob = config.settings.tools.native.glob!;
    assert.equal(glob.binary, "rg");
    assert.equal(glob.include_hidden, true);
    assert.equal(glob.max_results, 1000);
    assert.equal(glob.max_bytes, 65536);
    assert.equal(glob.max_line_chars, 1000);
    assert.equal(glob.timeout_ms, 10000);
    assert.deepEqual(glob.paths, config.settings.tools.native.read?.paths);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }

  const invalid = await makeGlobToolSession();
  try {
    for (const settings of [
      {binary: "   "},
      {include_hidden: "yes"},
      {max_results: 0},
      {max_bytes: 0},
      {max_line_chars: 5},
      {timeout_ms: 0},
    ]) {
      await refreshGlobSession(invalid, settings as NativeGlobSessionOptions);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.glob\./,
      );
    }
  } finally {
    await rm(invalid, {recursive: true, force: true});
  }
});

test("requires the glob definition when glob is configured", async () => {
  const sessionDir = await makeGlobToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [],
        tools: {native: {glob: {}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    await assert.rejects(
      loadToolCatalog(config),
      /tools\.native\.glob is configured but native\.glob is missing from tool_definition_paths/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("every returned path can be fed straight into read", {skip: ripgrepSkip}, async () => {
  const sessionDir = await makeGlobToolSession();
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  try {
    await writeTree(sessionDir);
    const globOutput = outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: "glob-chain",
            name: "glob",
            arguments: JSON.stringify({pattern: "*.test.ts"}),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
    const content = globOutput.split("--- CONTENT token=")[1]?.split(" ---\n")[1] ?? "";
    const paths = content
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line.length > 0 && !line.startsWith("---"));
    assert.deepEqual(paths, ["src/a.test.ts", "src/sub/b.test.ts"]);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
