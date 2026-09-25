import { readFile } from "node:fs/promises";

import { writeJsonAtomic, type DurabilityMode } from "./persistence.js";

export interface SessionPathProfileInfo {
  active_profile?: string;
  path_profiles: string[];
}

export interface SessionPathProfileUpdate extends SessionPathProfileInfo {
  active_profile: string;
  changed: boolean;
}

export class SessionPathProfileError extends Error {
  public constructor(message: string) {
    super(message);
    this.name = "SessionPathProfileError";
  }
}

type JsonRecord = Record<string, unknown>;

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function readSetting(settingPath: string): Promise<JsonRecord> {
  let text: string;
  try {
    text = await readFile(settingPath, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return {};
    }
    throw error;
  }
  const value: unknown = JSON.parse(text);
  if (!isRecord(value)) {
    throw new Error(`${settingPath}: session setting must be a JSON object`);
  }
  return value;
}

function pathProfilesFromSetting(setting: JsonRecord): SessionPathProfileInfo {
  const paths = setting.paths;
  if (paths === undefined) {
    return {path_profiles: []};
  }
  if (!isRecord(paths)) {
    throw new Error("session setting paths must be a JSON object");
  }
  const profiles = paths.profiles;
  if (profiles === undefined) {
    return {path_profiles: []};
  }
  if (!isRecord(profiles)) {
    throw new Error("session setting paths.profiles must be a JSON object");
  }
  return {
    ...(typeof paths.active_profile === "string"
      ? {active_profile: paths.active_profile}
      : {}),
    path_profiles: Object.keys(profiles),
  };
}

export async function readSessionPathProfiles(
  settingPath: string,
): Promise<SessionPathProfileInfo> {
  return pathProfilesFromSetting(await readSetting(settingPath));
}

export async function setSessionPathProfile(
  settingPath: string,
  profileName: string,
  durability: DurabilityMode,
): Promise<SessionPathProfileUpdate> {
  const setting = await readSetting(settingPath);
  const current = pathProfilesFromSetting(setting);
  if (current.path_profiles.length === 0) {
    throw new SessionPathProfileError("session has no configured path profiles");
  }
  if (!current.path_profiles.includes(profileName)) {
    throw new SessionPathProfileError(
      `path profile '${profileName}' is not defined in session setting paths.profiles`,
    );
  }
  const changed = current.active_profile !== profileName;
  if (changed) {
    const paths = setting.paths as JsonRecord;
    await writeJsonAtomic(
      settingPath,
      {...setting, paths: {...paths, active_profile: profileName}},
      durability,
    );
  }
  return {
    active_profile: profileName,
    path_profiles: current.path_profiles,
    changed,
  };
}
