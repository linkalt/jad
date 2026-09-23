import type {
  WebUiMessage,
  WebUiRunStatus,
  WebUiSessionRun,
} from "./plugins/web-ui/types.js";

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
 * newest normalized message snapshot. Every `ui.message` payload is already a
 * complete snapshot of the message, so re-attaching after a browser reload
 * means replaying the latest snapshot instead of buffering a delta history.
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

  /** Records the newest snapshot and forwards it to every subscriber. */
  public publishMessage(message: WebUiMessage): void {
    this.latest = message;
    this.publish("ui.message", {message});
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
 */
export class SessionRunRegistry {
  private readonly runs = new Map<string, SessionRun[]>();
  private readonly timers = new Set<NodeJS.Timeout>();

  public constructor(private readonly retentionMs: number) {}

  public begin(
    sessionRef: string,
    turnId: string,
    controller: AbortController,
  ): SessionRun {
    const run = new SessionRun(
      turnId,
      controller,
      new Date().toISOString(),
      (retired) => this.retire(sessionRef, retired),
    );
    const existing = this.runs.get(sessionRef);
    if (existing === undefined) {
      this.runs.set(sessionRef, [run]);
    } else {
      existing.push(run);
    }
    return run;
  }

  /**
   * The run a client should attach to: the active one when a run is in flight,
   * otherwise the most recently finished one that is still retained.
   */
  public current(sessionRef: string): SessionRun | undefined {
    const list = this.runs.get(sessionRef);
    if (list === undefined || list.length === 0) {
      return undefined;
    }
    // A queued run has not started yet, so the one that is executing is the
    // first running entry.
    return (
      list.find((run) => run.status === "running") ?? list[list.length - 1]
    );
  }

  public describe(sessionRef: string): WebUiSessionRun | undefined {
    return this.current(sessionRef)?.summary();
  }

  public stop(sessionRef: string): StopRunResult {
    const run = this.current(sessionRef);
    if (run === undefined || run.status !== "running") {
      return {stopped: false};
    }
    run.controller.abort(new Error("run stopped by the user"));
    return {stopped: true, turn_id: run.turnId};
  }

  public abortAll(reason: Error): void {
    for (const list of this.runs.values()) {
      for (const run of list) {
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

  private retire(sessionRef: string, run: SessionRun): void {
    const remove = (): void => {
      const list = this.runs.get(sessionRef);
      if (list === undefined) {
        return;
      }
      const remaining = list.filter((candidate) => candidate !== run);
      if (remaining.length === 0) {
        this.runs.delete(sessionRef);
      } else {
        this.runs.set(sessionRef, remaining);
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
