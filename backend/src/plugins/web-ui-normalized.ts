/**
 * Compatibility exports for the normalized web UI projection.
 * New code should import the focused adapters and reducer from ./web-ui/.
 */
export * from "./web-ui/types.js";
export {
  normalizeWebUiSession,
  responseParts,
  WebUiNormalizedRun,
} from "./web-ui/normalize-web-ui.js";
