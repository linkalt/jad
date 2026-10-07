import {Ajv2020} from "ajv/dist/2020.js";
import assert from "node:assert/strict";
import {mkdir, mkdtemp, readdir, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test, {after} from "node:test";

import {ConfigError, SessionConfigLoader} from "../src/config.js";
import {approvalPreview} from "../src/tools/approval.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {nativeBashDefinition} from "../src/tools/native/bash.js";
import {OutputCollector} from "../src/tools/native/child.js";
import {findExecLang, nativeExecDefinition, normalizeExecArguments} from "../src/tools/native/exec.js";
import {callChangesWorkspace} from "../src/checkpoint.js";
import {firstLine, informationFields, outputText} from "./tool-session.js";

const sessionDirs: string[] = [];

after(async () => {
  await Promise.all(sessionDirs.map((dir) => rm(dir, {recursive: true, force: true})));
});

interface ExecSession {
  sessionDir: string;
  run: (args: Record<string, unknown>) => Promise<string>;
}

async function writeExecSession(
  exec: Record<string, unknown> | undefined,
  bash: Record<string, unknown> | null = {},
): Promise<string> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-exec-tools-"));
  sessionDirs.push(sessionDir);
  await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
  await writeFile(
    join(sessionDir, "tools.json"),
    JSON.stringify({schema_version: 1, tools: [nativeBashDefinition, nativeExecDefinition]}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      cwd: sessionDir,
      tool_definition_paths: [join(sessionDir, "tools.json")],
      tools: {native: {...(bash ? {bash} : {}), ...(exec ? {exec} : {})}},
    }),
  );
  return sessionDir;
}

async function openExec(
  exec: Record<string, unknown> = {},
  bash: Record<string, unknown> = {},
): Promise<ExecSession> {
  const sessionDir = await writeExecSession(exec, bash);
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  let counter = 0;
  const run = async (args: Record<string, unknown>): Promise<string> =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {type: "function_call", call_id: `call-${(counter += 1)}`, name: "exec", arguments: JSON.stringify(args)},
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
  return {sessionDir, run};
}

function contentOf(result: string): string {
  const start = result.indexOf("\n", result.indexOf("--- CONTENT token="));
  const end = result.indexOf("\n--- END CONTENT");
  return start < 0 || end < 0 ? "" : result.slice(start + 1, end);
}

async function configError(exec: Record<string, unknown> | undefined, bash: Record<string, unknown> | null = {}): Promise<string> {
  const sessionDir = await writeExecSession(exec, bash);
  try {
    await new SessionConfigLoader(sessionDir).load();
  } catch (error) {
    assert.ok(error instanceof ConfigError, String(error));
    return error.message;
  }
  return "";
}

test("a call with a language runs the program and returns its output", async () => {
  const {run} = await openExec();
  const result = await run({lang: "python", code: "print(1 + 2)"});
  assert.equal(firstLine(result).status, "ok");
  assert.equal(contentOf(result), "3\n");
});

test("the smallest call is code alone, and it runs in bash", async () => {
  const {run} = await openExec();
  const result = await run({code: "echo $((6 * 7))"});
  assert.equal(firstLine(result).status, "ok");
  assert.equal(contentOf(result), "42\n");
  // $0 is bash's, not sh's: proves the default is the bash entry, not just a shell.
  assert.equal(contentOf(await run({code: "echo ${BASH_VERSION:+bash}"})), "bash\n");
});

test("null, an empty or a blank language means not given and falls back to the default", async () => {
  const {run} = await openExec();
  for (const lang of [null, "", "   "]) {
    assert.equal(contentOf(await run({lang, code: "echo $((6 * 7))"})), "42\n", JSON.stringify(lang));
  }
  assert.equal(contentOf(await run({lang: "python", code: "print(6 * 7)"})), "42\n");
});

test("default_lang is configuration: python as the default runs code alone as python", async () => {
  const python = await openExec({default_lang: "python"});
  assert.equal(contentOf(await python.run({code: "print(6 * 7)"})), "42\n");
  // An alias names the entry, and the entry is what is stored.
  const viaAlias = await new SessionConfigLoader(await writeExecSession({default_lang: " PY "})).load();
  assert.equal(viaAlias.settings.tools.native.exec?.default_lang, "python");
  // A user-defined language can be the default.
  const custom = await openExec({default_lang: "mybash", langs: {mybash: {argv: ["bash", "-s", "--"]}}});
  assert.equal(contentOf(await custom.run({code: "echo custom"})), "custom\n");
});

test("default_lang defaults to bash and is validated when the session loads", async () => {
  const defaults = (await new SessionConfigLoader(await writeExecSession({})).load()).settings.tools.native.exec!;
  assert.equal(defaults.default_lang, "bash");
  assert.match(await configError({default_lang: "cobol"}), /tools\.native\.exec\.default_lang "cobol" is not in tools\.native\.exec\.langs \(available: python, node, bash, sh, ruby, perl\)/);
  assert.match(await configError({default_lang: 3}), /default_lang must be a language name/);
  assert.match(await configError({default_lang: ""}), /default_lang "" is not in/);
  assert.equal((await new SessionConfigLoader(await writeExecSession({default_lang: null})).load()).settings.tools.native.exec?.default_lang, "bash");
});

test("normalization fills the default language so the approval question shows what will run", () => {
  assert.deepEqual(normalizeExecArguments({code: "ls"}, "bash"), {lang: "bash", code: "ls"});
  assert.deepEqual(normalizeExecArguments({code: "ls", language: "node"}, "bash"), {lang: "node", code: "ls"});
  assert.deepEqual(normalizeExecArguments({code: "ls", lang: null}, "bash"), {lang: "bash", code: "ls"});
  assert.deepEqual(normalizeExecArguments({code: "ls", lang: "  "}, "bash"), {lang: "bash", code: "ls"});
  // Without a default the field stays absent and the schema decides.
  assert.deepEqual(normalizeExecArguments({code: "ls"}), {code: "ls"});
  assert.equal(approvalPreview("exec", undefined, normalizeExecArguments({code: "ls"}, "bash"), 100).draft.lang, "bash");
});

test("the schema asks only for code", () => {
  assert.deepEqual(nativeExecDefinition.inputSchema.required, ["code"]);
});

test("node and bash take their code from standard input too", async () => {
  const {run} = await openExec();
  assert.equal(contentOf(await run({lang: "node", code: "console.log(6 * 7)"})), "42\n");
  assert.equal(contentOf(await run({lang: "bash", code: "echo $((6 * 7))"})), "42\n");
});

test("args reach the program as argv without any shell quoting", async () => {
  const {run} = await openExec();
  const code = "import sys\nfor value in sys.argv[1:]:\n    print(repr(value))";
  const result = await run({lang: "python", code, args: ["a b", "$HOME", "it's", "x;y", ""]});
  assert.equal(contentOf(result), "'a b'\n'$HOME'\n\"it's\"\n'x;y'\n''\n");
});

test("a language is found by alias and without regard to case", async () => {
  const {run} = await openExec();
  assert.equal(contentOf(await run({lang: "Python3", code: "print('p')"})), "p\n");
  assert.equal(contentOf(await run({lang: " JS ", code: "console.log('j')"})), "j\n");
});

test("an unknown language lists the available names", async () => {
  const {run} = await openExec();
  const result = await run({lang: "cobol", code: "x"});
  const fields = informationFields(result);
  assert.equal(firstLine(result).status, "invalid_input");
  assert.equal(fields.error_code, "unknown_lang");
  assert.match(fields.available ?? "", /python, node, bash/);
});

test("a non-zero exit is an error that still shows the output and the exit code", async () => {
  const {run} = await openExec();
  const result = await run({lang: "python", code: "import sys\nprint('partial')\nsys.stderr.write('boom\\n')\nsys.exit(3)"});
  const fields = informationFields(result);
  assert.equal(firstLine(result).status, "error");
  assert.equal(fields.error_code, "nonzero_exit");
  assert.equal(fields.exit_code, "3");
  assert.match(contentOf(result), /\[stdout\]\npartial\n\[stderr\]\nboom/);
});

test("the program can write a short answer to $EXEC_RESULT, shown ahead of the raw output", async () => {
  const {run} = await openExec();
  const code = [
    "import os",
    "print('noisy line')",
    "open(os.environ['EXEC_RESULT'], 'w').write('the answer is 42\\n')",
  ].join("\n");
  const result = await run({lang: "python", code});
  assert.equal(contentOf(result), "[result]\nthe answer is 42\n[output]\nnoisy line\n");
});

test("with a result file and no other output only the result is shown", async () => {
  const {run} = await openExec();
  const result = await run({lang: "bash", code: 'echo -n "just this" > "$EXEC_RESULT"'});
  assert.equal(contentOf(result), "[result]\njust this");
});

test("an empty result file is the same as none", async () => {
  const {run} = await openExec();
  const result = await run({lang: "bash", code: 'echo shown; : > "$EXEC_RESULT"'});
  assert.equal(contentOf(result), "shown\n");
});

test("a result longer than result_max_bytes is cut at a character and says how much was dropped", async () => {
  // 11 is odd, so the cut lands in the middle of the sixth two-byte character.
  const {run} = await openExec({result_max_bytes: 11});
  const result = await run({lang: "python", code: "import os\nopen(os.environ['EXEC_RESULT'], 'w').write('é' * 20)"});
  assert.equal(contentOf(result), `[result]\n${"é".repeat(5)}`);
  assert.equal(informationFields(result).result_truncated_bytes, "30");
});

test("code over code_max_bytes is refused whole and the program does not start", async () => {
  const {run, sessionDir} = await openExec({code_max_bytes: 20});
  const result = await run({lang: "bash", code: `touch ${join(sessionDir, "must-not-exist")}; echo xxxxxxxxxxxx`});
  assert.equal(informationFields(result).error_code, "code_too_large");
  assert.deepEqual((await readdir(sessionDir)).filter((name) => name === "must-not-exist"), []);
});

test("the code limit counts UTF-8 bytes, not characters", async () => {
  const {run} = await openExec({code_max_bytes: 12});
  assert.equal(informationFields(await run({lang: "bash", code: ": ✓✓✓✓✓"})).error_code, "code_too_large");
  assert.equal(firstLine(await run({lang: "bash", code: ": ✓✓✓"})).status, "ok");
});

test("a program that is not installed is reported as such, naming the setting to change", async () => {
  const {run} = await openExec({langs: {ghost: {argv: ["definitely-not-installed-xyz", "-"]}}});
  const result = await run({lang: "ghost", code: "x"});
  const fields = informationFields(result);
  assert.equal(fields.error_code, "interpreter_unavailable");
  assert.equal(fields.interpreter, "definitely-not-installed-xyz");
  assert.match(fields.suggestion ?? "", /tools\.native\.exec\.langs\.ghost\.argv/);
});

test("a language that wants a file gets one, the file is gone afterwards, and the code is not in argv", async () => {
  const {run} = await openExec({
    langs: {viafile: {argv: ["bash", "{file}"], code: "file", ext: ".sh"}},
  });
  const code = 'echo "ran $(basename "$0") $1"; echo "dir=$(dirname "$0")" >&2';
  const result = await run({lang: "viafile", code, args: ["z"]});
  const text = contentOf(result);
  assert.match(text, /ran code\.sh z\n/);
  const dir = /dir=(.+)\n/.exec(text)?.[1] ?? "";
  assert.match(dir, /exec-/);
  await assert.rejects(readFile(join(dir, "code.sh")));
});

test("a configured entry replaces a built-in one and brings its own aliases", async () => {
  const {run} = await openExec({langs: {python: {argv: ["python3", "-c", "print('replaced')", "--"], aliases: ["pyx"]}}});
  assert.equal(contentOf(await run({lang: "pyx", code: "print('unused')"})), "replaced\n");
});

test("the working directory and timeout come from bash's policy", async () => {
  const {run, sessionDir} = await openExec({}, {timeout_ms: 1000, max_timeout_ms: 1000});
  await mkdir(join(sessionDir, "sub"));
  assert.match(contentOf(await run({lang: "bash", code: "pwd", cwd: "sub"})), /\/sub\n$/);
  const slow = await run({lang: "bash", code: "sleep 5"});
  assert.equal(informationFields(slow).error_code, "timeout");
});

test("the environment is bash's: the harness credential never reaches the program", async () => {
  const {run} = await openExec({}, {env: {EXEC_VISIBLE: "yes"}});
  const result = await run({lang: "bash", code: 'echo "visible=$EXEC_VISIBLE token=${AGENT_API_TOKEN:-none}"'});
  assert.equal(contentOf(result), "visible=yes token=none\n");
});

test("output the model could not see in full is spilled and advertised, like bash", async () => {
  const {run} = await openExec({}, {stdout: {head_bytes: 50, tail_bytes: 50}, stderr: {head_bytes: 50, tail_bytes: 50}, max_bytes: 4096});
  const result = await run({lang: "python", code: "for i in range(200):\n    print(f'line {i:04d}')"});
  const fields = informationFields(result);
  assert.equal(fields.truncated, "true");
  assert.ok(fields.stdout_full, "stdout_full is reported");
  assert.match(fields.hint ?? "", /complete output is in/);
});

test("line-aware cutting never shows half a line and counts what it drops", async () => {
  const limits = {stdout: {headBytes: 25, tailBytes: 25}, stderr: {headBytes: 0, tailBytes: 0}, maxLineChars: 2000, stripAnsi: true};
  const lines = Array.from({length: 40}, (_, index) => `line-${String(index).padStart(3, "0")}`).join("\n") + "\n";
  const cutAnywhere = new OutputCollector(limits);
  cutAnywhere.push("stdout", Buffer.from(lines));
  cutAnywhere.end();
  const byLine = new OutputCollector({...limits, lineAware: true});
  byLine.push("stdout", Buffer.from(lines));
  byLine.end();
  const plain = cutAnywhere.render();
  const aware = byLine.render();
  assert.ok(plain.text.split("\n").some((line) => line.length > 0 && !/^line-\d{3}$/.test(line) && !line.startsWith("[...")), "the plain cut shows a half line");
  for (const line of aware.text.split("\n")) {
    assert.ok(line === "" || /^line-\d{3}$/.test(line) || /^\[\.\.\. \d+ bytes omitted \.\.\.\]$/.test(line), `unexpected line ${JSON.stringify(line)}`);
  }
  const produced = Buffer.byteLength(lines);
  const shown = Buffer.byteLength(aware.text.replace(/\[\.\.\. \d+ bytes omitted \.\.\.\]\n?/, ""));
  assert.equal(aware.omittedBytes, produced - shown, "the marker counts every dropped byte");
  assert.ok(aware.omittedBytes > plain.omittedBytes, "dropping partial lines drops more bytes");
});

test("line-aware cutting leaves one very long line alone", async () => {
  const limits = {stdout: {headBytes: 10, tailBytes: 10}, stderr: {headBytes: 0, tailBytes: 0}, maxLineChars: 2000, stripAnsi: true, lineAware: true};
  const collector = new OutputCollector(limits);
  collector.push("stdout", Buffer.from("x".repeat(100)));
  collector.end();
  const output = collector.render();
  assert.equal(output.stdout.shownBytes, 20);
  assert.equal(output.omittedBytes, 80);
});

test("exec cuts by line while bash with the same budget does not", async () => {
  // 43 is not a multiple of the 8-byte rows, so a plain cut lands inside a row.
  const bash = {stdout: {head_bytes: 43, tail_bytes: 43}, stderr: {head_bytes: 43, tail_bytes: 43}, max_bytes: 4096};
  const code = "for i in range(100):\n    print('row-%03d' % i)";
  const {run} = await openExec({}, bash);
  const viaExec = contentOf(await run({lang: "python", code}));
  assert.ok(viaExec.split("\n").every((line) => line === "" || /^row-\d{3}$/.test(line) || line.startsWith("[...")), viaExec);
  const off = await openExec({line_aware: false}, bash);
  const raw = contentOf(await off.run({lang: "python", code}));
  assert.ok(raw.split("\n").some((line) => line !== "" && !/^row-\d{3}$/.test(line) && !line.startsWith("[...")), raw);
});

test("normalizes the near-miss names a model uses", () => {
  assert.deepEqual(
    normalizeExecArguments({language: " Python ", source: "print(1)", argv: [1, true, "x"], workdir: "sub"}),
    {lang: "Python", code: "print(1)", args: ["1", "true", "x"], cwd: "sub"},
  );
  assert.deepEqual(normalizeExecArguments({lang: "node", code: "1", args: ["a"], script: "ignored"}), {lang: "node", code: "1", args: ["a"]});
});

test("findExecLang matches names and aliases only", () => {
  const langs = {python: {argv: ["python3", "-"], code: "stdin" as const, ext: ".py", aliases: ["py"]}};
  assert.equal(findExecLang(langs, "PY")?.name, "python");
  assert.equal(findExecLang(langs, "pyth"), undefined);
});

test("the approval question shows the language and the code, cut to the preview size", () => {
  const preview = approvalPreview("exec", undefined, {lang: "python", code: "print('x')\n".repeat(300), args: ["a"], cwd: "sub"}, 100);
  assert.equal(preview.title, "Run code");
  assert.equal(preview.draft.tool, "exec");
  assert.equal(preview.draft.lang, "python");
  assert.equal(String(preview.draft.code).length, 100);
  assert.equal(preview.draft.code_truncated_bytes, 3300);
  assert.deepEqual(preview.draft.args, ["a"]);
  assert.equal(preview.draft.cwd, "sub");
});

test("an exec call takes a checkpoint like bash does", () => {
  assert.equal(callChangesWorkspace({name: "exec", arguments: "{}"}, "native", false), true);
});

test("approval ask on exec holds the call; approval.default reaches it", async () => {
  const sessionDir = await writeExecSession({approval: "ask"});
  const config = await new SessionConfigLoader(sessionDir).load();
  assert.equal(config.settings.tools.native.exec?.approval, "ask");
  const catalog = await loadToolCatalog(config);
  assert.equal(catalog.definitions().some((definition) => definition.name === "exec"), true);
});

test("exec settings default and validate", async () => {
  const defaults = (await new SessionConfigLoader(await writeExecSession({})).load()).settings.tools.native.exec!;
  assert.equal(defaults.code_max_bytes, 262144);
  assert.equal(defaults.result_max_bytes, 16384);
  assert.equal(defaults.line_aware, true);
  assert.equal(defaults.approval, "off");
  assert.deepEqual(Object.keys(defaults.langs), ["python", "node", "bash", "sh", "ruby", "perl"]);
  assert.equal(defaults.default_lang, "bash");
  assert.match(await configError({code_max_bytes: 0}), /tools\.native\.exec\.code_max_bytes must be a safe integer between 1 and 10485760/);
  assert.match(await configError({code_max_bytes: 10485761}), /code_max_bytes/);
  assert.match(await configError({code_max_bytes: 1.5}), /code_max_bytes/);
  assert.match(await configError({result_max_bytes: "10"}), /result_max_bytes/);
  assert.match(await configError({line_aware: "yes"}), /line_aware must be a boolean/);
  assert.match(await configError({approval: "maybe"}), /approval must be "off" or "ask"/);
  assert.match(await configError({unknown_key: 1}), /unknown_key/);
});

test("exec needs bash, whose child-process settings it uses", async () => {
  assert.match(await configError({}, null), /tools\.native\.exec needs tools\.native\.bash/);
});

test("the language table is validated", async () => {
  assert.match(await configError({langs: "python"}), /langs must be an object/);
  assert.match(await configError({langs: {"Bad Name": {argv: ["x"]}}}), /not a valid language name/);
  assert.match(await configError({langs: {x: "python"}}), /langs\.x must be an object/);
  assert.match(await configError({langs: {x: {}}}), /argv must be 1 to 32 non-empty strings/);
  assert.match(await configError({langs: {x: {argv: []}}}), /argv must be 1 to 32/);
  assert.match(await configError({langs: {x: {argv: ["a", ""]}}}), /argv must be 1 to 32/);
  assert.match(await configError({langs: {x: {argv: ["a"], code: "pipe"}}}), /code must be "stdin" or "file"/);
  assert.match(await configError({langs: {x: {argv: ["a"], code: "file"}}}), /must contain \{file\} when code is "file"/);
  assert.match(await configError({langs: {x: {argv: ["a", "{file}"]}}}), /must not contain \{file\} when code is "stdin"/);
  assert.match(await configError({langs: {x: {argv: ["a"], ext: "py"}}}), /ext must be empty or a dot/);
  assert.match(await configError({langs: {x: {argv: ["a"], aliases: ["Bad Alias"]}}}), /aliases must be language names/);
  assert.match(await configError({langs: {x: {argv: ["a"], extra: 1}}}), /extra/);
  assert.match(await configError({langs: {x: {argv: ["a"], aliases: ["py"]}}}), /"py" names both python and x/);
  assert.match(await configError({langs: {node: {argv: ["a"], aliases: ["python"]}}}), /"python" names both python and node/);
});

test("the shipped definition document matches the code", async () => {
  const document = JSON.parse(await readFile(join(import.meta.dirname, "../../../sample/tools/native/exec.json"), "utf8")) as unknown;
  assert.deepEqual(document, {schema_version: 1, tools: [nativeExecDefinition]});
});

test("scratch directories do not accumulate: a call leaves none behind", async () => {
  const before = (await readdir(tmpdir())).filter((name) => name.startsWith("exec-")).length;
  const {run} = await openExec();
  await run({lang: "python", code: "print(1)"});
  await run({lang: "python", code: "import sys; sys.exit(1)"});
  await run({lang: "bash", code: "sleep 5", timeout_ms: 1000});
  const after = (await readdir(tmpdir())).filter((name) => name.startsWith("exec-")).length;
  assert.equal(after, before);
  await rm(join(tmpdir(), "exec-never"), {recursive: true, force: true});
});

test("the schema accepts and rejects the same exec settings as the loader", async () => {
  const schema = JSON.parse(await readFile(new URL("../../../sample/schemes/setting.schema.json", import.meta.url), "utf8"));
  const validate = new Ajv2020({strict: false, allErrors: true}).compile(schema);
  const accepts = (exec: unknown): boolean =>
    validate({schema_version: 1, session_name: "s", cwd: "/tmp", tools: {native: {bash: {}, exec}}}) as boolean;
  assert.equal(accepts({}), true, JSON.stringify(validate.errors));
  assert.equal(accepts({approval: "ask", code_max_bytes: 1, result_max_bytes: 10485760, line_aware: false}), true);
  assert.equal(accepts({langs: {deno: {argv: ["deno", "run", "-"], aliases: ["ts"]}}}), true, JSON.stringify(validate.errors));
  assert.equal(accepts({default_lang: "python"}), true, JSON.stringify(validate.errors));
  assert.equal(accepts({langs: {viafile: {argv: ["x", "{file}"], code: "file", ext: ".x"}}}), true);
  const rejected: unknown[] = [
    {default_lang: 3}, {default_lang: ""}, {default_lang: {}},
    {code_max_bytes: 0}, {code_max_bytes: 10485761}, {code_max_bytes: 1.5}, {result_max_bytes: "10"},
    {line_aware: "yes"}, {approval: "maybe"}, {unknown_key: 1}, {langs: {x: {}}}, {langs: {x: {argv: []}}},
    {langs: {x: {argv: ["a"], code: "pipe"}}}, {langs: {x: {argv: ["a"], ext: "py"}}},
    {langs: {x: {argv: ["a"], extra: 1}}}, {langs: {"Bad Name": {argv: ["a"]}}},
  ];
  for (const bad of rejected) {
    assert.equal(accepts(bad), false, `schema accepted ${JSON.stringify(bad)}`);
  }
});
