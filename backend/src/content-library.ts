import {createHash} from "node:crypto";
import {lstat, readdir, readFile, stat} from "node:fs/promises";
import {basename, extname, relative, resolve} from "node:path";

import {ConfigError} from "./config.js";
import {
  loadRootLibraryDirectories,
  type RootLibraryDirectories,
  type SessionRootOptions,
} from "./session-roots.js";

export type RootLibraryKind = "prompt" | "template" | "skill";

export interface RootLibraryAssetSummary {
  id: string;
  kind: RootLibraryKind;
  name: string;
  description: string;
  source: string;
}

export interface RootLibraryAsset extends RootLibraryAssetSummary {
  content: string;
}

export interface RootLibraryCatalog {
  prompts: RootLibraryAssetSummary[];
  templates: RootLibraryAssetSummary[];
  skills: RootLibraryAssetSummary[];
}

interface ParsedMarkdown {
  name?: string;
  title?: string;
  description?: string;
  body: string;
}

const ASSET_ID_PATTERN = /^[a-f0-9]{64}$/;
const SKILL_NAME_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

function unquoteScalar(value: string): string {
  const trimmed = value.trim();
  if (trimmed.startsWith('"') && trimmed.endsWith('"')) {
    try {
      const parsed: unknown = JSON.parse(trimmed);
      return typeof parsed === "string" ? parsed : trimmed;
    } catch {
      return trimmed.slice(1, -1);
    }
  }
  if (trimmed.startsWith("'") && trimmed.endsWith("'")) {
    return trimmed.slice(1, -1).replaceAll("''", "'");
  }
  return trimmed.replace(/\s+#.*$/, "").trim();
}

function parseFrontmatter(markdown: string): ParsedMarkdown {
  const match = /^(?:\uFEFF)?---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/.exec(markdown);
  if (!match) {
    return {body: markdown.trim()};
  }

  const lines = match[1]!.split(/\r?\n/);
  const metadata: {name?: string; description?: string; title?: string} = {};
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]!;
    const field = /^(name|title|description):\s*(.*)$/.exec(line);
    if (!field) continue;
    const key = field[1] as "name" | "title" | "description";
    const value = field[2]!.trim();
    if (key === "description" && /^(?:\||>)[+-]?$/.test(value)) {
      const block: string[] = [];
      while (index + 1 < lines.length && /^\s+/.test(lines[index + 1]!)) {
        block.push(lines[index + 1]!.trim());
        index += 1;
      }
      metadata.description = value.startsWith(">") ? block.join(" ") : block.join("\n");
      continue;
    }
    const scalar = unquoteScalar(value);
    if (scalar.length > 0) metadata[key] = scalar;
  }

  const body = markdown.slice(match[0].length).trim();
  return {
    ...(metadata.name === undefined ? {} : {name: metadata.name}),
    ...(metadata.title === undefined ? {} : {title: metadata.title}),
    ...(metadata.description === undefined ? {} : {description: metadata.description}),
    body,
  };
}

function firstParagraph(markdown: string): string {
  const paragraph = markdown
    .split(/\r?\n\s*\r?\n/)
    .map((part) => part.trim())
    .find((part) => part.length > 0 && !part.startsWith("#"));
  return paragraph?.replace(/\s+/g, " ") ?? "";
}

function displayName(filePath: string, parsed: ParsedMarkdown): string {
  if (parsed.name !== undefined) return parsed.name;
  if (parsed.title !== undefined) return parsed.title;
  const heading = /^#\s+(.+)$/m.exec(parsed.body)?.[1]?.trim();
  if (heading) return heading;
  return basename(filePath, extname(filePath)).replace(/[-_]+/g, " ");
}

function sourcePath(root: string, filePath: string): string {
  return relative(root, filePath).split("\\").join("/");
}

function assetId(kind: RootLibraryKind, filePath: string): string {
  return createHash("sha256").update(kind).update("\0").update(resolve(filePath)).digest("hex");
}

async function markdownFiles(root: string): Promise<string[]> {
  const pending = [root];
  const files: string[] = [];
  while (pending.length > 0) {
    const directory = pending.pop()!;
    let entries;
    try {
      entries = await readdir(directory, {withFileTypes: true});
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === "ENOENT" || code === "ENOTDIR" || code === "EACCES") continue;
      throw error;
    }
    entries.sort((left, right) => left.name.localeCompare(right.name));
    for (const entry of entries) {
      const path = resolve(directory, entry.name);
      if (entry.isDirectory()) {
        pending.push(path);
      } else if (entry.isFile() && extname(entry.name).toLowerCase() === ".md") {
        files.push(path);
      }
    }
  }
  return files.sort((left, right) => left.localeCompare(right));
}

async function skillFiles(root: string): Promise<string[]> {
  let entries;
  try {
    entries = await readdir(root, {withFileTypes: true});
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ENOTDIR" || code === "EACCES") return [];
    throw error;
  }
  const files: string[] = [];
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    const directory = resolve(root, entry.name);
    const path = resolve(directory, "SKILL.md");
    try {
      const info = await lstat(path);
      if (info.isFile()) files.push(path);
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code !== "ENOENT" && code !== "ENOTDIR" && code !== "EACCES") throw error;
    }
  }
  return files.sort((left, right) => left.localeCompare(right));
}

function directoriesForKind(
  directories: RootLibraryDirectories,
  kind: RootLibraryKind,
): string[] {
  if (kind === "prompt") return directories.prompt_dirs;
  if (kind === "template") return directories.template_dirs;
  return directories.skill_dirs;
}

interface LibraryFile {
  root: string;
  path: string;
}

async function filesForKind(
  kind: RootLibraryKind,
  directories: RootLibraryDirectories,
): Promise<LibraryFile[]> {
  const seen = new Set<string>();
  const files: LibraryFile[] = [];
  for (const root of directoriesForKind(directories, kind)) {
    let rootInfo;
    try {
      rootInfo = await stat(root);
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === "ENOENT" || code === "ENOTDIR" || code === "EACCES") continue;
      throw error;
    }
    if (!rootInfo.isDirectory()) {
      throw new ConfigError(`library directory is not a directory: ${root}`);
    }
    const paths = kind === "skill" ? await skillFiles(root) : await markdownFiles(root);
    for (const path of paths) {
      if (seen.has(path)) continue;
      seen.add(path);
      files.push({root, path});
    }
  }
  return files.sort((left, right) => left.path.localeCompare(right.path));
}

async function readAssetFile(
  kind: RootLibraryKind,
  root: string,
  filePath: string,
): Promise<RootLibraryAsset | undefined> {
  let markdown: string;
  try {
    const info = await lstat(filePath);
    if (!info.isFile()) return undefined;
    markdown = await readFile(filePath, "utf8");
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ENOTDIR" || code === "EACCES") return undefined;
    throw error;
  }

  const parsed = parseFrontmatter(markdown);
  if (parsed.body.length === 0) return undefined;
  if (kind === "skill") {
    const folderName = basename(resolve(filePath, ".."));
    if (
      parsed.name === undefined ||
      parsed.description === undefined ||
      parsed.description.length === 0 ||
      parsed.description.length > 1024 ||
      parsed.name.length > 64 ||
      !SKILL_NAME_PATTERN.test(parsed.name) ||
      parsed.name !== folderName
    ) {
      return undefined;
    }
  }
  const name = kind === "skill" ? parsed.name! : displayName(filePath, parsed);
  const description = parsed.description ?? firstParagraph(parsed.body);
  return {
    id: assetId(kind, filePath),
    kind,
    name,
    description,
    source: sourcePath(root, filePath),
    content: parsed.body,
  };
}

function summary(asset: RootLibraryAsset): RootLibraryAssetSummary {
  const {content: _content, ...entry} = asset;
  return entry;
}

async function listKind(
  kind: RootLibraryKind,
  directories: RootLibraryDirectories,
): Promise<RootLibraryAssetSummary[]> {
  const files = await filesForKind(kind, directories);
  const entries: RootLibraryAssetSummary[] = [];
  for (const file of files) {
    const asset = await readAssetFile(kind, file.root, file.path);
    if (asset !== undefined) entries.push(summary(asset));
  }
  return entries.sort((left, right) =>
    left.name.localeCompare(right.name) || left.source.localeCompare(right.source),
  );
}

export async function listRootLibrary(
  options: SessionRootOptions = {},
): Promise<RootLibraryCatalog> {
  const directories = await loadRootLibraryDirectories(options);
  const [prompts, templates, skills] = await Promise.all([
    listKind("prompt", directories),
    listKind("template", directories),
    listKind("skill", directories),
  ]);
  return {prompts, templates, skills};
}

/**
 * Skills visible to the model, in configured directory order. When two
 * directories define the same skill name the earlier directory wins, so a
 * project can override a shared skill.
 */
export async function listSkills(skillDirs: readonly string[]): Promise<RootLibraryAsset[]> {
  const byName = new Map<string, RootLibraryAsset>();
  for (const root of skillDirs) {
    const files = await filesForKind("skill", {
      prompt_dirs: [],
      template_dirs: [],
      skill_dirs: [root],
    });
    for (const file of files) {
      const asset = await readAssetFile("skill", file.root, file.path);
      if (asset !== undefined && !byName.has(asset.name)) byName.set(asset.name, asset);
    }
  }
  return [...byName.values()].sort((left, right) => left.name.localeCompare(right.name));
}

/**
 * One skill by name. A valid skill's folder name equals its name, so the file
 * is addressed directly instead of scanning every skill; the name pattern
 * rules out path separators and `..`.
 */
export async function readSkill(
  skillDirs: readonly string[],
  name: string,
): Promise<{asset: RootLibraryAsset; directory: string} | undefined> {
  if (!SKILL_NAME_PATTERN.test(name) || name.length > 64) return undefined;
  for (const root of skillDirs) {
    const directory = resolve(root, name);
    const asset = await readAssetFile("skill", root, resolve(directory, "SKILL.md"));
    if (asset !== undefined) return {asset, directory};
  }
  return undefined;
}

export async function readRootLibraryAsset(
  kind: RootLibraryKind,
  id: string,
  options: SessionRootOptions = {},
): Promise<RootLibraryAsset | undefined> {
  if (!ASSET_ID_PATTERN.test(id)) return undefined;
  const directories = await loadRootLibraryDirectories(options);
  const files = await filesForKind(kind, directories);
  for (const file of files) {
    if (assetId(kind, file.path) !== id) continue;
    return readAssetFile(kind, file.root, file.path);
  }
  return undefined;
}
