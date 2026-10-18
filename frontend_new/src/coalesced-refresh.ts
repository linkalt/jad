export interface CoalescedRefresh {
  request(): void;
  dispose(): void;
}

/** Run one refresh at a time and collapse concurrent triggers into one follow-up. */
export function createCoalescedRefresh(
  run: () => Promise<void>,
  onError: (error: unknown) => void,
): CoalescedRefresh {
  let inFlight = false;
  let pending = false;
  let disposed = false;

  const drain = async (): Promise<void> => {
    if (inFlight || disposed) {
      return;
    }
    inFlight = true;
    try {
      while (pending && !disposed) {
        pending = false;
        try {
          await run();
        } catch (error) {
          onError(error);
        }
      }
    } finally {
      inFlight = false;
      if (pending && !disposed) {
        void drain();
      }
    }
  };

  return {
    request() {
      if (disposed) {
        return;
      }
      pending = true;
      if (!inFlight) {
        void drain();
      }
    },
    dispose() {
      disposed = true;
      pending = false;
    },
  };
}
