/**
 * @file edit.ts
 * @input A path, the exact text to replace and its replacement
 * @output Exports the native `edit` definition and executor
 * @position Surgical text edits; the mutation itself is delegated to mutate.ts
 *
 * `edit` is the surgical counterpart of `write`: the agent sends only the text
 * it changes, and the exact match against the current file is its own
 * precondition check. There is deliberately no revision argument; a failed
 * match is reported with a bounded diagnostic instead of a guess.
 */

import type {Stats} from "node:fs";
import {stat} from "node:fs/promises";

import type {NativeEditToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {commitFile, errorCode, pathLocks} from "./mutate.js";
import {
  displayPath,
  inspectMutateTarget,
  readVerifiedRegularFile,
  resolveMutatePath,
  sameFileIdentity,
} from "./path.js";
import type {MutateTarget} from "./path.js";
import {
  countNewlines,
  countOccurrences,
  decodeUtf8,
  detectLineEnding,
  diagnoseMatch,
  lineNumberAt,
  occurrenceLineNumbers,
  replaceOccurrences,
  withLineEnding,
} from "./text.js";
import type {LineEnding} from "./text.js";

export const NATIVE_EDIT_ID = "native.edit";

export const nativeEditDefinition: ToolDefinition = {
  id: NATIVE_EDIT_ID,
  source: "native",
  name: "edit",
  description:
    "Replace exact text in an existing file. oldText must match the file's current text exactly and occur once unless replaceAll is true; an empty newText deletes the match. Use write to create files, replace whole files, or write binary content.",
  inputSchema: {
    type: "object",
    properties: {
      path: {
        type: "string",
        minLength: 1,
        description: "Path relative to cwd or an absolute path.",
      },
      oldText: {
        type: "string",
        minLength: 1,
        description: "Exact text to replace, copied from a read.",
      },
      newText: {
        type: "string",
        description: "Replacement text. Empty deletes oldText.",
      },
      replaceAll: {
        type: "boolean",
        description:
          "Replace every occurrence instead of requiring exactly one. Defaults to false.",
      },
      backup: {
        type: "boolean",
        description:
          "Back up the file before replacing it; the session decides where backups are kept. Defaults to true.",
      },
    },
    required: ["path", "oldText", "newText"],
    additionalProperties: false,
  },
};

const MAX_OCCURRENCE_LINES = 5;

type EditAction = "edited" | "unchanged";

interface EditArgs {
  path: string;
  oldText: string;
  newText: string;
  replaceAll?: boolean;
  backup?: boolean;
}

interface EditContext extends ToolExecutionContext {
  policy: NativeEditToolSettings;
}

function failure(
  callId: string,
  errorPath: string,
  errorCodeValue: string,
  summary: string,
  extra: Array<[string, unknown]> = [],
): string {
  return executionError(
    callId,
    "error",
    errorCodeValue,
    summary,
    [["path", errorPath], ...extra],
  );
}

function notFound(callId: string, target: MutateTarget): string {
  return executionError(
    callId,
    "not_found",
    "not_found",
    `Could not find ${target.displayPath}`,
    [
      ["path", target.errorPath],
      ["suggestion", "use write to create the file"],
    ],
  );
}

function editSummary(targetPath: string, replacements: number): string {
  return replacements > 1
    ? `Edited ${targetPath} (${replacements} replacements)`
    : `Edited ${targetPath}`;
}

/** Lines added by the replacement, so `end_line` is where an insert stops. */
function insertedLineSpan(text: string): number {
  if (text.length === 0) {
    return 0;
  }
  const newlines = countNewlines(text);
  return text.endsWith("\n") ? Math.max(0, newlines - 1) : newlines;
}

async function statOrUndefined(path: string): Promise<Stats | undefined> {
  try {
    return await stat(path);
  } catch {
    return undefined;
  }
}

function noMatchResult(
  callId: string,
  target: MutateTarget,
  text: string,
  oldText: string,
  lineEnding: LineEnding | undefined,
): string {
  const diagnostic = diagnoseMatch(text, oldText);
  return executionError(
    callId,
    "not_found",
    "no_match",
    `No exact match in ${target.displayPath}`,
    [
      ["path", target.errorPath],
      ...(lineEnding !== undefined
        ? [["line_ending", lineEnding] as [string, unknown]]
        : []),
      ...(diagnostic
        ? [
            ["nearest_line", diagnostic.nearestLine] as [string, unknown],
            ["matched_lines", diagnostic.matchedLines] as [string, unknown],
            ["expected", diagnostic.expected] as [string, unknown],
            ["actual", diagnostic.actual] as [string, unknown],
            ...(diagnostic.whitespaceOnly
              ? [["difference", "whitespace"] as [string, unknown]]
              : []),
          ]
        : []),
      [
        "suggestion",
        diagnostic
          ? "re-read the file around nearest_line and use its exact text"
          : "re-read the file and use its exact text",
      ],
    ],
  );
}

async function applyEdit(
  target: MutateTarget,
  editArgs: EditArgs,
  context: EditContext,
  policy: NativeEditToolSettings,
): Promise<string> {
  const {callId} = context;

  // The descriptor is opened without following a final symlink and must match
  // the identity recorded during target inspection before its bytes are used.
  if (!target.identity) {
    return failure(
      callId,
      target.errorPath,
      "changed_during_edit",
      "The file changed while it was being edited",
    );
  }
  const verified = await readVerifiedRegularFile(target.realPath, target.identity);
  const after = await statOrUndefined(target.realPath);
  if (
    !verified ||
    !after ||
    !after.isFile() ||
    !sameFileIdentity(after, target.identity) ||
    verified.data.length !== after.size
  ) {
    return failure(
      callId,
      target.errorPath,
      "changed_during_edit",
      "The file changed while it was being edited",
    );
  }
  const data = verified.data;

  const text = decodeUtf8(data);
  if (text === undefined || text.includes("\u0000")) {
    return executionError(
      callId,
      "unsupported_encoding",
      "binary_file",
      "The file is not UTF-8 text",
      [
        ["path", target.errorPath],
        ["suggestion", 'use write with contentEncoding "base64"'],
      ],
    );
  }

  const lineEnding = detectLineEnding(text);
  const oldText = withLineEnding(editArgs.oldText, lineEnding);
  const newText = withLineEnding(editArgs.newText, lineEnding);
  const replaceAll = editArgs.replaceAll ?? false;
  const occurrences = countOccurrences(text, oldText);

  if (occurrences === 0) {
    return noMatchResult(callId, target, text, oldText, lineEnding);
  }
  if (occurrences > 1 && !replaceAll) {
    return executionError(
      callId,
      "error",
      "ambiguous_match",
      `oldText occurs ${occurrences} times in ${target.displayPath}`,
      [
        ["path", target.errorPath],
        ["occurrences", occurrences],
        ...occurrenceLineNumbers(text, oldText, MAX_OCCURRENCE_LINES).map(
          (line) => ["line", line] as [string, unknown],
        ),
        ["suggestion", "add surrounding context to oldText or set replaceAll: true"],
      ],
    );
  }

  const replacements = replaceAll ? occurrences : 1;
  const estimatedBytes =
    data.length +
    replacements * (Buffer.byteLength(newText, "utf8") - Buffer.byteLength(oldText, "utf8"));
  if (estimatedBytes > policy.max_bytes) {
    return executionError(
      callId,
      "blocked",
      "size_limit",
      "The edit exceeds the configured byte limit",
      [
        ["path", target.errorPath],
        ["bytes", estimatedBytes],
        ["max_bytes", policy.max_bytes],
      ],
    );
  }

  const replaced = replaceOccurrences(text, oldText, newText, replacements);
  const nextData = Buffer.from(replaced.text, "utf8");
  const information = (action: EditAction): Array<[string, unknown]> => {
    const fields: Array<[string, unknown]> = [
      ["path", target.displayPath],
      ["action", action],
      ["replacements", replaced.count],
      ["bytes", nextData.length],
    ];
    if (lineEnding !== undefined) {
      fields.push(["line_ending", lineEnding]);
    }
    if (action === "edited" && replaced.count === 1 && replaced.firstIndex !== undefined) {
      const startLine = lineNumberAt(replaced.text, replaced.firstIndex);
      fields.push(
        ["start_line", startLine],
        ["end_line", startLine + insertedLineSpan(newText)],
      );
    }
    return fields;
  };

  if (nextData.equals(data)) {
    return renderToolResult({
      id: callId,
      status: "ok",
      summary: `Unchanged ${target.displayPath}`,
      information: information("unchanged"),
    });
  }

  const commit = await commitFile({
    target,
    settings: policy,
    data: nextData,
    backup: editArgs.backup ?? true,
  });
  if (commit.error === "backup_failed") {
    return failure(
      callId,
      target.errorPath,
      "backup_failed",
      "Could not create a backup before editing",
    );
  }
  if (commit.error === "target_changed") {
    return failure(
      callId,
      target.errorPath,
      "changed_during_edit",
      "The file changed while it was being edited",
    );
  }
  if (commit.error === "write_failed") {
    return failure(callId, target.errorPath, "write_failed", "Could not write the file", [
      ...(commit.systemError
        ? [["system_error", commit.systemError] as [string, unknown]]
        : []),
      ...(commit.backupPath
        ? [[
            "backup_path",
            displayPath(target.workspaceRoot, commit.backupPath),
          ] as [string, unknown]]
        : []),
    ]);
  }

  const resultInformation = information("edited");
  if (commit.backupPath) {
    resultInformation.push([
      "backup_path",
      displayPath(target.workspaceRoot, commit.backupPath),
    ]);
  }

  return renderToolResult({
    id: callId,
    status: "ok",
    summary: editSummary(target.displayPath, replaced.count),
    information: resultInformation,
  });
}

export async function executeNativeEdit(
  args: JsonRecord,
  context: EditContext,
): Promise<string> {
  const editArgs = args as unknown as EditArgs;
  const policy = context.policy;

  // Resolution and inspection have no filesystem side effect, so a rejected
  // edit never creates, renames, or removes anything.
  const resolved = await resolveMutatePath(
    editArgs.path,
    context.workspaceRoot,
    policy,
    context.callId,
  );
  if (resolved.error) {
    return resolved.error;
  }
  const path = resolved.path!;

  // Inspect after acquiring the shared write/edit lock. Otherwise another
  // native mutation can legitimately replace the inode between inspection and
  // this edit's descriptor read, turning serialized operations into a false
  // race failure.
  return pathLocks.runExclusive(path.candidate, async () => {
    const inspected = await inspectMutateTarget(path, policy, context.callId);
    if (inspected.error) {
      return inspected.error;
    }
    const target = inspected.target!;
    if (!target.existed) {
      return notFound(context.callId, target);
    }

    const size = target.size ?? 0;
    if (size > policy.max_bytes) {
      return executionError(
        context.callId,
        "blocked",
        "size_limit",
        "The file exceeds the configured byte limit",
        [
          ["path", target.errorPath],
          ["bytes", size],
          ["max_bytes", policy.max_bytes],
        ],
      );
    }
    return applyEdit(target, editArgs, context, policy);
  });
}
