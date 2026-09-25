import type { WebUiSessionConfig, WebUiSessionSnapshot } from "./types.js";

export function sessionSettingsFromConfig(
  config: WebUiSessionConfig,
): Pick<WebUiSessionSnapshot, "session_name" | "model" | "model_default" | "models"> {
  return {
    session_name: config.settings.session_name,
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
