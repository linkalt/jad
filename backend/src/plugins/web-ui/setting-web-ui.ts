import type { WebUiSessionConfig, WebUiSessionSnapshot } from "./types.js";

export function sessionSettingsFromConfig(
  config: WebUiSessionConfig,
): Pick<WebUiSessionSnapshot, "session_name" | "model"> {
  return {
    session_name: config.settings.session_name,
    model: {
      provider: config.model.provider,
      model: config.model.model,
    },
  };
}
