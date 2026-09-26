export interface StopConfirmation {
  stopped: boolean;
}

/** Detach the local stream only after the server confirms it stopped the run. */
export async function stopRunAfterConfirmation(
  requestStop: () => Promise<StopConfirmation>,
  detach: () => void,
): Promise<boolean> {
  const result = await requestStop();
  if (result.stopped) detach();
  return result.stopped;
}
