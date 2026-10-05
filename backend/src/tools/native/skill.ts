/**
 * @file skill.ts
 * @input An optional skill name from the model
 * @output Exports the native `skill` tool: one skill's instructions, or the list of skills
 * @position Read-only bridge from the configured skill libraries to the model
 *
 * One optional argument: a name loads that skill, no name lists them. The
 * directories are the global root's `libraries.skill_dirs`, the same ones the UI
 * shows, re-read per call so edits to `setting.json` apply without a restart.
 */
import type {NativeSkillToolSettings} from "../../config.js";
import {listSkills, readSkill, type RootLibraryAsset} from "../../content-library.js";
import type {JsonRecord} from "../../persistence.js";
import {loadLibraryDirectoriesForRoot} from "../../session-roots.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";

export const NATIVE_SKILL_ID = "native.skill";

export const nativeSkillDefinition: ToolDefinition = {
  id: NATIVE_SKILL_ID,
  source: "native",
  name: "skill",
  description: "Load a skill's instructions by name. Call without a name to list available skills.",
  inputSchema: {
    type: "object",
    properties: {
      name: {type: "string", description: "Skill name from the list."},
    },
    additionalProperties: false,
  },
};

interface SkillContext extends ToolExecutionContext {
  policy: NativeSkillToolSettings;
  /** Global root whose `setting.json` names the skill directories. */
  globalJadRoot: string;
}

/**
 * Tolerant name matching: skill names are lowercase and hyphenated, and a model
 * often writes `Agent_Tool Design` for `agent-tool-design`. An empty name means
 * "list", so a blank string is dropped instead of failing as an unknown skill.
 */
export function normalizeSkillArguments(args: JsonRecord): JsonRecord {
  if (typeof args.name !== "string") return args;
  const normalized = {...args};
  const name = args.name.trim().toLowerCase().replace(/[\s_]+/g, "-");
  if (name.length === 0) {
    delete normalized.name;
  } else {
    normalized.name = name;
  }
  return normalized;
}

/** Cuts to at most `maxBytes` UTF-8 bytes without leaving half a character. */
function cutUtf8(text: string, maxBytes: number): {text: string; cut: boolean} {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= maxBytes) return {text, cut: false};
  let end = maxBytes;
  // Step back over continuation bytes so the cut lands on a character boundary.
  while (end > 0 && (bytes[end]! & 0xc0) === 0x80) end -= 1;
  return {text: bytes.subarray(0, end).toString("utf8"), cut: true};
}

function oneLine(value: string): string {
  return value.replace(/\s+/g, " ").trim();
}

function renderList(id: string, skills: RootLibraryAsset[], maxBytes: number): string {
  if (skills.length === 0) {
    return renderToolResult({
      id,
      status: "ok",
      summary: "No skills are available",
      information: [["skills", 0]],
    });
  }
  const {text, cut} = cutUtf8(
    skills.map((skill) => `${skill.name}: ${oneLine(skill.description)}`).join("\n"),
    maxBytes,
  );
  return renderToolResult({
    id,
    status: "ok",
    summary: `${skills.length} skill${skills.length === 1 ? "" : "s"} available`,
    information: [
      ["skills", skills.length],
      ...(cut ? [["truncated", true] as [string, unknown]] : []),
    ],
    content: {encoding: "utf8", text},
  });
}

export async function executeNativeSkill(
  args: JsonRecord,
  context: SkillContext,
): Promise<string> {
  const id = context.callId;
  let skillDirs: string[];
  try {
    skillDirs = (await loadLibraryDirectoriesForRoot(context.globalJadRoot)).skill_dirs;
  } catch (error) {
    return executionError(id, "error", "skill_directories_unavailable", "Skill directories could not be loaded", [
      ["reason", error instanceof Error ? error.message : String(error)],
    ]);
  }

  const name = typeof args.name === "string" ? args.name : undefined;
  if (name === undefined) {
    return renderList(id, await listSkills(skillDirs), context.policy.max_bytes);
  }

  const found = await readSkill(skillDirs, name);
  if (found === undefined) {
    // Recoverable: hand back the valid names so the model can correct itself
    // in one step instead of probing.
    const available = (await listSkills(skillDirs)).map((skill) => skill.name);
    return executionError(id, "not_found", "skill_not_found", `Skill ${name} was not found`, [
      ["skill", name],
      ["available", available.length === 0 ? "none" : available.join(", ")],
    ]);
  }

  const {asset, directory} = found;
  const {text, cut} = cutUtf8(asset.content, context.policy.max_bytes);
  return renderToolResult({
    id,
    status: "ok",
    summary: `Loaded skill ${asset.name}`,
    information: [
      ["name", asset.name],
      // Relative files the skill mentions resolve against this directory.
      ["directory", directory],
      ["content_bytes", Buffer.byteLength(asset.content, "utf8")],
      ...(cut ? [["truncated", true] as [string, unknown]] : []),
    ],
    content: {encoding: "utf8", text},
  });
}
