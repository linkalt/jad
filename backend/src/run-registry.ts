import type {
  WebUiInteraction,
  WebUiMessage,
  WebUiPart,
  WebUiRunStatus,
  WebUiSessionRun,
} from "./plugins/web-ui/types.js";
import { messageAppendIncrement, messageIncrements } from "./plugins/web-ui/incremental-web-ui.js";
import type {WebUiMessageAppend} from "./plugins/web-ui/incremental-web-ui.js";

import {InteractionBroker} from "./interactions.js";
import {RunContentCache, type RunContentCacheStagedReader} from "./run-content-cache.js";
import {DEFAULT_ROOT_SERVER_SETTINGS} from "./session-roots.js";

export interface RunStreamEvent {
  event: string;
  data: unknown;
}

export type RunListener = (event: RunStreamEvent) => void;

/** Callable detach handle; `ready` resolves after a disk-backed snapshot replays. */
export type RunSubscription = (() => void) & {ready: Promise<void>};

interface RunSubscriber {
  listener: RunListener;
  active: boolean;
  initializing: boolean;
  queued: RunStreamEvent[];
}

interface RetainedTerminalEvent extends RunStreamEvent {
  hasMessage: boolean;
}

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
 * no incremental form (the first state, a status change). Pending user decisions
 * are separate run state, replayed as `interaction` events and mirrored onto the
 * latest message for clients that consume message snapshots.
 */
export class SessionRun {
  public status: WebUiRunStatus = "running";
  private pendingInteraction: WebUiInteraction | undefined;
  private interactionKnown = false;
  private latestUsage: RunStreamEvent | undefined;
  private readonly listeners = new Set<RunSubscriber>();
  private terminal: RetainedTerminalEvent[] = [];
  private readonly contentCache: RunContentCache;
  private lastContentCacheCapacity: Promise<void> = Promise.resolve();
  private disposalPromise: Promise<void> | undefined;

  public constructor(
    public readonly turnId: string,
    public readonly controller: AbortController,
    private readonly startedAt: string,
    private readonly onRetire: (run: SessionRun) => void,
    cache: {
      directory?: string;
      memoryBytes: number;
      writeBatchBytes?: number;
      flushIntervalMs?: number;
      maxIndexSegments?: number;
    } = {
      memoryBytes: DEFAULT_ROOT_SERVER_SETTINGS.run_content_memory_bytes,
      writeBatchBytes: DEFAULT_ROOT_SERVER_SETTINGS.run_content_write_batch_bytes,
      flushIntervalMs: DEFAULT_ROOT_SERVER_SETTINGS.run_content_flush_interval_ms,
      maxIndexSegments: DEFAULT_ROOT_SERVER_SETTINGS.run_content_max_index_segments,
    },
  ) {
    this.contentCache = new RunContentCache(cache);
    this.interactions = new InteractionBroker((pending) => this.setPending(pending));
  }

  /** The question a tool is waiting on; its answer arrives over HTTP. */
  public readonly interactions: InteractionBroker;

  /** Latest cached state; content is blank only after the cache spills to disk. */
  public get latest(): WebUiMessage | undefined {
    return this.contentCache.metadata();
  }

  /** Read the complete latest message, including content held in the spill file. */
  public readLatestMessage(): Promise<WebUiMessage | undefined> {
    return this.contentCache.read();
  }

  public async beginResponseContentCache(
    directory: string,
    responseKey: string,
    baselineMessage?: WebUiMessage,
  ): Promise<void> {
    await this.contentCache.beginResponse(directory, responseKey, baselineMessage);
  }

  public async handoffStagedOutputItem(
    responseKey: string,
    outputIndex: number,
    readers: ReadonlyMap<number, RunContentCacheStagedReader>,
  ): Promise<void> {
    await this.contentCache.handoffOutputItem(responseKey, outputIndex, readers);
  }

  public setContentFallbackMessageProvider(provider: () => WebUiMessage | undefined): void {
    this.contentCache.setFallbackMessageProvider(provider);
  }

  public async waitForContentCacheCapacity(): Promise<void> {
    await this.lastContentCacheCapacity;
  }

  public async commitResponseContentCache(responseKey: string, message: WebUiMessage): Promise<void> {
    await this.contentCache.commitResponse(responseKey, message);
  }

  public summary(): WebUiSessionRun {
    return {
      turn_id: this.turnId,
      started_at: this.startedAt,
      status: this.status,
    };
  }

  /** Records the newest state and forwards what changed to every subscriber. */
  public async publishMessage(
    message: WebUiMessage,
    append?: WebUiMessageAppend,
    outputIndex?: number,
    partIndex?: number,
  ): Promise<void> {
    const nextMessage = this.pendingInteraction === undefined
      ? message
      : {...message, pending: this.pendingInteraction};
    const previous = this.contentCache.metadata();
    const wasSpilled = this.contentCache.isSpilled;
    const update = this.contentCache.update(nextMessage, append, outputIndex, partIndex);
    this.lastContentCacheCapacity = update.capacity;
    const stored = update.metadata;
    if (previous === undefined) {
      this.publish("ui.message", {message: nextMessage});
      return update.capacity;
    }

    if (wasSpilled) {
      if (!this.contentCache.isSpilled) {
        // The spill failed; the cache deliberately falls back to a complete
        // in-memory snapshot so the live response remains usable.
        this.publish("ui.message", {message: nextMessage});
      } else {
        this.publishSpilledUpdates(previous, stored, nextMessage, append, update.changedContent);
      }
      return update.capacity;
    }

    const appended = append === undefined
      ? undefined
      : messageAppendIncrement(previous, nextMessage, append);
    if (appended !== undefined) {
      this.publish(appended.event, appended.data);
      return update.capacity;
    }
    const increments = messageIncrements(previous, nextMessage);
    if (increments === undefined) {
      this.publish("ui.message", {message: nextMessage});
      return update.capacity;
    }
    for (const increment of increments) {
      this.publish(increment.event, increment.data);
    }
    return update.capacity;
  }

  private publishSpilledUpdates(
    previous: WebUiMessage,
    stored: WebUiMessage,
    nextMessage: WebUiMessage,
    append: WebUiMessageAppend | undefined,
    changedContent: ReadonlySet<number>,
  ): void {
    if (messageIncrements(previous, stored) === undefined) {
      this.publish("ui.message", {message: nextMessage});
      return;
    }

    for (let index = 0; index < stored.parts.length; index += 1) {
      const before = previous.parts[index];
      const after = stored.parts[index];
      const livePart = nextMessage.parts[index];
      if (after === undefined || livePart === undefined) {
        continue;
      }
      const metadataChanged = before === undefined || !samePartMetadata(before, after);
      const contentChanged = changedContent.has(index);
      if (!metadataChanged && !contentChanged) {
        continue;
      }

      const appended = append?.index === index && contentChanged
        ? messageAppendIncrement(previous, stored, append)
        : undefined;
      if (appended !== undefined) {
        this.publish(appended.event, appended.data);
      } else {
        this.publish("ui.part", {
          id: nextMessage.id,
          index,
          part: livePart,
        });
      }
    }
  }

  /**
   * Publishes the run-level interaction state and mirrors it onto the latest
   * message when available for clients that still consume message snapshots.
   * The dedicated event is retained even before the first message exists.
   */
  public setPending(pending: WebUiInteraction | undefined): void {
    if (pending === this.pendingInteraction) {
      return;
    }
    if (pending === undefined && this.pendingInteraction === undefined) {
      return;
    }
    this.pendingInteraction = pending;
    this.interactionKnown = true;
    this.publish("interaction", {turn_id: this.turnId, pending: pending ?? null});

    const current = this.contentCache.metadata();
    if (current === undefined) {
      return;
    }
    const next = pending === undefined ? withoutPending(current) : {...current, pending};
    if (next === current) {
      return;
    }
    this.contentCache.setMetadata(next);
    if (!this.contentCache.isSpilled) {
      this.publish("ui.message", {message: next});
      return;
    }

    const snapshot = this.contentCache.read();
    void snapshot.then((message) => {
      if (message !== undefined && this.status === "running" && this.pendingInteraction === pending) {
        this.publish("ui.message", {message});
      }
    }).catch((error: unknown) => {
      console.warn(`[web-ui-run-cache] pending snapshot unavailable: ${error instanceof Error ? error.message : String(error)}`);
    });
  }

  public publish(event: string, data: unknown): void {
    const published = {event, data};
    if (event === "usage") {
      this.latestUsage = published;
    }
    this.emit(published);
  }

  /**
   * Records the end of the run. Terminal snapshots retain message metadata but
   * leave large content in the same cache file used for mid-run reattachment.
   */
  public finish(
    status: Exclude<WebUiRunStatus, "running">,
    events: readonly RunStreamEvent[],
    finalMessage?: WebUiMessage,
  ): void {
    this.interactions.cancel("The run ended before the draft was answered.");
    this.status = status;
    this.terminal = events.map(retainTerminalEvent);
    void this.contentCache.finish(finalMessage === undefined ? undefined : withoutPending(finalMessage)).catch((error: unknown) => {
      console.warn(`[web-ui-run-cache] terminal cleanup failed: ${error instanceof Error ? error.message : String(error)}`);
    });
    for (const event of events) {
      this.emit(event);
    }
    this.listeners.clear();
    this.onRetire(this);
  }

  /**
   * A running run replays its latest message, known interaction state and usage,
   * then streams new events. A finished run replays its terminal events.
   * `ready` resolves when a disk-backed initial snapshot has been hydrated.
   */
  public subscribe(listener: RunListener): RunSubscription {
    const subscriptionState: {active: boolean; subscriber?: RunSubscriber} = {active: true};
    const subscription = (() => {
      subscriptionState.active = false;
      if (subscriptionState.subscriber !== undefined) {
        subscriptionState.subscriber.active = false;
        this.listeners.delete(subscriptionState.subscriber);
      }
    }) as RunSubscription;
    subscription.ready = Promise.resolve();

    if (this.status !== "running") {
      if (this.latestUsage !== undefined) {
        listener(this.latestUsage);
      }
      if (this.contentCache.isSpilled && this.terminal.some((event) => event.hasMessage)) {
        subscription.ready = this.contentCache.read().then((message) => {
          if (subscriptionState.active) {
            this.replayTerminal(listener, message);
          }
        }).catch((error: unknown) => {
          console.warn(`[web-ui-run-cache] terminal snapshot unavailable: ${error instanceof Error ? error.message : String(error)}`);
          if (subscriptionState.active) {
            this.replayTerminal(listener, this.latest);
          }
        });
      } else {
        this.replayTerminal(listener, this.latest);
      }
      return subscription;
    }

    const subscriber: RunSubscriber = {
      listener,
      active: true,
      initializing: this.contentCache.isSpilled,
      queued: [],
    };
    subscriptionState.subscriber = subscriber;
    this.listeners.add(subscriber);
    if (!subscriber.initializing) {
      this.replayLiveState(listener, this.latest, this.interactionKnown, this.pendingInteraction, this.latestUsage);
      return subscription;
    }

    const snapshot = this.contentCache.read();
    const interactionKnown = this.interactionKnown;
    const pendingInteraction = this.pendingInteraction;
    const latestUsage = this.latestUsage;
    subscription.ready = this.replayRunning(subscriber, snapshot, interactionKnown, pendingInteraction, latestUsage);
    return subscription;
  }

  /** Release the temporary run cache when retention ends or the registry closes. */
  public dispose(): Promise<void> {
    this.disposalPromise ??= this.contentCache.dispose();
    return this.disposalPromise;
  }

  private async replayRunning(
    subscriber: RunSubscriber,
    snapshot: Promise<WebUiMessage | undefined>,
    interactionKnown: boolean,
    pendingInteraction: WebUiInteraction | undefined,
    latestUsage: RunStreamEvent | undefined,
  ): Promise<void> {
    let message: WebUiMessage | undefined;
    try {
      message = await snapshot;
    } catch (error) {
      console.warn(`[web-ui-run-cache] running snapshot unavailable: ${error instanceof Error ? error.message : String(error)}`);
      message = this.latest;
    }
    if (!subscriber.active) {
      return;
    }
    this.replayLiveState(subscriber.listener, message, interactionKnown, pendingInteraction, latestUsage);
    subscriber.initializing = false;
    for (const event of subscriber.queued.splice(0)) {
      if (subscriber.active) {
        subscriber.listener(event);
      }
    }
  }

  private replayLiveState(
    listener: RunListener,
    message: WebUiMessage | undefined,
    interactionKnown: boolean,
    pendingInteraction: WebUiInteraction | undefined,
    latestUsage: RunStreamEvent | undefined,
  ): void {
    if (message !== undefined) {
      listener({event: "ui.message", data: {message}});
    }
    if (interactionKnown) {
      listener({
        event: "interaction",
        data: {turn_id: this.turnId, pending: pendingInteraction ?? null},
      });
    }
    if (latestUsage !== undefined) {
      listener(latestUsage);
    }
  }

  private replayTerminal(listener: RunListener, message: WebUiMessage | undefined): void {
    for (const event of this.terminal) {
      if (!event.hasMessage || message === undefined || !isRecord(event.data)) {
        listener({event: event.event, data: event.data});
        continue;
      }
      listener({event: event.event, data: {...event.data, message}});
    }
  }

  private emit(event: RunStreamEvent): void {
    for (const subscriber of [...this.listeners]) {
      if (!subscriber.active) {
        continue;
      }
      if (subscriber.initializing) {
        subscriber.queued.push(event);
      } else {
        subscriber.listener(event);
      }
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

  public constructor(
    private readonly retentionMs: number,
    private readonly runContentMemoryBytes = DEFAULT_ROOT_SERVER_SETTINGS.run_content_memory_bytes,
    private readonly runContentWriteBatchBytes = DEFAULT_ROOT_SERVER_SETTINGS.run_content_write_batch_bytes,
    private readonly runContentFlushIntervalMs: number = DEFAULT_ROOT_SERVER_SETTINGS.run_content_flush_interval_ms,
    private readonly runContentMaxIndexSegments: number = DEFAULT_ROOT_SERVER_SETTINGS.run_content_max_index_segments,
  ) {}

  private static key(sessionRef: string, profile: string | undefined): string {
    return profile === undefined ? sessionRef : `${sessionRef}|${profile}`;
  }

  public begin(
    sessionRef: string,
    profile: string | undefined,
    turnId: string,
    controller: AbortController,
    cacheDirectory?: string,
  ): SessionRun {
    const run = new SessionRun(
      turnId,
      controller,
      new Date().toISOString(),
      (retired) => this.retire(sessionRef, profile, retired),
      {
        ...(cacheDirectory === undefined ? {} : {directory: cacheDirectory}),
        memoryBytes: this.runContentMemoryBytes,
        writeBatchBytes: this.runContentWriteBatchBytes,
        flushIntervalMs: this.runContentFlushIntervalMs,
        maxIndexSegments: this.runContentMaxIndexSegments,
      },
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
    for (const slot of this.runs.values()) {
      for (const run of slot.list) {
        void run.dispose().catch((error: unknown) => {
          console.warn(`[web-ui-run-cache] disposal failed: ${error instanceof Error ? error.message : String(error)}`);
        });
      }
    }
    this.runs.clear();
  }

  private retire(sessionRef: string, profile: string | undefined, run: SessionRun): void {
    const remove = (): void => {
      const key = SessionRunRegistry.key(sessionRef, profile);
      const slot = this.runs.get(key);
      if (slot !== undefined) {
        const remaining = slot.list.filter((candidate) => candidate !== run);
        if (remaining.length === 0) {
          this.runs.delete(key);
        } else {
          slot.list = remaining;
        }
      }
      void run.dispose().catch((error: unknown) => {
        console.warn(`[web-ui-run-cache] disposal failed: ${error instanceof Error ? error.message : String(error)}`);
      });
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

function retainTerminalEvent(event: RunStreamEvent): RetainedTerminalEvent {
  if (isRecord(event.data) && isWebUiMessage(event.data.message)) {
    const {message: _message, ...data} = event.data;
    return {event: event.event, data, hasMessage: true};
  }
  return {event: event.event, data: event.data, hasMessage: false};
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isWebUiMessage(value: unknown): value is WebUiMessage {
  return isRecord(value) &&
    typeof value.id === "string" &&
    typeof value.text === "string" &&
    Array.isArray(value.parts);
}

function samePartMetadata(left: WebUiPart, right: WebUiPart): boolean {
  return JSON.stringify(partMetadata(left)) === JSON.stringify(partMetadata(right));
}

function partMetadata(part: WebUiPart): unknown {
  switch (part.type) {
    case "text":
    case "reasoning": {
      const {text: _text, ...metadata} = part;
      return metadata;
    }
    case "tool_call": {
      const {arguments: _arguments, ...metadata} = part;
      return metadata;
    }
    case "tool_result": {
      const {output: _output, ...metadata} = part;
      return metadata;
    }
  }
}

/** The message without its pending question, or the same message when it has none. */
function withoutPending(message: WebUiMessage): WebUiMessage {
  if (message.pending === undefined) {
    return message;
  }
  const {pending: _pending, ...rest} = message;
  return rest;
}

interface SessionRunSlot {
  sessionRef: string;
  /** Absent when the session configures no path profiles. */
  profile: string | undefined;
  list: SessionRun[];
}
