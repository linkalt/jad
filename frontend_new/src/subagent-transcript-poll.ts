/**
 * @file subagent-transcript-poll.ts
 * @input A store, a call's cache key, a fetch function, the cadence (from config.json) and the page's visibility
 * @output The poll state of one open subagent transcript, pushed to `onState`
 * @position Used by components/SubagentTranscript.tsx while its panel is open
 *
 * The panel fills in as the child completes responses: fetch at once, then again every
 * `pollMs` while the call is unsettled or the last answer said `running: true`. An answer
 * without `running` is final (an older server never says it, so it gets one fetch). A hidden
 * tab sets no timer and asks again at once when it is visible. A failed read doubles the
 * delay up to `maxDelayMs` and the next answer resets it; an unsettled call whose child has
 * no folder yet (404) is "starting" and keeps the normal cadence. `stop()` leaves nothing
 * pending, so a closed panel has no timer at all.
 */
import type {WebUiSubagentTranscript} from './api.ts';
import type {SubagentTranscriptStore} from './subagent-transcript-store.ts';

export type SubagentPollState =
  | {phase: 'loading'}
  /** The call is unsettled and the child's folder is not there yet. */
  | {phase: 'starting'}
  /** `retryError` is set while a later read failed and the poller is backing off. */
  | {phase: 'ready'; transcript: WebUiSubagentTranscript; retryError?: string}
  | {phase: 'failed'; message: string};

/** What the host knows of the parent's call; it changes while the panel is open. */
export interface SubagentPollInput {
  /** The call has its tool result. */
  settled: boolean;
  /** The settled result's summary count (0 without one). */
  summarySteps: number;
}

export interface SubagentVisibility {
  isVisible(): boolean;
  /** Call `listener` when the visibility changes; returns the unsubscribe. */
  subscribe(listener: () => void): () => void;
}

export interface SubagentPollerOptions {
  store: SubagentTranscriptStore;
  key: string;
  fetch: () => Promise<WebUiSubagentTranscript>;
  pollMs: number;
  maxDelayMs: number;
  visibility: SubagentVisibility;
  input: SubagentPollInput;
  onState: (state: SubagentPollState) => void;
}

export interface SubagentPoller {
  start(): void;
  /** The host's facts changed (the call settled, or its summary grew). */
  update(input: SubagentPollInput): void;
  /** Ask again now, after a failure the user sees. */
  retry(): void;
  stop(): void;
}

const isNotFound = (error: unknown) => typeof error === 'object' && error !== null && (error as {status?: unknown}).status === 404;
const errorMessage = (error: unknown) => (error instanceof Error ? error.message : String(error));

export function createSubagentPoller(options: SubagentPollerOptions): SubagentPoller {
  const {store, key, pollMs, maxDelayMs, visibility} = options;
  let input = options.input;
  let stopped = false;
  let inFlight = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let unsubscribe: (() => void) | undefined;
  let failures = 0;
  let answer: WebUiSubagentTranscript | undefined;
  // The summary count a settled read was made for, so a summary the server never reaches
  // (a limit cut the snapshot) is not asked for again and again.
  let askedFor = -1;
  let state: SubagentPollState = {phase: 'loading'};

  const emit = (next: SubagentPollState) => {
    state = next;
    options.onState(next);
  };
  const clearTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };
  // Whether another read can still change what is shown: the child is going (last answer
  // `running`) or the call has not settled (its child may not have started or just ended).
  const wantsPoll = () => (answer === undefined ? !input.settled : answer.running === true || !input.settled);
  // A settled call whose summary counts more steps than the entry holds gets one more read.
  const behind = () =>
    answer !== undefined && answer.running !== true && input.settled && input.summarySteps > answer.steps.length && input.summarySteps !== askedFor;

  const schedule = (delay: number) => {
    clearTimer();
    if (stopped || !visibility.isVisible()) return;
    timer = setTimeout(() => {
      timer = undefined;
      if (visibility.isVisible()) fetchNow();
    }, delay);
  };
  const backoff = () => Math.min(maxDelayMs, pollMs * 2 ** Math.min(failures, 30));

  const fetchNow = () => {
    if (stopped || inFlight) return;
    clearTimer();
    inFlight = true;
    if (input.settled) askedFor = input.summarySteps;
    store.refresh(key, options.fetch).then(
      transcript => {
        inFlight = false;
        if (stopped) return;
        failures = 0;
        answer = transcript;
        emit({phase: 'ready', transcript});
        if (wantsPoll()) schedule(pollMs);
        else if (behind()) fetchNow();
      },
      (error: unknown) => {
        inFlight = false;
        if (stopped) return;
        if (isNotFound(error) && answer === undefined && !input.settled) {
          // The child has not made its folder yet: not a failure, so no backoff.
          emit({phase: 'starting'});
          schedule(pollMs);
          return;
        }
        failures += 1;
        const message = errorMessage(error);
        emit(answer === undefined ? {phase: 'failed', message} : {phase: 'ready', transcript: answer, retryError: message});
        if (wantsPoll()) schedule(backoff());
      },
    );
  };

  const onVisibility = () => {
    if (stopped) return;
    if (!visibility.isVisible()) {
      clearTimer();
      return;
    }
    // Back on screen: ask at once instead of waiting out the rest of a delay.
    if (!inFlight && (answer === undefined || wantsPoll() || behind())) fetchNow();
  };

  return {
    start() {
      stopped = false;
      unsubscribe = visibility.subscribe(onVisibility);
      const cached = store.peek(key);
      if (cached !== undefined) {
        answer = cached;
        emit({phase: 'ready', transcript: cached});
        // A finished child the summary does not outgrow is shown as it is: no request, no timer.
        if (cached.running !== true && input.settled && input.summarySteps <= cached.steps.length) return;
      } else {
        emit(state);
      }
      if (visibility.isVisible()) fetchNow();
    },
    update(next) {
      if (stopped || (next.settled === input.settled && next.summarySteps === input.summarySteps)) return;
      input = next;
      if (inFlight) return;
      if (behind()) {
        if (visibility.isVisible()) fetchNow();
        return;
      }
      if (!wantsPoll()) {
        clearTimer();
      } else if (timer === undefined) {
        schedule(pollMs);
      }
    },
    retry() {
      failures = 0;
      fetchNow();
    },
    stop() {
      stopped = true;
      clearTimer();
      unsubscribe?.();
      unsubscribe = undefined;
    },
  };
}
