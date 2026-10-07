/**
 * @file exec.ts
 * @input Raw `exec` arguments plus the session settings (tools.native.exec and tools.native.bash)
 * @output Exports the `exec` definition, normalizeExecArguments() and executeNativeExec()
 * @position Native tool that runs one inline program in a configured interpreter
 *
 * `bash` with a `stdin` text already runs an inline script, but the model has to know the
 * interpreter's flags and quote its arguments for a shell. `exec` takes the language and
 * the code, and the backend owns the command line: a call cannot be corrupted by quoting,
 * and the approval question can show the code as code.
 *
 * Everything about how a child runs (environment, timeouts, output budgets, spill files,
 * path policy) is `bash`'s, through runShellChild(), so the two tools bound and report
 * output the same way. What `exec` adds is the interpreter table, argv without a shell,
 * line-aware cutting of long output, and a result channel: the program may write its
 * answer to the file named by $EXEC_RESULT, and that text is shown ahead of the raw output.
 */

import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";

import type {NativeExecLangSettings, NativeExecToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError} from "../output.js";
import type {ToolDefinition} from "../types.js";
import {type NativeBashExecutionContext, runShellChild} from "./bash.js";

export const NATIVE_EXEC_ID = "native.exec";

/** Environment variable that names the file a program may write its answer to. */
export const EXEC_RESULT_ENV = "EXEC_RESULT";

export const nativeExecDefinition: ToolDefinition = {
  id: NATIVE_EXEC_ID,
  source: "native",
  name: "exec",
  description:
    "Run inline code in an interpreter (python, node, bash, ...). Use it to compute, filter or combine data in one call. Write a short answer to the file named by $EXEC_RESULT to see it ahead of the raw output.",
  inputSchema: {
    type: "object",
    properties: {
      lang: {
        type: "string",
        minLength: 1,
        description: "Interpreter name, e.g. python, node, bash. Defaults to bash.",
      },
      code: {
        type: "string",
        minLength: 1,
        description: "Program source.",
      },
      args: {
        type: "array",
        items: {type: "string"},
        description: "Arguments passed to the program as argv, with no shell quoting.",
      },
      cwd: {
        type: "string",
        minLength: 1,
        description:
          "Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root.",
      },
      timeout_ms: {
        type: "integer",
        description:
          "Wall-clock budget for this call in milliseconds; clamped to the configured range.",
      },
    },
    required: ["code"],
    additionalProperties: false,
  },
};

export interface NativeExecExecutionContext extends NativeBashExecutionContext {
  exec: NativeExecToolSettings;
}

function stringField(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

/**
 * Tolerant normalization: the near-miss names a model uses for the same intent, and
 * numbers or booleans given as arguments, become what the schema asks for.
 */
export function normalizeExecArguments(args: JsonRecord, defaultLang?: string): JsonRecord {
  const normalized = {...args};
  if (normalized.lang === null) {
    // A model that fills an optional field with null means "not given".
    delete normalized.lang;
  }
  if (normalized.lang === undefined) {
    const alias = args.language ?? args.interpreter;
    if (typeof alias === "string") {
      normalized.lang = alias;
    }
  }
  if (typeof normalized.lang === "string") {
    normalized.lang = normalized.lang.trim();
    if (normalized.lang === "") {
      // A blank language is "not given" too.
      delete normalized.lang;
    }
  }
  if (normalized.lang === undefined && defaultLang !== undefined) {
    // Filled here, not at run time, so the approval question shows what will run.
    normalized.lang = defaultLang;
  }
  if (normalized.code === undefined) {
    const alias = args.source ?? args.script ?? args.program;
    if (typeof alias === "string") {
      normalized.code = alias;
    }
  }
  if (normalized.args === undefined && Array.isArray(args.argv)) {
    normalized.args = args.argv;
  }
  if (Array.isArray(normalized.args)) {
    normalized.args = normalized.args.map((item) =>
      typeof item === "number" || typeof item === "boolean" ? String(item) : item
    );
  }
  if (normalized.cwd === undefined) {
    const alias = args.workdir ?? args.dir;
    if (typeof alias === "string") {
      normalized.cwd = alias;
    }
  }
  for (const key of ["language", "interpreter", "source", "script", "program", "argv", "workdir", "dir"]) {
    delete normalized[key];
  }
  return normalized;
}

/** The entry a name or alias selects, matched without regard to case. */
export function findExecLang(
  langs: Record<string, NativeExecLangSettings>,
  requested: string,
): {name: string; lang: NativeExecLangSettings} | undefined {
  const wanted = requested.trim().toLowerCase();
  for (const [name, lang] of Object.entries(langs)) {
    if (name === wanted || lang.aliases.includes(wanted)) {
      return {name, lang};
    }
  }
  return undefined;
}

function missingInput(callId: string, code: string, summary: string): string {
  return executionError(callId, "invalid_input", code, summary);
}

export async function executeNativeExec(
  args: JsonRecord,
  context: NativeExecExecutionContext,
): Promise<string> {
  const {callId, exec} = context;
  const requested = stringField(args.lang)?.trim();
  if (requested === undefined) {
    return missingInput(callId, "invalid_lang", "The language is empty");
  }
  const code = stringField(args.code);
  if (code === undefined || code.trim().length === 0) {
    return missingInput(callId, "invalid_code", "The code is empty");
  }
  const found = findExecLang(exec.langs, requested);
  if (!found) {
    return executionError(callId, "invalid_input", "unknown_lang", `Unknown language: ${requested}`, [
      ["available", Object.keys(exec.langs).join(", ")],
      ["suggestion", "use one of the available names, or add the interpreter to tools.native.exec.langs"],
    ]);
  }
  const codeBytes = Buffer.byteLength(code, "utf8");
  if (codeBytes > exec.code_max_bytes) {
    return executionError(
      callId,
      "invalid_input",
      "code_too_large",
      `code is ${codeBytes} bytes, over the limit of ${exec.code_max_bytes}`,
      [["suggestion", "write the program to a file with the write tool and run it with bash"]],
    );
  }
  const argv = args.args === undefined ? [] : args.args;
  if (!Array.isArray(argv) || argv.some((item) => typeof item !== "string")) {
    return missingInput(callId, "invalid_args", "args must be a list of strings");
  }

  // A private directory per call holds the code file (file delivery) and the result file;
  // it is removed afterwards, whatever the program did.
  const workDir = await mkdtemp(join(tmpdir(), "exec-"));
  try {
    const resultPath = join(workDir, "result.txt");
    const [program, ...flags] = found.lang.argv;
    let stdin: string | undefined;
    let programArgs: string[];
    if (found.lang.code === "file") {
      const codePath = join(workDir, `code${found.lang.ext}`);
      await writeFile(codePath, code, {mode: 0o600});
      programArgs = flags.map((flag) => flag.split("{file}").join(codePath));
    } else {
      stdin = code;
      programArgs = flags;
    }
    return await runShellChild(
      {
        args,
        file: program!,
        argv: [...programArgs, ...(argv as string[])],
        ...(stdin !== undefined ? {stdin} : {}),
        lineAware: exec.line_aware,
        extraEnv: {[EXEC_RESULT_ENV]: resultPath},
        resultFile: {path: resultPath, maxBytes: exec.result_max_bytes},
        program: {name: found.name, setting: `tools.native.exec.langs.${found.name}.argv`},
      },
      context,
    );
  } finally {
    await rm(workDir, {recursive: true, force: true}).catch(() => undefined);
  }
}
