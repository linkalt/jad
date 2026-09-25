import type { WebUiSessionConfig, WebUiSessionSnapshot } from "./types.js";

export function sessionSettingsFromConfig(
  config: WebUiSessionConfig,
): Pick<
  WebUiSessionSnapshot,
  "session_name" | "model" | "model_default" | "models" | "active_profile" | "path_profiles"
> {
  return {
    session_name: config.settings.session_name,
    ...(config.settings.paths?.active_profile === undefined
      ? {}
      : {active_profile: config.settings.paths.active_profile}),
    ...(config.pathProfiles === undefined || config.pathProfiles.length === 0
      ? {}
      : {path_profiles: config.pathProfiles}),
    ...(config.settings.paths?.model_default === undefined
      ? {}
      : {model_default: config.settings.paths.model_default}),
    ...(config.settings.paths?.models === undefined
      ? {}
      : {models: config.settings.paths.models}),
    model: {
      provider: config.model.provider,
      model: config.model.model,
    },
  };
}
