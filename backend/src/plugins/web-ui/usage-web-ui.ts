import type {SessionUsageSummary} from "../../persistence.js";
import type {WebUiSessionUsage} from "./types.js";

export function sessionUsageFromSummary(
  summary: SessionUsageSummary | undefined,
): WebUiSessionUsage | undefined {
  if (summary === undefined) return undefined;
  return {
    ...summary,
    ...(summary.current === undefined
      ? {}
      : {current: {...summary.current}}),
  };
}
