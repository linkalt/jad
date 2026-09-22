import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {access, chmod, mkdir, mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {runRipgrep} from "../src/tools/native/ripgrep.js";
import {
  firstLine,
  informationFields,
  makeGrepToolSession,
  outputText,
  refreshGrepSession,
  type NativeGrepSessionOptions,
} from "./tool-session.js";

const ripgrepSkip = ((): false | string => {
  try {
    execFileSync("rg", ["--version"], {stdio: "ignore"});
    return false;
  } catch {
    return "ripgrep is not installed";
  }
})();

const gitAvailable = ((): boolean => {
  try {
    execFileSync("git", ["--version"], {stdio: "ignore"});
    return true;
  } catch {
    return false;
  }
})();

type GrepRunner = (callId: string, args: Record<string, unknown>) => Promise<string>;

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

async function openGrepSession(options: NativeGrepSessionOptions = {}) {
  const sessionDir = await makeGrepToolSession(options);
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: GrepRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "grep",
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
  await writeFile(join(sessionDir, "src/a.ts"), "alpha\nbeta TODO\ngamma\ndelta\n", "utf8");
  await writeFile(join(sessionDir, "src/b.ts"), "nothing here\n", "utf8");
  await writeFile(join(sessionDir, "src/sub/c.ts"), "TODO again\n", "utf8");
  await writeFile(join(sessionDir, "src/notes.md"), "TODO in markdown\n", "utf8");
}

test("reaps a ripgrep wrapper's TERM-ignoring descendant after normal exit", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-harness-ripgrep-cleanup-"));
  try {
    const binary = join(directory, "fake-rg");
    const marker = join(directory, "orphan-marker");
    await writeFile(
      binary,
      "#!/bin/sh\n(trap '' TERM; sleep 1; touch orphan-marker) </dev/null >/dev/null 2>&1 &\nexit 1\n",
    );
    await chmod(binary, 0o700);
    const result = await runRipgrep({
      binary,
      flags: [],
      pattern: "unused",
      paths: ["."],
      cwd: directory,
      timeoutMs: 2000,
      maxMatches: 1,
      maxMatchesPerFile: 1,
      maxFileBytes: 1024,
      maxBytes: 1024,
      maxLineChars: 80,
    });
    assert.equal(result.failure, undefined);
    await delay(1500);
    assert.equal(await exists(marker), false);
  } finally {
    await rm(directory, {recursive: true, force: true});
  }
});

test("finds matches with the smallest call and groups them by file", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await writeTree(sessionDir);
    const output = await run("grep-minimal", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"3 matches in 3 files for TODO"/);
    const fields = informationFields(output);
    assert.equal(fields.path, ".");
    assert.equal(fields.pattern, "TODO");
    assert.equal(fields.files, "3");
    assert.equal(fields.matches, "3");
    assert.equal(fields.truncated, "false");
    assert.equal(fields.include, undefined);
    assert.equal(fields.context, undefined);
    assert.match(output, /--- CONTENT token=[\w-]+ encoding=utf8 ---/);
    assert.match(output, /src\/a\.ts\n2:beta TODO\n\n/);
    assert.match(output, /src\/notes\.md\n1:TODO in markdown\n\n/);
    assert.match(output, /src\/sub\/c\.ts\n1:TODO again/);
    assert.doesNotMatch(output, /error_code/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("scopes a search with a path and an include glob", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await writeTree(sessionDir);
    const scoped = await run("grep-path", {pattern: "TODO", path: "src/sub"});
    assert.equal(firstLine(scoped).status, "ok");
    assert.match(scoped, /1 matches in 1 files for TODO/);
    assert.equal(informationFields(scoped).path, "src/sub");
    assert.match(scoped, /src\/sub\/c\.ts\n1:TODO again/);
    assert.doesNotMatch(scoped, /src\/a\.ts/);

    const included = await run("grep-include", {pattern: "TODO", include: "*.md"});
    assert.equal(firstLine(included).status, "ok");
    assert.equal(informationFields(included).include, "*.md");
    assert.match(included, /src\/notes\.md\n1:TODO in markdown/);
    assert.doesNotMatch(included, /src\/a\.ts/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("treats the pattern as literal text when asked", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await writeFile(join(sessionDir, "dots.txt"), "value a.b here\nvalue axb here\n", "utf8");
    const regex = await run("grep-regex-dot", {pattern: "a.b"});
    assert.match(regex, /1:value a\.b here/);
    assert.match(regex, /2:value axb here/);

    const literal = await run("grep-literal-dot", {pattern: "a.b", literal: true});
    assert.equal(informationFields(literal).literal, "true");
    assert.match(literal, /1:value a\.b here/);
    assert.doesNotMatch(literal, /axb/);

    const metacharacters = await run("grep-literal-paren", {pattern: "a(b", literal: true});
    assert.equal(firstLine(metacharacters).status, "ok");
    assert.match(metacharacters, /No matches for a\(b/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("shows context lines with the dash separator", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await writeTree(sessionDir);
    const output = await run("grep-context", {pattern: "beta TODO", path: "src/a.ts", context: 1});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).context, "1");
    assert.match(output, /src\/a\.ts\n1-alpha\n2:beta TODO\n3-gamma/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("stops at the result cap and says so", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession({max_results: 2});
  try {
    await writeTree(sessionDir);
    const output = await run("grep-capped", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"Showing the first 2 matches in \d files for TODO"/);
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.truncation_reason, "max_results");
    assert.equal(fields.max_results, "2");
    assert.match(fields.hint ?? "", /narrow the path, include, or pattern/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("caps matches per file and reports it", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession({max_matches_per_file: 2});
  try {
    await writeFile(
      join(sessionDir, "many.txt"),
      Array.from({length: 6}, (_value, index) => `TODO ${index + 1}`).join("\n") + "\n",
      "utf8",
    );
    const output = await run("grep-per-file", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.matches, "2");
    assert.equal(fields.per_file_limit_reached, "true");
    assert.equal(fields.max_matches_per_file, "2");
    assert.match(fields.hint ?? "", /reached max_matches_per_file/);
    assert.match(output, /many\.txt\n1:TODO 1\n2:TODO 2/);
    assert.doesNotMatch(output, /TODO 3/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("cuts long lines and stops at the byte budget", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession({max_line_chars: 40, max_bytes: 96});
  try {
    await writeFile(join(sessionDir, "long.txt"), `${"x".repeat(300)} HIT\n`, "utf8");
    const output = await run("grep-long-line", {pattern: "HIT"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /\[\.\.\. truncated line\]/);
    assert.match(output, /1:xxxx/);
    assert.doesNotMatch(output, new RegExp("x".repeat(60)));
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("returns a partial result when the search times out", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession({timeout_ms: 1});
  try {
    await writeTree(sessionDir);
    const output = await run("grep-timeout", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.truncation_reason, "timeout");
    assert.match(fields.hint ?? "", /timed out/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports zero matches as a success with a hint", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await writeTree(sessionDir);
    const output = await run("grep-none", {pattern: "zzz-not-here"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"summary":"No matches for zzz-not-here"/);
    const fields = informationFields(output);
    assert.equal(fields.matches, "0");
    assert.equal(fields.files, "0");
    assert.equal(fields.truncated, "false");
    assert.match(fields.hint ?? "", /hidden files/);
    assert.doesNotMatch(output, /--- CONTENT token=/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("skips hidden files but searches explicitly named ones", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    await mkdir(join(sessionDir, ".hidden"), {recursive: true});
    await writeFile(join(sessionDir, ".hidden/d.txt"), "hidden TODO\n", "utf8");
    await writeFile(join(sessionDir, "visible.txt"), "visible TODO\n", "utf8");

    const traversal = await run("grep-hidden-default", {pattern: "TODO"});
    assert.match(traversal, /visible\.txt\n1:visible TODO/);
    assert.doesNotMatch(traversal, /hidden\/d\.txt/);

    const explicit = await run("grep-hidden-explicit", {
      pattern: "TODO",
      path: ".hidden/d.txt",
    });
    assert.equal(firstLine(explicit).status, "ok");
    assert.match(explicit, /\.hidden\/d\.txt\n1:hidden TODO/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("respects ignore files inside a git repository", {skip: ripgrepSkip || (gitAvailable ? false : "git is not installed")}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    execFileSync("git", ["init", "-q", "."], {cwd: sessionDir, stdio: "ignore"});
    await writeFile(join(sessionDir, "ignored.log"), "ignored TODO\n", "utf8");
    await writeFile(join(sessionDir, ".gitignore"), "ignored.log\n", "utf8");
    await writeFile(join(sessionDir, "kept.txt"), "kept TODO\n", "utf8");

    const traversal = await run("grep-ignored-default", {pattern: "TODO"});
    assert.match(traversal, /kept\.txt\n1:kept TODO/);
    assert.doesNotMatch(traversal, /ignored\.log/);

    const explicit = await run("grep-ignored-explicit", {pattern: "TODO", path: "ignored.log"});
    assert.equal(firstLine(explicit).status, "ok");
    assert.match(explicit, /ignored\.log\n1:ignored TODO/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects malformed patterns, missing paths, and denied paths", {skip: ripgrepSkip}, async () => {
  const sessionDir = await makeGrepToolSession();
  // Rule paths are normalised at load time, so deny an absolute path here.
  await refreshGrepSession(sessionDir, {
    paths: [{action: "deny", path: join(sessionDir, "secret")}],
  });
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: GrepRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "grep",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  try {
    await writeFile(join(sessionDir, "code.ts"), "TODO in workspace\n", "utf8");
    await mkdir(join(sessionDir, "secret"), {recursive: true});
    await writeFile(join(sessionDir, "secret/hidden.ts"), "TODO in secret\n", "utf8");

    const malformed = await run("grep-bad-pattern", {pattern: "a("});
    assert.equal(firstLine(malformed).status, "invalid_input");
    assert.match(malformed, /error_code: invalid_pattern/);
    assert.match(malformed, /system_error: .*regex parse error/);
    assert.match(malformed, /suggestion: use literal: true/);

    const missing = await run("grep-missing", {pattern: "TODO", path: "nope"});
    assert.equal(firstLine(missing).status, "not_found");
    assert.match(missing, /error_code: not_found/);

    const outside = await run("grep-outside", {pattern: "TODO", path: "../outside"});
    assert.equal(firstLine(outside).status, "forbidden");
    assert.match(outside, /error_code: path_outside_workspace/);

    const denied = await run("grep-denied", {pattern: "TODO", path: "secret"});
    assert.equal(firstLine(denied).status, "forbidden");
    assert.match(denied, /error_code: path_denied/);

    // A denied subtree must not leak through a broader search root either.
    const traversal = await run("grep-workspace", {pattern: "TODO"});
    assert.equal(firstLine(traversal).status, "ok");
    assert.match(traversal, /code\.ts\n1:TODO in workspace/);
    assert.match(traversal, /"summary":"1 matches in 1 files for TODO"/);
    assert.doesNotMatch(traversal, /secret|hidden\.ts/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("excludes denied files before they consume the match budget", {skip: ripgrepSkip}, async () => {
  const sessionDir = await makeGrepToolSession({max_results: 1});
  await refreshGrepSession(sessionDir, {
    max_results: 1,
    paths: [{action: "deny", path: join(sessionDir, "a-secret.txt")}],
  });
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const run: GrepRunner = async (callId, args) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {type: "function_call", call_id: callId, name: "grep", arguments: JSON.stringify(args)},
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  try {
    await writeFile(join(sessionDir, "a-secret.txt"), "TODO secret\n", "utf8");
    await writeFile(join(sessionDir, "z-visible.txt"), "TODO visible\n", "utf8");
    const output = await run("grep-deny-budget", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /z-visible\.txt\n1:TODO visible/);
    assert.match(output, /"summary":"1 matches in 1 files for TODO"/);
    assert.equal(informationFields(output).truncated, "false");
    assert.doesNotMatch(output, /a-secret|TODO secret/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reports an unusable ripgrep binary with a suggestion", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession({binary: "/nonexistent/rg-for-tests"});
  try {
    await writeFile(join(sessionDir, "code.ts"), "TODO\n", "utf8");
    const output = await run("grep-no-binary", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "error");
    assert.match(output, /error_code: ripgrep_unavailable/);
    assert.match(output, /suggestion: install ripgrep or point tools\.native\.grep\.binary/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates grep settings and inherits read path rules", async () => {
  const sessionDir = await makeGrepToolSession();
  try {
    const readRules = [{action: "deny", path: "secret"}];
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "grep.json")],
        tools: {native: {read: {paths: readRules}, grep: {max_results: 500}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    const grep = config.settings.tools.native.grep!;
    assert.equal(grep.binary, "rg");
    assert.equal(grep.max_results, 500);
    assert.equal(grep.max_matches_per_file, 20);
    assert.equal(grep.max_bytes, 65536);
    assert.equal(grep.max_line_chars, 240);
    assert.equal(grep.max_file_bytes, 8388608);
    assert.equal(grep.timeout_ms, 10000);
    assert.deepEqual(grep.paths, config.settings.tools.native.read?.paths);

    const overrides = [{action: "allow", path: "/"}];
    await refreshGrepSession(sessionDir, {paths: overrides});
    const overridden = await new SessionConfigLoader(sessionDir).load();
    assert.deepEqual(
      overridden.settings.tools.native.grep?.paths,
      [{action: "allow", path: "/"}],
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }

  const invalid = await makeGrepToolSession();
  try {
    for (const settings of [
      {binary: "  "},
      {max_results: 0},
      {max_matches_per_file: 0},
      {max_bytes: 0},
      {max_line_chars: 5},
      {max_file_bytes: 0},
      {timeout_ms: 0},
    ]) {
      await refreshGrepSession(invalid, settings);
      await assert.rejects(
        new SessionConfigLoader(invalid).load(),
        /tools\.native\.grep\./,
      );
    }
  } finally {
    await rm(invalid, {recursive: true, force: true});
  }
});

test("requires the grep definition when grep is configured", async () => {
  const sessionDir = await makeGrepToolSession();
  try {
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [],
        tools: {native: {grep: {}}},
      }),
    );
    const config = await new SessionConfigLoader(sessionDir).load();
    await assert.rejects(
      loadToolCatalog(config),
      /tools\.native\.grep is configured but native\.grep is missing from tool_definition_paths/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("keeps tool results bounded for a large repository tree", {skip: ripgrepSkip}, async () => {
  const {sessionDir, run} = await openGrepSession();
  try {
    for (let index = 0; index < 40; index += 1) {
      await writeFile(
        join(sessionDir, `file-${String(index).padStart(2, "0")}.txt`),
        `line one\nTODO in file ${index}\n`,
        "utf8",
      );
    }
    const output = await run("grep-large-tree", {pattern: "TODO"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.matches, "40");
    assert.equal(fields.files, "40");
    assert.ok(Buffer.byteLength(output, "utf8") < 64 * 1024 + 2048);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
