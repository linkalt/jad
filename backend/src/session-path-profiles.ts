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

export interface SessionPathModelDefaultUpdate extends SessionPathProfileInfo {
  active_profile: string;
  model_default: string;
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

export async function setSessionPathModelDefault(
  settingPath: string,
  profileName: string,
  modelConfigPath: string,
  durability: DurabilityMode,
): Promise<SessionPathModelDefaultUpdate> {
  const setting = await readSetting(settingPath);
  const current = pathProfilesFromSetting(setting);
  // The write targets the named profile, which need not be the stored default:
  // a client pinned to one profile selects the model inside that profile.
  const paths = setting.paths;
  if (!isRecord(paths) || !isRecord(paths.profiles)) {
    throw new SessionPathProfileError("session has no configured path profiles");
  }
  const profiles = paths.profiles;
  const profile = profiles[profileName];
  if (!isRecord(profile)) {
    throw new SessionPathProfileError(`path profile '${profileName}' is not defined in session setting paths.profiles`);
  }
  const configuredModels = Array.isArray(profile.models)
    ? profile.models.flatMap((model) => typeof model === "string" ? [model.trim()] : [])
    : typeof profile.model_default === "string"
      ? [profile.model_default.trim()]
      : [];
  if (!configuredModels.includes(modelConfigPath)) {
    throw new SessionPathProfileError("model config path is not available in the active session path profile");
  }
  const changed = profile.model_default !== modelConfigPath;
  if (changed) {
    await writeJsonAtomic(
      settingPath,
      {
        ...setting,
        paths: {
          ...paths,
          profiles: {
            ...profiles,
            [profileName]: {...profile, model_default: modelConfigPath},
          },
        },
      },
      durability,
    );
  }
  return {
    active_profile: profileName,
    path_profiles: current.path_profiles,
    model_default: modelConfigPath,
    changed,
  };
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
