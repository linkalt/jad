import type {
  WebUiMessage,
  WebUiRunStatus,
  WebUiSessionRun,
} from "./plugins/web-ui/types.js";
import { messageAppendIncrement, messageIncrements } from "./plugins/web-ui/incremental-web-ui.js";
import type {WebUiMessageAppend} from "./plugins/web-ui/incremental-web-ui.js";

export interface RunStreamEvent {
  event: string;
  data: unknown;
}

export type RunListener = (event: RunStreamEvent) => void;

export interface StopRunResult {
  stopped: boolean;
  turn_id?: string;
}

/**
 * One accepted run.
 *
 * A run outlives the HTTP request that started it, so the registry keeps the
 * newest normalized message. Subscribers receive only what changed since the
 * last published state — an appended part as `ui.delta`, a replaced or added
 * part as `ui.part` — and a full `ui.message` snapshot whenever the change has
 * no incremental form (the first state, a status change). Attaching after a
 * reload replays the newest snapshot, so a client that missed every delta is
 * still correct from that point on.
 */
export class SessionRun {
  public status: WebUiRunStatus = "running";
  public latest?: WebUiMessage;
  private readonly listeners = new Set<RunListener>();
  private terminal: RunStreamEvent[] = [];

  public constructor(
    public readonly turnId: string,
    public readonly controller: AbortController,
    private readonly startedAt: string,
    private readonly onRetire: (run: SessionRun) => void,
  ) {}

  public summary(): WebUiSessionRun {
    return {
      turn_id: this.turnId,
      started_at: this.startedAt,
      status: this.status,
    };
  }

  /** Records the newest state and forwards what changed to every subscriber. */
  public publishMessage(message: WebUiMessage, append?: WebUiMessageAppend): void {
    const previous = this.latest;
    this.latest = message;
    if (previous === undefined) {
      this.publish("ui.message", {message});
      return;
    }
    const appended = append === undefined
      ? undefined
      : messageAppendIncrement(previous, message, append);
    if (appended !== undefined) {
      this.publish(appended.event, appended.data);
      return;
    }
    const increments = messageIncrements(previous, message);
    if (increments === undefined) {
      this.publish("ui.message", {message});
      return;
    }
    for (const increment of increments) {
      this.publish(increment.event, increment.data);
    }
  }

  public publish(event: string, data: unknown): void {
    this.emit({event, data});
  }

  /**
   * Records the end of the run. The terminal events are replayed to listeners
   * that attach afterwards, so a reload that lands just after completion still
   * receives the final message and the `completed`/`done` pair instead of
   * silence.
   */
  public finish(
    status: Exclude<WebUiRunStatus, "running">,
    events: readonly RunStreamEvent[],
    finalMessage?: WebUiMessage,
  ): void {
    this.status = status;
    if (finalMessage !== undefined) {
      this.latest = finalMessage;
    }
    this.terminal = [...events];
    for (const event of events) {
      this.emit(event);
    }
    this.listeners.clear();
    this.onRetire(this);
  }

  /**
   * A running run replays its latest snapshot and then streams new events; a
   * finished one replays its recorded terminal events and produces nothing else.
   */
  public subscribe(listener: RunListener): () => void {
    if (this.status !== "running") {
      for (const event of this.terminal) {
        listener(event);
      }
      return () => {};
    }
    this.listeners.add(listener);
    if (this.latest !== undefined) {
      listener({event: "ui.message", data: {message: this.latest}});
    }
    return () => {
      this.listeners.delete(listener);
    };
  }

  private emit(event: RunStreamEvent): void {
    for (const listener of [...this.listeners]) {
      listener(event);
    }
  }
}

/**
 * Tracks the runs of every session so a client can discover an in-flight run,
 * attach to its event stream, and stop it explicitly — none of which is possible
 * from the request that started it once that request is gone.
 *
 * A run belongs to a `(session, profile)` pair: two profiles of one session keep
 * separate transcripts and separate run slots, so both can run at the same time.
 * An absent profile means "this session configures no profiles" and is the shape
 * every session had before profiles were addressable.
 */
export class SessionRunRegistry {
  private readonly runs = new Map<string, SessionRunSlot>();
  private readonly timers = new Set<NodeJS.Timeout>();

  public constructor(private readonly retentionMs: number) {}

  private static key(sessionRef: string, profile: string | undefined): string {
    return profile === undefined ? sessionRef : `${sessionRef}|${profile}`;
  }

  public begin(
    sessionRef: string,
    profile: string | undefined,
    turnId: string,
    controller: AbortController,
  ): SessionRun {
    const run = new SessionRun(
      turnId,
      controller,
      new Date().toISOString(),
      (retired) => this.retire(sessionRef, profile, retired),
    );
    const key = SessionRunRegistry.key(sessionRef, profile);
    const existing = this.runs.get(key);
    if (existing === undefined) {
      this.runs.set(key, {sessionRef, profile, list: [run]});
    } else {
      existing.list.push(run);
    }
    return run;
  }

  /**
   * The run a client should attach to: the active one when a run is in flight,
   * otherwise the most recently finished one that is still retained.
   */
  public current(sessionRef: string, profile: string | undefined): SessionRun | undefined {
    const slot = this.runs.get(SessionRunRegistry.key(sessionRef, profile));
    if (slot === undefined || slot.list.length === 0) {
      return undefined;
    }
    // A queued run has not started yet, so the one that is executing is the
    // first running entry.
    return (
      slot.list.find((run) => run.status === "running") ?? slot.list[slot.list.length - 1]
    );
  }

  public describe(sessionRef: string, profile: string | undefined): WebUiSessionRun | undefined {
    return this.current(sessionRef, profile)?.summary();
  }

  /**
   * Every run of a session that is running or still retained, keyed by profile.
   *
   * The list endpoint needs this: a session with runs in two profiles shows one
   * `run` for its stored default plus the rest here, so a client can render a
   * status per profile without opening the session.
   */
  public describeAll(sessionRef: string): Record<string, WebUiSessionRun> | undefined {
    let runs: Record<string, WebUiSessionRun> | undefined;
    for (const slot of this.runs.values()) {
      if (slot.sessionRef !== sessionRef || slot.profile === undefined) {
        continue;
      }
      const current = this.current(sessionRef, slot.profile);
      if (current === undefined) {
        continue;
      }
      runs ??= {};
      runs[slot.profile] = current.summary();
    }
    return runs;
  }

  /**
   * Every run of a session in any profile, for a fingerprint that must notice a
   * run starting or stopping in a profile the caller does not address.
   */
  public runsOf(sessionRef: string): WebUiSessionRun[] {
    const runs: WebUiSessionRun[] = [];
    for (const slot of this.runs.values()) {
      if (slot.sessionRef !== sessionRef) {
        continue;
      }
      const current = this.current(sessionRef, slot.profile);
      if (current !== undefined) {
        runs.push(current.summary());
      }
    }
    return runs;
  }

  public stop(sessionRef: string, profile: string | undefined): StopRunResult {
    const run = this.current(sessionRef, profile);
    if (run === undefined || run.status !== "running") {
      return {stopped: false};
    }
    run.controller.abort(new Error("run stopped by the user"));
    return {stopped: true, turn_id: run.turnId};
  }

  public abortAll(reason: Error): void {
    for (const slot of this.runs.values()) {
      for (const run of slot.list) {
        if (run.status === "running") {
          run.controller.abort(reason);
        }
      }
    }
  }

  public clear(): void {
    for (const timer of this.timers) {
      clearTimeout(timer);
    }
    this.timers.clear();
    this.runs.clear();
  }

  private retire(sessionRef: string, profile: string | undefined, run: SessionRun): void {
    const remove = (): void => {
      const key = SessionRunRegistry.key(sessionRef, profile);
      const slot = this.runs.get(key);
      if (slot === undefined) {
        return;
      }
      const remaining = slot.list.filter((candidate) => candidate !== run);
      if (remaining.length === 0) {
        this.runs.delete(key);
      } else {
        slot.list = remaining;
      }
    };

    if (this.retentionMs <= 0) {
      remove();
      return;
    }
    const timer = setTimeout(() => {
      this.timers.delete(timer);
      remove();
    }, this.retentionMs);
    timer.unref?.();
    this.timers.add(timer);
  }
}

interface SessionRunSlot {
  sessionRef: string;
  /** Absent when the session configures no path profiles. */
  profile: string | undefined;
  list: SessionRun[];
}
