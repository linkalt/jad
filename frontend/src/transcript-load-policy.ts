export interface TranscriptLoadPolicy {
  sessionId: string;
  activeRunSessionId: string | null;
  attachedRunSessionId: string | null;
  transcriptHydrated: boolean;
}

/**
 * A stream started in this page owns its optimistic turn, so polling must not
 * replace it. A newly attached page still needs its first transcript snapshot;
 * after that hydration, the stream owns subsequent updates.
 */
export function shouldSkipTranscriptLoad({
  sessionId,
  activeRunSessionId,
  attachedRunSessionId,
  transcriptHydrated,
}: TranscriptLoadPolicy): boolean {
  if (activeRunSessionId !== sessionId) {
    return false;
  }

  if (attachedRunSessionId !== sessionId) {
    return true;
  }

  return transcriptHydrated;
}
