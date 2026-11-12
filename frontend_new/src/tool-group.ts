/**
 * How a tool group with several calls is laid out: the first call is the
 * header row and the expander lists only the calls after it, so no call is
 * shown twice. The group stays expanded while the activity panel shows one of
 * the listed calls, so the row that opened the panel stays on screen. The
 * header call is always on screen, so the panel showing it holds nothing open.
 */
export type ToolGroupLayout<T extends {callId: string}> = {
  header: T;
  listed: T[];
  holdsOpenPanel: boolean;
};

export function toolGroupLayout<T extends {callId: string}>(
  calls: readonly T[],
  openKey: string | undefined,
): ToolGroupLayout<T> {
  const [header, ...listed] = calls;
  if (header === undefined) {
    throw new Error('a tool group needs at least one call');
  }
  const holdsOpenPanel =
    openKey !== undefined && listed.some(entry => openKey === `tool-${entry.callId}`);
  return {header, listed, holdsOpenPanel};
}
